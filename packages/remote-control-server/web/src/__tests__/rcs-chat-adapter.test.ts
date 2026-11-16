import { expect, test, spyOn } from 'bun:test'
import type { SetStateAction } from 'react'
import { RCSChatAdapter } from '../lib/rcs-chat-adapter'
import type { SessionUsage } from '../lib/rcs-chat-adapter'
import type { ThreadEntry } from '../lib/types'

test('terminal restore removes only the original prompt, even if old echo arrives late', () => {
  let entries: ThreadEntry[] = []
  const adapter = new RCSChatAdapter('session', action => {
    entries = typeof action === 'function' ? action(entries) : action
  })
  const user = (uuid: string) => ({
    type: 'user',
    direction: 'inbound' as const,
    payload: { uuid, content: 'same text' },
  })
  adapter.handleEvent(user('old-id'))
  adapter.handleEvent({
    type: 'user_message_retracted',
    direction: 'inbound',
    payload: { raw: { message_uuid: 'old-id' } },
  })
  adapter.handleEvent(user('new-id'))
  adapter.handleEvent(user('old-id'))
  expect(
    entries
      .filter(entry => entry.type === 'user_message')
      .map(entry => entry.id),
  ).toEqual(['user-new-id'])
  adapter.handleEvent(user('another-id'))
  expect(entries.filter(entry => entry.type === 'user_message')).toHaveLength(2)
})

test('history replay does not revive a terminal-retracted prompt', async () => {
  const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () =>
    Response.json({
      events: [
        {
          type: 'user',
          direction: 'outbound',
          payload: { uuid: 'old', content: 'hello' },
        },
        {
          type: 'user',
          direction: 'inbound',
          payload: { uuid: 'old', content: 'hello' },
        },
        {
          type: 'user_message_retracted',
          direction: 'inbound',
          payload: { raw: { message_uuid: 'old' } },
        },
        {
          type: 'user',
          direction: 'inbound',
          payload: { uuid: 'new', content: 'hello' },
        },
      ],
    })) as unknown as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    await adapter.loadHistory()
    expect(
      entries
        .filter(entry => entry.type === 'user_message')
        .map(entry => entry.id),
    ).toEqual(['user-new'])
  } finally {
    fetchSpy.mockRestore()
    if (storage) Object.defineProperty(globalThis, 'localStorage', storage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('reconnect replay removes an already displayed prompt after a missed retraction', async () => {
  const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () =>
    Response.json({
      events: [
        {
          type: 'user_message_retracted',
          direction: 'inbound',
          payload: { raw: { message_uuid: 'old' } },
        },
      ],
    })) as unknown as typeof fetch)
  try {
    for (const historyMode of ['full', 'metadata'] as const) {
      let entries: ThreadEntry[] = [
        { type: 'user_message', id: 'user-old', content: 'hello' },
      ]
      const adapter = new RCSChatAdapter(
        'session',
        action => {
          entries = typeof action === 'function' ? action(entries) : action
        },
        { historyMode },
      )
      await adapter.loadHistory()
      expect(entries).toEqual([])
    }
  } finally {
    fetchSpy.mockRestore()
    if (storage) Object.defineProperty(globalThis, 'localStorage', storage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('history keeps queued identical sends after the confirmed prompt and reply', async () => {
  const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () =>
    Response.json({
      events: [
        {
          type: 'user',
          direction: 'outbound',
          payload: { uuid: 'first', content: 'hello' },
        },
        {
          type: 'user',
          direction: 'outbound',
          payload: { uuid: 'second', content: 'hello' },
        },
        {
          type: 'user',
          direction: 'inbound',
          payload: { uuid: 'first', content: 'hello' },
        },
        {
          type: 'assistant',
          direction: 'inbound',
          payload: { uuid: 'answer', content: 'working' },
        },
      ],
    })) as unknown as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    await adapter.loadHistory()
    expect(entries.map(entry => entry.id)).toEqual([
      'user-first',
      'assistant-answer',
      'user-second',
    ])
  } finally {
    fetchSpy.mockRestore()
    if (storage) Object.defineProperty(globalThis, 'localStorage', storage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('propagates a terminal session rename to the current view', () => {
  const titles: string[] = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onSessionTitle: title => titles.push(title),
  })
  adapter.handleEvent({
    type: 'session_title',
    direction: 'inbound',
    payload: { title: 'New title' } as never,
  })
  expect(titles).toEqual(['New title'])
})

test('compaction status confirms the command without clearing other queued messages', () => {
  let entries: ThreadEntry[] = [
    { type: 'user_message', id: 'compact', content: '/compact', pending: true },
    { type: 'user_message', id: 'later', content: 'follow up', pending: true },
  ]
  const states: boolean[] = []
  const adapter = new RCSChatAdapter(
    'session',
    action => {
      entries = typeof action === 'function' ? action(entries) : action
    },
    { onCompaction: active => states.push(active) },
  )
  adapter.handleEvent({
    type: 'system',
    direction: 'inbound',
    payload: { subtype: 'status', status: 'compacting' },
  })
  expect(entries[0]).toMatchObject({ pending: false })
  expect(entries[1]).toMatchObject({ pending: true })
  adapter.handleEvent({
    type: 'system',
    direction: 'inbound',
    payload: { subtype: 'status', raw: { status: null } },
  })
  expect(states).toEqual([true, false])
})

test('retries an interrupted send using the same message UUID', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const ids: string[] = []
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    _input: Parameters<typeof fetch>[0],
    init?: Parameters<typeof fetch>[1],
  ) => {
    ids.push((JSON.parse(String(init?.body)) as { uuid: string }).uuid)
    if (ids.length === 1) throw new TypeError('connection lost')
    return Response.json({ status: 'ok' })
  }) as unknown as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    await adapter.sendMessage('hello')
    expect(ids).toHaveLength(2)
    expect(ids[1]).toBe(ids[0])
    expect(entries.filter(entry => entry.type === 'user_message')).toHaveLength(
      1,
    )
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('keeps an uncertain send visible when both responses are lost', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () => {
    throw new TypeError('offline')
  }) as unknown as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    await expect(adapter.sendMessage('hello')).rejects.toThrow('无法确认')
    expect(entries).toHaveLength(1)
    expect(entries[0]?.type === 'user_message' && entries[0].pending).toBe(true)
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('shows identical long answers from separate turns', () => {
  let entries: ThreadEntry[] = []
  const adapter = new RCSChatAdapter('session', action => {
    entries = typeof action === 'function' ? action(entries) : action
  })
  const answer = 'This is a deliberately repeated answer. '.repeat(3)
  for (const [user, assistant] of [
    ['u1', 'a1'],
    ['u2', 'a2'],
  ]) {
    adapter.handleEvent({
      type: 'user',
      direction: 'inbound',
      payload: { uuid: user, content: 'repeat' },
    })
    adapter.handleEvent({
      type: 'assistant',
      direction: 'inbound',
      payload: { uuid: assistant, content: answer },
    })
  }
  expect(
    entries.filter(entry => entry.type === 'assistant_message'),
  ).toHaveLength(2)
})

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

test('uses session totals reported by the terminal instead of a partial history', () => {
  let entries: ThreadEntry[] = []
  const seen: SessionUsage[] = []
  const adapter = new RCSChatAdapter(
    'session',
    action => {
      entries = typeof action === 'function' ? action(entries) : action
    },
    { onUsage: usage => seen.push(usage) },
  )
  adapter.handleEvent({
    type: 'usage_state',
    direction: 'inbound',
    payload: {
      raw: {
        current: {
          input_tokens: 10,
          output_tokens: 3,
          cache_creation_input_tokens: 20,
          cache_read_input_tokens: 70,
        },
        total_input_tokens: 100,
        total_output_tokens: 50,
        total_cache_creation_input_tokens: 100,
        total_cache_read_input_tokens: 800,
      },
    },
  })
  expect(seen.at(-1)).toEqual({
    current: { input: 10, output: 3, cacheCreation: 20, cacheRead: 70 },
    totals: { input: 100, output: 50 },
    cacheHitRate: 0.8,
    estimated: false,
  })
  adapter.seedUsageFromHistory([
    {
      id: 'older',
      usage: {
        inputTokens: 1,
        outputTokens: 2,
        cacheReadInputTokens: 3,
        cacheCreationInputTokens: 4,
      },
    },
  ])
  expect(seen).toHaveLength(1)
})

test('marks compacted context usage as estimated until the terminal reports actual usage', () => {
  const seen: SessionUsage[] = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onUsage: usage => seen.push(usage),
  })
  const raw = {
    current: {
      input_tokens: 17600,
      output_tokens: 0,
      cache_creation_input_tokens: 0,
      cache_read_input_tokens: 0,
    },
    total_input_tokens: 100,
    total_output_tokens: 20,
    total_cache_creation_input_tokens: 30,
    total_cache_read_input_tokens: 70,
  }
  adapter.handleEvent({
    type: 'usage_state',
    direction: 'inbound',
    payload: { raw: { ...raw, current_estimated: true } },
  })
  expect(seen.at(-1)?.contextEstimated).toBe(true)
  expect(seen.at(-1)?.current?.input).toBe(17600)
  adapter.handleEvent({
    type: 'usage_state',
    direction: 'inbound',
    payload: { raw: { ...raw, current_estimated: false } },
  })
  expect(seen.at(-1)?.contextEstimated).toBeUndefined()
})

test('older history pages never replace the current live usage', () => {
  let entries: ThreadEntry[] = []
  const seen: SessionUsage[] = []
  const adapter = new RCSChatAdapter(
    'session',
    action => {
      entries = typeof action === 'function' ? action(entries) : action
    },
    { onUsage: usage => seen.push(usage) },
  )
  const usage = (inputTokens: number) => ({
    inputTokens,
    outputTokens: 1,
    cacheReadInputTokens: 10,
    cacheCreationInputTokens: 0,
  })
  adapter.seedUsageFromHistory([
    { id: 'newer', timestamp: '2026-11-15T03:00:00Z', usage: usage(100) },
  ])
  adapter.seedUsageFromHistory([
    { id: 'newer', timestamp: '2026-11-15T03:00:00Z', usage: usage(100) },
    { id: 'older', timestamp: '2026-11-14T03:00:00Z', usage: usage(20) },
  ])
  expect(seen.at(-1)?.current?.input).toBe(100)
  adapter.handleEvent({
    type: 'assistant',
    direction: 'inbound',
    payload: {
      uuid: 'newer',
      content: 'hello',
      message: {
        usage: {
          input_tokens: 200,
          output_tokens: 2,
          cache_read_input_tokens: 50,
          cache_creation_input_tokens: 0,
        },
      },
    },
  })
  adapter.seedUsageFromHistory([
    { id: 'newer', timestamp: '2026-11-15T03:00:00Z', usage: usage(100) },
  ])
  expect(seen.at(-1)?.current?.input).toBe(200)
  expect(seen.at(-1)?.totals.input).toBe(220)
})

test('loading a large older page keeps the newest history in the bounded ledger', () => {
  const seen: SessionUsage[] = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onUsage: usage => seen.push(usage),
  })
  const usage = (inputTokens: number) => ({
    inputTokens,
    outputTokens: 1,
    cacheReadInputTokens: 0,
    cacheCreationInputTokens: 0,
  })
  adapter.seedUsageFromHistory([
    { id: 'new', timestamp: '2026-11-15T03:00:00Z', usage: usage(1000) },
  ])
  adapter.seedUsageFromHistory(
    Array.from({ length: 220 }, (_, i) => ({
      id: `old-${i}`,
      timestamp: new Date(Date.UTC(2026, 0, 1, 0, 0, i)).toISOString(),
      usage: usage(1),
    })),
  )
  expect(seen.at(-1)?.current?.input).toBe(1000)
  expect(seen.at(-1)?.totals.input).toBe(1199)
})

test('registers a control request before sending it, so a fast response is matched', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: {
      getItem: () => 'test-identity',
      setItem: () => {},
      removeItem: () => {},
    },
  })
  let registered = ''
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    _input: Parameters<typeof fetch>[0],
    init?: Parameters<typeof fetch>[1],
  ) => {
    const sent = JSON.parse(String(init?.body)) as { request_id: string }
    expect(registered).toBe(sent.request_id)
    return Response.json({ status: 'ok' })
  }) as unknown as typeof fetch)
  try {
    const adapter = new RCSChatAdapter('session', () => {})
    const id = await adapter.setUltracode(true, requestId => {
      registered = requestId
    })
    expect(registered).toBe(id)
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
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
    Response.json({ events })) as unknown as typeof fetch)
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

test('a device-view (metadata) resync applies replayed tool results', async () => {
  // The parent view owns the transcript, but a tool completion replayed by the
  // bridge must still land on the live card or it spins forever.
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    input: RequestInfo | URL,
  ) => {
    if (String(input).includes('/history')) {
      return Response.json({
        events: [
          {
            id: 'e1',
            type: 'tool_result',
            direction: 'inbound',
            payload: { tool_call_id: 'tool-9', content: 'done' },
          },
        ],
      })
    }
    return Response.json({ ok: true })
  }) as typeof fetch)
  try {
    let entries: ThreadEntry[] = [
      {
        type: 'tool_call',
        toolCall: { id: 'tool-9', title: 'Bash', status: 'running' },
      },
    ]
    const adapter = new RCSChatAdapter(
      'session',
      action => {
        entries = typeof action === 'function' ? action(entries) : action
      },
      { historyMode: 'metadata' },
    )
    await adapter.loadHistory()
    expect(entries[0]).toMatchObject({
      type: 'tool_call',
      toolCall: {
        id: 'tool-9',
        status: 'complete',
        rawOutput: { output: 'done' },
      },
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

test('a resync adoption stays correct when React invokes the updater twice', async () => {
  // React may replay a state updater (StrictMode, concurrent re-render). The
  // adoption must be decided from a snapshot, not from a set the first run
  // drains — otherwise run 2 appends the echo below the reply.
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
    const apply = (action: SetStateAction<ThreadEntry[]>) => {
      if (typeof action !== 'function') {
        entries = action
        return
      }
      action(entries) // discarded render
      entries = action(entries)
    }
    const adapter = new RCSChatAdapter('session', apply)
    await adapter.sendMessage('继续')
    adapter.handleEvent({
      type: 'assistant',
      direction: 'inbound',
      payload: { uuid: 'a1', content: '明白了' },
    })
    historyEvents = [
      {
        id: 'e1',
        type: 'user',
        direction: 'inbound',
        payload: { uuid: 'server-uuid', content: '继续' },
      },
      {
        id: 'e2',
        type: 'assistant',
        direction: 'inbound',
        payload: { uuid: 'a1', content: '明白了' },
      },
    ]
    await adapter.loadHistory()
    expect(
      entries.map(entry =>
        entry.type === 'user_message' ? `user:${entry.content}` : entry.type,
      ),
    ).toEqual(['user:继续', 'assistant_message'])
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('a web send with no inbound echo survives a full-mode history reload', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    input: RequestInfo | URL,
  ) => {
    if (String(input).includes('/history')) {
      return Response.json({
        events: [
          {
            id: 'e1',
            type: 'user',
            direction: 'outbound',
            payload: { uuid: 'u-send', message: { content: 'mid-turn send' } },
          },
          {
            id: 'e2',
            type: 'assistant',
            direction: 'inbound',
            seqNum: 2,
            payload: {
              uuid: 'a1',
              message: { content: [{ type: 'text', text: 'answer' }] },
            },
          },
        ],
      })
    }
    return Response.json({ ok: true })
  }) as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    await adapter.loadHistory()
    expect(
      entries.some(
        entry =>
          entry.type === 'user_message' && entry.content === 'mid-turn send',
      ),
    ).toBe(true)
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('an outbound send is folded into its inbound echo on reload', async () => {
  const originalStorage = Object.getOwnPropertyDescriptor(
    globalThis,
    'localStorage',
  )
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    input: RequestInfo | URL,
  ) => {
    if (String(input).includes('/history')) {
      return Response.json({
        events: [
          {
            id: 'e1',
            type: 'user',
            direction: 'outbound',
            payload: { uuid: 'u-send', message: { content: 'hello' } },
          },
          {
            id: 'e2',
            type: 'user',
            direction: 'inbound',
            payload: { uuid: 'u-echo', message: { content: 'hello' } },
          },
        ],
      })
    }
    return Response.json({ ok: true })
  }) as typeof fetch)
  try {
    let entries: ThreadEntry[] = []
    const adapter = new RCSChatAdapter('session', action => {
      entries = typeof action === 'function' ? action(entries) : action
    })
    await adapter.loadHistory()
    const users = entries.filter(entry => entry.type === 'user_message')
    expect(users).toHaveLength(1)
    expect(users[0]?.id).toBe('user-u-echo')
  } finally {
    fetchSpy.mockRestore()
    if (originalStorage)
      Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('a permission answered elsewhere releases the inline tool card', () => {
  let entries: ThreadEntry[] = []
  const resolved: string[] = []
  const adapter = new RCSChatAdapter(
    'session',
    action => {
      entries = typeof action === 'function' ? action(entries) : action
    },
    {
      onPermissionResolved: id => resolved.push(id),
    },
  )

  adapter.handleEvent({
    type: 'tool_use',
    direction: 'inbound',
    payload: { tool_call_id: 'toolu_1', tool_name: 'Bash', tool_input: {} },
  })
  adapter.handleEvent({
    type: 'control_request',
    direction: 'inbound',
    payload: {
      request_id: 'req-9',
      request: {
        subtype: 'can_use_tool',
        tool_name: 'Bash',
        input: {},
        description: '',
        tool_use_id: 'toolu_1',
      },
    },
  })

  const waiting = entries.find(e => e.type === 'tool_call')
  expect(waiting).toMatchObject({
    type: 'tool_call',
    toolCall: { status: 'waiting_for_confirmation' },
  })

  adapter.handleEvent({
    type: 'control_response',
    direction: 'inbound',
    payload: {
      response: { subtype: 'success', request_id: 'req-9', response: {} },
    },
  })

  expect(resolved).toEqual(['req-9'])
  const card = entries.find(e => e.type === 'tool_call')
  expect(card).toMatchObject({
    type: 'tool_call',
    toolCall: { status: 'running', permissionRequest: undefined },
  })
})

test('a permission_response from another client releases the inline tool card', () => {
  let entries: ThreadEntry[] = []
  const adapter = new RCSChatAdapter('session', action => {
    entries = typeof action === 'function' ? action(entries) : action
  })

  adapter.handleEvent({
    type: 'tool_use',
    direction: 'inbound',
    payload: { tool_call_id: 'toolu_2', tool_name: 'Edit', tool_input: {} },
  })
  adapter.handleEvent({
    type: 'control_request',
    direction: 'inbound',
    payload: {
      request_id: 'req-10',
      request: {
        subtype: 'can_use_tool',
        tool_name: 'Edit',
        input: {},
        description: '',
        tool_use_id: 'toolu_2',
      },
    },
  })
  adapter.handleEvent({
    type: 'permission_response',
    direction: 'outbound',
    payload: { approved: true, request_id: 'req-10' },
  })

  const card = entries.find(e => e.type === 'tool_call')
  expect(card).toMatchObject({
    type: 'tool_call',
    toolCall: { status: 'running', permissionRequest: undefined },
  })
})

test('a terminal_queue snapshot reports queued terminal prompts', () => {
  let entries: ThreadEntry[] = []
  const snapshots: Array<unknown | undefined> = []
  const adapter = new RCSChatAdapter(
    'session',
    action => {
      entries = typeof action === 'function' ? action(entries) : action
    },
    {
      onTerminalQueue: snapshot => snapshots.push(snapshot),
    },
  )
  adapter.handleEvent({
    type: 'terminal_queue',
    direction: 'inbound',
    payload: {
      raw: { type: 'terminal_queue', count: 2, previews: ['first', 'second'] },
    },
  })
  expect(snapshots).toEqual([{ count: 2, previews: ['first', 'second'] }])
  // No transcript rows: the executed message mirrors normally later.
  expect(entries).toEqual([])
})

test('an empty terminal_queue snapshot clears the state', () => {
  const snapshots: Array<unknown | undefined> = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onTerminalQueue: snapshot => snapshots.push(snapshot),
  })
  adapter.handleEvent({
    type: 'terminal_queue',
    direction: 'inbound',
    payload: {
      raw: { type: 'terminal_queue', count: 1, previews: ['queued'] },
    },
  })
  adapter.handleEvent({
    type: 'terminal_queue',
    direction: 'inbound',
    payload: { raw: { type: 'terminal_queue', count: 0, previews: [] } },
  })
  expect(snapshots).toEqual([{ count: 1, previews: ['queued'] }, undefined])
})

test('a malformed terminal_queue snapshot is ignored', () => {
  const snapshots: Array<unknown | undefined> = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onTerminalQueue: snapshot => snapshots.push(snapshot),
  })
  adapter.handleEvent({
    type: 'terminal_queue',
    direction: 'inbound',
    payload: { raw: { type: 'terminal_queue' } },
  })
  expect(snapshots).toEqual([])
})

test('a bridge disconnect clears the queued-terminal-prompts state', () => {
  const snapshots: Array<unknown | undefined> = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onTerminalQueue: snapshot => snapshots.push(snapshot),
  })
  adapter.handleEvent({
    type: 'terminal_queue',
    direction: 'inbound',
    payload: { raw: { type: 'terminal_queue', count: 1, previews: ['stale'] } },
  })
  adapter.handleEvent({
    type: 'bridge_state',
    direction: 'inbound',
    payload: { raw: { connected: false } },
  })
  expect(snapshots).toEqual([{ count: 1, previews: ['stale'] }, undefined])
})
