/** Vite's built entry chunk: the one file whose hash changes on every deploy. */
const ASSET_ENTRY = /\/assets\/[^"']+\.js$/

function entryName(path: string): string {
  return path.split('/').pop() ?? ''
}

function runningEntry(): string | null {
  for (const script of Array.from(
    document.querySelectorAll<HTMLScriptElement>('script[src]'),
  )) {
    try {
      const path = new URL(script.src, window.location.href).pathname
      if (ASSET_ENTRY.test(path)) return path
    } catch {
      // Ignore a malformed src; a later script may still identify the build.
    }
  }
  return null
}

/**
 * True when the server is serving a newer build than the one this page loaded.
 * The iOS shell keeps the web view alive for days, so without this a deploy
 * (including a bug fix) would only land after the user kills the app.
 */
export async function isUpdateAvailable(): Promise<boolean> {
  // Vite dev serves /src/main.tsx, so this is automatically a no-op there.
  const mine = runningEntry()
  if (!mine) return false
  try {
    const res = await fetch('/web/version', { cache: 'no-store' })
    if (!res.ok) return false
    const body = (await res.json()) as { entry?: unknown }
    if (typeof body.entry !== 'string' || !ASSET_ENTRY.test(body.entry)) {
      return false
    }
    // Compare file names only: the two paths may be reported with different
    // bases, and the hash is what identifies the build.
    return entryName(body.entry) !== entryName(mine)
  } catch {
    // Offline or a proxy in the way: try again on the next tick.
    return false
  }
}
