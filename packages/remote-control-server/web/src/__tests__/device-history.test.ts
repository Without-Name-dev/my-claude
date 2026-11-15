import { describe, expect, test } from 'bun:test'
import {
  appendLiveEntry,
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

  test('does not claim an agent finished when its result is absent from the history page', () => {
    const entries = transcriptToEntries([
      message('a1', 'assistant', [
        {
          type: 'tool_use',
          id: 'agent-1',
          name: 'Agent',
          input: { description: '检查前端' },
        },
      ]),
    ])
    expect(entries[0]).toMatchObject({
      type: 'tool_call',
      toolCall: { status: 'unknown' },
    })
    const live: ThreadEntry[] = [
      {
        type: 'tool_call',
        toolCall: { id: 'agent-1', title: 'Agent', status: 'running' },
      },
    ]
    expect(mergeThreadEntries(live, entries)[0]).toMatchObject({
      toolCall: { status: 'running' },
    })
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

  test('does not adopt another device’s same-text message with a different photo', () => {
    const pending: ThreadEntry = {
      type: 'user_message',
      id: 'user-local',
      content: 'look',
      pending: true,
      images: [{ mimeType: 'image/png', data: 'first' }],
    }
    const other: ThreadEntry = {
      type: 'user_message',
      id: 'user-other',
      content: 'look',
      images: [{ mimeType: 'image/png', data: 'second' }],
    }
    expect(mergeThreadEntries([pending], [other])).toHaveLength(2)
    expect(mergeHistoryPage([pending], [other])).toHaveLength(2)
  })

  test('adopts an un-echoed optimistic bubble after its pending flag was cleared', () => {
    // A missed echo plus a live assistant chunk clears `pending`, so the
    // resync replay must still recognize the bubble as ours and keep it in
    // place instead of appending the user message after the reply.
    const adoptable = new Set(['user-client-uuid'])
    const merged = mergeThreadEntries(
      [
        { type: 'user_message', id: 'user-client-uuid', content: 'hello' },
        {
          type: 'assistant_message',
          id: 'assistant-a1',
          chunks: [{ type: 'message', text: 'answer' }],
        },
      ],
      [
        { type: 'user_message', id: 'user-server-uuid', content: 'hello' },
        {
          type: 'assistant_message',
          id: 'assistant-a1',
          chunks: [{ type: 'message', text: 'answer' }],
        },
      ],
      adoptable,
    )
    expect(
      merged.map(entry =>
        entry.type === 'user_message' ? entry.id : entry.type,
      ),
    ).toEqual(['user-server-uuid', 'assistant_message'])
    expect(adoptable.size).toBe(0)
  })

  test('same-text bubbles that this client did not create are kept', () => {
    const merged = mergeThreadEntries(
      [{ type: 'user_message', id: 'user-someone-else', content: 'hello' }],
      [{ type: 'user_message', id: 'user-new', content: 'hello' }],
      new Set(),
    )
    expect(merged).toHaveLength(2)
  })

  test('an old matching history row does not consume a new queued message', () => {
    const old: ThreadEntry = {
      type: 'user_message',
      id: 'user-old',
      content: '继续',
    }
    const pending: ThreadEntry = {
      type: 'user_message',
      id: 'pending-new',
      content: '继续',
      pending: true,
    }
    expect(mergeThreadEntries([old, pending], [old])).toEqual([old, pending])
  })

  test('a matching id confirms a pending message without losing its image', () => {
    const images = [{ mimeType: 'image/png', data: 'photo' }]
    const pending: ThreadEntry = {
      type: 'user_message',
      id: 'user-1',
      content: '照片',
      images,
      pending: true,
    }
    const echo: ThreadEntry = {
      type: 'user_message',
      id: 'user-1',
      content: '照片',
    }
    expect(mergeThreadEntries([pending], [echo])).toEqual([{ ...echo, images }])
  })

  test('a reattach replay of a long turn under a fresh uuid is not duplicated', () => {
    const prose =
      '实锤了：两张截图是 22:35:00 和 22:35:46 通过 App 上传到电脑的，这就是照片变成路径引用的根本原因，真正的修复是让历史视图的发消息通道也支持图片直传。'
    const onScreen = transcriptToEntries([
      message('live-uuid', 'assistant', [{ type: 'text', text: prose }]),
    ])
    const replayed = transcriptToEntries([
      message('replay-uuid', 'assistant', [{ type: 'text', text: prose }]),
    ])
    expect(mergeThreadEntries(onScreen, replayed)).toHaveLength(1)
  })

  test('a short identical reply is still kept twice', () => {
    const onScreen = transcriptToEntries([
      message('live-uuid', 'assistant', [{ type: 'text', text: 'ok' }]),
    ])
    const replayed = transcriptToEntries([
      message('replay-uuid', 'assistant', [{ type: 'text', text: 'ok' }]),
    ])
    expect(mergeThreadEntries(onScreen, replayed)).toHaveLength(2)
  })

  test('identical long answers after different questions are both retained', () => {
    const prose = 'The same legitimate long reply. '.repeat(4)
    const first = transcriptToEntries([
      message('u1', 'user', [{ type: 'text', text: 'first question' }]),
      message('a1', 'assistant', [{ type: 'text', text: prose }]),
    ])
    const second = transcriptToEntries([
      message('u2', 'user', [{ type: 'text', text: 'second question' }]),
      message('a2', 'assistant', [{ type: 'text', text: prose }]),
    ])
    expect(
      mergeThreadEntries(first, second).filter(
        row => row.type === 'assistant_message',
      ),
    ).toHaveLength(2)
    expect(
      mergeHistoryPage(first, second, { pagePosition: 'newer' }).filter(
        row => row.type === 'assistant_message',
      ),
    ).toHaveLength(2)
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

  test('keeps live-only entries between their shared history neighbours', () => {
    const liveOnly: ThreadEntry = {
      type: 'assistant_message',
      id: 'assistant-live-between',
      chunks: [{ type: 'message', text: 'streamed in between' }],
    }
    const onScreen = [page[0]!, liveOnly, page[2]!]
    expect(
      mergeHistoryPage(onScreen, [page[0]!, page[2]!]).map(entryId),
    ).toEqual(onScreen.map(entryId))
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

  test('keeps a queued repeat until a new matching history row arrives', () => {
    const old: ThreadEntry = {
      type: 'user_message',
      id: 'user-old',
      content: '继续',
    }
    const pending: ThreadEntry = {
      type: 'user_message',
      id: 'pending-new',
      content: '继续',
      pending: true,
    }
    expect(mergeHistoryPage([old, pending], [old])).toEqual([old, pending])
    const echoed: ThreadEntry = {
      type: 'user_message',
      id: 'user-new',
      content: '继续',
    }
    expect(mergeHistoryPage([old, pending], [old, echoed])).toEqual([
      old,
      echoed,
    ])
  })

  test('only confirms one of two identical queued messages per new echo', () => {
    const old: ThreadEntry = {
      type: 'user_message',
      id: 'user-old',
      content: '继续',
    }
    const first: ThreadEntry = {
      type: 'user_message',
      id: 'pending-1',
      content: '继续',
      pending: true,
    }
    const second: ThreadEntry = {
      type: 'user_message',
      id: 'pending-2',
      content: '继续',
      pending: true,
    }
    const echoed: ThreadEntry = {
      type: 'user_message',
      id: 'user-new',
      content: '继续',
    }
    expect(mergeHistoryPage([old, first, second], [old, echoed])).toEqual([
      old,
      echoed,
      second,
    ])
  })

  test('does not downgrade a completed tool card with a page row missing its result', () => {
    const live: ThreadEntry[] = [
      {
        type: 'tool_call',
        toolCall: {
          id: 't1',
          title: 'Bash',
          status: 'complete',
          rawInput: { command: 'ls' },
          rawOutput: { output: 'file1' },
        },
      },
    ]
    const pageRows = transcriptToEntries([
      message('a1', 'assistant', [
        { type: 'tool_use', id: 't1', name: 'Bash', input: { command: 'ls' } },
      ]),
    ])
    const merged = mergeHistoryPage(live, pageRows)
    expect(merged).toHaveLength(1)
    expect(merged[0]).toMatchObject({
      type: 'tool_call',
      toolCall: { status: 'complete', rawOutput: { output: 'file1' } },
    })
  })

  test('a mid-stream page snapshot does not truncate the streamed reply', () => {
    const live: ThreadEntry[] = [
      {
        type: 'assistant_message',
        id: 'assistant-a1',
        chunks: [{ type: 'message', text: 'The full streamed answer.' }],
      },
    ]
    const pageRows = transcriptToEntries([
      message('a1', 'assistant', [{ type: 'text', text: 'The full' }]),
    ])
    const merged = mergeHistoryPage(live, pageRows)
    const first = merged[0]
    expect(first?.type === 'assistant_message' && first.chunks[0]?.text).toBe(
      'The full streamed answer.',
    )
  })

  test('a newest page with no shared id is appended after the older on-screen rows', () => {
    const older = transcriptToEntries([
      message('u1', 'user', [{ type: 'text', text: 'old question' }]),
      message('a1', 'assistant', [{ type: 'text', text: 'old answer' }]),
    ])
    // Backgrounded app: >30 rows were produced, so the newest window shares
    // no id with what is on screen.
    const newest = transcriptToEntries([
      message('u9', 'user', [{ type: 'text', text: 'new question' }]),
      message('a9', 'assistant', [{ type: 'text', text: 'new answer' }]),
    ])
    expect(
      mergeHistoryPage(older, newest, { pagePosition: 'newer' }).map(entryId),
    ).toEqual([...older.map(entryId), ...newest.map(entryId)])
  })

  test('adopts an un-echoed optimistic bubble after pending was cleared', () => {
    const onScreen: ThreadEntry[] = [
      {
        type: 'user_message',
        id: 'user-client',
        content: 'hello',
        optimistic: true,
      },
      {
        type: 'assistant_message',
        id: 'assistant-live',
        chunks: [{ type: 'message', text: 'answer' }],
      },
    ]
    const pageRows = transcriptToEntries([
      message('server-uuid', 'user', [{ type: 'text', text: 'hello' }]),
      message('history-a1', 'assistant', [{ type: 'text', text: 'answer' }]),
    ])
    const merged = mergeHistoryPage(onScreen, pageRows, {
      pagePosition: 'newer',
    })
    // One bubble (the echo adopted in place), one reply — not the replayed
    // pair appended below the original.
    expect(
      merged.map(entry =>
        entry.type === 'user_message' ? `user:${entry.content}` : entry.type,
      ),
    ).toEqual(['user:hello', 'assistant_message'])
  })

  test('an image-only queued bubble is not confirmed by another photo', () => {
    const onScreen: ThreadEntry[] = [
      {
        type: 'user_message',
        id: 'pending-1',
        content: '',
        images: [{ mimeType: 'image/png', data: 'MINE' }],
        pending: true,
      },
    ]
    const pageRows = transcriptToEntries([
      message('other-device', 'user', [
        { type: 'image', mimeType: 'image/png', data: 'OTHER' },
      ]),
    ])
    const merged = mergeHistoryPage(onScreen, pageRows)
    const queued = merged.find(
      entry => entry.type === 'user_message' && entry.id === 'pending-1',
    )
    expect(queued).toMatchObject({ pending: true })
    expect(
      merged.some(
        entry =>
          entry.type === 'user_message' && entry.id === 'user-other-device',
      ),
    ).toBe(true)
  })

  test('a replayed assistant turn under a fresh uuid is not duplicated by a sync', () => {
    const prose =
      '实锤了：历史同步时同一段长文本在重新挂接后以新的 uuid 再次到达，必须合并成一条回复而不是在聊天记录里显示两遍，否则用户会以为自己问了两遍或者终端执行了两次。'
    const onScreen = transcriptToEntries([
      message('live-uuid', 'assistant', [{ type: 'text', text: prose }]),
    ])
    const pageRows = transcriptToEntries([
      message('history-uuid', 'assistant', [{ type: 'text', text: prose }]),
    ])
    expect(
      mergeHistoryPage(onScreen, pageRows, { pagePosition: 'newer' }),
    ).toHaveLength(1)
  })
})

describe('appendLiveEntry', () => {
  test('keeps a queued prompt at the tail while the running turn replies', () => {
    const queued: ThreadEntry = {
      type: 'user_message',
      id: 'user-q',
      content: 'queued',
      pending: true,
    }
    const output: ThreadEntry = {
      type: 'assistant_message',
      id: 'assistant-a',
      chunks: [{ type: 'message', text: 'working' }],
    }
    expect(appendLiveEntry([queued], output)).toEqual([output, queued])
  })

  test('appends normally when no prompt is queued', () => {
    const reply: ThreadEntry = {
      type: 'assistant_message',
      id: 'assistant-a',
      chunks: [{ type: 'message', text: 'hi' }],
    }
    const tool: ThreadEntry = {
      type: 'tool_call',
      toolCall: { id: 't1', title: 'Bash', status: 'running' },
    }
    expect(appendLiveEntry([reply], tool)).toEqual([reply, tool])
  })
})

describe('echo confirmation', () => {
  test('a same-id echo clears optimistic so it cannot claim another send', () => {
    const optimistic: ThreadEntry = {
      type: 'user_message',
      id: 'user-1',
      clientId: 'user-1',
      content: 'hello',
      optimistic: true,
      pending: true,
    }
    const echo: ThreadEntry = {
      type: 'user_message',
      id: 'user-1',
      content: 'hello',
    }
    const [merged] = mergeThreadEntries([optimistic], [echo])
    expect(merged).toEqual({ ...echo, clientId: 'user-1' })
    expect(
      merged && 'optimistic' in merged ? merged.optimistic : undefined,
    ).toBeUndefined()
  })

  test('mergeHistoryPage adopts an unconfirmed bubble via adoptableIds', () => {
    const onScreen: ThreadEntry[] = [
      {
        type: 'user_message',
        id: 'user-local',
        content: '继续',
        clientId: 'user-local',
      },
    ]
    const pageRows: ThreadEntry[] = [
      { type: 'user_message', id: 'user-server', content: '继续' },
    ]
    const merged = mergeHistoryPage(onScreen, pageRows, {
      pagePosition: 'newer',
      adoptableIds: new Set(['user-local']),
    })
    expect(merged).toEqual([
      {
        type: 'user_message',
        id: 'user-server',
        content: '继续',
        clientId: 'user-local',
      },
    ])
  })
})
