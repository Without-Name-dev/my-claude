import { describe, expect, test } from 'bun:test'

import { createPendingControlFrameQueue } from '../pendingControlFrames.js'
import type { StdoutMessage } from '../../entrypoints/sdk/controlTypes.js'

type TransportMessage = StdoutMessage & { session_id?: string }

function frame(id: string): TransportMessage {
  return {
    type: 'control_response',
    response: { subtype: 'success', request_id: id, response: {} },
  } as unknown as TransportMessage
}

function requestIdOf(f: TransportMessage): string {
  return (
    (f as { response?: { request_id?: string } }).response?.request_id ?? ''
  )
}

describe('createPendingControlFrameQueue', () => {
  test('drains queued frames in FIFO order through the writer', () => {
    const queue = createPendingControlFrameQueue()
    queue.queue('response\u0000req-1', frame('req-1'))
    queue.queue('cancel\u0000req-2', frame('req-2'))

    const written: string[] = []
    const flushed = queue.drain(f => written.push(requestIdOf(f)))

    expect(flushed).toBe(2)
    expect(written).toEqual(['req-1', 'req-2'])
    expect(queue.size()).toBe(0)
  })

  test('re-queueing the same key replaces the earlier frame', () => {
    const queue = createPendingControlFrameQueue()
    queue.queue('response\u0000req-1', frame('req-1'))
    queue.queue('response\u0000req-1', frame('req-1'))

    expect(queue.size()).toBe(1)
  })

  test('drain with empty queue writes nothing', () => {
    const queue = createPendingControlFrameQueue()
    let writes = 0
    expect(queue.drain(() => writes++)).toBe(0)
    expect(writes).toBe(0)
  })

  test('evicts the oldest frame at the cap', () => {
    const queue = createPendingControlFrameQueue()
    for (let i = 0; i < 60; i++) {
      queue.queue(`response\u0000req-${i}`, frame(`req-${i}`))
    }

    expect(queue.size()).toBe(50)
    const written: string[] = []
    queue.drain(f => written.push(requestIdOf(f)))
    // The first 10 entries were evicted; req-10 is now the oldest.
    expect(written[0]).toBe('req-10')
    expect(written).toHaveLength(50)
  })

  test('re-queueing an existing key at the cap does not evict another frame', () => {
    const queue = createPendingControlFrameQueue()
    for (let i = 0; i < 50; i++) {
      queue.queue(`response\u0000req-${i}`, frame(`req-${i}`))
    }
    // A retry of req-25 must replace it in place, not drop the oldest entry.
    queue.queue('response\u0000req-25', frame('req-25-retry'))

    expect(queue.size()).toBe(50)
    const drained: string[] = []
    queue.drain(f => drained.push(requestIdOf(f)))
    expect(drained).toHaveLength(50)
    expect(drained[0]).toBe('req-0')
    expect(drained).toContain('req-25-retry')
    expect(drained).not.toContain('req-25')
  })

  test('clear empties the queue without writing', () => {
    const queue = createPendingControlFrameQueue()
    queue.queue('response\u0000req-1', frame('req-1'))
    queue.clear()

    expect(queue.size()).toBe(0)
    let writes = 0
    expect(queue.drain(() => writes++)).toBe(0)
    expect(writes).toBe(0)
  })

  test('reports queue and drain via callbacks', () => {
    const queued: Array<[string, number]> = []
    const drained: number[] = []
    const queue = createPendingControlFrameQueue(
      (label, pending) => queued.push([label, pending]),
      flushed => drained.push(flushed),
    )

    queue.queue('response\u0000req-1', frame('req-1'))
    queue.drain(() => {})

    expect(queued).toEqual([['response\u0000req-1', 1]])
    expect(drained).toEqual([1])
  })
})
