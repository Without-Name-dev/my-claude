import { expect, test } from 'bun:test'
import { QueryGuard } from '../QueryGuard'

test('a canceled turn may finish bridge state only before a successor begins', () => {
  const guard = new QueryGuard()
  const first = guard.tryStart()!
  guard.forceEnd()
  expect(guard.end(first)).toBe(false)
  expect(guard.wasForceEnded(first)).toBe(true)
  expect(guard.reserve()).toBe(true)
  expect(guard.wasForceEnded(first)).toBe(false)
  const next = guard.tryStart()!
  expect(guard.wasForceEnded(first)).toBe(false)
  expect(guard.end(next)).toBe(true)
  expect(guard.wasForceEnded(first)).toBe(false)
})
