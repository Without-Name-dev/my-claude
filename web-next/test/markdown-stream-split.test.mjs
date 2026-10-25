import assert from "node:assert/strict"
import test from "node:test"

import { safeMarkdownSplitIndex, splitMarkdownStream } from "../src/lib/markdown-stream-split.ts"

function prefix(text) {
  const index = safeMarkdownSplitIndex(text)
  return index === null ? null : text.slice(0, index)
}

test("paragraphs split at the last blank line before an open paragraph", () => {
  assert.equal(prefix("First.\n\nSecond.\n\nThird is still"), "First.\n\nSecond.\n\n")
  assert.equal(prefix("Only one paragraph that is still streaming"), null)
  // Trailing blank lines do not tell whether a list continues; wait for more text.
  assert.equal(prefix("Done.\n\n"), null)
})

test("fenced code is never split while open", () => {
  const open = "Intro.\n\n```swift\nlet a = 1\n\nlet b = 2\n"
  assert.equal(prefix(open), null)
  const closed = `${open}\`\`\`\n\nAfter.`
  assert.equal(prefix(closed), "Intro.\n\n```swift\nlet a = 1\n\nlet b = 2\n```\n\n")
  assert.equal(prefix("~~~\ntext\n\n~~~\n\nNext"), "~~~\ntext\n\n~~~\n\n")
})

test("loose lists and indented continuations stay together", () => {
  const list = "Steps:\n\n- one\n\n- two\n\n  detail\n\n3. three\n\n2) four"
  assert.equal(prefix(list), "Steps:\n\n")
  const afterList = `${list}\n\nPlain paragraph`
  assert.equal(prefix(afterList), `${list}\n\n`)
  assert.equal(prefix("Text.\n\n    code block"), "Text.\n\n")
  assert.equal(prefix("- a\n\n    nested code"), null)
})

test("the split advances one completed block at a time while streaming", () => {
  const chunks = ["First paragraph", " is still open", ".\n\nSecond", " paragraph", ".\n\nThird"]
  let text = ""
  let previousPrefix = ""
  for (const chunk of chunks) {
    text += chunk
    const { prefix: current, tail } = splitMarkdownStream(text)
    assert.ok(current.startsWith(previousPrefix), `prefix went backwards: ${JSON.stringify(current)}`)
    assert.equal(current + tail, text)
    // Completed text never re-enters the tail once the boundary has advanced.
    if (current) assert.ok(!tail.includes("First paragraph"))
    previousPrefix = current
  }
  assert.equal(splitMarkdownStream(text).prefix, "First paragraph is still open.\n\nSecond paragraph.\n\n")
  assert.equal(splitMarkdownStream(text).tail, "Third")
})

test("a list item that contains a fence stays one list across a blank line", () => {
  // The paragraph after the block belongs to the same list item. Splitting
  // there would parse it as a top-level paragraph and leave the list for good
  // (the chunk cache is append-only).
  assert.equal(prefix("- item\n\n  ```\n  code\n  ```\n\n  more text in item"), null)
  assert.equal(
    prefix("- item\n\n  ```\n  code\n  ```\n\n  more text in item\n\nAfter"),
    "- item\n\n  ```\n  code\n  ```\n\n  more text in item\n\n",
  )
  // Two items of one loose list, with a fence in the first, must not become
  // two separate lists.
  assert.equal(
    prefix("- a\n\n  ```\n  code\n  ```\n\n- b\n\nAfter"),
    "- a\n\n  ```\n  code\n  ```\n\n- b\n\n",
  )
})

test("display math is never split while open", () => {
  const open = "Before\n\n$$\na = 1\n\nb = 2"
  assert.equal(prefix(open), null)
  const closed = `${open}\n$$\n\nAfter`
  assert.equal(prefix(closed), "Before\n\n$$\na = 1\n\nb = 2\n$$\n\n")
  // A complete $$...$$ on one line is inline math, not an open block.
  assert.equal(prefix("Before\n\n$$x$$\n\nAfter"), "Before\n\n$$x$$\n\n")
})

test("unterminated fences and empty text fall back to one whole parse", () => {
  assert.deepEqual(splitMarkdownStream(""), { prefix: "", tail: "" })
  assert.deepEqual(splitMarkdownStream("```\nstill open\n\n"), { prefix: "", tail: "```\nstill open\n\n" })
  assert.deepEqual(splitMarkdownStream("$$\nstill open\n\n"), { prefix: "", tail: "$$\nstill open\n\n" })
})
