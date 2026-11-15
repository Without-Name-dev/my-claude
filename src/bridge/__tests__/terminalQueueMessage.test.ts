import { describe, expect, test } from 'bun:test'

import {
  buildTerminalQueueMessage,
  buildTerminalQueueSnapshot,
  terminalQueueSnapshotKey,
} from '../terminalQueueMessage.js'
import type { QueuedCommand } from '../../types/textInputTypes.js'

function prompt(
  value: QueuedCommand['value'],
  extra: Partial<QueuedCommand> = {},
): QueuedCommand {
  return { mode: 'prompt', value, ...extra }
}

describe('buildTerminalQueueSnapshot', () => {
  test('collects locally-typed prompts in queue order', () => {
    const snapshot = buildTerminalQueueSnapshot([
      prompt('first message'),
      prompt('second message'),
    ])
    expect(snapshot.count).toBe(2)
    expect(snapshot.previews).toEqual(['first message', 'second message'])
  })

  test('excludes remote, system-injected, and agent-scoped commands', () => {
    const snapshot = buildTerminalQueueSnapshot([
      prompt('from phone', { bridgeOrigin: true }),
      prompt('proactive tick', { isMeta: true }),
      prompt('channel xml', { origin: { kind: 'channel' } as never }),
      prompt('agent note', { agentId: 'agent-1' as never }),
      { mode: 'task-notification', value: 'notify' },
      prompt('kept'),
    ])
    expect(snapshot.previews).toEqual(['kept'])
    expect(snapshot.count).toBe(1)
  })

  test('truncates long previews and flattens whitespace', () => {
    const snapshot = buildTerminalQueueSnapshot([
      prompt(`${'word '.repeat(40)}\n\nnext   line`),
    ])
    expect(snapshot.previews).toHaveLength(1)
    expect(snapshot.previews[0]?.length).toBe(80)
    expect(snapshot.previews[0]?.endsWith('…')).toBe(true)
    expect(snapshot.previews[0]).not.toContain('\n')
  })

  test('extracts text blocks from content-block values', () => {
    const snapshot = buildTerminalQueueSnapshot([
      prompt([
        { type: 'text', text: 'hello ' },
        { type: 'text', text: 'blocks' },
      ]),
    ])
    expect(snapshot.previews).toEqual(['hello blocks'])
  })

  test('empty queue yields an empty snapshot', () => {
    expect(buildTerminalQueueSnapshot([])).toEqual({ count: 0, previews: [] })
  })
})

describe('terminalQueueSnapshotKey', () => {
  test('equal snapshots share a key, different ones do not', () => {
    const a = buildTerminalQueueSnapshot([prompt('a')])
    const b = buildTerminalQueueSnapshot([prompt('a')])
    const c = buildTerminalQueueSnapshot([prompt('a'), prompt('b')])
    expect(terminalQueueSnapshotKey(a)).toBe(terminalQueueSnapshotKey(b))
    expect(terminalQueueSnapshotKey(a)).not.toBe(terminalQueueSnapshotKey(c))
  })
})

describe('buildTerminalQueueMessage', () => {
  test('carries the snapshot with a fresh uuid', () => {
    const snapshot = buildTerminalQueueSnapshot([prompt('queued')])
    const message = buildTerminalQueueMessage(snapshot) as unknown as {
      type: string
      uuid: string
      count: number
      previews: string[]
    }
    expect(message.type).toBe('terminal_queue')
    expect(message.uuid).toBeTruthy()
    expect(message.count).toBe(1)
    expect(message.previews).toEqual(['queued'])
  })

  test('two builds produce distinct uuids', () => {
    const snapshot = buildTerminalQueueSnapshot([])
    const first = buildTerminalQueueMessage(snapshot) as unknown as {
      uuid: string
    }
    const second = buildTerminalQueueMessage(snapshot) as unknown as {
      uuid: string
    }
    expect(first.uuid).not.toBe(second.uuid)
  })
})
