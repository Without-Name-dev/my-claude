"use client"

import * as React from "react"

// The server coalesces streamed text and publishes it roughly every 300ms.
// Spread each new chunk over that window instead of increasing push frequency.
const STREAM_PUSH_INTERVAL_MS = 300
// A large backlog usually means the tab was backgrounded (RAF is paused) or
// the stream was reconnected. Replaying it would put the reply far behind.
const CATCH_UP_GRAPHEMES = 800
const MIN_GRAPHEMES_PER_SECOND = 24
// Keep the fastest typing legible. At 60fps this is at most 10 graphemes in a
// single frame, while still letting normal 300ms chunks finish on time.
const MAX_GRAPHEMES_PER_SECOND = 600
// A dropped frame should not turn into a burst of text in the next frame.
const MAX_FRAME_DELTA_MS = 50
const FALLBACK_FRAME_DELTA_MS = 1000 / 60

type GraphemeSegmenter = {
  segment(input: string): Iterable<{ segment: string }>
}

type GraphemeSegmenterConstructor = new (
  locales?: string | string[],
  options?: { granularity: "grapheme" },
) => GraphemeSegmenter

let cachedSegmenter: GraphemeSegmenter | null | undefined

function graphemeSegmenter(): GraphemeSegmenter | null {
  if (cachedSegmenter !== undefined) return cachedSegmenter
  const Segmenter = (Intl as typeof Intl & { Segmenter?: GraphemeSegmenterConstructor }).Segmenter
  cachedSegmenter = Segmenter ? new Segmenter(undefined, { granularity: "grapheme" }) : null
  return cachedSegmenter
}

/**
 * Split text into user-perceived characters. `slice` on UTF-16 indices can
 * split surrogate pairs and combining clusters (for example "👨‍👩‍👧‍👦" or
 * "é"), so always advance the typewriter by these segments instead.
 */
export function splitGraphemes(text: string): string[] {
  const segmenter = graphemeSegmenter()
  if (!segmenter) return Array.from(text)
  const graphemes: string[] = []
  for (const part of segmenter.segment(text)) graphemes.push(part.segment)
  return graphemes
}

function commonGraphemePrefixLength(left: readonly string[], right: readonly string[]): number {
  const limit = Math.min(left.length, right.length)
  let index = 0
  while (index < limit && left[index] === right[index]) index += 1
  return index
}

function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = React.useState(false)

  React.useEffect(() => {
    const query = window.matchMedia("(prefers-reduced-motion: reduce)")
    const update = () => setReduced(query.matches)
    update()
    query.addEventListener("change", update)
    return () => query.removeEventListener("change", update)
  }, [])

  return reduced
}

/**
 * Reveals `target` at a readable, adaptive pace while `streaming` is true.
 *
 * Progress is driven by requestAnimationFrame. Each frame consumes a
 * fractional number of grapheme clusters, so the output never splits a
 * character and a dropped frame does not produce an abrupt jump. The rate is
 * derived from the current backlog: enough to absorb one 300ms push, bounded
 * to stay legible. Backlogs beyond the catch-up limit snap to the target.
 */
export function useTypewriter(target: string, streaming: boolean): string {
  const reducedMotion = usePrefersReducedMotion()
  const targetGraphemes = React.useMemo(() => splitGraphemes(target), [target])
  const [visibleText, setVisibleText] = React.useState(() => streaming ? "" : target)

  const visibleTextRef = React.useRef(visibleText)
  const countRef = React.useRef(streaming ? 0 : targetGraphemes.length)
  const graphemesRef = React.useRef(targetGraphemes)
  const previousTargetRef = React.useRef(target)
  const targetRef = React.useRef(target)
  const streamingRef = React.useRef(streaming)
  const reducedMotionRef = React.useRef(reducedMotion)
  const animationFrameRef = React.useRef<number | null>(null)
  const lastFrameRef = React.useRef<number | null>(null)
  const fractionalGraphemesRef = React.useRef(0)

  streamingRef.current = streaming
  reducedMotionRef.current = reducedMotion
  targetRef.current = target

  const stopAnimation = React.useCallback(() => {
    if (animationFrameRef.current !== null) {
      window.cancelAnimationFrame(animationFrameRef.current)
      animationFrameRef.current = null
    }
    lastFrameRef.current = null
  }, [])

  const commitVisibleText = React.useCallback((next: string) => {
    if (visibleTextRef.current === next) return
    visibleTextRef.current = next
    setVisibleText(next)
  }, [])

  const commitTarget = React.useCallback(() => {
    const graphemes = graphemesRef.current
    countRef.current = graphemes.length
    fractionalGraphemesRef.current = 0
    lastFrameRef.current = null
    commitVisibleText(targetRef.current)
  }, [commitVisibleText])

  const tick = React.useCallback((now: number) => {
    animationFrameRef.current = null
    if (!streamingRef.current || reducedMotionRef.current) return

    const graphemes = graphemesRef.current
    let remaining = graphemes.length - countRef.current
    if (remaining <= 0) {
      lastFrameRef.current = null
      return
    }

    // The tab may have been hidden while the stream continued. Do not spend
    // seconds replaying a huge accumulated reply.
    if (remaining > CATCH_UP_GRAPHEMES) {
      commitTarget()
      return
    }

    const nowMs = Number.isFinite(now) ? now : performance.now()
    const deltaMs = lastFrameRef.current === null
      ? FALLBACK_FRAME_DELTA_MS
      : Math.min(Math.max(nowMs - lastFrameRef.current, 0), MAX_FRAME_DELTA_MS)
    lastFrameRef.current = nowMs

    const backlogRate = remaining * (1000 / STREAM_PUSH_INTERVAL_MS)
    const graphemesPerSecond = Math.min(
      MAX_GRAPHEMES_PER_SECOND,
      Math.max(MIN_GRAPHEMES_PER_SECOND, backlogRate),
    )
    fractionalGraphemesRef.current += graphemesPerSecond * deltaMs / 1000

    const step = Math.floor(fractionalGraphemesRef.current)
    if (step > 0) {
      fractionalGraphemesRef.current -= step
      const previousCount = countRef.current
      countRef.current = Math.min(graphemes.length, countRef.current + step)
      remaining = graphemes.length - countRef.current
      const revealed = graphemes.slice(previousCount, countRef.current).join("")
      commitVisibleText(
        remaining === 0 ? targetRef.current : visibleTextRef.current + revealed,
      )
    }

    if (countRef.current < graphemes.length) {
      animationFrameRef.current = window.requestAnimationFrame(tick)
    } else {
      lastFrameRef.current = null
    }
  }, [commitTarget, commitVisibleText])

  const scheduleAnimation = React.useCallback(() => {
    if (animationFrameRef.current !== null) return
    lastFrameRef.current = null
    animationFrameRef.current = window.requestAnimationFrame(tick)
  }, [tick])

  React.useLayoutEffect(() => {
    const previousTarget = previousTargetRef.current
    const previousGraphemes = graphemesRef.current
    const nextGraphemes = targetGraphemes
    const targetChanged = target !== previousTarget

    if (targetChanged) {
      if (!target.startsWith(previousTarget)) {
        // A revision can rewrite already-received text. Preserve only the
        // common grapheme prefix so we never display a mismatched tail.
        countRef.current = Math.min(
          countRef.current,
          commonGraphemePrefixLength(previousGraphemes, nextGraphemes),
        )
      }
      countRef.current = Math.min(countRef.current, nextGraphemes.length)
      graphemesRef.current = nextGraphemes
      previousTargetRef.current = target
      targetRef.current = target
    }

    if (!streaming || reducedMotion) {
      stopAnimation()
      commitTarget()
      return
    }

    const remaining = nextGraphemes.length - countRef.current
    if (remaining <= 0) {
      stopAnimation()
      commitTarget()
      return
    }

    if (remaining > CATCH_UP_GRAPHEMES) {
      stopAnimation()
      commitTarget()
      return
    }

    if (targetChanged) {
      // If a revision invalidated part of what is visible, clamp immediately
      // rather than waiting for the RAF loop to paint over the stale text.
      const visiblePrefix = countRef.current === 0
        ? ""
        : nextGraphemes.slice(0, countRef.current).join("")
      if (visiblePrefix !== visibleTextRef.current) {
        commitVisibleText(visiblePrefix)
      }
    }

    scheduleAnimation()
  }, [
    commitTarget,
    commitVisibleText,
    reducedMotion,
    scheduleAnimation,
    stopAnimation,
    streaming,
    target,
    targetGraphemes,
  ])

  React.useEffect(() => () => stopAnimation(), [stopAnimation])

  if (!streaming || reducedMotion) return target
  return visibleText
}
