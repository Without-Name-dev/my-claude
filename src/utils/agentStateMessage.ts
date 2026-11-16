import type { AppState } from '../state/AppState.js'
import type { LocalAgentTaskState } from '../tasks/LocalAgentTask/LocalAgentTask.js'

/** The same visible local-agent rows as the terminal, without prompts or output. */
export type AgentStateItem = {
  id: string
  name?: string
  description: string
  status: LocalAgentTaskState['status']
  startTime: number
  endTime?: number
  totalPausedMs?: number
  tokenCount?: number
  queuedCount: number
}

export function buildAgentStateSnapshot(
  tasks: AppState['tasks'],
  names: AppState['agentNameRegistry'],
): AgentStateItem[] {
  const nameById = new Map<string, string>()
  for (const [name, id] of names) nameById.set(id, name)
  return Object.values(tasks)
    .filter(
      (task): task is LocalAgentTaskState =>
        task.type === 'local_agent' &&
        task.agentType !== 'main-session' &&
        (task.evictAfter === undefined || task.evictAfter > Date.now()),
    )
    .sort((a, b) => a.startTime - b.startTime || a.id.localeCompare(b.id))
    .map(task => ({
      id: task.id,
      name: nameById.get(task.id),
      description: task.progress?.summary || task.description,
      status: task.status,
      startTime: task.startTime,
      endTime: task.endTime,
      totalPausedMs: task.totalPausedMs,
      tokenCount: task.progress?.tokenCount,
      queuedCount: task.pendingMessages.length,
    }))
}
