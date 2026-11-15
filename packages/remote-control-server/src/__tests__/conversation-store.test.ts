import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  test,
  spyOn,
} from 'bun:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { TranscriptMessage } from '../device/protocol'
import {
  listStoredMessages,
  recordBridgeEvent,
  resetConversationStoreForTest,
  saveTranscript,
  saveWebUserMessage,
} from '../device/conversation-store'

const deviceId = 'dev_test1'
const localSessionId = 'db094c06-6f2d-470d-aef1-dcce041193b7'
let dataDir = ''

beforeAll(() => {
  dataDir = ''
})

afterAll(async () => {
  resetConversationStoreForTest()
  if (dataDir) await rm(dataDir, { recursive: true, force: true })
})

beforeEach(async () => {
  // Each test gets its own database file: the store is a module singleton and
  // closing it does not delete the rows.
  resetConversationStoreForTest()
  if (dataDir) await rm(dataDir, { recursive: true, force: true })
  dataDir = await mkdtemp(join(tmpdir(), 'rcs-conversation-'))
  process.env.RCS_DATA_DIR = dataDir
})

function transcript(
  from: number,
  to: number,
  baseMs = Date.UTC(2026, 0, 1),
): TranscriptMessage[] {
  const messages: TranscriptMessage[] = []
  for (let index = from; index <= to; index += 1) {
    messages.push({
      id: `m${index}`,
      role: index % 2 === 0 ? 'assistant' : 'user',
      text: `message ${index}`,
      timestamp: new Date(baseMs + index * 1000).toISOString(),
      blocks: [{ type: 'text', text: `message ${index}` }],
    })
  }
  return messages
}

describe('conversation store', () => {
  test('replayed bridge messages do not move behind newer messages', () => {
    const now = spyOn(Date, 'now')
    try {
      now.mockReturnValue(1000)
      recordBridgeEvent(deviceId, localSessionId, 'assistant', {
        uuid: 'first',
        message: { content: 'first' },
      })
      now.mockReturnValue(2000)
      recordBridgeEvent(deviceId, localSessionId, 'user', {
        uuid: 'second',
        message: { content: 'second' },
      })
      now.mockReturnValue(3000)
      recordBridgeEvent(deviceId, localSessionId, 'assistant', {
        uuid: 'first',
        message: { content: 'first updated' },
      })
      expect(
        listStoredMessages(deviceId, localSessionId, 10).messages.map(
          m => m.id,
        ),
      ).toEqual(['first', 'second'])
    } finally {
      now.mockRestore()
    }
  })

  test('transcript ordering survives a later bridge replay', () => {
    const messages = transcript(0, 1)
    saveTranscript(deviceId, localSessionId, messages)
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'm0',
      message: { content: 'message 0' },
    })
    expect(
      listStoredMessages(deviceId, localSessionId, 10).messages.map(m => m.id),
    ).toEqual(['m0', 'm1'])
  })

  test('orders by message time even when older pages are backfilled later', () => {
    // Newest page first, then the older page (the order the UI asks for).
    saveTranscript(deviceId, localSessionId, transcript(20, 29))
    saveTranscript(deviceId, localSessionId, transcript(10, 19))

    const page = listStoredMessages(deviceId, localSessionId, 15)
    expect(page.total).toBe(20)
    expect(page.messages).toHaveLength(15)
    expect(page.messages[0]?.text).toBe('message 15')
    expect(page.messages.at(-1)?.text).toBe('message 29')
    expect(page.hasMore).toBe(true)

    const older = listStoredMessages(deviceId, localSessionId, 15, page.before)
    expect(older.messages[0]?.text).toBe('message 10')
    expect(older.messages.at(-1)?.text).toBe('message 14')
    expect(older.hasMore).toBe(false)
  })

  test('keeps missing timestamps in page order during backfill', () => {
    saveTranscript(deviceId, localSessionId, transcript(10, 19))
    const undated = transcript(0, 9).map(message => ({
      ...message,
      timestamp: undefined,
    }))
    saveTranscript(deviceId, localSessionId, undated)

    const page = listStoredMessages(deviceId, localSessionId, 20)
    expect(page.messages.map(message => message.text)).toEqual(
      Array.from({ length: 20 }, (_, index) => `message ${index}`),
    )
  })

  test('timestamp-less newest refresh stays after the cached rows', () => {
    saveTranscript(deviceId, localSessionId, transcript(0, 4))
    const undated = transcript(5, 9).map(message => ({
      ...message,
      timestamp: undefined,
    }))
    saveTranscript(deviceId, localSessionId, undated, 'newest')

    const page = listStoredMessages(deviceId, localSessionId, 20)
    expect(page.messages.map(message => message.text)).toEqual(
      Array.from({ length: 10 }, (_, index) => `message ${index}`),
    )
  })

  test('legacy bare-seq cursors page backwards instead of repeating the tail', () => {
    saveTranscript(deviceId, localSessionId, transcript(0, 9))
    // seq 1..10 for messages 0..9; seq < 6 is messages 0..4.
    const page = listStoredMessages(deviceId, localSessionId, 5, '6')
    expect(page.messages.map(message => message.text)).toEqual([
      'message 0',
      'message 1',
      'message 2',
      'message 3',
      'message 4',
    ])
    expect(page.hasMore).toBe(false)
  })

  test('legacy cursor pages retain assistant token usage', () => {
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'usage-older',
      message: {
        content: [{ type: 'text', text: 'hello' }],
        usage: {
          input_tokens: 7,
          output_tokens: 3,
          cache_read_input_tokens: 5,
        },
      },
    })
    saveWebUserMessage(deviceId, localSessionId, 'newer', 'next')
    const page = listStoredMessages(deviceId, localSessionId, 10, '2')
    expect(page.messages[0]?.usage).toMatchObject({
      inputTokens: 7,
      outputTokens: 3,
      cacheReadInputTokens: 5,
    })
  })

  test('dedupes by id and never downgrades richer content', () => {
    saveTranscript(deviceId, localSessionId, transcript(1, 1))
    saveTranscript(deviceId, localSessionId, transcript(1, 1))
    expect(listStoredMessages(deviceId, localSessionId, 10).total).toBe(1)

    const text = 'from bridge'
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'a1',
      message: {
        content: [
          { type: 'text', text },
          { type: 'tool_use', id: 'tool1', name: 'Bash', input: { cmd: 'ls' } },
        ],
      },
    })
    const page = listStoredMessages(deviceId, localSessionId, 10)
    const stored = page.messages.find(message => message.id === 'a1')
    expect(stored?.blocks).toHaveLength(2)
    expect(stored?.text).toBe(text)
  })

  test('web message and bridge echo share one row (same uuid)', () => {
    saveWebUserMessage(deviceId, localSessionId, 'outbox-1', '你好')
    expect(listStoredMessages(deviceId, localSessionId, 10).total).toBe(1)

    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'outbox-1',
      message: { content: '你好' },
    })
    const page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.total).toBe(1)
    expect(page.messages[0]?.id).toBe('outbox-1')
    expect(page.messages[0]?.text).toBe('你好')
  })

  test('web message with photos stores image blocks and survives the echo', () => {
    saveWebUserMessage(deviceId, localSessionId, 'outbox-2', '看图', [
      { mimeType: 'image/png', data: 'aGk=' },
    ])
    let page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.total).toBe(1)
    expect(page.messages[0]?.blocks).toEqual([
      { type: 'text', text: '看图' },
      { type: 'image', mimeType: 'image/png', data: 'aGk=' },
    ])

    // The CLI echo reuses the outbox uuid; the upsert must not strip the
    // photos even when the echo's own content is text-only.
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'outbox-2',
      message: { content: '看图' },
    })
    page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.total).toBe(1)
    expect(page.messages[0]?.blocks).toHaveLength(2)
  })

  test('image-only web message is stored without an empty text block', () => {
    saveWebUserMessage(deviceId, localSessionId, 'outbox-3', '', [
      { mimeType: 'image/jpeg', data: 'aGk=' },
    ])
    const page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.messages[0]?.blocks).toEqual([
      { type: 'image', mimeType: 'image/jpeg', data: 'aGk=' },
    ])
  })

  test('image-only web message keeps its photo through an empty-text echo', () => {
    saveWebUserMessage(deviceId, localSessionId, 'outbox-4', '', [
      { mimeType: 'image/png', data: 'aGk=' },
    ])
    // The CLI echo of an image-only turn carries no text. The echo's block
    // list is shorter (empty), so the upsert must keep the web row's photo.
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'outbox-4',
      message: { content: '' },
    })
    const page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.total).toBe(1)
    expect(page.messages[0]?.blocks).toEqual([
      { type: 'image', mimeType: 'image/png', data: 'aGk=' },
    ])
  })

  test('image-only web message keeps its photo through a text-only echo', () => {
    saveWebUserMessage(deviceId, localSessionId, 'outbox-5', '', [
      { mimeType: 'image/jpeg', data: 'aGk=' },
    ])
    // A one-block text echo is still shorter than the stored image block:
    // the photo must survive even when the echo adds text the web row lacked.
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'outbox-5',
      message: { content: '看图' },
    })
    const page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.total).toBe(1)
    expect(page.messages[0]?.blocks).toEqual([
      { type: 'image', mimeType: 'image/jpeg', data: 'aGk=' },
    ])
  })

  test('a long text-only echo cannot erase a previously saved photo', () => {
    saveWebUserMessage(deviceId, localSessionId, 'outbox-long', '', [
      { mimeType: 'image/png', data: 'aGk=' },
    ])
    const text = 'long text '.repeat(100)
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'outbox-long',
      message: { content: text },
    })
    const row = listStoredMessages(deviceId, localSessionId, 10).messages[0]
    expect(row?.text).toBe(text)
    expect(row?.blocks.some(block => block.type === 'image')).toBe(true)
  })

  test('image-only web message survives a full image-block echo (same uuid)', () => {
    saveWebUserMessage(deviceId, localSessionId, 'outbox-6', '', [
      { mimeType: 'image/png', data: 'aGk=' },
    ])
    // The realistic CLI echo re-sends the same image block. Equal length
    // means the stored row is kept as-is — no duplicate, no loss.
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'outbox-6',
      message: {
        content: [
          {
            type: 'image',
            source: { type: 'base64', media_type: 'image/png', data: 'aGk=' },
          },
        ],
      },
    })
    const page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.total).toBe(1)
    expect(page.messages[0]?.blocks).toEqual([
      { type: 'image', mimeType: 'image/png', data: 'aGk=' },
    ])
  })

  test('stores synthetic tool-result turns but ignores other synthetic noise', () => {
    // Tool results are the durable record of a completed tool call.
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'tool-result',
      isSynthetic: true,
      message: { content: [{ type: 'tool_result', content: 'ok' }] },
    })
    // Ordinary synthetic prompts stay out of the transcript.
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'synthetic-chatty',
      isSynthetic: true,
      message: { content: 'No response requested.' },
    })
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'empty',
      message: { content: [] },
    })
    const page = listStoredMessages(deviceId, localSessionId, 10)
    expect(page.total).toBe(1)
    expect(page.messages[0]?.id).toBe('tool-result')
  })

  test('a synthetic assistant echo is not stored (transcript backfill owns it)', () => {
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'synthetic-echo',
      message: { model: '<synthetic>', content: 'No response requested.' },
    })
    expect(listStoredMessages(deviceId, localSessionId, 10).total).toBe(0)
  })
})

describe('tool results', () => {
  test('a synthetic tool-result turn is stored and folds into its card on reload', () => {
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'a-tool',
      message: {
        content: [
          {
            type: 'tool_use',
            id: 'tool-x',
            name: 'Bash',
            input: { cmd: 'ls' },
          },
        ],
      },
    })
    // Tool results arrive as synthetic user turns with no text of their own.
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'u-tool',
      isSynthetic: true,
      message: {
        content: [
          {
            type: 'tool_result',
            tool_use_id: 'tool-x',
            content: 'file1\nfile2',
          },
        ],
      },
    })
    const page = listStoredMessages(deviceId, localSessionId, 10)
    const row = page.messages.find(message => message.id === 'u-tool')
    expect(row?.blocks).toEqual([
      { type: 'tool_result', toolUseId: 'tool-x', text: 'file1\nfile2' },
    ])
    // No text and no photos: never renders as an empty user bubble.
    expect(row?.text).toBe('')
  })

  test('other synthetic user turns are still dropped', () => {
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'synthetic-1',
      isSynthetic: true,
      message: { content: 'No response requested.' },
    })
    expect(
      listStoredMessages(deviceId, localSessionId, 10).messages.some(
        message => message.id === 'synthetic-1',
      ),
    ).toBe(false)
  })
})

describe('conversation ordering', () => {
  test('live rows keep receive order even when the wall clock steps back', () => {
    const now = spyOn(Date, 'now')
    try {
      now.mockReturnValue(5000)
      recordBridgeEvent(deviceId, localSessionId, 'assistant', {
        uuid: 'a1',
        message: { content: 'first' },
      })
      now.mockReturnValue(3000)
      recordBridgeEvent(deviceId, localSessionId, 'assistant', {
        uuid: 'a2',
        message: { content: 'second' },
      })
      expect(
        listStoredMessages(deviceId, localSessionId, 10).messages.map(
          m => m.id,
        ),
      ).toEqual(['a1', 'a2'])
    } finally {
      now.mockRestore()
    }
  })

  test('a transcript refresh never relocates an already-stored row', () => {
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'a1',
      message: { content: 'first' },
    })
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'a2',
      message: { content: 'second' },
    })
    // The device clock is far ahead; re-reading the same row must not move it
    // past the row that arrived after it.
    saveTranscript(deviceId, localSessionId, [
      {
        id: 'a1',
        role: 'assistant',
        text: 'first',
        timestamp: new Date(Date.now() + 86_400_000).toISOString(),
        blocks: [{ type: 'text', text: 'first' }],
      },
    ])
    expect(
      listStoredMessages(deviceId, localSessionId, 10).messages.map(m => m.id),
    ).toEqual(['a1', 'a2'])
  })

  test('a leading undated prompt sorts before its already-stored reply', () => {
    const replyTime = Date.UTC(2026, 0, 1, 12, 0, 0)
    const reply: TranscriptMessage = {
      id: 'm2',
      role: 'assistant',
      text: 'reply',
      timestamp: new Date(replyTime).toISOString(),
      blocks: [{ type: 'text', text: 'reply' }],
    }
    saveTranscript(deviceId, localSessionId, [reply], 'newest')
    saveTranscript(
      deviceId,
      localSessionId,
      [
        {
          id: 'm1',
          role: 'user',
          text: 'prompt',
          blocks: [{ type: 'text', text: 'prompt' }],
        },
        reply,
      ],
      'older',
    )
    expect(
      listStoredMessages(deviceId, localSessionId, 10).messages.map(m => m.id),
    ).toEqual(['m1', 'm2'])
  })
})
