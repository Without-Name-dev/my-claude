/** Snapshots reported by the terminal, not inferred from chat text. */
import type { ThreadEntry, ToolCallData } from './types'
export type ExecutionTask = {
  id: string
  subject: string
  status: 'pending' | 'in_progress' | 'completed'
  owner?: string
  activeForm?: string
  description?: string
  blockedBy: string[]
}

export type WorkflowAgent = {
  id: number
  label?: string
  phase?: string
  status: 'running' | 'done'
  resultKind?: string
  model?: string
  tokenCount?: number
  toolCount?: number
}

export type WorkflowRun = {
  runId: string
  workflowName: string
  status: 'running' | 'completed' | 'failed' | 'killed'
  description?: string
  error?: string
  declaredPhases: string[]
  phases: Array<{ title: string; status: 'running' | 'done' }>
  agents: WorkflowAgent[]
}

function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined
}
function str(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined
}
function strings(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === 'string')
    : []
}
function payloadRecord(value: unknown): Record<string, unknown> | undefined {
  const outer = record(value)
  return record(outer?.raw) ?? outer
}

export function agentProgressParentId(payload: unknown): string | undefined {
  return str(payloadRecord(payload)?.parent_tool_use_id)
}

export function parseTaskSnapshot(
  payload: unknown,
): ExecutionTask[] | undefined {
  const root = payloadRecord(payload)
  if (!Array.isArray(root?.tasks)) return undefined
  return root.tasks.flatMap(value => {
    const task = record(value)
    const id = str(task?.id)
    const subject = str(task?.subject)
    const status = task?.status
    if (
      !id ||
      !subject ||
      (status !== 'pending' &&
        status !== 'in_progress' &&
        status !== 'completed')
    )
      return []
    return [
      {
        id,
        subject,
        status,
        owner: str(task?.owner),
        activeForm: str(task?.activeForm),
        description: str(task?.description),
        blockedBy: strings(task?.blockedBy),
      },
    ]
  })
}

export function parseWorkflowSnapshot(
  payload: unknown,
): WorkflowRun[] | undefined {
  const root = payloadRecord(payload)
  if (!Array.isArray(root?.runs)) return undefined
  return root.runs.flatMap(value => {
    const run = record(value)
    const runId = str(run?.runId)
    const workflowName = str(run?.workflowName)
    const status = run?.status
    if (
      !runId ||
      !workflowName ||
      (status !== 'running' &&
        status !== 'completed' &&
        status !== 'failed' &&
        status !== 'killed')
    )
      return []
    const phases: WorkflowRun['phases'] = Array.isArray(run?.phases)
      ? run.phases.flatMap(value => {
          const phase = record(value)
          const title = str(phase?.title)
          return title &&
            (phase?.status === 'running' || phase?.status === 'done')
            ? [{ title, status: phase.status }]
            : []
        })
      : []
    const agents: WorkflowAgent[] = Array.isArray(run?.agents)
      ? run.agents.flatMap(value => {
          const agent = record(value)
          if (
            typeof agent?.id !== 'number' ||
            (agent.status !== 'running' && agent.status !== 'done')
          )
            return []
          return [
            {
              id: agent.id,
              status: agent.status,
              label: str(agent.label),
              phase: str(agent.phase),
              resultKind: str(agent.resultKind),
              model: str(agent.model),
              tokenCount:
                typeof agent.tokenCount === 'number'
                  ? agent.tokenCount
                  : undefined,
              toolCount:
                typeof agent.toolCount === 'number'
                  ? agent.toolCount
                  : undefined,
            },
          ]
        })
      : []
    return [
      {
        runId,
        workflowName,
        status,
        description: str(run?.description),
        error: str(run?.error),
        declaredPhases: strings(run?.declaredPhases),
        phases,
        agents,
      },
    ]
  })
}

/** Merge child-tool progress into the matching parent Agent call. */
export function applyAgentProgress(
  entries: ThreadEntry[],
  payload: unknown,
): ThreadEntry[] {
  const root = payloadRecord(payload)
  if (
    typeof root?.parent_tool_use_id !== 'string' ||
    !Array.isArray(root.activities)
  )
    return entries
  const index = entries.findIndex(
    entry =>
      entry.type === 'tool_call' &&
      entry.toolCall.id === root.parent_tool_use_id,
  )
  if (index < 0) return entries
  const entry = entries[index]
  if (!entry || entry.type !== 'tool_call') return entries
  const activities: NonNullable<ToolCallData['agentActivity']> = [
    ...(entry.toolCall.agentActivity ?? []),
  ]
  let changed = false
  for (const value of root.activities) {
    const activity = record(value)
    const id = str(activity?.id)
    if (
      !id ||
      (activity?.kind !== 'tool_use' && activity?.kind !== 'tool_result')
    )
      continue
    const existing = activities.findIndex(item => item.id === id)
    const current =
      existing >= 0 ? activities[existing]! : { id, status: 'running' as const }
    const next: NonNullable<ToolCallData['agentActivity']>[number] =
      activity.kind === 'tool_use'
        ? {
            ...current,
            name: str(activity.name) ?? current.name,
            hint: str(activity.hint) ?? current.hint,
          }
        : {
            ...current,
            status: activity.failed === true ? 'error' : 'complete',
            output: str(activity.output),
          }
    if (existing < 0) {
      activities.push(next)
      changed = true
    } else if (
      current.name !== next.name ||
      current.hint !== next.hint ||
      current.status !== next.status ||
      current.output !== next.output
    ) {
      activities[existing] = next
      changed = true
    }
  }
  const agentId = str(root.agent_id) ?? entry.toolCall.agentId
  if (!changed && agentId === entry.toolCall.agentId) return entries
  const next = [...entries]
  next[index] = {
    type: 'tool_call',
    toolCall: {
      ...entry.toolCall,
      agentId,
      agentActivity: activities.slice(-100),
    },
  }
  return next
}
