import { describe, test, expect, beforeEach, mock } from 'bun:test'

// Same shape as routes.test.ts — mock.module is process-global, so whichever
// file loads last must satisfy every test file in this directory.
mock.module('../config', () => ({
  config: {
    port: 3000,
    host: '0.0.0.0',
    apiKeys: ['test-api-key'],
    baseUrl: 'http://localhost:3000',
    pollTimeout: 1,
    heartbeatInterval: 20,
    jwtExpiresIn: 3600,
    disconnectTimeout: 300,
    webCorsOrigins: [],
    wsIdleTimeout: 30,
    wsKeepaliveInterval: 20,
    webAccessKey: '',
  },
  getBaseUrl: () => 'http://localhost:3000',
}))

import { Hono } from 'hono'
import webClientLogs from '../routes/web/client-logs'
import { resetClientLogStoreForTest } from '../services/client-log-store'

function createApp() {
  const app = new Hono()
  app.route('/web', webClientLogs)
  return app
}

function entry(seq: number, tag = 'api', message = `m${seq}`) {
  return { seq, at: Date.now() - 1000, tag, message }
}

async function postLogs(app: Hono, uuid: string, entries: unknown[]) {
  return app.request(`/web/client-logs?uuid=${uuid}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ entries }),
  })
}

describe('client-logs routes', () => {
  let app: Hono

  beforeEach(() => {
    resetClientLogStoreForTest()
    delete process.env.RCS_CLIENT_LOG_UPLOAD
    app = createApp()
  })

  test('config reports upload enabled by default', async () => {
    const res = await app.request('/web/client-logs/config?uuid=u1')
    expect(res.status).toBe(200)
    expect(await res.json()).toEqual({ enabled: true })
  })

  test('stores uploaded entries and returns them on GET', async () => {
    const post = await postLogs(app, 'u1', [
      entry(1),
      entry(2, 'sse', 'opened'),
    ])
    expect(post.status).toBe(200)
    expect(await post.json()).toEqual({ stored: 2, dropped: 0 })

    const res = await app.request('/web/client-logs?uuid=u1')
    expect(res.status).toBe(200)
    const body = (await res.json()) as { uuid: string; entries: unknown[] }
    expect(body.uuid).toBe('u1')
    expect(body.entries).toHaveLength(2)
  })

  test('GET defaults to the requesting uuid', async () => {
    await postLogs(app, 'u2', [entry(1, 'api', 'hello')])
    const res = await app.request('/web/client-logs?uuid=u2')
    const body = (await res.json()) as { entries: Array<{ message: string }> }
    expect(body.entries[0].message).toBe('hello')
  })

  test('rejects a body without entries[]', async () => {
    const res = await app.request('/web/client-logs?uuid=u1', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ nope: true }),
    })
    expect(res.status).toBe(400)
  })

  test('rejects invalid JSON', async () => {
    const res = await app.request('/web/client-logs?uuid=u1', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: 'not json',
    })
    expect(res.status).toBe(400)
  })

  test('caps a batch at 200 entries', async () => {
    const many = Array.from({ length: 250 }, (_, i) => entry(i + 1))
    const res = await postLogs(app, 'u1', many)
    expect(res.status).toBe(200)
    expect(await res.json()).toEqual({ stored: 200, dropped: 0 })
    const get = await app.request('/web/client-logs?uuid=u1&limit=1000')
    const body = (await get.json()) as { entries: unknown[] }
    expect(body.entries).toHaveLength(200)
  })

  test('replaying the same batch stores nothing (seq idempotency)', async () => {
    await postLogs(app, 'u1', [entry(1), entry(2)])
    const res = await postLogs(app, 'u1', [entry(1), entry(2)])
    expect(res.status).toBe(200)
    expect(await res.json()).toEqual({ stored: 0, dropped: 2 })
  })

  test('keeps only the newest 1000 entries per uuid', async () => {
    let seq = 0
    for (let batch = 0; batch < 6; batch++) {
      const entries = Array.from({ length: 200 }, () => entry(++seq))
      await postLogs(app, 'u1', entries)
    }
    const res = await app.request('/web/client-logs?uuid=u1&limit=1000')
    const body = (await res.json()) as { entries: Array<{ seq: number }> }
    expect(body.entries).toHaveLength(1000)
    expect(body.entries[0].seq).toBe(201)
    expect(body.entries[999].seq).toBe(1200)
  })

  test('drops malformed entries but stores valid ones', async () => {
    const res = await postLogs(app, 'u1', [
      entry(1),
      'garbage',
      null,
      { seq: -5, at: Date.now(), tag: 'x', message: 'y' },
      { seq: 2.5, at: Date.now(), tag: 'x', message: 'y' },
    ])
    expect(res.status).toBe(200)
    expect(await res.json()).toEqual({ stored: 1, dropped: 4 })
  })

  test('truncates oversized tag and message', async () => {
    await postLogs(app, 'u1', [
      {
        seq: 1,
        at: Date.now(),
        tag: 't'.repeat(100),
        message: 'x'.repeat(5000),
      },
    ])
    const res = await app.request('/web/client-logs?uuid=u1')
    const body = (await res.json()) as {
      entries: Array<{ tag: string; message: string }>
    }
    expect(body.entries[0].tag).toHaveLength(32)
    expect(body.entries[0].message).toHaveLength(2000)
  })

  test('rate limits more than 20 uploads per minute', async () => {
    let lastStatus = 200
    for (let i = 0; i < 21; i++) {
      const res = await postLogs(app, 'u1', [entry(i + 1)])
      lastStatus = res.status
    }
    expect(lastStatus).toBe(429)
    // A different uuid is unaffected.
    const other = await postLogs(app, 'u2', [entry(1)])
    expect(other.status).toBe(200)
  })

  test('RCS_CLIENT_LOG_UPLOAD=0 disables config, ingest and retrieval', async () => {
    process.env.RCS_CLIENT_LOG_UPLOAD = '0'
    try {
      const config = await app.request('/web/client-logs/config?uuid=u1')
      expect(config.status).toBe(200)
      expect(await config.json()).toEqual({ enabled: false })

      const post = await postLogs(app, 'u1', [entry(1)])
      expect(post.status).toBe(403)

      const get = await app.request('/web/client-logs?uuid=u1')
      expect(get.status).toBe(403)
    } finally {
      delete process.env.RCS_CLIENT_LOG_UPLOAD
    }
  })

  test('requires a uuid when no access key is configured', async () => {
    const res = await app.request('/web/client-logs/config')
    expect(res.status).toBe(401)
  })
})
