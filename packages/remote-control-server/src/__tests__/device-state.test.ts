import { afterAll, beforeEach, describe, expect, test } from 'bun:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  authenticateDeviceToken,
  enqueueOutbox,
  flushState,
  getDevice,
  getMapping,
  getRuntime,
  hashSecret,
  listOutbox,
  loadState,
  resetStateForTest,
  setMapping,
  setRuntime,
  updateOutbox,
  upsertDevice,
  verifySecret,
  type DeviceRecord,
  type OutboxEntry,
} from '../device/state'

const localSessionId = 'db094c06-6f2d-470d-aef1-dcce041193b7'
let dataDir = ''

function makeDevice(secret: string): DeviceRecord {
  return {
    id: 'dev_test1',
    name: 'TEST-PC',
    platform: 'win32',
    configDir: 'C:\\Users\\test\\.claude',
    secretHash: hashSecret(secret),
    secretPrefix: secret.slice(0, 6),
    capabilities: ['open_session'],
    createdAt: Date.now(),
    lastSeenAt: null,
  }
}

beforeEach(async () => {
  if (!dataDir) {
    dataDir = await mkdtemp(join(tmpdir(), 'rcs-device-state-'))
    process.env.RCS_DATA_DIR = dataDir
  }
  resetStateForTest()
  await loadState()
})

afterAll(async () => {
  if (dataDir) await rm(dataDir, { recursive: true, force: true })
})

describe('device state', () => {
  test('enrolls a device and authenticates only its own token', () => {
    const secret = 'a'.repeat(43)
    upsertDevice(makeDevice(secret))

    expect(getDevice('dev_test1')?.name).toBe('TEST-PC')
    expect(authenticateDeviceToken(secret)?.id).toBe('dev_test1')
    expect(authenticateDeviceToken('b'.repeat(43))).toBeUndefined()
    expect(authenticateDeviceToken('')).toBeUndefined()
    expect(verifySecret(getDevice('dev_test1')!, secret)).toBe(true)
  })

  test('runtime evidence repairs the local→bridge mapping', () => {
    setRuntime('dev_test1', localSessionId, {
      localSessionId,
      cwd: 'C:\\work',
      processAlive: true,
      bridgeSessionId: 'session_abcdef0123456789',
      verifiedAt: Date.now(),
      source: 'registry',
    })
    expect(getMapping('dev_test1', localSessionId)?.bridgeSessionId).toBe(
      'session_abcdef0123456789',
    )
    expect(getMapping('dev_test1', localSessionId)?.cwd).toBe('C:\\work')
  })

  test('runtime evidence for a resumed session retargets the bridge mapping', () => {
    setMapping({
      deviceId: 'dev_test1',
      localSessionId,
      bridgeSessionId: 'session_abcdef0123456789',
      cwd: 'C:work',
      updatedAt: Date.now(),
    })
    // The CLI re-mints its registry sessionId on /resume while the bridge
    // keeps its server session. The report is the CLI's own claim about
    // which conversation the bridge now serves: the live row must FOLLOW
    // the resume, or the resumed session stays uncontrollable from the web.
    const resumedLocal = '11111111-2222-3333-4444-555555555555'
    setRuntime('dev_test1', resumedLocal, {
      localSessionId: resumedLocal,
      cwd: 'C:work',
      processAlive: true,
      bridgeSessionId: 'session_abcdef0123456789',
      verifiedAt: Date.now(),
      source: 'registry',
    })

    expect(getRuntime('dev_test1', resumedLocal)?.state.processAlive).toBe(true)
    // The pre-resume local id no longer claims the bridge.
    expect(getRuntime('dev_test1', localSessionId)).toBeUndefined()
    expect(getMapping('dev_test1', localSessionId)).toBeUndefined()
    expect(getMapping('dev_test1', resumedLocal)?.bridgeSessionId).toBe(
      'session_abcdef0123456789',
    )
  })

  test('a bridge id maps to exactly one local session', () => {
    const otherLocal = '99999999-8888-7777-6666-555555555555'
    setMapping({
      deviceId: 'dev_test1',
      localSessionId,
      bridgeSessionId: 'session_abcdef0123456789',
      cwd: 'C:work',
      updatedAt: Date.now() - 1000,
    })
    setMapping({
      deviceId: 'dev_test1',
      localSessionId: otherLocal,
      bridgeSessionId: 'session_abcdef0123456789',
      cwd: 'C:work',
      updatedAt: Date.now(),
    })

    expect(getMapping('dev_test1', localSessionId)).toBeUndefined()
    expect(getMapping('dev_test1', otherLocal)?.bridgeSessionId).toBe(
      'session_abcdef0123456789',
    )
  })

  test('outbox lists only unsent entries and updates them', () => {
    const entry: OutboxEntry = {
      id: 'msg1',
      deviceId: 'dev_test1',
      localSessionId,
      payload: { content: 'hi' },
      state: 'queued',
      createdAt: Date.now(),
      updatedAt: Date.now(),
      attempts: 0,
    }
    enqueueOutbox(entry)
    expect(listOutbox('dev_test1', localSessionId)).toHaveLength(1)

    updateOutbox('msg1', { state: 'sent' })
    expect(listOutbox('dev_test1', localSessionId)).toHaveLength(0)
  })

  test('keeps every pending message even when the outbox exceeds its history target', async () => {
    const now = Date.now()
    const pendingSessionId = '11111111-1111-4111-8111-111111111111'
    for (let i = 0; i < 5001; i++) {
      enqueueOutbox({
        id: `pending-${i}`,
        deviceId: 'dev_test1',
        localSessionId: pendingSessionId,
        payload: { content: 'pending' },
        state: 'queued',
        createdAt: now,
        updatedAt: now,
        attempts: 0,
      })
    }
    await flushState()
    resetStateForTest()
    await loadState()
    const pending = listOutbox('dev_test1', pendingSessionId)
    expect(pending).toHaveLength(5001)
    expect(pending[0]?.id).toBe('pending-0')
  })

  test('mappings survive a state reload', async () => {
    setMapping({
      deviceId: 'dev_test1',
      localSessionId,
      bridgeSessionId: 'session_abcdef0123456789',
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    await flushState()

    resetStateForTest()
    await loadState()

    expect(getMapping('dev_test1', localSessionId)?.bridgeSessionId).toBe(
      'session_abcdef0123456789',
    )
  })

  test('does not automatically resend a message whose delivery was uncertain at restart', async () => {
    enqueueOutbox({
      id: 'uncertain-message',
      deviceId: 'dev_test1',
      localSessionId,
      payload: { content: 'run once' },
      state: 'unconfirmed',
      bridgeSessionId: 'session_abcdef0123456789',
      createdAt: Date.now(),
      updatedAt: Date.now(),
      attempts: 1,
    })
    await flushState()
    resetStateForTest()
    await loadState()

    const [entry] = listOutbox('dev_test1', localSessionId)
    expect(entry?.state).toBe('unconfirmed')
    expect(entry?.requiresReview).toBe(true)
  })
})
