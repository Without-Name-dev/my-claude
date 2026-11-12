/**
 * Ultracode availability — a port of the official CLI's gating semantics.
 *
 * Ultracode is session-only state (AppState.ultracodeActive), never persisted.
 * It requires: the Workflow tool compiled in (WORKFLOW_SCRIPTS), dynamic
 * workflows not disabled in settings, and a model that supports xhigh effort
 * (the official CLI gates ultracode on exactly that capability).
 */
import { feature } from 'bun:bundle'
import { getInitialSettings } from './settings/settings.js'
import { isEnvTruthy } from './envUtils.js'
import { modelSupportsXhighEffort } from './effort.js'

/** Managed kill-switch, mirrors CLAUDE_CODE_DISABLE_WORKFLOWS upstream. */
export function isWorkflowsHardDisabled(): boolean {
  return isEnvTruthy(process.env.CLAUDE_CODE_DISABLE_WORKFLOWS)
}

/**
 * Dynamic workflows enabled (user setting `enableWorkflows`, default true).
 * Layers: env kill-switch > user setting.
 */
export function isWorkflowsEnabled(): boolean {
  if (!feature('WORKFLOW_SCRIPTS')) return false
  if (isWorkflowsHardDisabled()) return false
  const settings = getInitialSettings()
  if (settings.disableWorkflows === true) return false
  return settings.enableWorkflows !== false
}

/**
 * Ultracode needs the model to support xhigh effort (official Hit()). A model
 * that cannot think that hard gets workflows but not ultracode's default-on
 * behaviour.
 */
export function isUltracodeAvailable(model: string | undefined): boolean {
  if (!isWorkflowsEnabled()) return false
  if (model === undefined || model === '') return true
  return modelSupportsXhighEffort(model)
}

/** The reason text shown by /effort when the toggle is unavailable. */
export function ultracodeUnavailableReason(model: string | undefined): string {
  if (isWorkflowsHardDisabled()) {
    return 'Ultracode needs dynamic workflows enabled (disabled by CLAUDE_CODE_DISABLE_WORKFLOWS)'
  }
  if (!isWorkflowsEnabled()) {
    return 'Ultracode needs dynamic workflows enabled (see /config)'
  }
  return `Ultracode isn't available on ${model ?? 'this model'}`
}
