/** Coordinate re-deliveries even while a previous bridge is shutting down. */
export function createInboundDeduplicator(limit = 5000) {
  const completed = new Set<string>()
  const pending = new Map<string, Promise<boolean>>()

  return async function run(
    id: string | undefined,
    deliver: () => Promise<boolean>,
  ): Promise<void> {
    if (!id) {
      await deliver()
      return
    }
    while (true) {
      if (completed.has(id)) return
      const previous = pending.get(id)
      if (previous) {
        try {
          await previous
        } catch {
          /* The next delivery can retry. */
        }
        continue
      }
      const attempt = deliver()
      pending.set(id, attempt)
      try {
        if (await attempt) {
          completed.add(id)
          while (completed.size > limit) {
            const oldest = completed.keys().next().value
            if (oldest === undefined) break
            completed.delete(oldest)
          }
        }
      } finally {
        if (pending.get(id) === attempt) pending.delete(id)
      }
      return
    }
  }
}
