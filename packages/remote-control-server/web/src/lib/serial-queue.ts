/** Run submitted tasks in order, keeping later tasks alive after a failure. */
export function createSerialQueue() {
  let tail: Promise<void> = Promise.resolve()
  return function enqueue<T>(task: () => Promise<T>): Promise<T> {
    const result = tail.then(task)
    tail = result.then(
      () => undefined,
      () => undefined,
    )
    return result
  }
}
