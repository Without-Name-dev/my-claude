import { expect, test } from 'bun:test'
import { buildWorkflowStateMessage } from '../bridgeSnapshot.js'
import type { RunProgress } from '../progress/store.js'

test('bridge workflow state contains visible progress without scripts or return payloads', () => {
  const run: RunProgress = {
    runId: 'run-1',
    workflowName: 'audit',
    status: 'running',
    phases: [{ title: 'scan', status: 'running' }],
    declaredPhases: ['scan', 'verify'],
    currentPhase: 'scan',
    agents: [
      {
        id: 7,
        label: 'frontend',
        phase: 'scan',
        status: 'running',
        toolCount: 2,
      },
    ],
    agentCount: 1,
    startedAt: 1,
    updatedAt: 2,
    returnValue: { private: 'not transmitted' },
  }
  const msg = buildWorkflowStateMessage([run]) as unknown as {
    type: string
    runs: Array<Record<string, unknown>>
  }
  expect(msg.type).toBe('workflow_state')
  expect(msg.runs[0]).toMatchObject({
    runId: 'run-1',
    phases: run.phases,
    agents: run.agents,
  })
  expect(msg.runs[0]).not.toHaveProperty('returnValue')
})
