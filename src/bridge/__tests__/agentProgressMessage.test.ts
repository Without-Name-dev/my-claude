import { expect, test } from 'bun:test'
import type { Message } from '../../types/message.js'
import { buildAgentProgressMessage } from '../agentProgressMessage.js'

test('extracts a subagent tool call and its parent without sending the agent prompt', () => {
  const message = {
    type: 'progress',
    uuid: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    parentToolUseID: 'parent-1',
    data: {
      type: 'agent_progress',
      agentId: 'child-1',
      prompt: 'private prompt',
      message: {
        type: 'assistant',
        message: {
          content: [
            {
              type: 'tool_use',
              id: 'tool-1',
              name: 'Read',
              input: { file_path: 'src/a.ts' },
            },
          ],
        },
      },
    },
  } as unknown as Message
  const result = buildAgentProgressMessage(message) as unknown as Record<
    string,
    unknown
  >
  expect(result).toMatchObject({
    type: 'agent_progress',
    parent_tool_use_id: 'parent-1',
    agent_id: 'child-1',
    activities: [
      { kind: 'tool_use', id: 'tool-1', name: 'Read', hint: 'src/a.ts' },
    ],
  })
  expect(JSON.stringify(result)).not.toContain('private prompt')
})

test('extracts a bounded child tool result and ignores unrelated progress', () => {
  const message = {
    type: 'progress',
    uuid: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    parentToolUseID: 'parent-1',
    data: {
      type: 'agent_progress',
      message: {
        type: 'user',
        message: {
          content: [
            {
              type: 'tool_result',
              tool_use_id: 'tool-1',
              content: 'x'.repeat(2000),
              is_error: true,
            },
          ],
        },
      },
    },
  } as unknown as Message
  const result = buildAgentProgressMessage(message) as unknown as {
    activities: Array<{ output: string; failed: boolean }>
  }
  expect(result.activities[0]?.output).toHaveLength(1000)
  expect(result.activities[0]?.failed).toBe(true)
  expect(
    buildAgentProgressMessage({ ...message, data: { type: 'shell_progress' } }),
  ).toBeUndefined()
})
