import type { DeviceSessionSummary } from '../api/devices'

export type RecentSession = DeviceSessionSummary & {
  deviceId: string
  deviceName: string
}

/** Compare activity across devices, not the order in which requests resolve. */
export function rankRecentSessions(sessions: RecentSession[]): RecentSession[] {
  return [...sessions].sort((a, b) => {
    const time =
      (Number.isFinite(b.updatedAt) ? b.updatedAt : 0) -
      (Number.isFinite(a.updatedAt) ? a.updatedAt : 0)
    return (
      time ||
      a.deviceId.localeCompare(b.deviceId) ||
      a.localSessionId.localeCompare(b.localSessionId)
    )
  })
}

/** Keep previously loaded older pages only when the refreshed page is partial. */
export function mergeSessionCatalog(
  previous: DeviceSessionSummary[],
  fresh: DeviceSessionSummary[],
  hasMore: boolean,
): DeviceSessionSummary[] {
  if (!hasMore) return fresh
  const freshIds = new Set(fresh.map(session => session.localSessionId))
  return [
    ...fresh,
    ...previous.filter(session => !freshIds.has(session.localSessionId)),
  ]
}
