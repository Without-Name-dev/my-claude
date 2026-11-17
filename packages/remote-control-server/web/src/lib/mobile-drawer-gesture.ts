/** Resolve a drag without mistaking a vertical list scroll for navigation. */
export function drawerGestureDirection(
  dx: number,
  dy: number,
  opening: boolean,
): 'wait' | 'drag' | 'ignore' {
  if (Math.max(Math.abs(dx), Math.abs(dy)) < 8) return 'wait'
  if (Math.abs(dx) < Math.abs(dy) * 1.25 || (opening ? dx <= 0 : dx >= 0))
    return 'ignore'
  return 'drag'
}

/** A short flick or a deliberate drag past a quarter of the drawer commits. */
export function shouldCommitDrawerGesture(
  distance: number,
  width: number,
  elapsedMs: number,
): boolean {
  if (width <= 0 || distance <= 0) return false
  return (
    distance >= width * 0.25 ||
    (distance >= 30 && distance / Math.max(elapsedMs, 1) >= 0.45)
  )
}
