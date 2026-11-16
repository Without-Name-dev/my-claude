import { randomUUID } from 'crypto'
import type { SDKMessage } from 'src/entrypoints/agentSdkTypes.js'
import type { Task } from './tasks.js'
import type { AgentStateItem } from './agentStateMessage.js'

export type TaskStateItem = Pick<
  Task,
  | 'id'
  | 'subject'
  | 'description'
  | 'activeForm'
  | 'status'
  | 'owner'
  | 'blocks'
  | 'blockedBy'
>

export type TaskStateMessage = SDKMessage & {
  type: 'task_state'
  uuid: string
  task_list_id: string
  tasks: TaskStateItem[]
  agents?: AgentStateItem[]
}

export type TaskStateSnapshot = Pick<
  TaskStateMessage,
  'task_list_id' | 'tasks' | 'agents'
>

function toTaskStateItem(task: Task): TaskStateItem {
  return {
    id: task.id,
    subject: task.subject,
    description: task.description,
    activeForm: task.activeForm,
    status: task.status,
    owner: task.owner,
    blocks: [...task.blocks],
    blockedBy: [...task.blockedBy],
  }
}

function compareTaskStateItems(a: TaskStateItem, b: TaskStateItem): number {
  return a.id.localeCompare(b.id)
}

export function buildTaskStateSnapshot(
  taskListId: string,
  tasks: Task[],
  agents?: AgentStateItem[],
): TaskStateSnapshot {
  return {
    task_list_id: taskListId,
    tasks: tasks
      .filter(task => !task.metadata?._internal)
      .map(toTaskStateItem)
      .sort(compareTaskStateItems),
    ...(agents ? { agents } : {}),
  }
}

export function getTaskStateSnapshotKey(
  taskListId: string,
  tasks: Task[],
  agents?: AgentStateItem[],
): string {
  return JSON.stringify(buildTaskStateSnapshot(taskListId, tasks, agents))
}

export function buildTaskStateMessage(
  taskListId: string,
  tasks: Task[],
  agents?: AgentStateItem[],
): TaskStateMessage {
  const snapshot = buildTaskStateSnapshot(taskListId, tasks, agents)
  return {
    type: 'task_state',
    uuid: randomUUID(),
    ...snapshot,
  }
}
