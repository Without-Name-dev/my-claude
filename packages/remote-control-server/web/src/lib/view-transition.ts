import { flushSync } from 'react-dom'

type DocumentWithTransition = Document & {
  startViewTransition?: (callback: () => void) => unknown
}

/**
 * Run a navigation state update inside a View Transition when the WebView
 * supports it, so list <-> chat behaves like a native push/pop. Falls back to
 * an instant update on older iOS and when Reduce Motion is on.
 */
export function withViewTransition(update: () => void): void {
  const doc = document as DocumentWithTransition
  const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
  if (typeof doc.startViewTransition === 'function' && !reduce) {
    doc.startViewTransition(() => flushSync(update))
  } else {
    update()
  }
}

export type HapticKind = 'light' | 'medium' | 'success' | 'warning' | 'error'

/**
 * Ask the native shell for a haptic. No-op in a regular browser.
 */
export function haptic(kind: HapticKind = 'light'): void {
  const bridge = (
    window as unknown as {
      webkit?: {
        messageHandlers?: { haptic?: { postMessage: (value: string) => void } }
      }
    }
  ).webkit
  bridge?.messageHandlers?.haptic?.postMessage(kind)
}
