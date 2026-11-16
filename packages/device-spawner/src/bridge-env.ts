/** Bypass inherited proxies for this device's explicitly configured RCS host. */
export function withDirectBridgeHost(
  env: NodeJS.ProcessEnv,
  serverUrl: string,
): NodeJS.ProcessEnv {
  const hostname = new URL(serverUrl).hostname.replace(/^\[|\]$/g, '')
  const existing = [env.NO_PROXY, env.no_proxy].filter(Boolean).join(',')
  const hosts = existing
    .split(',')
    .map(value => value.trim())
    .filter(Boolean)
  if (!hosts.some(value => value.toLowerCase() === hostname.toLowerCase()))
    hosts.push(hostname)
  const noProxy = hosts.join(',')
  return { ...env, NO_PROXY: noProxy, no_proxy: noProxy }
}

/** A hidden launcher may inherit a headless environment from its parent. */
export function withInteractiveTerminalColors(
  env: NodeJS.ProcessEnv,
): NodeJS.ProcessEnv {
  const result = { ...env }
  if (result.TERM?.toLowerCase() === 'dumb' && result.NO_COLOR !== undefined) {
    delete result.NO_COLOR
    result.TERM = 'xterm-256color'
    // These children are intentionally opened in a visible Windows terminal.
    // Keep an explicitly configured FORCE_COLOR value, including 0.
    if (result.FORCE_COLOR === undefined) result.FORCE_COLOR = '3'
  }
  return result
}
