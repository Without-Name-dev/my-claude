/**
 * 离线缓存层 - 使用 IndexedDB 缓存会话数据
 */

export type CachedSession = {
  id: string;
  data: unknown;
  lastModified: string;
  syncedAt: string;
};

export type CachedTimeline = {
  sessionId: string;
  data: unknown;
  lastModified: string;
  syncedAt: string;
};

const DB_NAME = "aa-offline-cache";
const SESSION_STORE = "sessions";
const TIMELINE_STORE = "timelines";
const METADATA_STORE = "metadata";
const DB_VERSION = 1;

let dbPromise: Promise<IDBDatabase> | null = null;

function openDb(): Promise<IDBDatabase> {
  if (dbPromise) return dbPromise;
  dbPromise = new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = () => {
      const db = req.result;

      if (!db.objectStoreNames.contains(SESSION_STORE)) {
        const store = db.createObjectStore(SESSION_STORE, { keyPath: "id" });
        store.createIndex("lastModified", "lastModified", { unique: false });
        store.createIndex("syncedAt", "syncedAt", { unique: false });
      }

      if (!db.objectStoreNames.contains(TIMELINE_STORE)) {
        const store = db.createObjectStore(TIMELINE_STORE, { keyPath: "sessionId" });
        store.createIndex("lastModified", "lastModified", { unique: false });
      }

      if (!db.objectStoreNames.contains(METADATA_STORE)) {
        db.createObjectStore(METADATA_STORE, { keyPath: "key" });
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error("indexedDB open failed"));
  });
  return dbPromise;
}

// Session cache operations
export async function putSession(session: CachedSession): Promise<void> {
  const db = await openDb();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(SESSION_STORE, "readwrite");
    tx.objectStore(SESSION_STORE).put(session);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("put session failed"));
  });
}

export async function getSession(id: string): Promise<CachedSession | null> {
  const db = await openDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(SESSION_STORE, "readonly");
    const req = tx.objectStore(SESSION_STORE).get(id);
    req.onsuccess = () => resolve(req.result ?? null);
    req.onerror = () => reject(req.error ?? new Error("get session failed"));
  });
}

export async function getAllSessions(limit?: number): Promise<CachedSession[]> {
  const db = await openDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(SESSION_STORE, "readonly");
    const store = tx.objectStore(SESSION_STORE);
    const index = store.index("lastModified");
    const req = limit ? index.openCursor(null, "prev") : index.getAll();

    if (!limit) {
      (req as IDBRequest<CachedSession[]>).onsuccess = () => {
        resolve((req as IDBRequest<CachedSession[]>).result ?? []);
      };
    } else {
      const results: CachedSession[] = [];
      (req as IDBRequest<IDBCursorWithValue | null>).onsuccess = () => {
        const cursor = (req as IDBRequest<IDBCursorWithValue | null>).result;
        if (cursor && results.length < limit) {
          results.push(cursor.value);
          cursor.continue();
        } else {
          resolve(results);
        }
      };
    }
    req.onerror = () => reject(req.error ?? new Error("get all sessions failed"));
  });
}

export async function deleteSession(id: string): Promise<void> {
  const db = await openDb();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(SESSION_STORE, "readwrite");
    tx.objectStore(SESSION_STORE).delete(id);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("delete session failed"));
  });
}

// Timeline cache operations
export async function putTimeline(timeline: CachedTimeline): Promise<void> {
  const db = await openDb();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(TIMELINE_STORE, "readwrite");
    tx.objectStore(TIMELINE_STORE).put(timeline);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("put timeline failed"));
  });
}

export async function getTimeline(sessionId: string): Promise<CachedTimeline | null> {
  const db = await openDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(TIMELINE_STORE, "readonly");
    const req = tx.objectStore(TIMELINE_STORE).get(sessionId);
    req.onsuccess = () => resolve(req.result ?? null);
    req.onerror = () => reject(req.error ?? new Error("get timeline failed"));
  });
}

export async function deleteTimeline(sessionId: string): Promise<void> {
  const db = await openDb();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(TIMELINE_STORE, "readwrite");
    tx.objectStore(TIMELINE_STORE).delete(sessionId);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("delete timeline failed"));
  });
}

// Metadata operations
export async function setMetadata(key: string, value: unknown): Promise<void> {
  const db = await openDb();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(METADATA_STORE, "readwrite");
    tx.objectStore(METADATA_STORE).put({ key, value, updatedAt: new Date().toISOString() });
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("set metadata failed"));
  });
}

export async function getMetadata<T = unknown>(key: string): Promise<T | null> {
  const db = await openDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(METADATA_STORE, "readonly");
    const req = tx.objectStore(METADATA_STORE).get(key);
    req.onsuccess = () => resolve(req.result?.value ?? null);
    req.onerror = () => reject(req.error ?? new Error("get metadata failed"));
  });
}

// Sync status
export async function getLastSyncTime(): Promise<string | null> {
  return getMetadata<string>("lastSyncTime");
}

export async function setLastSyncTime(time: string): Promise<void> {
  return setMetadata("lastSyncTime", time);
}

// Clear all cached data
export async function clearAllCache(): Promise<void> {
  const db = await openDb();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction([SESSION_STORE, TIMELINE_STORE, METADATA_STORE], "readwrite");
    tx.objectStore(SESSION_STORE).clear();
    tx.objectStore(TIMELINE_STORE).clear();
    tx.objectStore(METADATA_STORE).clear();
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("clear cache failed"));
  });
}
