import type { AppState } from './AppState.js'
import { getBackgroundAgentTasks } from '../hooks/useBackgroundAgentTasks.js'
import type { InputEvent } from '@anthropic/ink'
import type { Store } from './store.js'

/** Enter the visible list at the currently viewed row (main is index -1). */
export function focusBackgroundAgents(state: AppState): AppState {
  const agents = getBackgroundAgentTasks(state.tasks)
  if (agents.length === 0 || state.footerSelection === 'bg_agent') return state
  return {
    ...state,
    footerSelection: 'bg_agent',
    selectedBgAgentIndex: agents.findIndex(
      agent => agent.agentId === state.viewingAgentTaskId,
    ),
  }
}

/** Claim only the advertised shortcut, leaving normal arrows and history alone. */
export function handleBackgroundAgentShortcut(
  event: InputEvent,
  store: Store<AppState>,
): boolean {
  const key = event.key
  if (!key.shift || !key.downArrow || key.ctrl || key.meta || key.super)
    return false
  if (getBackgroundAgentTasks(store.getState().tasks).length === 0) return false
  store.setState(focusBackgroundAgents)
  event.stopImmediatePropagation()
  return true
}
