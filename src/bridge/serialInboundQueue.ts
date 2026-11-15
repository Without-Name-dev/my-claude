/** Process inbound prompts in arrival order, even when attachment loading waits. */
export function createSerialInboundQueue<T>(
  handle: (message: T) => Promise<void>,
) {
  let tail: Promise<void> = Promise.resolve()
  return (message: T): Promise<void> => {
    const result = tail.then(() => handle(message))
    // A failed message must not permanently block every later prompt.
    tail = result.catch(() => {})
    return result
  }
}
