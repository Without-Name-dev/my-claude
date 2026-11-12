import { feature } from 'bun:bundle'
import { randomUUID } from 'crypto'
import { getSdkBetas, getSessionId } from 'src/bootstrap/state.js'
import { DEFAULT_OUTPUT_STYLE_NAME } from 'src/constants/outputStyles.js'
import type {
  ApiKeySource,
  PermissionMode,
  SDKMessage,
} from 'src/entrypoints/agentSdkTypes.js'
import {
  AGENT_TOOL_NAME,
  LEGACY_AGENT_TOOL_NAME,
} from '@claude-code-best/builtin-tools/tools/AgentTool/constants.js'
import { getAnthropicApiKeyWithSource } from '../auth.js'
import { getCwd } from '../cwd.js'
import { getFastModeState } from '../fastMode.js'
import { getSettings_DEPRECATED } from '../settings/settings.js'

// TODO(next-minor): remove this translation once SDK consumers have migrated
// to the 'Agent' tool name. The wire name was renamed Task → Agent in #19647,
// but emitting the new name in init/result events broke SDK consumers on a
// patch-level release. Keep emitting 'Task' until the next minor.
export function sdkCompatToolName(name: string): string {
  return name === AGENT_TOOL_NAME ? LEGACY_AGENT_TOOL_NAME : name
}

type CommandLike = { name: string; userInvocable?: boolean }

export type SystemInitInputs = {
  tools: ReadonlyArray<{ name: string }>
  mcpClients: ReadonlyArray<{ name: string; type: string }>
  model: string
  permissionMode: PermissionMode
  commands: ReadonlyArray<CommandLike>
  agents: ReadonlyArray<{ agentType: string }>
  skills: ReadonlyArray<CommandLike>
  plugins: ReadonlyArray<{ name: string; path: string; source: string }>
  fastMode: boolean | undefined
  /**
   * Model picker entries for remote clients. The SDK wire type only carries
   * the current model; the RCS web needs the list to render a selector.
   */
  models?: ReadonlyArray<{ id: string; name: string; description?: string }>
  /** Descriptions for `slash_commands`, which the wire type leaves as names. */
  commandDetails?: ReadonlyArray<{ name: string; description: string }>
  /** Current effort level, so a remote picker can show what is actually set. */
  effort?: string
  /** Whether the current model can enable Ultracode (dynamic workflows + xhigh). */
  ultracodeAvailable?: boolean
  ultracodeActive?: boolean
  /** Context window of the current model, for remote context-percentage bars. */
  contextWindow?: number
}

/**
 * Build the `system/init` SDKMessage — the first message on the SDK stream
 * carrying session metadata (cwd, tools, model, commands, etc.) that remote
 * clients use to render pickers and gate UI.
 *
 * Called from two paths that must produce identical shapes:
 *   - QueryEngine (spawn-bridge / print-mode / SDK) — yielded as the first
 *     stream message per query turn
 *   - useReplBridge (REPL Remote Control) — sent via writeSdkMessages() on
 *     bridge connect, since REPL uses query() directly and never hits the
 *     QueryEngine SDKMessage layer
 */
export function buildSystemInitMessage(inputs: SystemInitInputs): SDKMessage {
  const settings = getSettings_DEPRECATED()
  const outputStyle = settings?.outputStyle ?? DEFAULT_OUTPUT_STYLE_NAME

  const initMessage: SDKMessage = {
    type: 'system',
    subtype: 'init',
    cwd: getCwd(),
    session_id: getSessionId(),
    tools: inputs.tools.map(tool => sdkCompatToolName(tool.name)),
    mcp_servers: inputs.mcpClients.map(client => ({
      name: client.name,
      status: client.type,
    })),
    model: inputs.model,
    permissionMode: inputs.permissionMode,
    slash_commands: inputs.commands
      .filter(c => c.userInvocable !== false)
      .map(c => c.name),
    apiKeySource: getAnthropicApiKeyWithSource().source as ApiKeySource,
    betas: getSdkBetas(),
    claude_code_version: MACRO.VERSION,
    output_style: outputStyle,
    agents: inputs.agents.map(agent => agent.agentType),
    skills: inputs.skills
      .filter(s => s.userInvocable !== false)
      .map(skill => skill.name),
    plugins: inputs.plugins.map(plugin => ({
      name: plugin.name,
      path: plugin.path,
      source: plugin.source,
    })),
    uuid: randomUUID(),
  }
  // Hidden from public SDK types — ant-only UDS messaging socket path
  if (feature('UDS_INBOX')) {
    /* eslint-disable @typescript-eslint/no-require-imports */
    ;(initMessage as Record<string, unknown>).messaging_socket_path =
      require('../udsMessaging.js').getUdsMessagingSocketPath()
    /* eslint-enable @typescript-eslint/no-require-imports */
  }
  initMessage.fast_mode_state = getFastModeState(inputs.model, inputs.fastMode)
  // Extra fields for remote-control clients. They are additive and ignored by
  // SDK consumers that only know the generated wire type.
  const remote = initMessage as SDKMessage & {
    models?: ReadonlyArray<{ id: string; name: string; description?: string }>
    command_details?: ReadonlyArray<{ name: string; description: string }>
    effort?: string
    ultracode_available?: boolean
    ultracode_active?: boolean
    context_window?: number
  }
  remote.models = inputs.models ?? []
  remote.command_details = inputs.commandDetails ?? []
  if (inputs.effort !== undefined) remote.effort = inputs.effort
  if (inputs.ultracodeAvailable !== undefined) {
    remote.ultracode_available = inputs.ultracodeAvailable
  }
  if (inputs.ultracodeActive !== undefined) {
    remote.ultracode_active = inputs.ultracodeActive
  }
  if (typeof inputs.contextWindow === 'number' && inputs.contextWindow > 0) {
    remote.context_window = inputs.contextWindow
  }
  return initMessage
}
