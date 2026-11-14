import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import {
  mkdir,
  mkdtemp,
  realpath,
  rm,
  unlink,
  writeFile,
} from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { sanitizePath } from '../../../../src/utils/sessionStoragePortable'
import { listSessionsImpl } from '../../../../src/utils/listSessionsImpl'
import {
  listProjects,
  listSessions,
  readSession,
  selectFreshTerminalSessions,
} from '../sessions'
import type { BunProcess, RegistryEntry } from '../registry'

const localSessionId = 'db094c06-6f2d-470d-aef1-dcce041193b7'
let root = ''
let cwd = ''
let previousConfigDir: string | undefined

function line(record: Record<string, unknown>): string {
  return JSON.stringify(record)
}

beforeAll(async () => {
  root = await mkdtemp(join(tmpdir(), 'spawner-sessions-'))
  cwd = join(root, 'work')
  await mkdir(cwd, { recursive: true })
  cwd = await realpath(cwd)
  const projectDir = join(root, 'projects', sanitizePath(cwd))
  await mkdir(projectDir, { recursive: true })

  const transcript = [
    line({
      type: 'user',
      uuid: 'u1',
      cwd,
      sessionId: localSessionId,
      timestamp: '2026-11-04T01:00:00.000Z',
      message: { role: 'user', content: 'hello' },
    }),
    line({
      type: 'assistant',
      uuid: 'a1',
      cwd,
      sessionId: localSessionId,
      timestamp: '2026-11-04T01:00:01.000Z',
      message: {
        role: 'assistant',
        content: [
          { type: 'text', text: 'hi' },
          { type: 'tool_use', id: 't1', name: 'Read', input: { path: 'x' } },
        ],
      },
    }),
    line({
      type: 'assistant',
      uuid: 'a2',
      cwd,
      sessionId: localSessionId,
      message: {
        role: 'assistant',
        content: [{ type: 'thinking', thinking: 'hmm' }],
      },
    }),
    line({
      type: 'user',
      uuid: 'u2',
      cwd,
      sessionId: localSessionId,
      message: {
        role: 'user',
        content: [{ type: 'tool_result', tool_use_id: 't1', content: 'ok' }],
      },
    }),
    line({
      type: 'user',
      uuid: 'u3',
      cwd,
      sessionId: localSessionId,
      message: { role: 'user', content: 'second' },
    }),
    line({
      type: 'assistant',
      uuid: 'a3',
      cwd,
      sessionId: localSessionId,
      message: {
        role: 'assistant',
        content: [{ type: 'text', text: 'reply2' }],
      },
    }),
    // Sidechain/meta entries must never surface in the main conversation.
    line({
      type: 'assistant',
      uuid: 'side1',
      isSidechain: true,
      message: {
        role: 'assistant',
        content: [{ type: 'text', text: 'hidden' }],
      },
    }),
  ].join('\n')
  await writeFile(
    join(projectDir, `${localSessionId}.jsonl`),
    `${transcript}\n`,
  )

  previousConfigDir = process.env.CLAUDE_CONFIG_DIR
  process.env.CLAUDE_CONFIG_DIR = root
})

afterAll(async () => {
  if (previousConfigDir === undefined) delete process.env.CLAUDE_CONFIG_DIR
  else process.env.CLAUDE_CONFIG_DIR = previousConfigDir
  if (root) await rm(root, { recursive: true, force: true })
})

describe('local session catalog', () => {
  test('lists the project and its sessions', async () => {
    const projects = await listProjects()
    const project = projects.find(item => item.path === cwd)
    expect(project).toBeDefined()
    expect(project?.sessionCount).toBe(1)

    const result = await listSessions({
      projectId: sanitizePath(cwd),
      limit: 10,
    })
    expect(result.sessions).toHaveLength(1)
    expect(result.sessions[0]?.localSessionId).toBe(localSessionId)
    expect(result.sessions[0]?.title).toBe('hello')
  })

  test('keeps older sessions when paginating alongside a new live terminal', async () => {
    if (process.platform !== 'win32') return
    const projectDir = join(root, 'projects', sanitizePath(cwd))
    const ids = [
      '11111111-1111-4111-8111-111111111111',
      '22222222-2222-4222-8222-222222222222',
      '33333333-3333-4333-8333-333333333333',
    ]
    const registryPath = join(root, 'sessions', `${process.pid}.json`)
    try {
      await mkdir(join(root, 'sessions'), { recursive: true })
      for (const id of ids) {
        await writeFile(
          join(projectDir, `${id}.jsonl`),
          `${line({
            type: 'user',
            uuid: id,
            cwd,
            sessionId: id,
            message: { role: 'user', content: `pagination ${id}` },
          })}\n`,
        )
      }
      await writeFile(
        registryPath,
        line({
          pid: process.pid,
          sessionId: '44444444-4444-4444-8444-444444444444',
          cwd,
          bridgeSessionId: 'session_abcdef0123456789',
          updatedAt: Date.now() + 60_000,
        }),
      )
      const expected = await listSessionsImpl({ dir: cwd, limit: 2 })
      const page = await listSessions(
        { projectPath: cwd, limit: 2, offset: 1 },
        root,
      )
      expect(page.sessions.map(item => item.localSessionId)).toEqual(
        expected.map(item => item.sessionId),
      )
      expect(page.hasMore).toBe(true)
    } finally {
      await unlink(registryPath).catch(() => {})
      for (const id of ids)
        await unlink(join(projectDir, `${id}.jsonl`)).catch(() => {})
    }
  }, 15_000)

  test('reads the newest page and paginates backwards', async () => {
    const tail = await readSession({
      localSessionId,
      cwd,
      limit: 2,
    })
    expect(tail.messages.map(message => message.id)).toEqual(['u3', 'a3'])
    expect(tail.hasMore).toBe(true)
    expect(tail.before).toBe('u3')

    const older = await readSession({
      localSessionId,
      cwd,
      limit: 2,
      before: tail.before,
    })
    expect(older.messages.map(message => message.id)).toEqual(['a2', 'u2'])
    expect(older.hasMore).toBe(true)

    const oldest = await readSession({
      localSessionId,
      cwd,
      limit: 2,
      before: older.before,
    })
    expect(oldest.messages.map(message => message.id)).toEqual(['u1', 'a1'])
    expect(oldest.hasMore).toBe(false)
    expect(oldest.before).toBeUndefined()
  })

  test('normalizes tool calls and thinking, and drops sidechains', async () => {
    const page = await readSession({ localSessionId, cwd, limit: 100 })
    const assistant = page.messages.find(message => message.id === 'a1')
    expect(assistant?.blocks[0]).toEqual({ type: 'text', text: 'hi' })
    expect(assistant?.blocks[1]).toMatchObject({
      type: 'tool_use',
      name: 'Read',
    })
    const thought = page.messages.find(message => message.id === 'a2')
    expect(thought?.blocks[0]).toEqual({ type: 'thinking', text: 'hmm' })
    expect(page.messages.some(message => message.text === 'hidden')).toBe(false)
  })

  test('an unknown cursor returns an empty page instead of a wrong slice', async () => {
    const page = await readSession({
      localSessionId,
      cwd,
      limit: 2,
      before: 'does-not-exist',
    })
    expect(page.messages).toEqual([])
    expect(page.hasMore).toBe(false)
  })
})

describe('fresh terminal sessions (no transcript yet)', () => {
  const freshId = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'
  const resumedId = '11111111-2222-3333-4444-555555555555'
  const startedAt = Date.now() - 5_000

  function entry(overrides: Partial<RegistryEntry>): RegistryEntry {
    return {
      pid: 4242,
      sessionId: freshId,
      cwd,
      startedAt,
      bridgeSessionId: 'session_0123456789abcdef0123456789abcdef',
      ...overrides,
    }
  }

  function proc(overrides: Partial<BunProcess>): BunProcess {
    return {
      name: 'bun.exe',
      commandLine: '"bun.exe" "cli.js" --remote-control',
      startedAt,
      ...overrides,
    }
  }

  test('lists a live bridge terminal without --resume', () => {
    const result = selectFreshTerminalSessions(
      [entry({})],
      new Map([[4242, proc({})]]),
      [],
    )
    expect(result).toHaveLength(1)
    expect(result[0]).toMatchObject({
      localSessionId: freshId,
      cwd,
      title: '新会话',
    })
  })

  test('skips resumed terminals, dead pids, missing bridges and known sessions', () => {
    const listed = [
      {
        localSessionId: freshId,
        projectId: sanitizePath(cwd),
        cwd,
        title: 'already listed',
        summary: '',
        updatedAt: 1,
      },
    ]
    const result = selectFreshTerminalSessions(
      [
        entry({}), // already covered by the file-based catalog
        entry({ pid: 5, sessionId: resumedId }), // no live process
        entry({ pid: 6, bridgeSessionId: undefined }), // no bridge
      ],
      new Map([
        [4242, proc({})],
        [6, proc({})],
      ]),
      listed,
    )
    expect(result).toHaveLength(0)

    const resumed = selectFreshTerminalSessions(
      [entry({ pid: 7, sessionId: resumedId })],
      new Map([
        [
          7,
          proc({
            commandLine: '"bun.exe" "cli.js" --resume 999 --remote-control',
          }),
        ],
      ]),
      [],
    )
    expect(resumed).toHaveLength(0)

    const staleStart = selectFreshTerminalSessions(
      [entry({ pid: 8, startedAt: startedAt - 60_000 })],
      new Map([[8, proc({})]]),
      [],
    )
    expect(staleStart).toHaveLength(0)
  })

  test('readSession returns an empty page for a live session with no transcript', async () => {
    const liveId = 'abcdefab-cdef-abcd-efab-cdefabcdefab'
    const sessionsDir = join(root, 'sessions')
    await mkdir(sessionsDir, { recursive: true })
    await writeFile(
      join(sessionsDir, `${process.pid}.json`),
      JSON.stringify({
        pid: process.pid,
        sessionId: liveId,
        cwd,
        startedAt: Date.now() - 1000,
        bridgeSessionId: 'session_0123456789abcdef0123456789abcdef',
      }),
    )
    const page = await readSession({
      localSessionId: liveId,
      cwd,
      limit: 5,
      configDir: root,
    })
    expect(page.messages).toEqual([])
    expect(page.hasMore).toBe(false)
    expect(page.total).toBe(0)
  })
})
