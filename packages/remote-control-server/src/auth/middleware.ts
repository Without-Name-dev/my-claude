import type { Context, Next } from 'hono'
import { timingSafeEqual } from 'node:crypto'
import { config } from '../config'
import { validateApiKey } from './api-key'
import { verifyWorkerJwt } from './jwt'
import { resolveToken } from './token'

const WS_AUTH_PROTOCOL_PREFIX = 'rcs.auth.'

/** Encode a bearer token for WebSocket clients that cannot send auth headers. */
export function encodeWebSocketAuthProtocol(token: string): string {
  return `${WS_AUTH_PROTOCOL_PREFIX}${Buffer.from(token, 'utf8').toString('base64url')}`
}

function decodeWebSocketAuthProtocol(
  protocolHeader: string | undefined,
): string | undefined {
  if (!protocolHeader) {
    return undefined
  }

  for (const protocol of protocolHeader.split(',')) {
    const trimmed = protocol.trim()
    if (!trimmed.startsWith(WS_AUTH_PROTOCOL_PREFIX)) {
      continue
    }

    const encoded = trimmed.slice(WS_AUTH_PROTOCOL_PREFIX.length)
    if (!encoded) {
      return undefined
    }

    try {
      const token = Buffer.from(encoded, 'base64url').toString('utf8')
      return token.length > 0 ? token : undefined
    } catch {
      return undefined
    }
  }

  return undefined
}

/** Extract a Bearer token from the Authorization header only. */
export function extractBearerToken(c: Context): string | undefined {
  const authHeader = c.req.header('Authorization')
  return authHeader?.startsWith('Bearer ')
    ? authHeader.slice('Bearer '.length)
    : undefined
}

/** Extract auth for WebSocket upgrades without putting secrets in query strings. */
export function extractWebSocketAuthToken(c: Context): string | undefined {
  return (
    extractBearerToken(c) ??
    decodeWebSocketAuthProtocol(c.req.header('Sec-WebSocket-Protocol'))
  )
}

/**
 * Unified authentication middleware — supports two modes:
 *
 * 1. **Token mode** (Web UI): Bearer token resolved via server-side lookup → username injected
 * 2. **API Key mode** (CLI bridge): Valid API key + X-Username header → username injected
 */
export async function apiKeyAuth(c: Context, next: Next) {
  const token = extractBearerToken(c)

  // Try token authentication (Web UI)
  const tokenUsername = resolveToken(token)
  if (tokenUsername) {
    c.set('username', tokenUsername)
    await next()
    return
  }

  // Try API Key authentication (CLI bridge)
  if (validateApiKey(token)) {
    // Extract username from X-Username header or ?username= query param
    const username = c.req.header('X-Username') || c.req.query('username')
    if (username) {
      c.set('username', username)
    }
    await next()
    return
  }

  return c.json(
    {
      error: { type: 'unauthorized', message: 'Invalid or missing auth token' },
    },
    401,
  )
}

/**
 * Session ingress authentication — accepts both API key and worker JWT.
 *
 * Used for SSE stream, CCR worker events, and WebSocket ingress endpoints.
 * On JWT validation, stores the decoded payload in c.set("jwtPayload") for
 * downstream handlers to inspect session_id if needed.
 */
export async function sessionIngressAuth(c: Context, next: Next) {
  const token = extractWebSocketAuthToken(c)

  if (!token) {
    return c.json(
      { error: { type: 'unauthorized', message: 'Missing auth token' } },
      401,
    )
  }

  // Try API key first (backward compatible)
  if (validateApiKey(token)) {
    await next()
    return
  }

  // Try JWT verification — validate session_id matches route param
  const payload = verifyWorkerJwt(token)
  if (payload) {
    const routeSessionId = c.req.param('id') || c.req.param('sessionId')
    if (routeSessionId && payload.session_id !== routeSessionId) {
      return c.json(
        {
          error: {
            type: 'forbidden',
            message: 'JWT session_id does not match target session',
          },
        },
        403,
      )
    }
    c.set('jwtPayload', payload)
    await next()
    return
  }

  return c.json(
    { error: { type: 'unauthorized', message: 'Invalid API key or JWT' } },
    401,
  )
}

/** Accept CLI headers but don't validate them */
export async function acceptCliHeaders(c: Context, next: Next) {
  await next()
}

/**
 * Extract UUID from request — query param ?uuid= or header X-UUID
 */
export function getUuidFromRequest(c: Context): string | undefined {
  return c.req.query('uuid') || c.req.header('X-UUID')
}

/**
 * UUID-based auth for Web UI routes (no-login mode).
 * Accepts UUID in query param/header, OR a valid API key via Authorization header.
 *
 * When `RCS_WEB_ACCESS_KEY` is configured, the UUID fallback is disabled: the
 * request must carry the access key (or a server API key). The key travels as
 * a Bearer token on fetches and as the `rcs_access_key` cookie on the SSE
 * EventSource, which cannot send headers. On a publicly reachable server,
 * trusting any self-declared uuid would hand out conversation history and
 * remote-control capabilities to strangers.
 */
export async function uuidAuth(c: Context, next: Next) {
  // Try API key auth via Authorization header
  const bearer = extractBearerToken(c)
  if (bearer && validateApiKey(bearer)) {
    // Valid API key — generate a stable UUID from the key for downstream use
    const uuid = getUuidFromRequest(c)
    c.set('uuid', uuid || bearer)
    await next()
    return
  }

  const accessKey = config.webAccessKey
  if (accessKey) {
    const presented =
      bearer ?? readAccessCookie(c.req.header('Cookie') ?? undefined)
    const matches =
      presented !== undefined &&
      presented.length === accessKey.length &&
      timingSafeEqual(Buffer.from(presented), Buffer.from(accessKey))
    if (!matches) {
      // Diagnostic only — never log the value, just whether one arrived and
      // how long it is. This is how we tell "browser sent nothing" apart from
      // "browser sent the wrong key" when the gate loops.
      console.warn(
        `[auth] 401 ${c.req.method} ${c.req.path} bearer=${
          bearer ? `len:${bearer.length}` : 'none'
        } cookie=${c.req.header('Cookie') ? 'yes' : 'no'} expectedLen:${accessKey.length}`,
      )
      return c.json(
        {
          error: {
            type: 'unauthorized',
            message: '需要访问密钥（Access Key）',
          },
        },
        401,
      )
    }
    const uuid = getUuidFromRequest(c)
    c.set('uuid', uuid || 'web-key-holder')
    await next()
    return
  }

  // Fall back to UUID auth (private/loopback deployments only)
  const uuid = getUuidFromRequest(c)
  if (!uuid) {
    return c.json(
      { error: { type: 'unauthorized', message: 'Missing UUID' } },
      401,
    )
  }
  c.set('uuid', uuid)
  await next()
}

/** The web app mirrors the access key into this cookie so the SSE
 *  EventSource — which cannot send headers — authenticates too. */
const ACCESS_COOKIE = 'rcs_access_key'

function readAccessCookie(header: string | undefined): string | undefined {
  if (!header) return undefined
  for (const part of header.split(';')) {
    const eq = part.indexOf('=')
    if (eq < 0) continue
    if (part.slice(0, eq).trim() === ACCESS_COOKIE) {
      try {
        return decodeURIComponent(part.slice(eq + 1).trim())
      } catch {
        return undefined
      }
    }
  }
  return undefined
}
