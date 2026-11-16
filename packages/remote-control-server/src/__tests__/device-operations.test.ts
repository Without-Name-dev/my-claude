import { afterAll, beforeEach, describe, expect, test } from 'bun:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  ensureOpenNew,
  getOperation,
  handleDeviceEvent,
  queueMessage,
} from '../device/operations'
import { listOutbox, loadState, resetStateForTest } from '../device/state'
import {
  handleWebSocketClose,
  handleWebSocketOpen,
} from '../transport/ws-handler'
import { removeEventBus } from '../transport/event-bus'
import { resetConversationStoreForTest } from '../device/conversation-store'

const deviceId = 'dev_test1'
let dataDir = ''

beforeEach(async () => {
  if (!dataDir) {
    dataDir = await mkdtemp(join(tmpdir(), 'rcs-ops-'))
    process.env.RCS_DATA_DIR = dataDir
  }
  resetStateForTest()
  await loadState()
})

afterAll(async () => {
  resetStateForTest()
  resetConversationStoreForTest()
  if (dataDir) await rm(dataDir, { recursive: true, force: true })
})

describe('open_new_session operation events', () => {
  test('pending-<operationId> failure events reach the operation record', async () => {
    // The device is offline, so ensureOpenNew already marked spawn_failed —
    // but a real spawner failure (e.g. config_dir_mismatch) fires before the
    // CLI mints a session id and must still land with its own failureCode.
    const record = await ensureOpenNew(deviceId, 'C:\\work', '')
    handleDeviceEvent(deviceId, {
      type: 'operation',
      operationId: record.id,
      localSessionId: `pending-${record.id}`,
      state: 'failed',
      failureCode: 'config_dir_mismatch',
      detail: 'config dir mismatch',
    })

    const updated = getOperation(record.id)
    expect(updated?.state).toBe('failed')
    expect(updated?.failureCode).toBe('config_dir_mismatch')
    expect(updated?.detail).toBe('config dir mismatch')
  })

  test('the pending placeholder is never adopted as resolvedLocalSessionId', async () => {
    const record = await ensureOpenNew(deviceId, 'C:\\work', '')
    handleDeviceEvent(deviceId, {
      type: 'operation',
      operationId: record.id,
      localSessionId: `pending-${record.id}`,
      state: 'starting',
    })

    expect(getOperation(record.id)?.resolvedLocalSessionId).toBeUndefined()
  })

  test('an operation event from another device cannot modify this device operation', async () => {
    const record = await ensureOpenNew(deviceId, 'C:\\work', '')
    const previousState = record.state
    handleDeviceEvent('dev_other', {
      type: 'operation',
      operationId: record.id,
      localSessionId: `pending-${record.id}`,
      state: 'starting',
      detail: 'wrong device',
    })
    expect(getOperation(record.id)?.state).toBe(previousState)
    expect(getOperation(record.id)?.detail).not.toBe('wrong device')
  })

  test('a ready event resolves the real session id', async () => {
    const record = await ensureOpenNew(deviceId, 'C:\\work', '')
    const realId = '11111111-2222-3333-4444-555555555555'
    handleDeviceEvent(deviceId, {
      type: 'operation',
      operationId: record.id,
      localSessionId: realId,
      state: 'ready',
      bridgeSessionId: 'session_abcdef0123456789',
    })

    expect(getOperation(record.id)?.resolvedLocalSessionId).toBe(realId)
  })

  test('does not report ready or publish queued messages until the bridge actually attaches', async () => {
    const record = await ensureOpenNew(deviceId, 'C:\\work', '')
    const realId = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee'
    const bridge = 'session_abcdef0123456789'
    const queued = queueMessage(deviceId, realId, 'fixture message')
    handleDeviceEvent(deviceId, {
      type: 'operation',
      operationId: record.id,
      localSessionId: realId,
      state: 'ready',
      bridgeSessionId: bridge,
    })
    expect(getOperation(record.id)?.state).toBe('waiting_for_user')
    expect(
      listOutbox(deviceId, realId).find(row => row.id === queued.id)?.state,
    ).toBe('queued')
    const sent: string[] = []
    const ws = {
      readyState: 1,
      send: (value: string) => sent.push(value),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      for (
        let i = 0;
        i < 100 && !sent.some(frame => frame.includes(queued.id));
        i++
      ) {
        await new Promise(resolve => setTimeout(resolve, 20))
      }
      expect(getOperation(record.id)?.state).toBe('ready')
      expect(sent.filter(frame => frame.includes(queued.id))).toHaveLength(1)
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })
})
