import { afterAll, beforeEach, describe, expect, test } from 'bun:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  ensureOpenNew,
  getOperation,
  handleDeviceEvent,
} from '../device/operations'
import { loadState, resetStateForTest } from '../device/state'
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
})
