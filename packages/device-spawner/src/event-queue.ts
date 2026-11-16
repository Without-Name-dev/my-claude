import type { DeviceEvent } from '../../remote-control-server/src/device/protocol'

/** Coalesce progress and preserve terminal operation results during outages. */
export function enqueueEvent(queue: string[], event: DeviceEvent): void {
  const message = JSON.stringify({ type: 'event', event })
  if (event.type === 'operation') {
    const existing = queue.findIndex(item => {
      try {
        const previous = JSON.parse(item) as { event?: DeviceEvent }
        return (
          previous.event?.type === 'operation' &&
          previous.event.operationId === event.operationId
        )
      } catch {
        return false
      }
    })
    if (existing >= 0) {
      queue[existing] = message
      return
    }
    if (queue.length >= 100) {
      const disposable = queue.findIndex(item => {
        try {
          return (
            (JSON.parse(item) as { event?: DeviceEvent }).event?.type !==
            'operation'
          )
        } catch {
          return true
        }
      })
      if (disposable >= 0) queue.splice(disposable, 1)
      else if (event.state !== 'ready' && event.state !== 'failed') return
    }
    // A terminal result must not be dropped merely because the buffer is full.
    queue.push(message)
    return
  }
  if (event.type === 'session_state' || event.type === 'catalog_changed') {
    const existing = queue.findIndex(item => {
      try {
        return (
          (JSON.parse(item) as { event?: DeviceEvent }).event?.type ===
          event.type
        )
      } catch {
        return false
      }
    })
    if (existing >= 0) {
      queue[existing] = message
      return
    }
  }
  if (queue.length < 100) queue.push(message)
}

/** Remove an event only after the current connection accepted it for sending. */
export function flushEventQueue(
  queue: string[],
  send: (message: string) => boolean,
): void {
  while (queue.length > 0) {
    if (!send(queue[0]!)) return
    queue.shift()
  }
}
