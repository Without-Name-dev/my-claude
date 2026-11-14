import type { SDKMessage } from '../entrypoints/agentSdkTypes.js'
import type { Message } from '../types/message.js'

type AgentActivity =
  | { kind: 'tool_use'; id: string; name: string; hint?: string }
  | { kind: 'tool_result'; id: string; output: string; failed: boolean }

/** Extract the visible child tool activity from the terminal's progress row. */
export function buildAgentProgressMessage(
  message: Message,
): SDKMessage | undefined {
  if (
    message.type !== 'progress' ||
    typeof message.parentToolUseID !== 'string'
  )
    return
  const data = message.data
  if (!data || typeof data !== 'object' || Array.isArray(data)) return
  const progress = data as Record<string, unknown>
  if (progress.type !== 'agent_progress') return
  const nested = progress.message
  if (!nested || typeof nested !== 'object' || Array.isArray(nested)) return
  const childMessage = (nested as Record<string, unknown>).message
  if (
    !childMessage ||
    typeof childMessage !== 'object' ||
    Array.isArray(childMessage)
  )
    return
  const blocks = (childMessage as Record<string, unknown>).content
  if (!Array.isArray(blocks)) return
  const activities = blocks.flatMap<AgentActivity>(value => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return []
    const block = value as Record<string, unknown>
    if (
      block.type === 'tool_use' &&
      typeof block.id === 'string' &&
      typeof block.name === 'string'
    ) {
      const input =
        block.input &&
        typeof block.input === 'object' &&
        !Array.isArray(block.input)
          ? (block.input as Record<string, unknown>)
          : {}
      const hint = [
        'description',
        'command',
        'file_path',
        'path',
        'pattern',
        'query',
        'url',
      ]
        .map(key => input[key])
        .find(value => typeof value === 'string')
      return [
        {
          kind: 'tool_use',
          id: block.id,
          name: block.name,
          hint: typeof hint === 'string' ? hint.slice(0, 500) : undefined,
        },
      ]
    }
    if (block.type === 'tool_result' && typeof block.tool_use_id === 'string') {
      const content =
        typeof block.content === 'string'
          ? block.content
          : Array.isArray(block.content)
            ? block.content
                .flatMap(item =>
                  item &&
                  typeof item === 'object' &&
                  typeof item.text === 'string'
                    ? [item.text]
                    : [],
                )
                .join('\n')
            : ''
      return [
        {
          kind: 'tool_result',
          id: block.tool_use_id,
          output: content.slice(0, 1000),
          failed: block.is_error === true,
        },
      ]
    }
    return []
  })
  if (activities.length === 0) return
  return {
    type: 'agent_progress',
    uuid: message.uuid,
    parent_tool_use_id: message.parentToolUseID,
    agent_id:
      typeof progress.agentId === 'string' ? progress.agentId : undefined,
    activities,
  } as unknown as SDKMessage
}
