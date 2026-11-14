import { expect, test, spyOn } from 'bun:test'
import type { SetStateAction } from 'react'
import { RCSChatAdapter } from '../lib/rcs-chat-adapter'
import type { SessionUsage } from '../lib/rcs-chat-adapter'
import type { ThreadEntry } from '../lib/types'

test('shows a message written in the local terminal', () => {
  let entries: ThreadEntry[] = []
  const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
    entries = typeof action === 'function' ? action(entries) : action
  }
  const adapter = new RCSChatAdapter('session', setEntries)
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: { uuid: 'terminal-message', content: 'Hello from terminal' },
  })
  expect(entries).toEqual([
    {
      type: 'user_message',
      id: 'user-terminal-message',
      content: 'Hello from terminal',
    },
  ])
})

test('shows an image-only terminal message on another device', () => {
  let entries: ThreadEntry[] = []
  const adapter = new RCSChatAdapter('session', action => {
    entries = typeof action === 'function' ? action(entries) : action
  })
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: {
      uuid: 'photo-id',
      content: '',
      message: {
        content: [
          {
            type: 'image',
            source: { type: 'base64', media_type: 'image/png', data: 'aGk=' },
          },
        ],
      },
    },
  })
  expect(entries).toEqual([
    {
      type: 'user_message',
      id: 'user-photo-id',
      content: '',
      images: [{ mimeType: 'image/png', data: 'aGk=' }],
    },
  ])
})

test('clears pending when an image-only message is echoed', () => {
  let entries: ThreadEntry[] = [
    {
      type: 'user_message',
      id: 'user-photo-id',
      content: '',
      pending: true,
      images: [{ mimeType: 'image/png', data: 'aGk=' }],
    },
  ]
  const adapter = new RCSChatAdapter('session', action => {
    entries = typeof action === 'function' ? action(entries) : action
  })
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: {
      uuid: 'photo-id',
      content: '',
      message: {
        content: [
          {
            type: 'image',
            source: { type: 'base64', media_type: 'image/png', data: 'aGk=' },
          },
        ],
      },
    },
  })
  expect(entries).toHaveLength(1)
  expect(entries[0]?.type === 'user_message' && entries[0].pending).toBe(false)
})

test('does not duplicate a web message echoed by the terminal', () => {
  let entries: ThreadEntry[] = [
    { type: 'user_message', id: 'user-message-1', content: 'Hello from web' },
  ]
  const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
    entries = typeof action === 'function' ? action(entries) : action
  }
  const adapter = new RCSChatAdapter('session', setEntries)
  adapter.handleEvent({
    type: 'user',
    direction: 'outbound',
    payload: { uuid: 'message-1', content: 'Hello from web' },
  })
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: { uuid: 'message-1', content: 'Hello from web' },
  })
  expect(entries).toHaveLength(1)
})

test('keeps both bubbles when the person repeats the same text in the terminal', () => {
  let entries: ThreadEntry[] = []
  const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
    entries = typeof action === 'function' ? action(entries) : action
  }
  const adapter = new RCSChatAdapter('session', setEntries)
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: { uuid: 'uuid-1', content: '好' },
  })
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: { uuid: 'uuid-2', content: '好' },
  })
  expect(entries).toHaveLength(2)
  expect(
    entries
      .filter(entry => entry.type === 'user_message')
      .map(entry => entry.id),
  ).toEqual(['user-uuid-1', 'user-uuid-2'])
})

test('keeps literal angle-bracket prose verbatim so it matches its history twin', () => {
  let entries: ThreadEntry[] = []
  const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
    entries = typeof action === 'function' ? action(entries) : action
  }
  const adapter = new RCSChatAdapter('session', setEntries)
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: { uuid: 'tagged', content: 'run <b>npm test</b> now' },
  })
  expect(entries).toEqual([
    {
      type: 'user_message',
      id: 'user-tagged',
      content: 'run <b>npm test</b> now',
    },
  ])
})

test('shows the command name for a slash-command echo', () => {
  let entries: ThreadEntry[] = []
  const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
    entries = typeof action === 'function' ? action(entries) : action
  }
  const adapter = new RCSChatAdapter('session', setEntries)
  adapter.handleEvent({
    type: 'user',
    direction: 'inbound',
    payload: { uuid: 'cmd', content: '<command-name>commit</command-name>' },
  })
  expect(entries).toEqual([
    { type: 'user_message', id: 'user-cmd', content: 'commit' },
  ])
})

test('adopts a rewritten-uuid echo even after pending state was cleared', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () =>
    Response.json({ ok: true })) as unknown as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
      entries = typeof action === 'function' ? action(entries) : action
    }
    const adapter = new RCSChatAdapter('session', setEntries)
    await adapter.sendMessage('继续')
    // An assistant chunk proves the queue was consumed: clearStalePending
    // strips the optimistic bubble's pending flag before any echo lands.
    adapter.handleEvent({
      type: 'assistant',
      direction: 'inbound',
      payload: { uuid: 'a1', content: 'working' },
    })
    expect(
      entries.some(entry => entry.type === 'user_message' && entry.pending),
    ).toBe(false)
    // A server restart rewrote the echo's uuid: adoption must still find the
    // optimistic bubble (via the adapter's own id set), not append a twin.
    adapter.handleEvent({
      type: 'user',
      direction: 'inbound',
      payload: { uuid: 'rewritten-echo', content: '继续' },
    })
    const bubbles = entries.filter(entry => entry.type === 'user_message')
    expect(bubbles).toHaveLength(1)
    expect(bubbles[0]?.id).toBe('user-rewritten-echo')
    expect(bubbles[0]?.content).toBe('继续')
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('adoption updater stays deterministic under StrictMode double-invocation', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () =>
    Response.json({ ok: true })) as unknown as typeof fetch)
  try {
    // React StrictMode (dev) invokes each state updater twice; the adapter's
    // adoption decision must not observe its own set-draining side effect.
    let entries: ThreadEntry[] = []
    let doubleInvoke = false
    const apply = (action: SetStateAction<ThreadEntry[]>) => {
      if (!doubleInvoke || typeof action !== 'function') {
        entries = typeof action === 'function' ? action(entries) : action
        return
      }
      const first = action(entries)
      const second = action(entries)
      expect(second).toEqual(first)
      entries = second
    }
    const adapter = new RCSChatAdapter('session', apply)
    await adapter.sendMessage('GO-ON')
    adapter.handleEvent({
      type: 'assistant',
      direction: 'inbound',
      payload: { uuid: 'a1', content: 'working' },
    })
    doubleInvoke = true
    adapter.handleEvent({
      type: 'user',
      direction: 'inbound',
      payload: { uuid: 'rewritten', content: 'GO-ON' },
    })
    doubleInvoke = false
    const bubbles = entries.filter(entry => entry.type === 'user_message')
    expect(bubbles).toHaveLength(1)
    expect(bubbles[0]?.id).toBe('user-rewritten')
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('seeds usage from history and does not double-count live echoes', () => {
  let entries: ThreadEntry[] = []
  const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
    entries = typeof action === 'function' ? action(entries) : action
  }
  const seen: SessionUsage[] = []
  const adapter = new RCSChatAdapter('session', setEntries, {
    onUsage: usage => seen.push(usage),
  })
  adapter.seedUsageFromHistory([
    {
      id: 'm1',
      usage: {
        inputTokens: 100,
        outputTokens: 10,
        cacheReadInputTokens: 300,
        cacheCreationInputTokens: 100,
      },
    },
  ])
  // Conversation average = cache reads / (input + write + read) = 300/500.
  const seeded = seen[seen.length - 1]!
  expect(seeded.cacheHitRate).toBeCloseTo(0.6)
  expect(seeded.totals).toEqual({ input: 100, output: 10 })

  // The live stream replays the same assistant message (same uuid): the
  // ledger is keyed by id, so it overwrites the seed instead of adding to it.
  adapter.handleEvent({
    type: 'assistant',
    direction: 'inbound',
    payload: {
      uuid: 'm1',
      content: 'hi',
      message: {
        usage: {
          input_tokens: 100,
          output_tokens: 10,
          cache_read_input_tokens: 300,
          cache_creation_input_tokens: 100,
        },
      },
    },
  })
  const live = seen[seen.length - 1]!
  expect(live.totals).toEqual({ input: 100, output: 10 })
  expect(live.cacheHitRate).toBeCloseTo(0.6)
})

test('uses the sent message UUID to deduplicate a bridge echo', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  let sentBody: { uuid: string } | undefined
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    _input,
    init,
  ) => {
    sentBody = JSON.parse(String(init?.body))
    return Response.json({ ok: true })
  }) as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
      entries = typeof action === 'function' ? action(entries) : action
    }
    const adapter = new RCSChatAdapter('session', setEntries)
    await adapter.sendMessage('Hello from web')
    expect(sentBody?.uuid).toBeDefined()
    adapter.handleEvent({
      type: 'user',
      direction: 'inbound',
      payload: { uuid: sentBody?.uuid, content: 'Hello from web' },
    })
    expect(entries).toHaveLength(1)
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('sends eight large photos once each without exceeding a 32 MiB proxy limit', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  let body = ''
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    _input,
    init,
  ) => {
    body = String(init?.body)
    return Response.json({ ok: true })
  }) as typeof fetch)
  try {
    const adapter = new RCSChatAdapter('session', () => {})
    const images = Array.from({ length: 8 }, (_, i) => ({
      mimeType: 'image/jpeg',
      data: `${i}${'A'.repeat(Math.ceil((2 * 1024 * 1024 * 4) / 3))}`,
    }))
    await adapter.sendMessage('photos', images)
    expect(Buffer.byteLength(body)).toBeLessThan(32 * 1024 * 1024)
    const payload = JSON.parse(body)
    expect(payload.message).toBeUndefined()
    expect(
      payload.content.filter(
        (block: { type: string }) => block.type === 'image',
      ),
    ).toHaveLength(8)
    expect(payload.content[1].source.data).toBe(images[0]?.data)
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('dismisses a permission card answered from the web or resolved in the terminal', () => {
  const resolved: string[] = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onPermissionResolved: requestId => resolved.push(requestId),
  })
  // Web answer published as an outbound permission_response.
  adapter.handleEvent({
    type: 'permission_response',
    direction: 'outbound',
    payload: { approved: true, request_id: 'req-web' } as any,
  })
  // CLI resolved it locally (terminal, hook, classifier) and cancelled it.
  adapter.handleEvent({
    type: 'control_cancel_request',
    direction: 'inbound',
    payload: { request_id: 'req-local', session_id: 'session' } as any,
  })
  expect(resolved).toEqual(['req-web', 'req-local'])
})

async function replayHistory(
  events: Array<Record<string, unknown>>,
  options?: ConstructorParameters<typeof RCSChatAdapter>[2],
): Promise<void> {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () =>
    Response.json({ events })) as typeof fetch)
  try {
    const adapter = new RCSChatAdapter('session', () => {}, options)
    await adapter.loadHistory()
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
}

const askRequestEvent = {
  id: 'e1',
  type: 'control_request',
  direction: 'inbound',
  payload: {
    request_id: 'req-1',
    request: {
      subtype: 'can_use_tool',
      tool_name: 'AskUserQuestion',
      input: { questions: [] },
      description: '',
    },
  },
}

test('does not re-raise a permission card that the web already answered', async () => {
  const requested: string[] = []
  await replayHistory(
    [
      askRequestEvent,
      {
        id: 'e2',
        type: 'permission_response',
        direction: 'outbound',
        payload: { approved: true, request_id: 'req-1' },
      },
    ],
    {
      onPermissionRequest: permission => requested.push(permission.requestId),
    },
  )
  expect(requested).toEqual([])
})

test('does not re-raise a permission card the terminal resolved locally', async () => {
  const requested: string[] = []
  await replayHistory(
    [
      askRequestEvent,
      {
        id: 'e2',
        type: 'control_cancel_request',
        direction: 'inbound',
        payload: { request_id: 'req-1', session_id: 'session' },
      },
    ],
    {
      onPermissionRequest: permission => requested.push(permission.requestId),
    },
  )
  expect(requested).toEqual([])
})

test('still re-raises a permission card that is genuinely unanswered', async () => {
  const requested: string[] = []
  await replayHistory([askRequestEvent], {
    onPermissionRequest: permission => requested.push(permission.requestId),
  })
  expect(requested).toEqual(['req-1'])
})

test('a resync completes a running tool card whose tool_use left the replay window', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  let historyEvents: unknown[] = []
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    input: RequestInfo | URL,
  ) => {
    if (String(input).includes('/history')) {
      return Response.json({ events: historyEvents })
    }
    return Response.json({ ok: true })
  }) as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    // Live: the Bash card is on screen and still running.
    adapter.handleEvent({
      type: 'assistant',
      direction: 'inbound',
      payload: {
        uuid: 'a1',
        message: {
          content: [
            { type: 'text', text: 'running' },
            {
              type: 'tool_use',
              id: 'tool-1',
              name: 'Bash',
              input: { command: 'ls' },
            },
          ],
        },
      },
    } as any)
    // The reconnect replay only kept the result; the tool_use event was
    // evicted from the rolling history.
    historyEvents = [
      {
        id: 'e1',
        type: 'user',
        direction: 'inbound',
        payload: {
          uuid: 'u2',
          message: {
            content: [
              {
                type: 'tool_result',
                tool_use_id: 'tool-1',
                content: 'file1\nfile2',
              },
            ],
          },
        },
      },
    ]
    await adapter.loadHistory()
    const card = entries.find(
      entry => entry.type === 'tool_call' && entry.toolCall.id === 'tool-1',
    )
    expect(card?.type === 'tool_call' && card.toolCall.status).toBe('complete')
    expect(card?.type === 'tool_call' && card.toolCall.rawOutput).toEqual({
      output: 'file1\nfile2',
    })
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('a resync keeps the sent message above the reply it triggered', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  let historyEvents: unknown[] = []
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    input: RequestInfo | URL,
  ) => {
    if (String(input).includes('/history')) {
      return Response.json({ events: historyEvents })
    }
    return Response.json({ ok: true })
  }) as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    await adapter.sendMessage('还有会话模式似乎没有自动的模式')
    // The echo was lost in an SSE gap; the reply it triggered lands first and
    // clears the optimistic bubble's pending flag.
    adapter.handleEvent({
      type: 'assistant',
      direction: 'inbound',
      payload: { uuid: 'a1', content: '明白了' },
    })
    // Resync replay: the echo arrives now, under the server-side uuid.
    historyEvents = [
      {
        id: 'e1',
        type: 'user',
        direction: 'inbound',
        payload: {
          uuid: 'server-uuid',
          content: '还有会话模式似乎没有自动的模式',
        },
      },
      {
        id: 'e2',
        type: 'assistant',
        direction: 'inbound',
        payload: { uuid: 'a1', content: '明白了' },
      },
    ]
    await adapter.loadHistory()
    const shape = entries.map(entry =>
      entry.type === 'user_message'
        ? `user:${entry.content}`
        : entry.type === 'assistant_message'
          ? 'assistant'
          : entry.type,
    )
    expect(shape).toEqual(['user:还有会话模式似乎没有自动的模式', 'assistant'])
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})
