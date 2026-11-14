import { describe, test, expect, beforeEach, mock } from 'bun:test'

// Mock config
const mockConfig = {
  port: 3000,
  host: '0.0.0.0',
  apiKeys: ['test-api-key'],
  baseUrl: 'http://localhost:3000',
  pollTimeout: 8,
  heartbeatInterval: 20,
  jwtExpiresIn: 3600,
  disconnectTimeout: 300,
  webCorsOrigins: [],
  wsIdleTimeout: 30,
  wsKeepaliveInterval: 20,
}

mock.module('../config', () => ({
  config: mockConfig,
  getBaseUrl: () => 'http://localhost:3000',
}))

import { Hono } from 'hono'
import { storeReset } from '../store'
import {
  removeEventBus,
  getAllEventBuses,
  getEventBus,
} from '../transport/event-bus'
import { createSSEWriter, createSSEStream } from '../transport/sse-writer'
import { publishSessionEvent } from '../services/transport'

/** Read up to N bytes from a Response stream, then cancel */
async function readPartialStream(
  res: Response,
  maxBytes = 4096,
): Promise<string> {
  const reader = res.body?.getReader()
  if (!reader) return ''
  const chunks: Uint8Array[] = []
  let totalBytes = 0
  try {
    while (totalBytes < maxBytes) {
      const { done, value } = await reader.read()
      if (done) break
      chunks.push(value)
      totalBytes += value.length
      // Cancel after we have some data (first keepalive + any initial events)
      if (totalBytes > 0) break
    }
  } finally {
    reader.cancel()
  }
  const combined = new Uint8Array(totalBytes)
  let offset = 0
  for (const chunk of chunks) {
    combined.set(chunk, offset)
    offset += chunk.length
  }
  return new TextDecoder().decode(combined)
}

describe('SSE Writer', () => {
  describe('createSSEWriter', () => {
    test('creates SSEWriter with send and close methods', () => {
      const app = new Hono()
      let capturedWriter: ReturnType<typeof createSSEWriter> | null = null

      app.get('/test', c => {
        capturedWriter = createSSEWriter(c)
        return c.text('ok')
      })

      app.request('/test')
      expect(capturedWriter).not.toBeNull()
      expect(typeof capturedWriter!.send).toBe('function')
      expect(typeof capturedWriter!.close).toBe('function')
    })
  })

  describe('createSSEStream', () => {
    beforeEach(() => {
      storeReset()
      for (const [key] of getAllEventBuses()) {
        removeEventBus(key)
      }
    })

    test('sends terminal image data once while retaining the message and metadata', async () => {
      const sessionId = 'sse-image'
      publishSessionEvent(sessionId, 'system', { content: 'before' }, 'inbound')
      const image = 'UNIQUE_BASE64_IMAGE_DATA'
      publishSessionEvent(
        sessionId,
        'user',
        {
          uuid: 'photo-id',
          isSynthetic: false,
          message: {
            role: 'user',
            content: [
              {
                type: 'image',
                source: {
                  type: 'base64',
                  media_type: 'image/png',
                  data: image,
                },
              },
            ],
          },
        },
        'inbound',
      )
      const app = new Hono()
      app.get('/stream', c => createSSEStream(c, sessionId, 1))
      const response = await app.request('/stream')
      const text = await readPartialStream(response)
      expect(text.split(image)).toHaveLength(2)
      expect(text).toContain('photo-id')
      expect(text).toContain('image/png')
    })

    test('returns Response with correct SSE headers', async () => {
      const app = new Hono()

      app.get('/stream/:sessionId', c => {
        const sessionId = c.req.param('sessionId')
        return createSSEStream(c, sessionId, 0)
      })

      const res = await app.request('/stream/s1')
      expect(res.status).toBe(200)
      expect(res.headers.get('Content-Type')).toBe('text/event-stream')
      expect(res.headers.get('Cache-Control')).toBe('no-cache')
      expect(res.headers.get('Connection')).toBe('keep-alive')
      expect(res.headers.get('X-Accel-Buffering')).toBe('no')

      // Cancel the stream
      res.body?.cancel()
    })

    test('sends initial keepalive', async () => {
      const app = new Hono()

      app.get('/stream/:sessionId', c => {
        const sessionId = c.req.param('sessionId')
        return createSSEStream(c, sessionId, 0)
      })

      const res = await app.request('/stream/s2')
      const text = await readPartialStream(res)
      expect(text).toContain(': keepalive')
    })

    test('sends historical events when fromSeqNum > 0', async () => {
      // Pre-populate event bus with events
      const bus = getEventBus('s3')
      bus.publish({
        id: 'e1',
        sessionId: 's3',
        type: 'user',
        payload: { content: 'hello' },
        direction: 'outbound',
      })
      bus.publish({
        id: 'e2',
        sessionId: 's3',
        type: 'assistant',
        payload: { content: 'hi' },
        direction: 'inbound',
      })

      const app = new Hono()

      app.get('/stream/:sessionId', c => {
        const sessionId = c.req.param('sessionId')
        const fromSeq = parseInt(c.req.query('fromSeq') || '0', 10)
        return createSSEStream(c, sessionId, fromSeq)
      })

      const res = await app.request('/stream/s3?fromSeq=1')
      const text = await readPartialStream(res)
      // Should replay events since seq 1 (i.e., event 2)
      expect(text).toContain('"seqNum":2')
      expect(text).toContain('assistant')
    })

    test('asks for a resync when the cursor cannot be replayed', async () => {
      // A restarted bus numbers events from 1 again, so the browser's cursor
      // is ahead of everything the server has.
      const bus = getEventBus('s4')
      bus.publish({
        id: 'e1',
        sessionId: 's4',
        type: 'user',
        payload: {},
        direction: 'outbound',
      })

      const app = new Hono()
      app.get('/stream/:sessionId', c => {
        const sessionId = c.req.param('sessionId')
        const fromSeq = parseInt(c.req.query('fromSeq') || '0', 10)
        return createSSEStream(c, sessionId, fromSeq)
      })

      const res = await app.request('/stream/s4?fromSeq=500')
      const text = await readPartialStream(res)
      expect(text).toContain('event: resync')
      expect(text).toContain('"seqNum":1')
      // The incomplete tail must not be sent as if the stream were whole.
      expect(text).not.toContain('event: message')
    })

    test('no historical events when fromSeqNum is 0', async () => {
      const bus = getEventBus('s5')
      bus.publish({
        id: 'e1',
        sessionId: 's5',
        type: 'user',
        payload: {},
        direction: 'outbound',
      })

      const app = new Hono()

      app.get('/stream/:sessionId', c => {
        const sessionId = c.req.param('sessionId')
        return createSSEStream(c, sessionId, 0)
      })

      const res = await app.request('/stream/s5')
      const text = await readPartialStream(res)
      // With fromSeqNum=0, no historical replay, just keepalive
      expect(text).toContain(': keepalive')
      // Should NOT contain event data (only keepalive)
      expect(text).not.toContain('event: message')
    })

    test('snapshots the stored session status for mid-turn subscribers', async () => {
      // A phone that opens the stream while a turn is already running missed
      // the session_status event that started it.
      const { storeCreateSession, storeUpdateSession } = await import(
        '../store'
      )
      const session = storeCreateSession({})
      storeUpdateSession(session.id, { status: 'running' })

      const app = new Hono()
      app.get('/stream/:sessionId', c => {
        const sessionId = c.req.param('sessionId')
        return createSSEStream(c, sessionId, 0)
      })

      const res = await app.request(`/stream/${session.id}`)
      // keepalive and snapshot enqueue as separate chunks; read both.
      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let text = ''
      for (let i = 0; i < 3; i += 1) {
        const { value } = await reader.read()
        text += decoder.decode(value ?? new Uint8Array())
        if (text.includes('session_status')) break
      }
      reader.cancel()
      expect(text).toContain('event: message')
      expect(text).toContain('"session_status"')
      expect(text).toContain('"running"')
      expect(text).toContain('"since"')
      // No id line: the snapshot must not move the client's replay cursor.
      expect(text).not.toMatch(/^id: /m)
    })

    test('streams an image-only user turn with its content blocks intact', async () => {
      // The device photo pipeline publishes user turns whose content is an
      // Anthropic block array. normalizePayload flattens a display string but
      // keeps the array on message.content — the SSE frame must carry it
      // through untouched (no string coercion anywhere in the path).
      const { publishSessionEvent } = await import('../services/transport')
      const app = new Hono()
      app.get('/stream/:sessionId', c => {
        const sessionId = c.req.param('sessionId')
        return createSSEStream(c, sessionId, 0)
      })

      const res = await app.request('/stream/s-img')
      const blocks = [
        {
          type: 'image',
          source: { type: 'base64', media_type: 'image/png', data: 'aGk=' },
        },
      ]
      publishSessionEvent(
        's-img',
        'user',
        {
          type: 'user',
          uuid: 'u-img',
          content: blocks,
          message: { content: blocks },
        },
        'outbound',
      )
      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let text = ''
      for (let i = 0; i < 3; i += 1) {
        const { value } = await reader.read()
        text += decoder.decode(value ?? new Uint8Array())
        if (text.includes('"u-img"')) break
      }
      reader.cancel()
      expect(text).toContain('"type":"user"')
      expect(text).toContain('"content":[{"type":"image"')
      expect(text).toContain('"data":"aGk="')
    })
  })
})
