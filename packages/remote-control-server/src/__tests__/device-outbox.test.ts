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
  setMapping,
  type OutboxEntry,
} from '../device/state'
import {
  deliverNow,
  flushOutbox,
  markQueuedFailed,
  markUnconfirmedSent,
  queueMessage,
} from '../device/operations'
import { resetConversationStoreForTest } from '../device/conversation-store'
import type { UserContentBlock } from '../device/protocol'
import { getEventBus, removeEventBus } from '../transport/event-bus'
import { publishSessionEvent } from '../services/transport'
import {
  handleWebSocketClose,
  handleWebSocketOpen,
} from '../transport/ws-handler'

const deviceId = 'dev_test1'
const localSessionId = 'db094c06-6f2d-470d-aef1-dcce041193b7'
let dataDir = ''
const dataDirs: string[] = []

function mapBridge(bridgeSessionId: string, sessionId = localSessionId) {
  setMapping({
    deviceId,
    localSessionId: sessionId,
    bridgeSessionId,
    cwd: 'C:\\test',
    updatedAt: Date.now(),
  })
}

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
  resetConversationStoreForTest()
  dataDir = await mkdtemp(join(tmpdir(), 'rcs-outbox-'))
  dataDirs.push(dataDir)
  process.env.RCS_DATA_DIR = dataDir
  resetStateForTest()
  await loadState()
})

afterAll(async () => {
  resetStateForTest()
  // The outbox recovery path opens the conversation store in this data dir;
  // SQLite holds the file open until the connection is closed.
  resetConversationStoreForTest()
  for (const dir of dataDirs) await rm(dir, { recursive: true, force: true })
})

describe('outbox failure handling', () => {
  test('never publishes to a bridge after it has moved to another conversation', async () => {
    const bridgeId = `moved-${crypto.randomUUID()}`
    mapBridge(bridgeId)
    const queued = queueMessage(deviceId, localSessionId, 'must stay queued')
    mapBridge(bridgeId, crypto.randomUUID())
    await flushOutbox(deviceId, localSessionId, bridgeId)
    expect(
      listOutbox(deviceId, localSessionId).find(row => row.id === queued.id)
        ?.state,
    ).toBe('queued')
    expect(getEventBus(bridgeId).getEventsSince(0)).toHaveLength(0)
    await expect(
      deliverNow(
        bridgeId,
        deviceId,
        localSessionId,
        'must stay queued',
        queued.id,
      ),
    ).rejects.toThrow()
    removeEventBus(bridgeId)
  })
  test('a socket write without CLI echo remains uncertain after server restart', async () => {
    const bridgeId = `unacknowledged-${crypto.randomUUID()}`
    mapBridge(bridgeId)
    const ws = {
      readyState: 1,
      send() {},
      close() {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridgeId)
    try {
      const delivery = await deliverNow(
        bridgeId,
        deviceId,
        localSessionId,
        'hello',
        crypto.randomUUID(),
      )
      expect(delivery.state).toBe('unconfirmed')
      await flushState({ throwOnError: true })
      resetStateForTest()
      await loadState()
      const restored = listOutbox(deviceId, localSessionId).find(
        row => row.id === delivery.entry.id,
      )
      expect(restored?.state).toBe('unconfirmed')
      expect(restored?.requiresReview).toBe(true)
    } finally {
      handleWebSocketClose(ws, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })
  test('concurrent retries with the same client UUID publish once', async () => {
    const bridgeId = `durable-retry-${crypto.randomUUID()}`
    const testSession = crypto.randomUUID()
    mapBridge(bridgeId, testSession)
    try {
      const [first, retry] = await Promise.all([
        deliverNow(bridgeId, deviceId, testSession, 'hello', 'client-uuid'),
        deliverNow(bridgeId, deviceId, testSession, 'hello', 'client-uuid'),
      ])
      expect(first.entry.id).toBe('client-uuid')
      expect(retry.entry.id).toBe('client-uuid')
      expect(
        getEventBus(bridgeId)
          .getEventsSince(0)
          .filter(event => event.direction === 'outbound'),
      ).toHaveLength(1)
      expect(listOutbox(deviceId, testSession)).toHaveLength(1)
      await expect(
        deliverNow(bridgeId, deviceId, testSession, 'different', 'client-uuid'),
      ).rejects.toThrow()
    } finally {
      removeEventBus(bridgeId)
    }
  })

  test('a direct web send survives restart without blindly replaying uncertain delivery', async () => {
    const bridgeId = `durable-restart-${crypto.randomUUID()}`
    const testSession = crypto.randomUUID()
    mapBridge(bridgeId, testSession)
    await deliverNow(
      bridgeId,
      deviceId,
      testSession,
      'queued',
      'client-restart',
    )
    await flushState({ throwOnError: true })
    removeEventBus(bridgeId)
    resetStateForTest()
    await loadState()
    const entry = listOutbox(deviceId, testSession).find(
      row => row.id === 'client-restart',
    )
    expect(entry?.requiresReview).toBe(true)
    const retry = await deliverNow(
      bridgeId,
      deviceId,
      testSession,
      'queued',
      'client-restart',
    )
    expect(retry.state).toBe('unconfirmed')
    expect(getEventBus(bridgeId).getEventsSince(0)).toHaveLength(0)
    removeEventBus(bridgeId)
  })

  test('a retried web prompt reaches an attached terminal only once, even after other events', () => {
    const bridgeId = `retry-${crypto.randomUUID()}`
    const received: string[] = []
    const socket = {
      readyState: 1,
      send(value: string) {
        received.push(value)
      },
      close() {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(socket, bridgeId)
    try {
      const first = { uuid: 'prompt-1', content: 'first' }
      publishSessionEvent(bridgeId, 'user', first, 'outbound')
      publishSessionEvent(
        bridgeId,
        'user',
        { uuid: 'prompt-2', content: 'second' },
        'outbound',
      )
      publishSessionEvent(
        bridgeId,
        'assistant',
        { content: 'working' },
        'inbound',
      )
      publishSessionEvent(bridgeId, 'user', first, 'outbound')
      expect(received.map(value => JSON.parse(value).uuid)).toEqual([
        'prompt-1',
        'prompt-2',
      ])
    } finally {
      handleWebSocketClose(socket, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })

  test('a failed replay does not skip the failed message on the next connection', async () => {
    const bridgeId = 'session_1234567890abcdef'
    mapBridge(bridgeId)
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
      expect(listOutbox(deviceId, localSessionId)).toHaveLength(2)
      await flushState()
    } finally {
      handleWebSocketClose(working, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })

  test('failed bridge writes remain unconfirmed and replay in order', async () => {
    const bridgeId = 'session_1234567890abcdef'
    mapBridge(bridgeId)
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
      // Socket writes alone do not prove that the terminal received them.
      expect(listOutbox(deviceId, localSessionId)).toHaveLength(2)
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

  test('an unconfirmed prompt whose bus event was evicted is re-queued, not stranded', async () => {
    const bridgeId = 'session_feedfacecafebeef'
    mapBridge(bridgeId)
    const queued = queueMessage(deviceId, localSessionId, 'survive eviction')
    // No transport attached: the prompt was published and stays unconfirmed.
    await flushOutbox(deviceId, localSessionId, bridgeId)
    expect(
      listOutbox(deviceId, localSessionId).find(row => row.id === queued.id)
        ?.state,
    ).toBe('unconfirmed')
    // A long tool-heavy turn pushes the publish event out of the rolling bus
    // (5000 kept, trimmed to 2500), so the attach replay can no longer carry
    // it and the row would never be confirmed.
    const bus = getEventBus(bridgeId)
    for (let i = 0; i < 5001; i += 1) {
      bus.publish({
        id: `evt-${i}`,
        sessionId: bridgeId,
        type: 'assistant',
        payload: {},
        direction: 'inbound',
      })
    }
    const sent: string[] = []
    const ws = {
      readyState: 1,
      send: (data: string) => sent.push(data),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridgeId)
    try {
      markUnconfirmedSent(deviceId, localSessionId, bridgeId)
      // Re-queued under the same uuid, then flushed to the attached bridge.
      await flushOutbox(deviceId, localSessionId, bridgeId)
      expect(
        listOutbox(deviceId, localSessionId).some(row => row.id === queued.id),
      ).toBe(true)
      expect(sent.some(frame => frame.includes(queued.id))).toBe(true)
    } finally {
      handleWebSocketClose(ws, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })

  test('a bridge attach cannot confirm a message before it was published', async () => {
    const bridgeId = 'session_0123456789abcdef'
    mapBridge(bridgeId)
    enqueueOutbox(entry('preparing', 'queued'))
    const flush = flushOutbox(deviceId, localSessionId, bridgeId)
    markUnconfirmedSent(deviceId, localSessionId, bridgeId)
    expect(['queued', 'unconfirmed']).toContain(
      listOutbox(deviceId, localSessionId)[0]?.state,
    )
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
    resetConversationStoreForTest()
    const dir = await mkdtemp(join(tmpdir(), 'rcs-outbox-img-'))
    dirs.push(dir)
    process.env.RCS_DATA_DIR = dir
    resetStateForTest()
    await loadState()
  })
  afterAll(async () => {
    resetConversationStoreForTest()
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
    mapBridge(bridgeId)
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
    mapBridge(bridgeId)
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
      expect(delivered.state).toBe('unconfirmed')
      // The bridge WS frame is the SDK shape: message.content must still be
      // the block array, not a flattened string.
      const frame = sent
        .map(data => JSON.parse(data) as Record<string, unknown>)
        .find(message => message.type === 'user') as {
        message: { content: unknown }
      }
      expect(frame.message.content).toEqual(imageBlocks)
      expect(delivered.entry.state).toBe('unconfirmed')
      // A socket write alone cannot confirm that the terminal received it.
      expect(
        listOutbox(deviceId, localSessionId).some(
          entry => entry.id === delivered.entry.id,
        ),
      ).toBe(true)
    } finally {
      handleWebSocketClose(ws, bridgeId, 1000, '', token)
      removeEventBus(bridgeId)
    }
  })
})
