/**
 * 带离线缓存的 API 包装器
 */

import {
  putSession,
  getSession,
  putTimeline,
  getTimeline,
  setLastSyncTime,
} from "@/lib/offline-cache";

export class OfflineApiClient {
  async getSessionWithCache<T>(
    sessionId: string,
    fetchFn: () => Promise<T>,
  ): Promise<T> {
    const isOnline = typeof navigator !== "undefined" ? navigator.onLine : true;

    if (isOnline) {
      try {
        const data = await fetchFn();
        await putSession({
          id: sessionId,
          data,
          lastModified: new Date().toISOString(),
          syncedAt: new Date().toISOString(),
        });
        return data;
      } catch (error) {
        const cached = await getSession(sessionId);
        if (cached) {
          return cached.data as T;
        }
        throw error;
      }
    } else {
      const cached = await getSession(sessionId);
      if (cached) {
        return cached.data as T;
      }
      throw new Error("Offline: No cached data available");
    }
  }

  async getTimelineWithCache<T>(
    sessionId: string,
    fetchFn: () => Promise<T>,
  ): Promise<T> {
    const isOnline = typeof navigator !== "undefined" ? navigator.onLine : true;

    if (isOnline) {
      try {
        const data = await fetchFn();
        await putTimeline({
          sessionId,
          data,
          lastModified: new Date().toISOString(),
          syncedAt: new Date().toISOString(),
        });
        return data;
      } catch (error) {
        const cached = await getTimeline(sessionId);
        if (cached) {
          return cached.data as T;
        }
        throw error;
      }
    } else {
      const cached = await getTimeline(sessionId);
      if (cached) {
        return cached.data as T;
      }
      throw new Error("Offline: No cached timeline available");
    }
  }

  async syncAllSessions<T>(fetchFn: () => Promise<T>): Promise<T> {
    const data = await fetchFn();
    await setLastSyncTime(new Date().toISOString());
    return data;
  }
}
