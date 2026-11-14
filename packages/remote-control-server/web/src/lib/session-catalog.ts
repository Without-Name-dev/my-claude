import type { DeviceSessionSummary } from '../api/devices'

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
