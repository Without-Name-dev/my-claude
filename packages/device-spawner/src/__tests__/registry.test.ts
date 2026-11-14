import { afterAll, describe, expect, test } from 'bun:test'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  isProcessAlive,
  readRegistry,
  samePath,
  verifySession,
} from '../registry'

const localSessionId = 'db094c06-6f2d-470d-aef1-dcce041193b7'
const otherSessionId = 'ee907512-a260-41d0-9b26-20ef20bcfed6'

async function makeConfigDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'spawner-registry-'))
  await mkdir(join(dir, 'sessions'), { recursive: true })
  return dir
}

const dirs: string[] = []

async function configDir(): Promise<string> {
  const dir = await makeConfigDir()
  dirs.push(dir)
  return dir
}

afterAll(async () => {
  for (const dir of dirs) await rm(dir, { recursive: true, force: true })
})

describe('session registry', () => {
  test('reads valid pid files and ignores malformed ones', async () => {
    const dir = await configDir()
    await writeFile(
      join(dir, 'sessions', `${process.pid}.json`),
      JSON.stringify({
        pid: process.pid,
        sessionId: localSessionId,
        cwd: 'C:\\work',
        startedAt: Date.now(),
        bridgeSessionId: 'session_abcdef0123456789',
      }),
    )
    await writeFile(join(dir, 'sessions', 'notes.json'), '{}')
    await writeFile(
      join(dir, 'sessions', '999999.json'),
      JSON.stringify({ pid: 999999, sessionId: 'not-a-uuid', cwd: '' }),
    )

    const entries = await readRegistry(dir)
    expect(entries).toHaveLength(1)
    expect(entries[0]?.sessionId).toBe(localSessionId)
    expect(entries[0]?.bridgeSessionId).toBe('session_abcdef0123456789')
  })

  test('verifySession reports a dead registry entry as not running', async () => {
    const dir = await configDir()
    await writeFile(
      join(dir, 'sessions', '999999.json'),
      JSON.stringify({
        pid: 999999,
        sessionId: otherSessionId,
        cwd: 'C:\\work',
        startedAt: Date.now() - 1000,
      }),
    )
    const result = await verifySession(dir, otherSessionId)
    expect(result.state.processAlive).toBe(false)
    expect(result.state.bridgeSessionId).toBeUndefined()
  })

  test('verifySession trusts a live pid when command-line checks are off', async () => {
    const dir = await configDir()
    await writeFile(
      join(dir, 'sessions', `${process.pid}.json`),
      JSON.stringify({
        pid: process.pid,
        sessionId: localSessionId,
        cwd: 'C:\\work',
        startedAt: Date.now(),
      }),
    )
    const result = await verifySession(dir, localSessionId, undefined, {
      verifyCommandLine: false,
    })
    expect(result.state.processAlive).toBe(true)
    expect(result.state.pid).toBe(process.pid)
    expect(result.state.source).toBe('registry')
  })

  test('accepts a CLI that registers long after process startup but rejects a reused PID', async () => {
    const dir = await configDir()
    const registeredAt = Date.now()
    await writeFile(
      join(dir, 'sessions', `${process.pid}.json`),
      JSON.stringify({
        pid: process.pid,
        sessionId: localSessionId,
        cwd: 'C:\\work',
        startedAt: registeredAt,
      }),
    )
    const processInfo = (startedAt: number) =>
      new Map([
        [
          process.pid,
          {
            name: 'bun.exe',
            commandLine: 'bun cli.js',
            startedAt,
          },
        ],
      ])
    const slowStartup = await verifySession(dir, localSessionId, undefined, {
      verifyCommandLine: false,
      processes: processInfo(registeredAt - 120_000),
    })
    expect(slowStartup.state.processAlive).toBe(true)
    const reusedPid = await verifySession(dir, localSessionId, undefined, {
      verifyCommandLine: false,
      processes: processInfo(registeredAt + 120_000),
    })
    expect(reusedPid.state.processAlive).toBe(false)
  })

  test('reuses the supplied registry snapshot during a state report', async () => {
    const dir = await configDir()
    const entries = [
      { pid: process.pid, sessionId: localSessionId, cwd: 'C:\\work' },
    ]
    const processes = new Map([
      [
        process.pid,
        { name: 'bun.exe', commandLine: 'bun cli.js', startedAt: 0 },
      ],
    ])
    const result = await verifySession(dir, localSessionId, undefined, {
      verifyCommandLine: false,
      entries,
      processes,
    })
    expect(result.state.processAlive).toBe(true)
    expect(result.state.pid).toBe(process.pid)
  })

  test('samePath is case-insensitive on Windows and trims separators', () => {
    if (process.platform === 'win32') {
      expect(samePath('C:\\Work\\', 'c:/work')).toBe(true)
      expect(samePath('C:\\work', 'C:\\work2')).toBe(false)
    } else {
      expect(samePath('/work/', '/work')).toBe(true)
      expect(samePath('/work', '/work2')).toBe(false)
    }
  })

  test('isProcessAlive rejects impossible pids', () => {
    expect(isProcessAlive(0)).toBe(false)
    expect(isProcessAlive(-1)).toBe(false)
    expect(isProcessAlive(999999)).toBe(false)
  })
})
