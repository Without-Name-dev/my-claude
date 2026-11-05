import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import { mkdir, mkdtemp, realpath, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { sanitizePath } from '../../../../src/utils/sessionStoragePortable'
import { listProjects, listSessions, readSession } from '../sessions'

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
