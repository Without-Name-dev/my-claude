export const config = {
  version: process.env.RCS_VERSION || '0.1.0',
  port: parseInt(process.env.RCS_PORT || '3000', 10),
  host: process.env.RCS_HOST || '0.0.0.0',
  apiKeys: (process.env.RCS_API_KEYS || '').split(',').filter(Boolean),
  baseUrl: process.env.RCS_BASE_URL || '',
  pollTimeout: parseInt(process.env.RCS_POLL_TIMEOUT || '8', 10),
  heartbeatInterval: parseInt(process.env.RCS_HEARTBEAT_INTERVAL || '20', 10),
  jwtExpiresIn: parseInt(process.env.RCS_JWT_EXPIRES_IN || '3600', 10),
  disconnectTimeout: parseInt(process.env.RCS_DISCONNECT_TIMEOUT || '300', 10),
  webCorsOrigins: (process.env.RCS_WEB_CORS_ORIGINS || '')
    .split(',')
    .map(origin => origin.trim())
    .filter(Boolean),
  /** Bun WebSocket idle timeout (seconds). Bun sends protocol-level pings after
   *  this many seconds of no received data. Must be shorter than any reverse
   *  proxy's idle timeout (nginx default 60s, Cloudflare 100s). Default 30s. */
  wsIdleTimeout: parseInt(process.env.RCS_WS_IDLE_TIMEOUT || '30', 10),
  /** Server→client keep_alive data-frame interval (seconds). Keeps reverse
   *  proxies from closing idle connections. Default 20s. */
  wsKeepaliveInterval: parseInt(
    process.env.RCS_WS_KEEPALIVE_INTERVAL || '20',
    10,
  ),
  /**
   * Shared secret for the /web routes. Without it every /web endpoint accepts
   * any ?uuid= — fine on a loopback dev server, not on a public one: the device
   * APIs expose full conversation history and can queue prompts the Spawner
   * will execute when the machine wakes. When set, web clients must present it
   * as a Bearer token (a valid server API key also passes).
   */
  webAccessKey: process.env.RCS_WEB_ACCESS_KEY || '',
} as const

export function getBaseUrl(): string {
  const url = config.baseUrl || `http://localhost:${config.port}`
  return url.replace(/\/+$/, '')
}
