export type ComposerAction =
  | 'send'
  | 'stop'
  | 'sending'
  | 'stopping'
  | 'preparing'
  | 'disabled'

/** A follow-up draft always takes priority over interrupting the current turn. */
export function getComposerAction(state: {
  hasDraft: boolean
  disabled: boolean
  isLoading: boolean
  canInterrupt: boolean
  sending: boolean
  stopping: boolean
  preparing: boolean
}): ComposerAction {
  if (state.stopping) return 'stopping'
  if (state.sending) return 'sending'
  if (state.preparing) return 'preparing'
  if (state.hasDraft) return state.disabled ? 'disabled' : 'send'
  if (state.isLoading && state.canInterrupt) return 'stop'
  return 'disabled'
}
