/**
 * Durable state for the device channel.
 *
 * The RCS itself is deliberately in-memory for bridge sessions, but the
 * Spawner layer must survive an RCS restart: device enrollment, the
 * local-session → bridge-session mapping, the last-known catalog, and
 * messages that were queued while a terminal was starting.
 *
 * Storage is one JSON document written with temp-file + rename. The data is
 * small (a handful of devices, a few hundred sessions); SQLite would add
 * schema/migration weight without a benefit at this size.
 */

import { createHash, timingSafeEqual } from 'node:crypto'
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { log } from '../logger'
import type {
  ProjectSummary,
  SessionSummary,
  SessionRuntimeState,
} from './protocol'

const STATE_VERSION = 1

export type DeviceRecord = {
  id: string
  name: string
  platform: string
  configDir: string
  /** SHA-256 of the device token. The token itself is never stored. */
  secretHash: string
  secretPrefix: string
  capabilities: string[]
  createdAt: number
  lastSeenAt: number | null
}

export type SessionMapping = {
  deviceId: string
  localSessionId: string
  bridgeSessionId: string
  cwd: string
  title?: string
  updatedAt: number
}

export type CatalogCache = {
  deviceId: string
  projects: ProjectSummary[]
  sessions: SessionSummary[]
  updatedAt: number
}

export type OutboxEntry = {
  id: string
  deviceId: string
  localSessionId: string
  bridgeSessionId?: string
  /** The event body handed to the bridge transport. */
  payload: unknown
  state: 'queued' | 'sent' | 'unconfirmed' | 'failed'
  detail?: string
  createdAt: number
  updatedAt: number
  attempts: number
}

export type RuntimeCacheEntry = {
  state: SessionRuntimeState
  /** Server-side wall clock for the last device report. */
  reportedAt: number
}

type PersistedState = {
  version: number
  devices: Record<string, DeviceRecord>
  mappings: Record<string, SessionMapping>
  catalogs: Record<string, CatalogCache>
  outbox: OutboxEntry[]
}

let state: PersistedState = emptyState()
let loaded = false
let saveTimer: ReturnType<typeof setTimeout> | undefined
let saveChain: Promise<void> = Promise.resolve()

/** Runtime-only evidence; never persisted (it is only meaningful live). */
const runtimes = new Map<string, RuntimeCacheEntry>()

export function emptyState(): PersistedState {
  return {
    version: STATE_VERSION,
    devices: {},
    mappings: {},
    catalogs: {},
    outbox: [],
  }
}

export function dataDir(): string {
  const override = process.env.RCS_DATA_DIR
  if (override) return override
  return join(homedir(), '.ccremote', 'rcs-data')
}

function statePath(): string {
  return join(dataDir(), 'device-state.json')
}

export function runtimeKey(deviceId: string, localSessionId: string): string {
  return `${deviceId}\u0000${localSessionId}`
}

export function mappingKey(deviceId: string, localSessionId: string): string {
  return `${deviceId}\u0000${localSessionId}`
}

export async function loadState(): Promise<void> {
  if (loaded) return
  loaded = true
  try {
    const raw = await readFile(statePath(), 'utf8')
    const parsed = JSON.parse(raw) as Partial<PersistedState>
    if (parsed.version !== STATE_VERSION) {
      log(`[device] ignoring unknown state version ${String(parsed.version)}`)
      return
    }
    state = {
      version: STATE_VERSION,
      devices: parsed.devices ?? {},
      mappings: parsed.mappings ?? {},
      catalogs: parsed.catalogs ?? {},
      outbox: Array.isArray(parsed.outbox) ? parsed.outbox : [],
    }
    // The event bus is in-memory: an `unconfirmed` entry published before the
    // restart was never delivered (no bridge subscriber survived). Requeue it
    // so the next bridge attach sends it exactly once.
    for (const entry of state.outbox) {
      if (entry.state === 'unconfirmed') {
        entry.state = 'queued'
        entry.detail = '服务重启，等待终端重新连接后发送'
      }
    }
    trimOutbox()
    log(
      `[device] loaded ${Object.keys(state.devices).length} device(s), ` +
        `${Object.keys(state.mappings).length} mapping(s), ${state.outbox.length} queued message(s)`,
    )
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') {
      log(`[device] state load failed: ${(error as Error).message}`)
    }
  }
}

export function getState(): PersistedState {
  return state
}

export function getDevice(id: string): DeviceRecord | undefined {
  return state.devices[id]
}

export function listDevices(): DeviceRecord[] {
  return Object.values(state.devices).sort((a, b) =>
    a.name.localeCompare(b.name),
  )
}

export function upsertDevice(record: DeviceRecord): void {
  state.devices[record.id] = record
  scheduleSave()
}

const lastSeenPersist = new Map<string, number>()
const LAST_SEEN_PERSIST_MS = 5 * 60 * 1000

export function touchDevice(id: string): void {
  const device = state.devices[id]
  if (!device) return
  device.lastSeenAt = Date.now()
  // Heartbeats arrive every 15s; rewriting the whole state document for a
  // cosmetic timestamp is wasteful. Persist at most every five minutes (and
  // opportunistically whenever any other change flushes the document).
  const now = Date.now()
  const last = lastSeenPersist.get(id) ?? 0
  if (now - last < LAST_SEEN_PERSIST_MS) return
  lastSeenPersist.set(id, now)
  scheduleSave()
}

export function hashSecret(secret: string): string {
  return createHash('sha256').update(secret).digest('hex')
}

export function verifySecret(record: DeviceRecord, secret: string): boolean {
  const expected = Buffer.from(record.secretHash, 'hex')
  const actual = Buffer.from(hashSecret(secret), 'hex')
  return expected.length === actual.length && timingSafeEqual(expected, actual)
}

/** Resolve a WebSocket bearer token to an enrolled device. */
export function authenticateDeviceToken(
  token: string,
): DeviceRecord | undefined {
  if (!token) return undefined
  const digest = hashSecret(token)
  for (const device of Object.values(state.devices)) {
    const expected = Buffer.from(device.secretHash, 'hex')
    const actual = Buffer.from(digest, 'hex')
    if (
      expected.length === actual.length &&
      timingSafeEqual(expected, actual)
    ) {
      return device
    }
  }
  return undefined
}

export function setMapping(mapping: SessionMapping): void {
  const key = mappingKey(mapping.deviceId, mapping.localSessionId)
  // A bridge session belongs to exactly one local session. A stale mapping
  // left under another key would make ingestBridgeMessage persist messages
  // into the wrong conversation, so remove it (newest evidence wins).
  for (const [existingKey, existing] of Object.entries(state.mappings)) {
    if (
      existingKey !== key &&
      existing.deviceId === mapping.deviceId &&
      existing.bridgeSessionId === mapping.bridgeSessionId
    ) {
      if (existing.updatedAt > mapping.updatedAt) return
      delete state.mappings[existingKey]
    }
  }
  state.mappings[key] = mapping
  scheduleSave()
}

export function clearMapping(deviceId: string, localSessionId: string): void {
  delete state.mappings[mappingKey(deviceId, localSessionId)]
  scheduleSave()
}

export function getMapping(
  deviceId: string,
  localSessionId: string,
): SessionMapping | undefined {
  return state.mappings[mappingKey(deviceId, localSessionId)]
}

export function findMappingByBridgeId(
  bridgeSessionId: string,
): SessionMapping | undefined {
  return Object.values(state.mappings).find(
    mapping => mapping.bridgeSessionId === bridgeSessionId,
  )
}

export function setCatalog(
  deviceId: string,
  projects: ProjectSummary[],
  sessions: SessionSummary[],
): void {
  state.catalogs[deviceId] = {
    deviceId,
    projects,
    sessions,
    updatedAt: Date.now(),
  }
  scheduleSave()
}

export function getCatalog(deviceId: string): CatalogCache | undefined {
  return state.catalogs[deviceId]
}

export function upsertCatalogSession(
  deviceId: string,
  session: SessionSummary,
): void {
  const catalog = state.catalogs[deviceId]
  if (!catalog) return
  const index = catalog.sessions.findIndex(
    item => item.localSessionId === session.localSessionId,
  )
  if (index >= 0) catalog.sessions[index] = session
  else catalog.sessions.push(session)
  catalog.updatedAt = Date.now()
  scheduleSave()
}

export function enqueueOutbox(entry: OutboxEntry): void {
  state.outbox.push(entry)
  trimOutbox()
  scheduleSave()
}

/**
 * Local sessions with messages waiting to be delivered. Used when a Spawner
 * reconnects: the queued prompt is only useful if the terminal is opened for
 * it, which may have been impossible while the machine was offline.
 */
export function listQueuedSessionIds(deviceId: string): string[] {
  const ids = new Set<string>()
  for (const entry of state.outbox) {
    if (entry.deviceId === deviceId && entry.state === 'queued') {
      ids.add(entry.localSessionId)
    }
  }
  return [...ids]
}

export function listOutbox(
  deviceId: string,
  localSessionId: string,
): OutboxEntry[] {
  return state.outbox.filter(
    entry =>
      entry.deviceId === deviceId &&
      entry.localSessionId === localSessionId &&
      (entry.state === 'queued' || entry.state === 'unconfirmed'),
  )
}

/** Turn previously failed (never-published) rows back into queued work. */
export function requeueFailedOutbox(
  deviceId: string,
  localSessionId: string,
): void {
  let changed = false
  for (const entry of state.outbox) {
    if (
      entry.deviceId === deviceId &&
      entry.localSessionId === localSessionId &&
      entry.state === 'failed'
    ) {
      entry.state = 'queued'
      entry.detail = undefined
      entry.updatedAt = Date.now()
      changed = true
    }
  }
  if (changed) scheduleSave()
}

export function updateOutbox(
  id: string,
  patch: Partial<Pick<OutboxEntry, 'state' | 'detail' | 'bridgeSessionId'>>,
): void {
  const entry = state.outbox.find(item => item.id === id)
  if (!entry) return
  Object.assign(entry, patch, { updatedAt: Date.now() })
  scheduleSave()
}

function trimOutbox(): void {
  const cutoff = Date.now() - 7 * 24 * 60 * 60 * 1000
  state.outbox = state.outbox.filter(
    entry =>
      entry.state === 'queued' ||
      entry.state === 'unconfirmed' ||
      entry.updatedAt >= cutoff,
  )
  if (state.outbox.length > 5000) {
    // Never trade an undelivered message for an old delivered one.
    const pending = state.outbox.filter(
      entry => entry.state === 'queued' || entry.state === 'unconfirmed',
    )
    const keep = new Set(pending.slice(-5000).map(entry => entry.id))
    for (const entry of [...state.outbox].reverse()) {
      if (keep.size >= 5000) break
      keep.add(entry.id)
    }
    state.outbox = state.outbox.filter(entry => keep.has(entry.id))
  }
}

// ---------- Runtime evidence (memory only) ----------

export function setRuntime(
  deviceId: string,
  localSessionId: string,
  reported: SessionRuntimeState,
): void {
  // After `--resume`, the CLI can report a different local session id for the
  // same bridge session. The conversation (and the web route) is keyed by the
  // original local session, so follow the bridge mapping instead of creating
  // a second runtime entry that the UI never asks for.
  let canonicalLocal = localSessionId
  if (reported.bridgeSessionId) {
    const byBridge = findMappingByBridgeId(reported.bridgeSessionId)
    if (byBridge && byBridge.deviceId === deviceId) {
      canonicalLocal = byBridge.localSessionId
    }
  }
  const state =
    canonicalLocal === localSessionId
      ? reported
      : { ...reported, localSessionId: canonicalLocal }

  runtimes.set(runtimeKey(deviceId, canonicalLocal), {
    state,
    reportedAt: Date.now(),
  })
  // Keep the bridge mapping in sync with what the device actually observes.
  // Only write when it changed: heartbeats arrive every 15s and must not
  // rewrite the state file each time.
  if (reported.bridgeSessionId) {
    const existing = getMapping(deviceId, canonicalLocal)
    if (
      !existing ||
      existing.bridgeSessionId !== reported.bridgeSessionId ||
      (reported.cwd && existing.cwd !== reported.cwd)
    ) {
      setMapping({
        deviceId,
        localSessionId: canonicalLocal,
        bridgeSessionId: reported.bridgeSessionId,
        cwd: reported.cwd || existing?.cwd || '',
        updatedAt: Date.now(),
      })
    }
  }
}

export function getRuntime(
  deviceId: string,
  localSessionId: string,
): RuntimeCacheEntry | undefined {
  return runtimes.get(runtimeKey(deviceId, localSessionId))
}

export function clearRuntimesForDevice(deviceId: string): void {
  for (const key of [...runtimes.keys()]) {
    if (key.startsWith(`${deviceId}\u0000`)) runtimes.delete(key)
  }
}

// ---------- Persistence ----------

export function scheduleSave(): void {
  if (saveTimer) return
  saveTimer = setTimeout(() => {
    saveTimer = undefined
    void flushState()
  }, 250)
}

export async function flushState(): Promise<void> {
  // A rejected write must not poison the chain: without the catch, every
  // later save would be skipped and the state would silently stop persisting.
  saveChain = saveChain
    .catch(() => {})
    .then(async () => {
      const dir = dataDir()
      await mkdir(dir, { recursive: true })
      const tmp = join(dir, `.device-state-${process.pid}.tmp`)
      await writeFile(tmp, JSON.stringify(state, null, 1), 'utf8')
      await rename(tmp, statePath())
    })
  try {
    await saveChain
  } catch (error) {
    log(`[device] state save failed: ${(error as Error).message}`)
  }
}

/** Test seam: reset in-memory state without touching disk. */
export function resetStateForTest(): void {
  state = emptyState()
  runtimes.clear()
  loaded = false
}
