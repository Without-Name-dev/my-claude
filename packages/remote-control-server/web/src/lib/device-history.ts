import type { AssistantChunk, ThreadEntry } from './types'
import type { TranscriptMessage } from '../api/devices'

/**
 * Convert a local transcript page into the same ThreadEntry shape the live
 * bridge chat uses, so reading history and chatting render identically.
 *
 * The transcript's tool_result-only user turns are not messages a human sent;
 * they are rendered as tool output and must not appear as empty bubbles.
 */
function entryKey(entry: ThreadEntry): string {
  if (entry.type === 'user_message') return `user:${entry.id}`
  if (entry.type === 'assistant_message') return `assistant:${entry.id}`
  if (entry.type === 'tool_call') return `tool:${entry.toolCall.id}`
  return `${entry.type}:${entry.id}`
}

/**
 * Merge two history sources (server-cached transcript + live bridge events)
 * without showing the same message twice. Stable ids are the primary key; a
 * pending optimistic user bubble is adopted by the echo with the same text.
 */
export function mergeThreadEntries(
  base: ThreadEntry[],
  extra: ThreadEntry[],
): ThreadEntry[] {
  const indexByKey = new Map(
    base.map((entry, index) => [entryKey(entry), index]),
  )
  const merged = [...base]
  for (const entry of extra) {
    if (entry.type === 'user_message') {
      const pendingIndex = merged.findIndex(
        existing =>
          existing.type === 'user_message' &&
          existing.id.startsWith('pending-') &&
          existing.content === entry.content,
      )
      if (pendingIndex >= 0) {
        merged[pendingIndex] = entry
        indexByKey.set(entryKey(entry), pendingIndex)
        continue
      }
    }
    const key = entryKey(entry)
    const existingIndex = indexByKey.get(key)
    if (existingIndex !== undefined) {
      const existing = merged[existingIndex]
      // Same id: history may know more than the streamed snapshot. Upgrade a
      // running tool card to its completed result instead of keeping it stuck.
      if (entry.type === 'tool_call' && existing?.type === 'tool_call') {
        const incoming = entry.toolCall
        const current = existing.toolCall
        if (
          (incoming.status !== 'running' && current.status === 'running') ||
          (incoming.rawOutput !== undefined && current.rawOutput === undefined)
        ) {
          merged[existingIndex] = {
            type: 'tool_call',
            toolCall: { ...current, ...incoming },
          }
        }
      } else if (
        entry.type === 'assistant_message' &&
        existing?.type === 'assistant_message' &&
        entry.chunks.length > existing.chunks.length
      ) {
        merged[existingIndex] = entry
      }
      continue
    }
    indexByKey.set(key, merged.length)
    merged.push(entry)
  }
  return merged
}

/**
 * Merge a contiguous oldest-first transcript page into the on-screen list
 * without reordering either side. Rows older than the first shared anchor are
 * inserted before it; rows after the anchor are the live/newer tail and stay
 * at the end. `mergeThreadEntries` is insertion-only and must not be used for
 * this direction (it appends older history after newer messages).
 */
export function mergeHistoryPage(
  onScreen: ThreadEntry[],
  page: ThreadEntry[],
): ThreadEntry[] {
  if (page.length === 0) return onScreen
  const pageKeys = new Set(page.map(entryKey))
  const base = onScreen.map(entry => {
    if (entry.type !== 'user_message' || !entry.id.startsWith('pending-')) {
      return entry
    }
    const echo = page.find(
      candidate =>
        candidate.type === 'user_message' &&
        candidate.content === entry.content,
    )
    return echo ?? entry
  })
  if (base.length === 0) return page

  const anchor = base.findIndex(entry => pageKeys.has(entryKey(entry)))
  if (anchor < 0) {
    // Disjoint: assume the on-screen rows are a newer live tail (they are
    // written to the store before broadcast, so a real overlap would exist).
    return [
      ...page.filter(
        entry => !onScreen.some(e => entryKey(e) === entryKey(entry)),
      ),
      ...base,
    ]
  }
  const olderOnly = base.slice(0, anchor)
  const newerOnly = base
    .slice(anchor)
    .filter(entry => !pageKeys.has(entryKey(entry)))
  return [...olderOnly, ...page, ...newerOnly]
}

export function transcriptToEntries(
  messages: TranscriptMessage[],
): ThreadEntry[] {
  const entries: ThreadEntry[] = []
  let toolIndex = 0

  for (const message of messages) {
    if (message.role === 'user') {
      // Tool results arrive as user turns; fold them into their tool card
      // instead of rendering an empty user bubble.
      for (const block of message.blocks) {
        if (block.type !== 'tool_result' || !block.toolUseId) continue
        const index = entries.findIndex(
          entry =>
            entry.type === 'tool_call' && entry.toolCall.id === block.toolUseId,
        )
        if (index < 0) continue
        const entry = entries[index]
        if (entry?.type !== 'tool_call') continue
        entries[index] = {
          type: 'tool_call',
          toolCall: {
            ...entry.toolCall,
            status: block.isError ? 'error' : 'complete',
            rawOutput: { output: block.text },
          },
        }
      }
      if (message.text.trim().length > 0) {
        // The REPL writes its own slash-command scaffolding into the
        // transcript: a caveat, a <command-name> echo, and stdout wrappers.
        // Show the command the person actually ran, not those tags.
        const commandName = message.text.match(
          /<command-name>([^<]*)<\/command-name>/,
        )
        if (commandName?.[1]?.trim()) {
          entries.push({
            type: 'user_message',
            id: `user-${message.id}`,
            content: commandName[1].trim(),
          })
        } else if (
          !message.isMeta &&
          // Injected machine notices are not something the person typed; a raw
          // <task-notification> block in a right-aligned bubble reads as a bug.
          !/^\s*<(task-notification|system-reminder)\b/i.test(message.text) &&
          !/<local-command-caveat>|<command-message>|<command-args>|<\/?local-command-std(out|err)>/.test(
            message.text,
          )
        ) {
          entries.push({
            type: 'user_message',
            id: `user-${message.id}`,
            content: message.text,
          })
        }
      }
      continue
    }

    const chunks: AssistantChunk[] = []
    const toolEntries: ThreadEntry[] = []
    for (const block of message.blocks) {
      if (block.type === 'text' && block.text.length > 0) {
        chunks.push({ type: 'message', text: block.text })
      } else if (block.type === 'thinking' && block.text.length > 0) {
        chunks.push({ type: 'thought', text: block.text })
      } else if (block.type === 'tool_use') {
        const input =
          block.input &&
          typeof block.input === 'object' &&
          !Array.isArray(block.input)
            ? (block.input as Record<string, unknown>)
            : undefined
        toolEntries.push({
          type: 'tool_call',
          toolCall: {
            id: block.id ?? `history-tool-${toolIndex++}`,
            title: block.name,
            status: 'complete',
            rawInput: input,
          },
        })
      }
    }
    // The live renderer shows the assistant bubble first, then its tool cards.
    // Keep history identical so a reload does not reorder the conversation.
    if (chunks.length > 0) {
      entries.push({
        type: 'assistant_message',
        id: `assistant-${message.id}`,
        chunks,
      })
    }
    entries.push(...toolEntries)
  }

  return entries
}

export function formatRelativeTime(
  timestamp: number | null | undefined,
): string {
  if (!timestamp) return ''
  const delta = Date.now() - timestamp
  if (delta < 60_000) return '刚刚'
  if (delta < 3_600_000) return `${Math.floor(delta / 60_000)} 分钟前`
  if (delta < 86_400_000) return `${Math.floor(delta / 3_600_000)} 小时前`
  if (delta < 30 * 86_400_000) return `${Math.floor(delta / 86_400_000)} 天前`
  return new Date(timestamp).toLocaleDateString()
}
