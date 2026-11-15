import { randomUUID } from 'crypto'
import type { ContentBlockParam } from '@anthropic-ai/sdk/resources'
import type { SDKMessage } from '../entrypoints/agentSdkTypes.js'
import type { QueuedCommand } from '../types/textInputTypes.js'

const PREVIEW_CHARS = 80

export type TerminalQueueSnapshot = {
  count: number
  previews: string[]
}

function previewText(value: string | Array<ContentBlockParam>): string {
  const text =
    typeof value === 'string'
      ? value
      : value
          .filter(block => block.type === 'text')
          .map(block => (block as { text: string }).text)
          .join(' ')
  const flattened = text.replace(/\s+/g, ' ').trim()
  if (flattened.length <= PREVIEW_CHARS) return flattened
  return `${flattened.slice(0, PREVIEW_CHARS - 1)}…`
}

/**
 * Prompts typed at the terminal while a turn is running. They sit invisibly
 * in the local command queue until the turn ends, so a phone watching the
 * session sees nothing happening. The snapshot makes them visible remotely.
 *
 * Remote (bridge) prompts are excluded — the sending client already renders
 * its own pending bubble — as is system-injected input (meta/origin/agent
 * notifications): only human keyboard input is meaningfully "queued" to a
 * remote observer.
 */
export function buildTerminalQueueSnapshot(
  commands: readonly QueuedCommand[],
): TerminalQueueSnapshot {
  const previews: string[] = []
  for (const command of commands) {
    if (command.mode !== 'prompt') continue
    if (command.bridgeOrigin === true) continue
    if (command.isMeta) continue
    if (command.agentId !== undefined) continue
    if (command.origin !== undefined) continue
    previews.push(previewText(command.value))
  }
  return { count: previews.length, previews }
}

/** Dedup key: two snapshots with equal previews publish identical state. */
export function terminalQueueSnapshotKey(
  snapshot: TerminalQueueSnapshot,
): string {
  return JSON.stringify(snapshot.previews)
}

export function buildTerminalQueueMessage(
  snapshot: TerminalQueueSnapshot,
): SDKMessage {
  return {
    type: 'terminal_queue',
    uuid: randomUUID(),
    count: snapshot.count,
    previews: snapshot.previews,
  } as unknown as SDKMessage
}
