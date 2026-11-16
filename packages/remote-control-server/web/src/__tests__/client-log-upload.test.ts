import { describe, test, expect, beforeEach, afterEach } from 'bun:test'

// In-memory localStorage mock. Installed per-test and restored afterwards —
// a module-level globalThis assignment here would clobber the fetch/localStorage
// mocks of whichever test file Bun happens to run next in this process.
let store: Record<string, string> = {}
const calls: Array<{ url: string; opts: RequestInit }> = []
const respond = { ok: true, status: 200 }
let savedGlobals: {
  fetch: unknown
  localStorage: unknown
  navigator: unknown
} | null = null

function mockFetch(url: string, opts: RequestInit): Promise<Response> {
  calls.push({ url, opts })
  let body: unknown = {}
  if (url.includes('/client-logs/config')) {
    body = respond.status === 403 ? { enabled: false } : { enabled: true }
  }
  return Promise.resolve({
    ok: respond.ok,
    status: respond.status,
    json: async () => body,
  } as Response)
}

beforeEach(() => {
  savedGlobals = {
    fetch: (globalThis as any).fetch,
    localStorage: (globalThis as any).localStorage,
    navigator: (globalThis as any).navigator,
  }
  store = {}
  calls.length = 0
  respond.ok = true
  respond.status = 200
  ;(globalThis as any).localStorage = {
    getItem: (k: string) => store[k] ?? null,
    setItem: (k: string, v: string) => {
      store[k] = v
    },
    removeItem: (k: string) => {
      delete store[k]
    },
    clear: () => {
      store = {}
    },
    get length() {
      return Object.keys(store).length
    },
    key: () => null,
  }
  ;(globalThis as any).fetch = mockFetch
})

afterEach(() => {
  if (!savedGlobals) return
  ;(globalThis as any).fetch = savedGlobals.fetch
  ;(globalThis as any).localStorage = savedGlobals.localStorage
  ;(globalThis as any).navigator = savedGlobals.navigator
})

const { debugLog } = await import('../lib/debug-log')
const uploader = await import('../lib/client-log-upload')

/**
 * Enabling uploads fires a background flush (which may ship entries other
 * test files left in the shared ring buffer). Let it settle and clear the
 * call log so each test counts only its own POSTs.
 */
async function enableUpload() {
  uploader.setUploadOverride(null)
  await new Promise(resolve => setTimeout(resolve, 0))
  calls.length = 0
}

describe('client-log-upload', () => {
  test('buildUploadBody caps batches at 200 entries and carries the uuid', () => {
    store['rcs_uuid'] = 'u-test'
    const entries = Array.from({ length: 250 }, (_, i) => ({
      seq: i + 1,
      at: 1,
      tag: 't',
      message: `m${i}`,
    }))
    const body = JSON.parse(uploader.buildUploadBody(entries))
    expect(body.uuid).toBe('u-test')
    expect(body.entries).toHaveLength(200)
    expect(body.entries[0].seq).toBe(1)
    expect(body.entries[199].seq).toBe(200)
  })

  test('flush posts pending entries to /web/client-logs and advances the cursor', async () => {
    await enableUpload()
    debugLog('api', 'flush-marker-1')
    debugLog('sse', 'flush-marker-2')
    await uploader.flushClientLogs('test')
    expect(calls.length).toBe(1)
    expect(calls[0].url).toContain('/web/client-logs?uuid=')
    expect(calls[0].opts.method).toBe('POST')
    const sent = JSON.parse(calls[0].opts.body as string)
    expect(
      sent.entries.some(
        (e: { message: string }) => e.message === 'flush-marker-1',
      ),
    ).toBe(true)
    expect(
      sent.entries.some(
        (e: { message: string }) => e.message === 'flush-marker-2',
      ),
    ).toBe(true)
    // Nothing new since the flush → no second POST.
    await uploader.flushClientLogs('test')
    expect(calls.length).toBe(1)
  })

  test('a local override of off blocks uploads entirely', async () => {
    uploader.setUploadOverride(false)
    debugLog('api', 'blocked-marker')
    await uploader.flushClientLogs('test')
    expect(calls.length).toBe(0)
    expect(uploader.getUploadState().enabled).toBe(false)
    uploader.setUploadOverride(null)
  })

  test('a 403 from the server disables uploading until the config allows it', async () => {
    await enableUpload()
    respond.ok = false
    respond.status = 403
    debugLog('api', 'forbidden-marker')
    await uploader.flushClientLogs('test')
    expect(calls.length).toBe(1)
    expect(uploader.getUploadState().serverEnabled).toBe(false)
    expect(uploader.getUploadState().enabled).toBe(false)
    // Further flushes are skipped without touching the network.
    debugLog('api', 'after-forbidden')
    await uploader.flushClientLogs('test')
    expect(calls.length).toBe(1)
    // The server re-enabling via config restores uploads.
    respond.ok = true
    respond.status = 200
    await uploader.refreshUploadConfig()
    expect(uploader.getUploadState().serverEnabled).toBe(true)
    expect(uploader.getUploadState().enabled).toBe(true)
  })

  test('sendBeacon ships pending entries and advances the cursor on success', async () => {
    const beaconCalls: Array<{ url: string; blob: unknown }> = []
    ;(globalThis as any).navigator = {
      sendBeacon: (url: string, blob: unknown) => {
        beaconCalls.push({ url, blob })
        return true
      },
    }
    await enableUpload()
    debugLog('lifecycle', 'beacon-marker')
    const before = uploader.getUploadState().lastShippedSeq
    uploader.flushClientLogsBeacon()
    expect(beaconCalls.length).toBe(1)
    expect(beaconCalls[0].url).toContain('/web/client-logs?uuid=')
    expect(uploader.getUploadState().lastShippedSeq).toBeGreaterThan(before)
    // Already shipped → a beacon immediately after is a no-op.
    uploader.flushClientLogsBeacon()
    expect(beaconCalls.length).toBe(1)
  })
})
