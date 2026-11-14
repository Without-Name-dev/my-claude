import { randomUUID } from 'node:crypto'
import type { SDKMessage } from '../entrypoints/agentSdkTypes.js'
import type { RunProgress } from './progress/store.js'

/** The same progress visible in /workflows, without return values or scripts. */
export function buildWorkflowStateMessage(
  runs: readonly RunProgress[],
): SDKMessage {
  return {
    type: 'workflow_state',
    uuid: randomUUID(),
    runs: runs.map(run => ({
      runId: run.runId,
      workflowName: run.workflowName,
      status: run.status,
      description: run.description,
      startedAt: run.startedAt,
      updatedAt: run.updatedAt,
      error: run.error,
      currentPhase: run.currentPhase,
      declaredPhases: run.declaredPhases,
      phases: run.phases,
      agents: run.agents,
    })),
  } as unknown as SDKMessage
}
