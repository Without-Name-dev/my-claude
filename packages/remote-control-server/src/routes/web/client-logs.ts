import { Hono } from 'hono'
import { uuidAuth } from '../../auth/middleware'
import {
  isClientLogRateLimited,
  isClientLogUploadEnabled,
  listClientLogs,
  recordClientLogs,
} from '../../services/client-log-store'

const app = new Hono()

/**
 * GET /web/client-logs/config — the client bootstraps its upload decision
 * from this. Unauthenticated in spirit (still behind uuidAuth) so a fresh
 * page load learns "server says off" before shipping anything.
 */
app.get('/client-logs/config', uuidAuth, async c => {
  return c.json({ enabled: isClientLogUploadEnabled() }, 200)
})

/**
 * POST /web/client-logs — batch upload from the web/iOS client.
 * Body: { entries: [{ seq, at, tag, message }, ...] }. The uuid owning the
 * buffer comes from auth (?uuid= or the access-key holder), never the body.
 */
app.post('/client-logs', uuidAuth, async c => {
  if (!isClientLogUploadEnabled()) {
    return c.json(
      {
        error: {
          type: 'client_logs_disabled',
          message: 'Client log upload is disabled on this server',
        },
      },
      403,
    )
  }
  const uuid = c.get('uuid')!
  if (isClientLogRateLimited(uuid)) {
    return c.json(
      { error: { type: 'rate_limited', message: 'Too many log uploads' } },
      429,
    )
  }
  const body = await c.req.json().catch(() => null)
  const entries =
    typeof body === 'object' &&
    body !== null &&
    Array.isArray((body as Record<string, unknown>).entries)
      ? (body as Record<string, unknown>).entries
      : null
  if (!entries) {
    return c.json(
      { error: { type: 'bad_request', message: 'entries[] is required' } },
      400,
    )
  }
  const result = recordClientLogs(uuid, entries)
  return c.json(result, 200)
})

/**
 * GET /web/client-logs?uuid=<target>&limit=N — read a client's buffer back
 * for diagnosis. `uuid` defaults to the requester's own.
 */
app.get('/client-logs', uuidAuth, async c => {
  if (!isClientLogUploadEnabled()) {
    return c.json(
      {
        error: {
          type: 'client_logs_disabled',
          message: 'Client log upload is disabled on this server',
        },
      },
      403,
    )
  }
  const target = c.req.query('uuid') || c.get('uuid')!
  const limitRaw = c.req.query('limit')
  const parsed = limitRaw ? parseInt(limitRaw, 10) : 200
  const limit = Number.isFinite(parsed) ? parsed : 200
  return c.json({ uuid: target, entries: listClientLogs(target, limit) }, 200)
})

export default app
