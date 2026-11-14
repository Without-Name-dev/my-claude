import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  test,
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

  test('ignores synthetic user turns and assistant noise', () => {
    recordBridgeEvent(deviceId, localSessionId, 'user', {
      uuid: 'tool-result',
      isSynthetic: true,
      message: { content: [{ type: 'tool_result', content: 'ok' }] },
    })
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'empty',
      message: { content: [] },
    })
    expect(listStoredMessages(deviceId, localSessionId, 10).total).toBe(0)
  })

  test('a synthetic assistant echo is not stored (transcript backfill owns it)', () => {
    recordBridgeEvent(deviceId, localSessionId, 'assistant', {
      uuid: 'synthetic-echo',
      message: { model: '<synthetic>', content: 'No response requested.' },
    })
    expect(listStoredMessages(deviceId, localSessionId, 10).total).toBe(0)
  })
})
