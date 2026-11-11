import { haptic } from './view-transition'

const BASE_TITLE = 'Claude Remote'
/** Turns shorter than this are over before you could look away. */
const QUIET_UNDER_MS = 3000

let noticePending = false

/**
 * A long turn often ends while the phone is in your hand but your attention is
 * elsewhere. Fire a short success haptic, and mark the title when the page is
 * hidden so coming back to the app shows that something finished.
 *
 * Best-effort by nature: iOS suspends the web view shortly after backgrounding,
 * so a turn that completes while the app is away may not run this at all. A
 * guaranteed background alert needs server push (APNs), not a timer in the page.
 */
export function turnFinished(durationMs: number): void {
  if (durationMs < QUIET_UNDER_MS) return
  if (document.hidden) {
    noticePending = true
    document.title = `● 回复完成 · ${BASE_TITLE}`
    return
  }
  haptic('success')
}

/** Clear the unread marker once the user is actually looking at the page. */
export function clearTurnNotice(): void {
  if (!noticePending) return
  noticePending = false
  document.title = BASE_TITLE
}
