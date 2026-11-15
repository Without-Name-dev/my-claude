import type { StdoutMessage } from '../entrypoints/sdk/controlTypes.js'

type TransportMessage = StdoutMessage & { session_id?: string }

/**
 * Bounded FIFO queue for control frames (control_response,
 * control_cancel_request) produced while the transport is disconnected.
 *
 * A remote permission answer or cancel is a promise to the user that the
 * prompt was handled; dropping it when `transport === null` left the
 * permission card pending forever on every client. Keyed by type+request_id
 * so a retry of the same answer replaces the earlier frame instead of piling
 * up. Capped so a long outage cannot grow memory unboundedly — oldest entry
 * is evicted first.
 */
const MAX_PENDING_CONTROL_FRAMES = 50

export type PendingControlFrameQueue = {
  queue: (key: string, frame: TransportMessage) => void
  /** Sends every queued frame through the given writer and empties the queue. */
  drain: (write: (frame: TransportMessage) => void) => number
  size: () => number
  clear: () => void
}

export function createPendingControlFrameQueue(
  onQueue?: (label: string, pending: number) => void,
  onDrain?: (flushed: number) => void,
): PendingControlFrameQueue {
  const frames = new Map<string, TransportMessage>()

  return {
    queue(key, frame) {
      // A retry of an already-queued key replaces in place — evicting for it
      // would drop a different pending answer.
      if (!frames.has(key) && frames.size >= MAX_PENDING_CONTROL_FRAMES) {
        const oldest = frames.keys().next().value
        if (oldest !== undefined) frames.delete(oldest)
      }
      frames.set(key, frame)
      onQueue?.(key, frames.size)
    },
    drain(write) {
      if (frames.size === 0) return 0
      const pending = [...frames.values()]
      frames.clear()
      for (const frame of pending) write(frame)
      onDrain?.(pending.length)
      return pending.length
    },
    size() {
      return frames.size
    },
    clear() {
      frames.clear()
    },
  }
}
