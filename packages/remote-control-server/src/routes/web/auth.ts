import { Hono } from 'hono'
import { uuidAuth } from '../../auth/middleware'
import { storeBindSession } from '../../store'
import {
  resolveExistingWebSessionId,
  toWebSessionId,
} from '../../services/session'

const app = new Hono()

/** POST /web/bind — Bind a session to a UUID. Gated by uuidAuth like every
 *  other /web route: claiming arbitrary sessions must not be anonymous. */
app.post('/bind', uuidAuth, async c => {
  const body = await c.req.json()
  const sessionId = body.sessionId
  // UUID can come from query param (api.js sends it in URL) or body
  const uuid = c.req.query('uuid') || body.uuid

  if (!sessionId || !uuid) {
    return c.json({ error: 'sessionId and uuid are required' }, 400)
  }

  const resolvedSessionId = resolveExistingWebSessionId(sessionId)
  if (!resolvedSessionId) {
    return c.json({ error: 'Session not found' }, 404)
  }

  storeBindSession(resolvedSessionId, uuid)
  return c.json({ ok: true, sessionId: toWebSessionId(resolvedSessionId) })
})

export default app
