import { afterAll, beforeEach, describe, expect, test } from 'bun:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  enqueueOutbox,
  listOutbox,
  loadState,
  requeueFailedOutbox,
  resetStateForTest,
  type OutboxEntry,
} from '../device/state'
import { markQueuedFailed } from '../device/operations'

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
})
