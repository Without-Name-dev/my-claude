import type { Task } from './tasks.js'

// Keep the context bounded; the complete list remains available via TaskList.
export const TASK_HANDOFF_LIMIT = 30

export function buildTaskHandoff(tasks: readonly Task[]): {
  content: Task[]
  itemCount: number
} | null {
  const unfinished = tasks.filter(task => task.status !== 'completed')
  if (unfinished.length === 0) return null
  const ordered = [...unfinished].sort((a, b) => {
    const statusOrder =
      Number(b.status === 'in_progress') - Number(a.status === 'in_progress')
    return statusOrder || a.id.localeCompare(b.id, undefined, { numeric: true })
  })
  return {
    content: ordered.slice(0, TASK_HANDOFF_LIMIT).map(task => ({
      id: task.id,
      subject: task.subject.replace(/\s+/g, ' ').slice(0, 200),
      description: '',
      status: task.status,
      owner: task.owner,
      blocks: [],
      blockedBy: task.blockedBy.slice(0, 30),
    })),
    itemCount: unfinished.length,
  }
}

export function formatTaskHandoff(
  tasks: readonly Task[],
  itemCount: number,
): string {
  const items = tasks
    .map(task => {
      const subject = task.subject.replace(/\s+/g, ' ').slice(0, 200)
      const owner = task.owner ? `; owner: ${task.owner}` : ''
      const blocked = task.blockedBy.length
        ? `; blocked by: ${task.blockedBy.join(', ')}`
        : ''
      return `#${task.id} [${task.status}${owner}${blocked}] ${subject}`
    })
    .join('\n')
  const omitted = Math.max(0, itemCount - tasks.length)
  return `Unfinished tasks from this session are listed below. These are persistent records, not proof that work is currently running. Follow the user's current request and preserve earlier unfinished work unless the user changes or cancels it. When continuing related work, use TaskGet to inspect its details, verify what was actually done, and update the appropriate task when its full completion criteria are met. Do not mark unfinished work completed or delete it merely to clear the list. If work is interrupted or blocked, preserve the remaining work and tell the user when relevant. Do not start unrelated work or duplicate existing tasks solely because of this reminder.\n\n${items}${omitted ? `\n... ${omitted} more unfinished tasks; use TaskList to see all.` : ''}`
}
