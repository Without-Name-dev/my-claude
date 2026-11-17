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

export type BackgroundAgent = {
  id: string
  name?: string
  description: string
  status: 'pending' | 'running' | 'completed' | 'failed' | 'killed'
  startTime: number
  endTime?: number
  totalPausedMs?: number
  tokenCount?: number
  queuedCount: number
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

/** Old completed/stopped runs are history, not current work. */
export function splitWorkflowRuns(runs: readonly WorkflowRun[]): {
  active: WorkflowRun[]
  history: WorkflowRun[]
} {
  const active: WorkflowRun[] = []
  const history: WorkflowRun[] = []
  for (const run of runs) {
    if (run.status === 'running') active.push(run)
    else history.push(run)
  }
  return { active, history }
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

/** The agent list is absent on older CLIs; an empty array clears old rows. */
export function parseBackgroundAgents(
  payload: unknown,
): BackgroundAgent[] | undefined {
  const root = payloadRecord(payload)
  if (!Array.isArray(root?.agents)) return undefined
  return root.agents.flatMap(value => {
    const agent = record(value)
    const id = str(agent?.id)
    const description = str(agent?.description)
    const status = agent?.status
    if (
      !id ||
      description === undefined ||
      (status !== 'pending' &&
        status !== 'running' &&
        status !== 'completed' &&
        status !== 'failed' &&
        status !== 'killed') ||
      typeof agent?.startTime !== 'number' ||
      !Number.isFinite(agent.startTime)
    )
      return []
    const optionalNumber = (v: unknown) =>
      typeof v === 'number' && Number.isFinite(v) ? v : undefined
    return [
      {
        id,
        description,
        status,
        startTime: agent.startTime,
        name: str(agent.name),
        endTime: optionalNumber(agent.endTime),
        totalPausedMs: optionalNumber(agent.totalPausedMs),
        tokenCount: optionalNumber(agent.tokenCount),
        queuedCount: Math.max(
          0,
          Math.floor(optionalNumber(agent.queuedCount) ?? 0),
        ),
      },
    ]
  })
}

/** Terminal prompts queued while a turn runs, mirrored by the CLI. */
export type TerminalQueueSnapshot = {
  count: number
  previews: string[]
  /** Absent when connected to a CLI predating remote queue tracking. */
  queuedMessageIds?: string[]
}

/** The CLI queue is authoritative while it still contains a web message. */
export function markTerminalQueuedMessages(
  entries: ThreadEntry[],
  snapshot: TerminalQueueSnapshot | null,
): ThreadEntry[] {
  if (!snapshot?.queuedMessageIds?.length) return entries
  const ids = new Set(snapshot.queuedMessageIds.map(id => `user-${id}`))
  return entries.map(entry =>
    entry.type === 'user_message' &&
    (ids.has(entry.id) || (entry.clientId && ids.has(entry.clientId)))
      ? { ...entry, pending: true }
      : entry,
  )
}

export function parseTerminalQueueSnapshot(
  payload: unknown,
): TerminalQueueSnapshot | undefined {
  const root = payloadRecord(payload)
  if (!root || typeof root.count !== 'number') return undefined
  if (!Array.isArray(root.previews)) return undefined
  const previews = strings(root.previews)
  return {
    count: Math.max(0, Math.floor(root.count)),
    previews,
    ...(Array.isArray(root.queuedMessageIds)
      ? { queuedMessageIds: strings(root.queuedMessageIds) }
      : {}),
  }
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
