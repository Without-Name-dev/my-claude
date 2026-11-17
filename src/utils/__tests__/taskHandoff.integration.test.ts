import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import { mkdtemp, rm } from 'fs/promises'
import { tmpdir } from 'os'
import { join } from 'path'
import type { ToolUseContext } from '../../Tool'
import {
  getPostCompactTaskHandoff,
  getTaskReminderAttachments,
} from '../attachments'
import { getClaudeConfigHomeDir } from '../envUtils'
import { normalizeAttachmentForAPI } from '../messages'
import { createTask, listTasks } from '../tasks'

const previous = {
  config: process.env.CLAUDE_CONFIG_DIR,
  list: process.env.CLAUDE_CODE_TASK_LIST_ID,
  enabled: process.env.CLAUDE_CODE_ENABLE_TASKS,
}
let dir: string
const listId = 'handoff-integration'
const context = {
  options: { tools: [{ name: 'TaskUpdate' }] },
} as unknown as ToolUseContext

beforeAll(async () => {
  dir = await mkdtemp(join(tmpdir(), 'ccb-task-handoff-'))
  process.env.CLAUDE_CONFIG_DIR = dir
  process.env.CLAUDE_CODE_TASK_LIST_ID = listId
  process.env.CLAUDE_CODE_ENABLE_TASKS = '1'
  getClaudeConfigHomeDir.cache.clear?.()
  for (const [subject, status] of [
    ['old audit', 'in_progress'],
    ['old verification', 'pending'],
    ['new work', 'completed'],
  ] as const) {
    await createTask(listId, {
      subject,
      status,
      description: '',
      blocks: [],
      blockedBy: [],
    })
  }
})

afterAll(async () => {
  for (const [key, value] of Object.entries({
    CLAUDE_CONFIG_DIR: previous.config,
    CLAUDE_CODE_TASK_LIST_ID: previous.list,
    CLAUDE_CODE_ENABLE_TASKS: previous.enabled,
  })) {
    if (value === undefined) delete process.env[key]
    else process.env[key] = value
  }
  getClaudeConfigHomeDir.cache.clear?.()
  if (dir) await rm(dir, { recursive: true, force: true })
})

describe('task handoff integration', () => {
  test('loads unfinished tasks for a new prompt regardless of recent task updates', async () => {
    const result = await getTaskReminderAttachments([], context, {
      handoff: true,
    })
    expect(result).toHaveLength(1)
    expect(result[0]).toMatchObject({
      type: 'task_reminder',
      reason: 'handoff',
      itemCount: 2,
    })
    if (result[0]?.type !== 'task_reminder')
      throw new Error('missing task handoff')
    expect(result[0].content.map(task => task.subject)).toEqual([
      'old audit',
      'old verification',
    ])
    const apiMessages = normalizeAttachmentForAPI(result[0])
    expect(JSON.stringify(apiMessages)).toContain('old audit')
    expect(JSON.stringify(apiMessages)).toContain(
      'Do not mark unfinished work completed',
    )
    // Directory enumeration order differs across filesystems. Assert each
    // task's status by identity rather than depending on its directory order.
    expect(
      Object.fromEntries(
        (await listTasks(listId)).map(task => [task.subject, task.status]),
      ),
    ).toEqual({
      'old audit': 'in_progress',
      'old verification': 'pending',
      'new work': 'completed',
    })
  })

  test('restores the same tasks after compaction without injecting into a subagent', async () => {
    expect(await getPostCompactTaskHandoff(context)).toMatchObject([
      { type: 'task_reminder', reason: 'handoff', itemCount: 2 },
    ])
    expect(
      await getPostCompactTaskHandoff({
        ...context,
        agentId: 'agent-1',
      } as ToolUseContext),
    ).toEqual([])
  })
})
