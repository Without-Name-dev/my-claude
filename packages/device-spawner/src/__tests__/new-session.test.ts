import { expect, test } from 'bun:test'
import { findNewSession } from '../control'

test('a new session never matches an unrelated terminal in the same directory', () => {
  const cwd = process.cwd()
  const now = Date.now()
  const unrelated = {
    pid: process.pid,
    cwd,
    startedAt: now,
    sessionId: 'existing',
  }
  const launched = {
    pid: process.pid,
    cwd,
    startedAt: now,
    sessionId: 'new-session',
  }
  expect(findNewSession([unrelated], 'new-session', cwd, now)).toBeUndefined()
  expect(
    findNewSession([unrelated, launched], 'new-session', cwd, now),
  ).toEqual(launched)
})
