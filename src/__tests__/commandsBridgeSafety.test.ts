import { describe, expect, test } from 'bun:test'

import { isBridgeSafeCommand } from '../commands.js'
import clear from '../commands/clear/index.js'
import plan from '../commands/plan/index.js'
import proactive from '../commands/proactive.js'

describe('isBridgeSafeCommand', () => {
  test('blocks local-jsx commands even when they opt into bridgeSafe', () => {
    // local-jsx renders Ink UI, which the remote client cannot display; the
    // opt-in flag alone is no longer enough (plan/proactive used to pass).
    expect(isBridgeSafeCommand(plan)).toBe(false)
    expect(isBridgeSafeCommand(proactive)).toBe(false)
  })

  test('continues allowing explicit local bridge-safe commands', () => {
    expect(isBridgeSafeCommand(clear)).toBe(true)
  })
})
