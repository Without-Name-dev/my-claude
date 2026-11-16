import { expect, test } from 'bun:test'
import {
  findNewSession,
  newSessionWaitDetail,
  openNewSession,
} from '../control'
import { defaultConfig } from '../config'
import { openSession } from '../open'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

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

test('reports a slow startup without claiming an unregistered terminal is open', () => {
  expect(newSessionWaitDetail(19_999, false)).toBeUndefined()
  expect(newSessionWaitDetail(20_000, false)).toContain('尚未检测到终端连接')
  expect(newSessionWaitDetail(20_000, true)).toContain('终端已打开')
})

test('reports a missing terminal runtime immediately instead of waiting for bridge timeout', async () => {
  const config = {
    ...defaultConfig(),
    claudeConfigDir: process.cwd(),
    bunPath: join(tmpdir(), 'ccb-missing-runtime-for-test.exe'),
    allowedRoots: [process.cwd()],
  }
  const events: { state: string; failureCode?: string; detail?: string }[] = []
  await openNewSession(
    config,
    {
      operationId: 'test-operation',
      cwd: process.cwd(),
      configDir: process.cwd(),
    },
    event => events.push(event),
  )
  expect(events.at(-1)?.state).toBe('failed')
  expect(events.at(-1)?.failureCode).toBe('spawn_failed')
  expect(events.at(-1)?.detail).toContain('找不到 Bun 程序')
})

test('rejects invalid session ids before constructing a launcher', async () => {
  const config = {
    ...defaultConfig(),
    claudeConfigDir: process.cwd(),
    allowedRoots: [process.cwd()],
  }
  const events: { state: string; failureCode?: string }[] = []
  await openSession(
    config,
    {
      operationId: 'test',
      localSessionId: '../unexpected',
      cwd: process.cwd(),
      configDir: process.cwd(),
    },
    event => events.push(event),
  )
  expect(events.at(-1)?.failureCode).toBe('session_not_found')
})
