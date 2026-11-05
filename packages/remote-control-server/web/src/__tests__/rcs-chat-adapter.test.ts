import { expect, test, spyOn } from 'bun:test'
import type { SetStateAction } from 'react'
import { RCSChatAdapter } from '../lib/rcs-chat-adapter'
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
