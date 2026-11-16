import { describe, expect, test } from 'bun:test'
import {
  buildTaskHandoff,
  formatTaskHandoff,
  TASK_HANDOFF_LIMIT,
} from '../taskHandoff'
import type { Task } from '../tasks'

function task(
  id: string,
  status: Task['status'],
  subject = `Task ${id}`,
): Task {
  return { id, subject, description: '', status, blocks: [], blockedBy: [] }
}

describe('task handoff', () => {
  test('keeps the old unfinished tasks when newer tasks were completed', () => {
    const tasks = [
      task('1', 'in_progress', '审计新增功能并找出可复现 bug'),
      task('2', 'pending', '修复确认的 bug'),
      task('3', 'pending', '全量验证修复结果'),
      ...['5', '6', '7', '8'].map(id => task(id, 'completed')),
    ]
    const snapshot = buildTaskHandoff(tasks)
    expect(snapshot?.content.map(item => item.id)).toEqual(['1', '2', '3'])
    expect(snapshot?.itemCount).toBe(3)
    expect(tasks[0]?.status).toBe('in_progress')
    expect(formatTaskHandoff(snapshot!.content, snapshot!.itemCount)).toContain(
      'Do not mark unfinished work completed',
    )
  })

  test('does not create a handoff when no work remains', () => {
    expect(buildTaskHandoff([])).toBeNull()
    expect(buildTaskHandoff([task('1', 'completed')])).toBeNull()
  })

  test('limits context while keeping active tasks and reporting omitted work', () => {
    const tasks = Array.from({ length: 35 }, (_, i) =>
      task(String(i + 1), 'pending'),
    )
    tasks.push({
      ...task('40', 'in_progress', 'Work\ncontinued'),
      owner: 'agent-a',
      blockedBy: ['2'],
    })
    const snapshot = buildTaskHandoff(tasks)!
    expect(snapshot.content).toHaveLength(TASK_HANDOFF_LIMIT)
    expect(snapshot.itemCount).toBe(36)
    expect(snapshot.content[0]?.id).toBe('40')
    const text = formatTaskHandoff(snapshot.content, snapshot.itemCount)
    expect(text).toContain(
      '[in_progress; owner: agent-a; blocked by: 2] Work continued',
    )
    expect(text).toContain('6 more unfinished tasks')
  })
})
