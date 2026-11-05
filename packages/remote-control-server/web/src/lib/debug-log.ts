/**
 * Client-side debug log for diagnosing the web end.
 *
 * Captures console.error, unhandled rejections, and tagged app events (SSE
 * failures, control responses, API errors) into a ring buffer. Hidden by
 * default; open with ?debug=1 (kept for the session) — a small log button
 * then appears in the header.
 */

export type DebugEntry = {
  at: number
  tag: string
  message: string
}

const MAX_ENTRIES = 300
const entries: DebugEntry[] = []
const listeners = new Set<() => void>()

let installed = false

export function isDebugEnabled(): boolean {
  if (typeof window === 'undefined') return false
  const params = new URLSearchParams(window.location.search)
  return (
    params.get('debug') === '1' || sessionStorage.getItem('cc_debug') === '1'
  )
}

export function enableDebug(): void {
  sessionStorage.setItem('cc_debug', '1')
  notify()
}

export function subscribeDebug(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

function notify(): void {
  for (const listener of listeners) listener()
}

export function debugLog(tag: string, message: string): void {
  entries.push({ at: Date.now(), tag, message })
  if (entries.length > MAX_ENTRIES)
    entries.splice(0, entries.length - MAX_ENTRIES)
  notify()
}

export function getDebugEntries(): DebugEntry[] {
  return entries
}

export function installDebugCapture(): void {
  if (installed || typeof window === 'undefined') return
  installed = true

  window.addEventListener('error', event => {
    debugLog('window', `${event.message} @ ${event.filename}:${event.lineno}`)
  })

  window.addEventListener('unhandledrejection', event => {
    const reason = event.reason
    debugLog(
      'promise',
      reason instanceof Error
        ? `${reason.message}\n${reason.stack ?? ''}`
        : String(reason),
    )
  })

  const originalError = console.error.bind(console)
  console.error = (...args: unknown[]) => {
    debugLog(
      'console',
      args
        .map(arg =>
          arg instanceof Error
            ? `${arg.message}\n${arg.stack ?? ''}`
            : String(arg),
        )
        .join(' '),
    )
    originalError(...args)
  }
}
