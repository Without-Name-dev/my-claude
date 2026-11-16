import { expect, spyOn, test } from 'bun:test'
import {
  applyAgentProgress,
  parseTaskSnapshot,
  parseBackgroundAgents,
  parseWorkflowSnapshot,
  splitWorkflowRuns,
} from '../lib/execution-state'
import { RCSChatAdapter } from '../lib/rcs-chat-adapter'
import type {
  BackgroundAgent,
  ExecutionTask,
  WorkflowRun,
} from '../lib/execution-state'
import type { ThreadEntry } from '../lib/types'
import type { SetStateAction } from 'react'

test('background agent snapshots update and clear independently of task list', () => {
  let agents: BackgroundAgent[] = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onBackgroundAgents: next => {
      agents = next
    },
  })
  adapter.handleEvent({
    type: 'task_state',
    payload: {
      raw: {
        tasks: [],
        agents: [
          {
            id: 'a',
            name: 'verification',
            description: '验证修复',
            status: 'running',
            startTime: 100,
            tokenCount: 12,
            queuedCount: 1,
          },
        ],
      },
    },
  })
  expect(
    agents.map(a => [
      a.name,
      a.description,
      a.status,
      a.tokenCount,
      a.queuedCount,
    ]),
  ).toEqual([['verification', '验证修复', 'running', 12, 1]])
  expect(parseBackgroundAgents({ raw: { tasks: [] } })).toBeUndefined()
  adapter.handleEvent({
    type: 'task_state',
    payload: { raw: { tasks: [], agents: [] } },
  })
  expect(agents).toEqual([])
})

test('completed and stopped workflows remain history instead of active work', () => {
  const runs = parseWorkflowSnapshot({
    runs: [
      { runId: '1', workflowName: 'one', status: 'completed' },
      { runId: '2', workflowName: 'two', status: 'completed' },
      { runId: '3', workflowName: 'three', status: 'killed' },
    ],
  })!
  expect(splitWorkflowRuns(runs).active).toEqual([])
  expect(splitWorkflowRuns(runs).history).toHaveLength(3)
  const running = parseWorkflowSnapshot({
    runs: [{ runId: '4', workflowName: 'new', status: 'running' }],
  })![0]!
  expect(splitWorkflowRuns([...runs, running]).active).toEqual([running])
})

test('task snapshots retain terminal status, owner and blockers; empty snapshot clears the list', () => {
  let tasks: ExecutionTask[] = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onTasks: value => {
      tasks = value
    },
  })
  adapter.handleEvent({
    type: 'task_state',
    payload: {
      raw: {
        tasks: [
          {
            id: '1',
            subject: '修复布局',
            status: 'in_progress',
            owner: 'agent-1',
            blockedBy: ['2'],
          },
          { id: '2', subject: '排查问题', status: 'completed', blockedBy: [] },
        ],
      },
    },
  })
  expect(
    tasks.map(task => [task.subject, task.status, task.owner, task.blockedBy]),
  ).toEqual([
    ['修复布局', 'in_progress', 'agent-1', ['2']],
    ['排查问题', 'completed', undefined, []],
  ])
  adapter.handleEvent({ type: 'task_state', payload: { raw: { tasks: [] } } })
  expect(tasks).toEqual([])
  expect(parseTaskSnapshot({ raw: {} })).toBeUndefined()
})

test('workflow snapshots show actual phase and subagent progress without fabricating status', () => {
  let workflows: WorkflowRun[] = []
  const adapter = new RCSChatAdapter('session', () => {}, {
    onWorkflows: value => {
      workflows = value
    },
  })
  adapter.handleEvent({
    type: 'workflow_state',
    payload: {
      raw: {
        runs: [
          {
            runId: 'run-1',
            workflowName: 'audit',
            status: 'running',
            declaredPhases: ['扫描', '复核'],
            phases: [{ title: '扫描', status: 'running' }],
            agents: [
              {
                id: 1,
                label: '检查前端',
                phase: '扫描',
                status: 'running',
                tokenCount: 1234,
                toolCount: 3,
              },
            ],
          },
        ],
      },
    },
  })
  expect(workflows[0]?.phases).toEqual([{ title: '扫描', status: 'running' }])
  expect(workflows[0]?.agents[0]).toMatchObject({
    label: '检查前端',
    status: 'running',
    toolCount: 3,
  })
  expect(
    parseWorkflowSnapshot({
      runs: [{ runId: 'bad', workflowName: 'x', status: 'unknown' }],
    }),
  ).toEqual([])
  adapter.handleEvent({
    type: 'workflow_state',
    payload: { raw: { runs: [] } },
  })
  expect(workflows).toEqual([])
})

test('device view restores the latest task snapshot from bridge history on open', async () => {
  const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'fixture', setItem: () => {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async () =>
    Response.json({
      events: [
        {
          type: 'task_state',
          payload: {
            raw: {
              tasks: [
                {
                  id: '1',
                  subject: '核对',
                  status: 'completed',
                  blockedBy: [],
                },
              ],
              agents: [
                {
                  id: 'agent',
                  name: 'verification',
                  description: '检查修复',
                  status: 'running',
                  startTime: 100,
                  queuedCount: 0,
                },
              ],
            },
          },
        },
      ],
    })) as unknown as typeof fetch)
  try {
    let tasks: ExecutionTask[] = []
    let agents: BackgroundAgent[] = []
    const adapter = new RCSChatAdapter('session', () => {}, {
      historyMode: 'metadata',
      onTasks: value => {
        tasks = value
      },
      onBackgroundAgents: value => {
        agents = value
      },
    })
    await adapter.loadHistory()
    expect(tasks[0]).toMatchObject({ subject: '核对', status: 'completed' })
    expect(agents[0]).toMatchObject({ name: 'verification', status: 'running' })
  } finally {
    fetchSpy.mockRestore()
    if (storage) Object.defineProperty(globalThis, 'localStorage', storage)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('subagent child tools and results update their parent without changing its status', () => {
  let entries: ThreadEntry[] = [
    {
      type: 'tool_call',
      toolCall: { id: 'parent', title: 'Agent', status: 'running' },
    },
  ]
  const setEntries = (action: SetStateAction<ThreadEntry[]>) => {
    entries = typeof action === 'function' ? action(entries) : action
  }
  const adapter = new RCSChatAdapter('session', setEntries)
  adapter.handleEvent({
    type: 'agent_progress',
    payload: {
      raw: {
        parent_tool_use_id: 'parent',
        agent_id: 'child',
        activities: [
          { kind: 'tool_use', id: 't1', name: 'Read', hint: 'a.ts' },
        ],
      },
    },
  })
  adapter.handleEvent({
    type: 'agent_progress',
    payload: {
      raw: {
        parent_tool_use_id: 'parent',
        activities: [
          { kind: 'tool_result', id: 't1', output: 'done', failed: false },
        ],
      },
    },
  })
  expect(entries[0]).toMatchObject({
    toolCall: {
      status: 'running',
      agentId: 'child',
      agentActivity: [
        {
          id: 't1',
          name: 'Read',
          hint: 'a.ts',
          output: 'done',
          status: 'complete',
        },
      ],
    },
  })
  const replayed = applyAgentProgress(entries, {
    parent_tool_use_id: 'parent',
    activities: [
      { kind: 'tool_result', id: 't1', output: 'done', failed: false },
    ],
  })
  expect(replayed).toBe(entries)
})
