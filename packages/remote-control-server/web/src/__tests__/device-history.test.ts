import { describe, expect, test } from 'bun:test'
import {
  mergeHistoryPage,
  mergeThreadEntries,
  transcriptToEntries,
} from '../lib/device-history'
import type { ThreadEntry } from '../lib/types'
import type { TranscriptMessage } from '../api/devices'

function message(
  id: string,
  role: TranscriptMessage['role'],
  blocks: TranscriptMessage['blocks'],
): TranscriptMessage {
  const text = blocks
    .filter(block => block.type === 'text')
    .map(block => (block.type === 'text' ? block.text : ''))
    .join('')
  return { id, role, text, blocks }
}

describe('transcriptToEntries', () => {
  test('renders user and assistant text', () => {
    const entries = transcriptToEntries([
      message('u1', 'user', [{ type: 'text', text: 'hello' }]),
      message('a1', 'assistant', [{ type: 'text', text: 'hi' }]),
    ])
    expect(entries).toEqual([
      { type: 'user_message', id: 'user-u1', content: 'hello' },
      {
        type: 'assistant_message',
        id: 'assistant-a1',
        chunks: [{ type: 'message', text: 'hi' }],
      },
    ])
  })

  test('maps thinking and tool calls, and skips tool-result-only user turns', () => {
    const entries = transcriptToEntries([
      message('a1', 'assistant', [
        { type: 'thinking', text: 'reasoning' },
        { type: 'tool_use', id: 't1', name: 'Read', input: { path: 'x' } },
      ]),
      message('u2', 'user', [
        { type: 'tool_result', toolUseId: 't1', text: 'ok' },
      ]),
    ])
    expect(entries).toHaveLength(2)
    // Assistant text precedes its tool cards, matching the live renderer.
    expect(entries[0]).toMatchObject({
      type: 'assistant_message',
      chunks: [{ type: 'thought', text: 'reasoning' }],
    })
    expect(entries[1]).toMatchObject({
      type: 'tool_call',
      toolCall: {
        id: 't1',
        title: 'Read',
        status: 'complete',
        rawOutput: { output: 'ok' },
      },
    })
  })

  test('does not create empty user bubbles', () => {
    const entries = transcriptToEntries([
      message('u1', 'user', [{ type: 'image' }]),
    ])
    expect(entries).toEqual([])
  })
})

describe('mergeThreadEntries', () => {
  test('does not duplicate the same assistant message from two sources', () => {
    const history = transcriptToEntries([
      message('a1', 'assistant', [{ type: 'text', text: 'hi' }]),
    ])
    const live = [
      {
        type: 'assistant_message' as const,
        id: 'assistant-a1',
        chunks: [{ type: 'message' as const, text: 'hi' }],
      },
    ]
    const merged = mergeThreadEntries(history, live)
    expect(merged).toHaveLength(1)
    const first = merged[0]
    expect(first?.type).toBe('assistant_message')
    if (first?.type === 'assistant_message') {
      expect(first.id).toBe('assistant-a1')
    }
  })

  test('adopts an optimistic pending user bubble instead of adding a second', () => {
    const merged = mergeThreadEntries(
      [{ type: 'user_message', id: 'pending-1', content: 'hello' }],
      [{ type: 'user_message', id: 'user-u1', content: 'hello' }],
    )
    expect(merged).toEqual([
      { type: 'user_message', id: 'user-u1', content: 'hello' },
    ])
  })

  test('keeps a genuinely repeated message with a different id', () => {
    const merged = mergeThreadEntries(
      [{ type: 'user_message', id: 'user-u1', content: 'again' }],
      [{ type: 'user_message', id: 'user-u2', content: 'again' }],
    )
    expect(merged).toHaveLength(2)
  })
})

const entryId = (entry: ThreadEntry): string =>
  entry.type === 'tool_call' ? `tool:${entry.toolCall.id}` : entry.id

describe('mergeHistoryPage', () => {
  const page = transcriptToEntries(
    Array.from({ length: 100 }, (_, index) =>
      message(`m${index}`, index % 2 === 0 ? 'assistant' : 'user', [
        { type: 'text', text: `message ${index}` },
      ]),
    ),
  )

  test('places an older unseen page before the on-screen live tail', () => {
    const onScreen = page.slice(80)
    const merged = mergeHistoryPage(onScreen, page)
    expect(merged.map(entryId)).toEqual(page.map(entryId))
  })

  test('keeps a live entry that is newer than the page at the end', () => {
    const live = [
      ...page.slice(80),
      {
        type: 'assistant_message' as const,
        id: 'assistant-live',
        chunks: [{ type: 'message' as const, text: 'just now' }],
      },
    ]
    const merged = mergeHistoryPage(live, page)
    expect(merged[0] ? entryId(merged[0]) : undefined).toBe(
      page[0] ? entryId(page[0]) : undefined,
    )
    expect(merged.at(-1) ? entryId(merged.at(-1)!) : undefined).toBe(
      'assistant-live',
    )
    expect(merged).toHaveLength(101)
  })

  test('adopts an optimistic pending user bubble', () => {
    const merged = mergeHistoryPage(
      [{ type: 'user_message', id: 'pending-1', content: 'hello' }],
      [{ type: 'user_message', id: 'user-u1', content: 'hello' }],
    )
    expect(merged).toEqual([
      { type: 'user_message', id: 'user-u1', content: 'hello' },
    ])
  })
})
