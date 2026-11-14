import { resolve } from 'node:path'

type ManualBridgeInput = {
  args: string[]
  isTerminal: boolean
  cliPath: string
  configDir: string
  spawnerConfig: unknown
  existingBaseUrl?: string
  existingToken?: string
  disabled?: boolean
  autoConnectEnabled?: boolean
}

type ManualBridgeResult = {
  args: string[]
  baseUrl?: string
  token?: string
}

const CLI_COMMANDS = new Set([
  'auth',
  'mcp',
  'server',
  'ssh',
  'open',
  'plugin',
  'agents',
  'doctor',
  'update',
  'daemon',
  'ps',
  'logs',
  'attach',
  'kill',
  'new',
  'list',
  'reply',
  'remote-control',
  'rc',
  'remote',
  'sync',
  'bridge',
  'environment-runner',
  'self-hosted-runner',
  'assistant',
  'auto-mode',
  // Subcommands the CLI registers but the list above missed. Appending
  // --remote-control to a subcommand makes Commander fail with
  // "unknown option" (verified: `mcp list --remote-control` exits 1), so
  // every non-interactive subcommand must be listed here.
  'help',
  'setup-token',
  'up',
  'rollback',
  'install',
  'completion',
  'log',
  'error',
  'export',
  'task',
  'autonomy',
  'serve',
])

function sameFile(a: string, b: string): boolean {
  const normalize = (path: string) => {
    const normalized = resolve(path).replace(/\\/g, '/').replace(/\/+$/, '')
    return process.platform === 'win32' ? normalized.toLowerCase() : normalized
  }
  return normalize(a) === normalize(b)
}

/** Configure only an interactive launch of the exact CLI and profile enrolled by the Spawner. */
export function resolveManualBridge(
  input: ManualBridgeInput,
): ManualBridgeResult {
  const unchanged = { args: input.args }
  if (!input.isTerminal || input.disabled) return unchanged
  if (
    input.args.some(arg =>
      [
        '-p',
        '--print',
        '-h',
        '--help',
        '-v',
        '--version',
        '--init',
        '--init-only',
        '--maintenance',
      ].includes(arg),
    )
  )
    return unchanged
  if (
    input.args.some(
      arg => arg.startsWith('--daemon-worker=') || arg === '--sdk-url',
    )
  )
    return unchanged
  if (input.args[0] && CLI_COMMANDS.has(input.args[0])) return unchanged
  if (
    !input.spawnerConfig ||
    typeof input.spawnerConfig !== 'object' ||
    Array.isArray(input.spawnerConfig)
  )
    return unchanged
  const config = input.spawnerConfig as Record<string, unknown>
  if (
    typeof config.cliPath !== 'string' ||
    typeof config.claudeConfigDir !== 'string' ||
    !sameFile(config.cliPath, input.cliPath) ||
    !sameFile(config.claudeConfigDir, input.configDir)
  )
    return unchanged

  let baseUrl = input.existingBaseUrl
  let token = input.existingToken
  if (
    !baseUrl &&
    !token &&
    typeof config.serverUrl === 'string' &&
    typeof config.enrollKey === 'string'
  ) {
    baseUrl = config.serverUrl
    token = config.enrollKey
  }
  if (!baseUrl || !token) return unchanged
  try {
    const parsed = new URL(baseUrl)
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:')
      return unchanged
  } catch {
    return unchanged
  }

  // Only tokens before a `--` separator are real options; after it they are
  // literal prompt text the user asked to send to the model.
  const optionRegion = input.args.slice(
    0,
    input.args.indexOf('--') === -1
      ? input.args.length
      : input.args.indexOf('--'),
  )
  const explicitBridge = optionRegion.some(
    arg =>
      arg === '--remote-control' ||
      arg === '--rc' ||
      arg.startsWith('--remote-control=') ||
      arg.startsWith('--rc='),
  )
  // Explicit opt-out and no explicit bridge flag: keep the launch exactly as
  // the user typed it — no flag AND no credential env injection.
  if (!explicitBridge && input.autoConnectEnabled === false) return unchanged
  const args = explicitBridge ? input.args : withRemoteControlFlag(input.args)
  return { args, baseUrl, token }
}

/**
 * Append --remote-control, but never after a `--` separator: everything
 * after `--` is a positional prompt, so the flag would be typed into the
 * conversation instead of parsed by Commander.
 */
function withRemoteControlFlag(args: string[]): string[] {
  const separator = args.indexOf('--')
  if (separator === -1) return [...args, '--remote-control']
  return [
    ...args.slice(0, separator),
    '--remote-control',
    ...args.slice(separator),
  ]
}
