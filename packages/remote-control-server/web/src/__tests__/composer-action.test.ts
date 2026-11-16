import { describe, expect, test } from 'bun:test'
import { getComposerAction } from '../lib/composer-action'

const idle = {
  hasDraft: false,
  disabled: false,
  isLoading: false,
  canInterrupt: true,
  sending: false,
  stopping: false,
  preparing: false,
}

describe('combined composer button', () => {
  test('only offers stop for an empty draft during an interruptible turn', () => {
    expect(getComposerAction(idle)).toBe('disabled')
    expect(getComposerAction({ ...idle, isLoading: true })).toBe('stop')
    expect(
      getComposerAction({ ...idle, isLoading: true, canInterrupt: false }),
    ).toBe('disabled')
  })

  test('sends a draft even while the current turn is running', () => {
    expect(getComposerAction({ ...idle, hasDraft: true })).toBe('send')
    expect(
      getComposerAction({ ...idle, hasDraft: true, isLoading: true }),
    ).toBe('send')
    expect(
      getComposerAction({
        ...idle,
        hasDraft: true,
        isLoading: true,
        disabled: true,
      }),
    ).toBe('disabled')
  })

  test('never changes a pending send into a stop or omits an attachment', () => {
    expect(getComposerAction({ ...idle, isLoading: true, sending: true })).toBe(
      'sending',
    )
    expect(
      getComposerAction({
        ...idle,
        isLoading: true,
        hasDraft: true,
        sending: true,
      }),
    ).toBe('sending')
    expect(
      getComposerAction({ ...idle, isLoading: true, preparing: true }),
    ).toBe('preparing')
    expect(
      getComposerAction({
        ...idle,
        isLoading: true,
        hasDraft: true,
        preparing: true,
      }),
    ).toBe('preparing')
    expect(
      getComposerAction({
        ...idle,
        isLoading: true,
        hasDraft: true,
        stopping: true,
      }),
    ).toBe('stopping')
  })
})
