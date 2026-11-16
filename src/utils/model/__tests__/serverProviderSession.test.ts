import { afterEach, expect, test } from 'bun:test'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  readSessionProvider,
  saveSessionProvider,
} from '../serverProviderSession.js'

const oldConfig = process.env.CLAUDE_CONFIG_DIR
const dirs: string[] = []
afterEach(() => {
  if (oldConfig === undefined) delete process.env.CLAUDE_CONFIG_DIR
  else process.env.CLAUDE_CONFIG_DIR = oldConfig
  for (const dir of dirs.splice(0))
    rmSync(dir, { recursive: true, force: true })
})

test('persists independent session selections without credentials and removes only the requested selection', () => {
  const dir = mkdtempSync(join(tmpdir(), 'ccb-provider-session-'))
  dirs.push(dir)
  process.env.CLAUDE_CONFIG_DIR = dir
  saveSessionProvider('session-a', { id: 'provider-a', model: 'model-a' })
  saveSessionProvider('session-b', { id: 'provider-b', model: 'model-b' })
  expect(readSessionProvider('session-a')).toEqual({
    id: 'provider-a',
    model: 'model-a',
  })
  expect(readSessionProvider('session-b')).toEqual({
    id: 'provider-b',
    model: 'model-b',
  })
  expect(
    JSON.parse(
      readFileSync(
        join(dir, 'server-provider-sessions', 'session-a.json'),
        'utf8',
      ),
    ),
  ).toEqual({ id: 'provider-a', model: 'model-a' })
  saveSessionProvider('session-a', null)
  expect(readSessionProvider('session-a')).toBeNull()
  expect(readSessionProvider('session-b')?.id).toBe('provider-b')
  expect(() => readSessionProvider('../outside')).toThrow()
})
