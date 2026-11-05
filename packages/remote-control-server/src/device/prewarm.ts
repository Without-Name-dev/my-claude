/**
 * Warm the server-side catalog as soon as a Spawner connects.
 *
 * Listing projects and sessions is a multi-second disk scan on the operator's
 * machine. Doing it once in the background means the web client gets an
 * instant first paint instead of waiting for the same scan on every open.
 */

import { log } from '../logger'
import { requestDevice, isDeviceOnline } from './hub'
import type { ProjectSummary, SessionSummary, TranscriptMessage } from './protocol'
import { saveTranscript } from './conversation-store'
import { getCatalog, setCatalog } from './state'

const warmedAt = new Map<string, number>()
const WARM_INTERVAL_MS = 30_000

/**
 * Cache the newest conversations while the machine is awake. The web can then
 * read them with the device asleep, and the first open is instant. Bounded to
 * the most recent sessions so this is a few hundred KB, not a full sync.
 */
async function cacheRecentTranscripts(
  deviceId: string,
  sessions: SessionSummary[],
): Promise<void> {
  const recent = [...sessions]
    .sort((a, b) => b.updatedAt - a.updatedAt)
    .slice(0, 12)
  for (const session of recent) {
    if (!isDeviceOnline(deviceId)) return
    try {
      const page = (await requestDevice(
        deviceId,
        'read_session',
        { localSessionId: session.localSessionId, limit: 100 },
        20_000,
      )) as { messages?: TranscriptMessage[] }
      if (Array.isArray(page.messages)) {
        saveTranscript(deviceId, session.localSessionId, page.messages, 'newest')
      }
    } catch {
      // A single unreadable transcript must not stop the prewarm.
    }
  }
}

export function warmCatalog(deviceId: string): void {
  const last = warmedAt.get(deviceId)
  if (last && Date.now() - last < WARM_INTERVAL_MS) return
  warmedAt.set(deviceId, Date.now())

  void (async () => {
    try {
      const result = (await requestDevice(
        deviceId,
        'list_projects',
        {},
        30_000,
      )) as { projects?: ProjectSummary[] }
      if (Array.isArray(result.projects)) {
        setCatalog(
          deviceId,
          result.projects,
          getCatalog(deviceId)?.sessions ?? [],
        )
      }
    } catch (error) {
      log(`[device] catalog prewarm (projects) failed: ${(error as Error).message}`)
      return
    }
    try {
      const result = (await requestDevice(
        deviceId,
        'list_sessions',
        { limit: 500 },
        60_000,
      )) as { sessions?: SessionSummary[] }
      if (Array.isArray(result.sessions)) {
        setCatalog(
          deviceId,
          getCatalog(deviceId)?.projects ?? [],
          result.sessions,
        )
        await cacheRecentTranscripts(deviceId, result.sessions)
      }
    } catch (error) {
      log(`[device] catalog prewarm (sessions) failed: ${(error as Error).message}`)
    }
  })()
}
