import { afterEach, beforeEach, describe, expect, spyOn, test } from 'bun:test'
import { sseBus } from '../lib/rcs-chat-adapter'

class FakeEventSource {
  static instances: FakeEventSource[] = []
  listeners = new Map<string, (event: { data: string }) => void>()
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  closed = false
  constructor(_url: string) {
    FakeEventSource.instances.push(this)
  }
  addEventListener(type: string, callback: (event: { data: string }) => void) {
    this.listeners.set(type, callback)
  }
  close() {
    this.closed = true
  }
  message(seqNum: number) {
    this.listeners.get('message')?.({
      data: JSON.stringify({
        type: 'user',
        direction: 'inbound',
        payload: {},
        seqNum,
      }),
    })
  }
  resync(seqNum: number) {
    this.listeners.get('resync')?.({ data: JSON.stringify({ seqNum }) })
  }
}

const originalFetch = globalThis.fetch
const originalEventSource = globalThis.EventSource
const originalLocalStorage = globalThis.localStorage
const originalSessionStorage = globalThis.sessionStorage
let now = 100_000
let clock: ReturnType<typeof spyOn>

beforeEach(() => {
  FakeEventSource.instances = []
  now = 100_000
  clock = spyOn(Date, 'now').mockImplementation(() => now)
  const storage = () => {
    const values = new Map<string, string>()
    return {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => {
        values.set(key, value)
      },
      removeItem: (key: string) => {
        values.delete(key)
      },
    } as Storage
  }
  globalThis.localStorage = storage()
  globalThis.sessionStorage = storage()
  globalThis.EventSource = FakeEventSource as unknown as typeof EventSource
})

afterEach(() => {
  sseBus.disconnect()
  clock.mockRestore()
  globalThis.fetch = originalFetch
  globalThis.EventSource = originalEventSource
  globalThis.localStorage = originalLocalStorage
  globalThis.sessionStorage = originalSessionStorage
})

describe('SSE reconnection', () => {
  test('keeps retrying through a long network outage and resumes on open', async () => {
    globalThis.fetch = (() =>
      Promise.reject(new Error('offline'))) as unknown as typeof fetch
    let terminalErrors = 0
    const connectionStates: boolean[] = []
    sseBus.connect(
      'offline-test',
      () => {
        terminalErrors += 1
      },
      undefined,
      undefined,
      state => connectionStates.push(state),
    )
    const source = FakeEventSource.instances[0]!
    source.onopen?.()
    expect(connectionStates[0]).toBe(true)
    for (let i = 0; i < 7; i += 1) {
      now += 3000
      source.onerror?.()
      await Promise.resolve()
    }
    expect(source.closed).toBe(false)
    expect(terminalErrors).toBe(0)
    expect(connectionStates.at(-1)).toBe(false)
    source.onopen?.()
    expect(connectionStates.at(-1)).toBe(true)
    const received: number[] = []
    const unsubscribe = sseBus.onEvent(event => received.push(event.seqNum!))
    source.message(1)
    unsubscribe()
    expect(received).toEqual([1])
  })

  test('drops replayed first events after reconnect and accepts a server reset', () => {
    const received: number[] = []
    let resyncs = 0
    const unsubscribe = sseBus.onEvent(event => received.push(event.seqNum!))
    sseBus.connect('replay-test', undefined, undefined, () => {
      resyncs += 1
    })
    const source = FakeEventSource.instances[0]!
    source.onopen?.()
    source.message(1)
    source.message(2)
    source.onopen?.()
    source.message(2)
    source.message(3)
    source.onopen?.()
    source.resync(0)
    source.message(1)
    unsubscribe()
    expect(received).toEqual([1, 2, 3, 1])
    expect(resyncs).toBe(1)
  })

  test('ignores events from a previous session after switching sessions', () => {
    const received: number[] = []
    const unsubscribe = sseBus.onEvent(event => received.push(event.seqNum!))
    sseBus.connect('previous-session')
    const previous = FakeEventSource.instances[0]!
    previous.onopen?.()
    sseBus.connect('new-session')
    const current = FakeEventSource.instances[1]!
    current.onopen?.()
    previous.message(100)
    current.message(1)
    unsubscribe()
    expect(received).toEqual([1])
  })
})
