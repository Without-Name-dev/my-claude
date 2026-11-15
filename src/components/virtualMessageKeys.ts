/** Keep virtual rows aligned when messages are inserted, removed or reordered. */
export function reconcileVirtualMessageKeys<T>(
  previous: string[],
  messages: readonly T[],
  itemKey: (message: T) => string,
  keyFunctionChanged = false,
): string[] {
  if (!keyFunctionChanged && messages.length >= previous.length) {
    let prefixMatches = true
    for (let i = 0; i < previous.length; i++) {
      if (previous[i] !== itemKey(messages[i]!)) {
        prefixMatches = false
        break
      }
    }
    if (prefixMatches) {
      for (let i = previous.length; i < messages.length; i++) {
        previous.push(itemKey(messages[i]!))
      }
      return previous
    }
  }
  return messages.map(itemKey)
}
