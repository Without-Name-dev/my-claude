/**
 * Streaming Markdown grows at its tail. Text before a safe block boundary cannot
 * be changed by anything appended later, so a renderer can keep the parsed
 * prefix and re-parse only the open tail. This mirrors the iOS
 * `MarkdownStreamSplit`.
 *
 * A boundary is the start of a line that follows a blank line, sits outside a
 * fenced code block or display-math block, and cannot continue the block before
 * it: a list item or an indented line may continue a loose list or an indented
 * code block, so those do not qualify. Returns null when no such boundary exists
 * (short text, an unterminated fence/math block, or text that ends in blank
 * lines whose successor is unknown).
 */
export type MarkdownStreamSplit = {
  prefix: string
  tail: string
}

export function splitMarkdownStream(text: string): MarkdownStreamSplit {
  const index = safeMarkdownSplitIndex(text)
  if (index === null) return { prefix: "", tail: text }
  return { prefix: text.slice(0, index), tail: text.slice(index) }
}

export function safeMarkdownSplitIndex(text: string): number | null {
  let candidate: number | null = null
  // `listContext` remembers whether the fence opened as part of a list (an
  // indented fence line, or one on the marker line). A fence does not continue
  // a list item, but the item can continue after it, so the flag is restored
  // when the fence closes. Without it, the indented paragraph after a code
  // block inside a list would look like a safe boundary and the list would be
  // split in two.
  let fence: { marker: string; length: number; listContext: boolean } | null = null
  let math: { length: number; listContext: boolean } | null = null
  let pendingBlank: number | null = null
  let previousWasListLike = false
  let position = 0
  while (position < text.length) {
    const lineStart = position
    const newline = text.indexOf("\n", position)
    const lineEnd = newline === -1 ? text.length : newline
    const next = newline === -1 ? text.length : newline + 1
    const line = text.slice(lineStart, lineEnd)
    // Leading indentation decides whether the line continues the block above;
    // a trailing CR from a CRLF transcript is not part of the Markdown line.
    const trimmed = line.replace(/^[ \t]+/, "").replace(/\r$/, "")
    position = next
    if (fence) {
      if (closesFence(trimmed, fence)) {
        previousWasListLike = fence.listContext
        fence = null
      }
      pendingBlank = null
      continue
    }
    if (math) {
      if (closesMath(trimmed, math)) {
        previousWasListLike = math.listContext
        math = null
      }
      pendingBlank = null
      continue
    }
    if (trimmed.length === 0) {
      // A run of blank lines ends at the same boundary; keep the latest.
      pendingBlank = next
      continue
    }
    const listLike = continuesPreviousBlock(line)
    if (pendingBlank !== null) {
      if (pendingBlank === lineStart && !(listLike && previousWasListLike)) {
        candidate = lineStart
      }
      pendingBlank = null
    }
    previousWasListLike = listLike
    const open = opensFence(trimmed)
    if (open) {
      fence = { ...open, listContext: previousWasListLike }
      previousWasListLike = false
      continue
    }
    const openMath = opensMath(trimmed)
    if (openMath) {
      math = { ...openMath, listContext: previousWasListLike }
      previousWasListLike = false
    }
  }
  return fence === null && math === null ? candidate : null
}

/**
 * Indented lines and list markers may continue a loose list or an indented code
 * block across the blank line, so they cannot start a segment.
 */
function continuesPreviousBlock(line: string): boolean {
  const first = line.charAt(0)
  if (first === "") return true
  if (first === " " || first === "\t") return true
  if ("-+*".indexOf(first) !== -1) {
    const rest = line.slice(1)
    return rest.length === 0 || rest.charAt(0) === " " || rest.charAt(0) === "\t"
  }
  if (first >= "0" && first <= "9") {
    const digits = /^[0-9]+/.exec(line)?.[0] ?? ""
    if (digits.length > 9) return false
    const rest = line.slice(digits.length)
    const delimiter = rest.charAt(0)
    if (delimiter !== "." && delimiter !== ")") return false
    const after = rest.slice(1)
    return after.length === 0 || after.charAt(0) === " " || after.charAt(0) === "\t"
  }
  return false
}

function opensFence(trimmed: string): { marker: string; length: number } | null {
  const marker = trimmed.charAt(0)
  if (marker !== "`" && marker !== "~") return null
  const length = countPrefix(trimmed, marker)
  if (length < 3) return null
  // An info string may follow a backtick fence only if it has no backticks.
  if (marker === "`" && trimmed.slice(length).indexOf("`") !== -1) return null
  return { marker, length }
}

function closesFence(trimmed: string, open: { marker: string; length: number }): boolean {
  const length = countPrefix(trimmed, open.marker)
  if (length < open.length) return false
  const rest = trimmed.slice(length)
  for (const character of rest) {
    if (character !== " " && character !== "\t") return false
  }
  return true
}

/**
 * Starts a display-math block. remark-math treats a line beginning with `$$`
 * as a flow fence (the rest of the line is an info string) and ends it at a
 * line containing only `$$`. A complete `$$...$$` on one line is inline math,
 * not a block.
 */
function opensMath(trimmed: string): { length: number } | null {
  const length = countPrefix(trimmed, "$")
  if (length < 2) return null
  const rest = trimmed.slice(length)
  // `$$...$$` with content on one line is inline math. `$$   $$` is not: the
  // parser sees whitespace and then treats the trailing `$$` as meta text, so
  // it still opens a block.
  if (rest.length >= 2 && rest.endsWith("$$") && rest.slice(0, -2).trim().length > 0) return null
  return { length }
}

function closesMath(trimmed: string, open: { length: number }): boolean {
  const length = countPrefix(trimmed, "$")
  if (length < open.length) return false
  const rest = trimmed.slice(length)
  for (const character of rest) {
    if (character !== " " && character !== "\t") return false
  }
  return true
}

function countPrefix(text: string, character: string): number {
  let length = 0
  while (length < text.length && text.charAt(length) === character) length += 1
  return length
}
