import { afterAll, beforeEach, describe, expect, test } from 'bun:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  enqueueOutbox,
  flushState,
  listOutbox,
  loadState,
  requeueFailedOutbox,
  reviewUnconfirmedOutbox,
  resetStateForTest,
  type OutboxEntry,
} from '../device/state'
import {
  deliverNow,
  flushOutbox,
  markQueuedFailed,
  markUnconfirmedSent,
  queueMessage,
} from '../device/operations'
import type { UserContentBlock } from '../device/protocol'
import { getEventBus, removeEventBus } from '../transport/event-bus'
import {
  handleWebSocketClose,
  handleWebSocketOpen,
} from '../transport/ws-handler'

const deviceId = 'dev_test1'
const localSessionId = 'db094c06-6f2d-470d-aef1-dcce041193b7'
let dataDir = ''

function entry(id: string, state: OutboxEntry['state']): OutboxEntry {
  return {
    id,
    deviceId,
    localSessionId,
    payload: { type: 'user', uuid: id, content: id },
    state,
    createdAt: Date.now(),
    updatedAt: Date.now(),
    attempts: 0,
  }
}

beforeEach(async () => {
  if (!dataDir) {
    dataDir = await mkdtemp(join(tmpdir(), 'rcs-outbox-'))
    process.env.RCS_DATA_DIR = dataDir
  }
  resetStateForTest()
  await loadState()
})

afterAll(async () => {
  resetStateForTest()
  if (dataDir) await rm(dataDir, { recursive: true, force: true })
})

describe('outbox failure handling', () => {
  test('a failed replay does not skip the failed message on the next connection', async () => {
    const bridgeId = 'session_1234567890abcdef'
    await deliverNow(bridgeId, deviceId, localSessionId, 'replay one')
    await deliverNow(bridgeId, deviceId, localSessionId, 'replay two')
    let writes = 0
    const failing = {
      readyState: 1,
      send() {
        if (++writes === 2) throw new Error('replay failed')
      },
      close() {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    handleWebSocketOpen(failing, bridgeId)
    const received: string[] = []
    const working = {
      readyState: 1,
      send(value: string) {
        received.push(value)
      },
      close() {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(working, bridgeId)
    try {
      expect(received).toHaveLength(1)
      expect(received[0]).toContain('replay two')
      markUnconfirmedSent(deviceId, localSessionId, bridgeId)
      expect(listOutbox(deviceId, localSessionId)).toHaveLength(0)
      await flushState()
    } finally {
      handleWebSocketClose(working, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })

  test('failed bridge writes remain unconfirmed and replay in order', async () => {
    const bridgeId = 'session_1234567890abcdef'
    const failing = {
      readyState: 1,
      send() {
        throw new Error('socket failed')
      },
      close() {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(failing, bridgeId)
    const first = await deliverNow(bridgeId, deviceId, localSessionId, 'first')
    expect(first.state).toBe('unconfirmed')
    const second = await deliverNow(
      bridgeId,
      deviceId,
      localSessionId,
      'second',
    )
    expect(second.state).toBe('unconfirmed')
    const received: string[] = []
    const working = {
      readyState: 1,
      send(value: string) {
        received.push(value)
      },
      close() {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const newToken = handleWebSocketOpen(working, bridgeId)
    try {
      expect(received).toHaveLength(2)
      expect(received[0]).toContain('first')
      expect(received[1]).toContain('second')
      markUnconfirmedSent(deviceId, localSessionId, bridgeId)
      // Sent entries are removed from the pending outbox.
      expect(listOutbox(deviceId, localSessionId)).toHaveLength(0)
      await flushState()
    } finally {
      handleWebSocketClose(working, bridgeId, 1000, '', newToken)
      handleWebSocketClose(failing, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })

  test('a recoverable failure never destroys a queued prompt', async () => {
    enqueueOutbox(entry('m1', 'queued'))
    await markQueuedFailed(
      deviceId,
      localSessionId,
      'already_running_without_bridge',
      '终端在运行',
    )
    expect(listOutbox(deviceId, localSessionId)[0]?.state).toBe('queued')
  })

  test('a permanent failure fails only never-published rows', async () => {
    enqueueOutbox(entry('m1', 'queued'))
    enqueueOutbox(entry('m2', 'unconfirmed'))
    await markQueuedFailed(
      deviceId,
      localSessionId,
      'session_not_found',
      '找不到会话',
    )
    const rows = listOutbox(deviceId, localSessionId)
    expect(rows.find(row => row.id === 'm1')).toBeUndefined()
    // Unconfirmed rows were already published; a bridge replay may deliver
    // them, so they must keep their state.
    expect(rows.find(row => row.id === 'm2')?.state).toBe('unconfirmed')
  })

  test('force restart requeues previously failed messages', () => {
    enqueueOutbox(entry('m1', 'failed'))
    enqueueOutbox(entry('m2', 'failed'))
    requeueFailedOutbox(deviceId, localSessionId)
    const rows = listOutbox(deviceId, localSessionId)
    expect(rows.map(row => row.state)).toEqual(['queued', 'queued'])
    expect(rows.map(row => row.id).sort()).toEqual(['m1', 'm2'])
  })

  test('restart and reconnect leave uncertain messages for review, without replay', async () => {
    const bridgeId = 'session_abcdef0123456789'
    enqueueOutbox({
      ...entry('uncertain', 'unconfirmed'),
      bridgeSessionId: bridgeId,
    })
    await flushState()
    resetStateForTest()
    await loadState()

    markUnconfirmedSent(deviceId, localSessionId, bridgeId)
    await flushOutbox(deviceId, localSessionId, bridgeId)

    const [row] = listOutbox(deviceId, localSessionId)
    expect(row?.state).toBe('unconfirmed')
    expect(row?.requiresReview).toBe(true)
    expect(getEventBus(bridgeId).getEventsSince(0)).toHaveLength(0)
    expect(reviewUnconfirmedOutbox(deviceId, localSessionId)).toBe(1)
    expect(listOutbox(deviceId, localSessionId)).toHaveLength(0)
    requeueFailedOutbox(deviceId, localSessionId)
    expect(listOutbox(deviceId, localSessionId)).toHaveLength(0)
  })

  test('a bridge attach cannot confirm a message before it was published', async () => {
    const bridgeId = 'session_0123456789abcdef'
    enqueueOutbox(entry('preparing', 'queued'))
    const flush = flushOutbox(deviceId, localSessionId, bridgeId)
    markUnconfirmedSent(deviceId, localSessionId, bridgeId)
    expect(listOutbox(deviceId, localSessionId)[0]?.state).toBe('unconfirmed')
    await flush
    // No bridge was attached; publishing to the in-memory bus is not receipt.
    expect(listOutbox(deviceId, localSessionId)[0]?.state).toBe('unconfirmed')
  })
})

describe('image content blocks through the outbox', () => {
  const imageBlocks: UserContentBlock[] = [
    {
      type: 'image',
      source: { type: 'base64', media_type: 'image/png', data: 'aGk=' },
    },
  ]
  // A private data dir per test: the shared one replays earlier tests'
  // persisted outbox rows through loadState and pollutes listOutbox.
  const dirs: string[] = []
  beforeEach(async () => {
    const dir = await mkdtemp(join(tmpdir(), 'rcs-outbox-img-'))
    dirs.push(dir)
    process.env.RCS_DATA_DIR = dir
    resetStateForTest()
    await loadState()
  })
  afterAll(async () => {
    for (const dir of dirs) await rm(dir, { recursive: true, force: true })
  })

  test('queueMessage stores a pure image turn as an image-only content array', () => {
    const entry = queueMessage(deviceId, localSessionId, imageBlocks)
    const payload = entry.payload as {
      type: string
      uuid: string
      content: unknown
    }
    expect(payload.type).toBe('user')
    // The outbox id doubles as the bridge uuid the CLI echoes back.
    expect(payload.uuid).toBe(entry.id)
    expect(payload.content).toEqual(imageBlocks)
    // Storing the same base64 array twice doubles persisted data and can
    // exceed the server's HTTP limit for photos selected on a phone.
    expect('message' in payload).toBe(false)
    expect((payload.content as Array<{ type: string }>)[0]?.type).toBe('image')
  })

  test('queueMessage orders the text block before the image blocks', () => {
    const content: UserContentBlock[] = [
      { type: 'text', text: '看图' },
      ...imageBlocks,
    ]
    const entry = queueMessage(deviceId, localSessionId, content)
    const blocks = (entry.payload as { content: Array<{ type: string }> })
      .content
    expect(blocks.map(block => block.type)).toEqual(['text', 'image'])
  })

  test('flushOutbox republishes array content untouched on retry', async () => {
    const bridgeId = 'session_feedface0badcafe'
    const queued = queueMessage(deviceId, localSessionId, imageBlocks)
    // No bridge transport is attached: the entry is published to the bus but
    // stays unconfirmed — exactly the retry path a reconnect replays.
    await flushOutbox(deviceId, localSessionId, bridgeId)
    const row = listOutbox(deviceId, localSessionId).find(
      entry => entry.id === queued.id,
    )
    expect(row?.state).toBe('unconfirmed')
    const events = getEventBus(bridgeId).getEventsSince(0)
    expect(events).toHaveLength(1)
    const raw = (events[0]!.payload as { raw: { content: unknown } }).raw
    expect(raw.content).toEqual(imageBlocks)
    removeEventBus(bridgeId)
  })

  test('deliverNow forwards the image array to a live bridge transport', async () => {
    const bridgeId = 'session_00c0ffee01234567'
    const sent: string[] = []
    const ws = {
      readyState: 1,
      send: (data: string) => sent.push(data),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridgeId)
    try {
      const delivered = await deliverNow(
        bridgeId,
        deviceId,
        localSessionId,
        imageBlocks,
      )
      expect(delivered.state).toBe('sent')
      // The bridge WS frame is the SDK shape: message.content must still be
      // the block array, not a flattened string.
      const frame = sent
        .map(data => JSON.parse(data) as Record<string, unknown>)
        .find(message => message.type === 'user') as {
        message: { content: unknown }
      }
      expect(frame.message.content).toEqual(imageBlocks)
      expect(delivered.entry.state).toBe('sent')
      // Delivered rows leave the pending view (listOutbox only returns
      // queued/unconfirmed), so the image turn is fully handed off.
      expect(
        listOutbox(deviceId, localSessionId).some(
          entry => entry.id === delivered.entry.id,
        ),
      ).toBe(false)
    } finally {
      handleWebSocketClose(ws, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })
})
