import { describe, expect, test } from 'bun:test'
import { shouldShowPromptPlaceholder } from '../promptPlaceholder'

describe('prompt placeholder', () => {
  test('hides when a long session renders the submitted message inside a capped window', () => {
    const all = Array.from({ length: 850 }, (_, index) => ({
      uuid: `message-${index}`,
    }))
    const displayed = all.slice(-500)
    expect(
      shouldShowPromptPlaceholder('hello', false, 'message-849', displayed),
    ).toBe(false)
  })

  test('remains visible while a deferred view has not rendered the new message', () => {
    const displayed = Array.from({ length: 500 }, (_, index) => ({
      uuid: `message-${index}`,
    }))
    expect(
      shouldShowPromptPlaceholder('hello', false, undefined, displayed),
    ).toBe(true)
    expect(
      shouldShowPromptPlaceholder('hello', false, 'new-message', displayed),
    ).toBe(true)
    expect(
      shouldShowPromptPlaceholder('hello', false, 'new-message', [
        ...displayed.slice(1),
        { uuid: 'new-message' },
      ]),
    ).toBe(false)
  })

  test('does not show a leader placeholder while viewing an agent', () => {
    expect(shouldShowPromptPlaceholder('hello', true, undefined, [])).toBe(
      false,
    )
    expect(shouldShowPromptPlaceholder(undefined, false, undefined, [])).toBe(
      false,
    )
  })
})
