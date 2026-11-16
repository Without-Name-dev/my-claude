/** Keep liveness independent from potentially slow local process discovery. */
export function createHeartbeatTick<T>(options: {
  send: (running?: T) => boolean
  collect: () => Promise<T>
  currentConnection: () => unknown
  isCurrentConnection: (connection: unknown) => boolean
  onError: (error: unknown) => void
}): () => void {
  let collecting = false
  return () => {
    const connection = options.currentConnection()
    if (!options.send()) return
    if (collecting) return
    collecting = true
    void options
      .collect()
      .then(running => {
        if (options.isCurrentConnection(connection)) options.send(running)
      })
      .catch(options.onError)
      .finally(() => {
        collecting = false
      })
  }
}

/** Keep at least two heartbeat opportunities within the server's 60s window. */
export function heartbeatInterval(ms: number): number {
  return Math.min(25_000, Math.max(5_000, ms))
}
