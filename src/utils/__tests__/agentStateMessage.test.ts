import { expect, test } from 'bun:test'
import type { AppState } from '../../state/AppState.js'
import { buildAgentStateSnapshot } from '../agentStateMessage.js'
import {
  buildTaskStateMessage,
  getTaskStateSnapshotKey,
} from '../taskStateMessage.js'

test('mirrors visible local agents without exposing prompts, messages or internal tasks', () => {
  const tasks = {
    a: {
      id: 'a',
      type: 'local_agent',
      agentType: 'verification',
      status: 'running',
      description: '验证修复',
      startTime: 100,
      pendingMessages: ['secret'],
      prompt: 'private prompt',
      progress: { summary: '复核中', tokenCount: 12 },
    },
    hidden: {
      id: 'hidden',
      type: 'local_agent',
      agentType: 'verification',
      evictAfter: 0,
      startTime: 1,
    },
    foreground: {
      id: 'foreground',
      type: 'local_agent',
      agentType: 'general-purpose',
      isBackgrounded: false,
      startTime: 1,
      status: 'running',
      description: '前台代理',
      pendingMessages: [],
    },
    main: {
      id: 'main',
      type: 'local_agent',
      agentType: 'main-session',
      startTime: 1,
    },
    shell: { id: 'shell', type: 'local_bash', startTime: 1 },
  } as unknown as AppState['tasks']
  const agents = buildAgentStateSnapshot(
    tasks,
    new Map([['verification', 'a']]) as AppState['agentNameRegistry'],
  )
  expect(agents).toEqual([
    {
      id: 'foreground',
      name: undefined,
      description: '前台代理',
      status: 'running',
      startTime: 1,
      endTime: undefined,
      totalPausedMs: undefined,
      tokenCount: undefined,
      queuedCount: 0,
    },
    {
      id: 'a',
      name: 'verification',
      description: '复核中',
      status: 'running',
      startTime: 100,
      endTime: undefined,
      totalPausedMs: undefined,
      tokenCount: 12,
      queuedCount: 1,
    },
  ])
  expect(JSON.stringify(agents)).not.toContain('private prompt')
  expect(JSON.stringify(agents)).not.toContain('secret')
  const message = buildTaskStateMessage('list', [], agents)
  expect(message.agents).toEqual(agents)
  expect(getTaskStateSnapshotKey('list', [], agents)).not.toBe(
    getTaskStateSnapshotKey('list', [], []),
  )
})
