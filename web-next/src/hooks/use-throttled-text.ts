"use client"

import * as React from "react"

const DEFAULT_THROTTLE_MS = 50

/**
 * Commit a fast-moving value at a bounded rate, while always exposing the
 * latest value once `enabled` turns false.
 *
 * The typewriter advances on every animation frame so it stays smooth. The
 * Markdown renderer is much more expensive than a plain string, so it receives
 * this throttled value (20 commits per second by default) and is free to stay
 * memoized on frames where the text did not change.
 */
export function useThrottledText(
  value: string,
  enabled: boolean,
  intervalMs = DEFAULT_THROTTLE_MS,
): string {
  const [rendered, setRendered] = React.useState(value)
  const latestRef = React.useRef(value)
  const lastCommitRef = React.useRef(0)
  const timeoutRef = React.useRef<number | null>(null)
  latestRef.current = value

  const clearPendingCommit = React.useCallback(() => {
    if (timeoutRef.current === null) return
    window.clearTimeout(timeoutRef.current)
    timeoutRef.current = null
  }, [])

  React.useEffect(() => {
    if (!enabled) {
      clearPendingCommit()
      lastCommitRef.current = performance.now()
      setRendered(value)
      return
    }

    const now = performance.now()
    const remainingDelay = intervalMs - (now - lastCommitRef.current)
    if (remainingDelay <= 0) {
      clearPendingCommit()
      lastCommitRef.current = now
      setRendered(value)
      return
    }

    // A trailing commit is enough while text is still arriving. Keep the
    // latest value in a ref so a single timer always flushes the newest frame
    // instead of scheduling one timer per animation frame.
    if (timeoutRef.current !== null) return
    timeoutRef.current = window.setTimeout(() => {
      timeoutRef.current = null
      lastCommitRef.current = performance.now()
      setRendered(latestRef.current)
    }, remainingDelay)
  }, [clearPendingCommit, enabled, intervalMs, value])

  React.useEffect(() => clearPendingCommit, [clearPendingCommit])

  return enabled ? rendered : value
}
