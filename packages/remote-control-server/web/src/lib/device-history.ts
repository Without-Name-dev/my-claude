import type { AssistantChunk, ThreadEntry, UserMessageEntry } from './types'
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

/** Text-less (image-only) echoes may only adopt a bubble with the same photos. */
function sameMessageAttachments(
  a: UserMessageEntry,
  b: UserMessageEntry,
): boolean {
  if (a.content !== '' || b.content !== '') return true
  const aImages = a.images ?? []
  const bImages = b.images ?? []
  return (
    aImages.length === bImages.length &&
    aImages.every((image, index) => image.data === bImages[index]?.data)
  )
}

/**
 * Content signature of an assistant entry: its prose text with thoughts
 * stripped. Long signatures are unique per message in practice, which is what
 * the replay fallback below relies on.
 */
export function assistantProse(entry: ThreadEntry): string {
  if (entry.type !== 'assistant_message') return ''
  return entry.chunks
    .filter(chunk => chunk.type === 'message')
    .map(chunk => (chunk.type === 'message' ? chunk.text : ''))
    .join('')
    .trim()
}

/**
 * A bridge reattach (server restart, terminal resume) can replay transcript
 * entries under fresh uuids. Id-based dedupe then misses them and an entire
 * old turn appears again at the bottom of the transcript. For long messages
 * the prose itself is a reliable identity, so fall back to an exact-text
 * match. Short replies ("ok") stay id-only: identical short texts do occur
 * legitimately and must keep both bubbles.
 */
const REPLAY_TEXT_FALLBACK_MIN = 60

export function findAssistantReplayTwin(
  entries: ThreadEntry[],
  candidate: ThreadEntry,
): number {
  const prose = assistantProse(candidate)
  if (prose.length < REPLAY_TEXT_FALLBACK_MIN) return -1
  for (let index = entries.length - 1; index >= 0; index -= 1) {
    const entry = entries[index]
    if (entry?.type !== 'assistant_message') continue
    if (assistantProse(entry) === prose) return index
  }
  return -1
}

/**
 * Merge two history sources (server-cached transcript + live bridge events)
 * without showing the same message twice. Stable ids are the primary key; a
 * pending optimistic user bubble is adopted by the echo with the same text.
 *
 * `adoptableIds` carries the ids of optimistic bubbles this client created
 * that have NOT been echoed yet. A resync replay can arrive after the echo was
 * missed and `clearStalePending` already cleared the pending flag — without
 * this set the echo could not adopt its bubble, so it was appended at the
 * bottom, *after* the reply it had triggered (the user's message appeared
 * below the answer). The set is drained as each bubble is adopted.
 */
export function mergeThreadEntries(
  base: ThreadEntry[],
  extra: ThreadEntry[],
  adoptableIds?: Set<string>,
): ThreadEntry[] {
  const indexByKey = new Map(
    base.map((entry, index) => [entryKey(entry), index]),
  )
  const merged = [...base]
  for (const entry of extra) {
    if (
      entry.type === 'assistant_message' &&
      !indexByKey.has(entryKey(entry))
    ) {
      // Replay under fresh uuids: same long prose already on screen means
      // this is the same turn re-delivered, not a new reply.
      const twin = findAssistantReplayTwin(merged, entry)
      if (twin >= 0) {
        indexByKey.set(entryKey(entry), twin)
        continue
      }
    }
    const key = entryKey(entry)
    const existingIndex = indexByKey.get(key)
    if (entry.type === 'user_message' && existingIndex === undefined) {
      const pendingIndex = merged.findIndex(
        existing =>
          existing.type === 'user_message' &&
          (existing.pending === true ||
            existing.id.startsWith('pending-') ||
            adoptableIds?.has(existing.id) === true) &&
          existing.content === entry.content &&
          sameMessageAttachments(existing, entry),
      )
      if (pendingIndex >= 0) {
        // Keep the optimistic attachments: history carries the text but not
        // the photos the browser sent with it.
        const optimistic = merged[pendingIndex] as UserMessageEntry
        merged[pendingIndex] =
          optimistic.images && entry.type === 'user_message'
            ? { ...entry, images: optimistic.images }
            : entry
        indexByKey.set(entryKey(entry), pendingIndex)
        adoptableIds?.delete(optimistic.id)
        continue
      }
    }
    if (existingIndex !== undefined) {
      const existing = merged[existingIndex]
      // Same id: history may know more than the streamed snapshot. Upgrade a
      // running tool card to its completed result instead of keeping it stuck.
      if (entry.type === 'user_message' && existing?.type === 'user_message') {
        if (existing.pending && !entry.pending) {
          merged[existingIndex] = {
            ...entry,
            images: existing.images ?? entry.images,
          }
        }
      } else if (entry.type === 'tool_call' && existing?.type === 'tool_call') {
        const incoming = entry.toolCall
        const current = existing.toolCall
        if (
          (incoming.status !== 'running' &&
            incoming.status !== 'unknown' &&
            (current.status === 'running' || current.status === 'unknown')) ||
          (incoming.rawOutput !== undefined && current.rawOutput === undefined)
        ) {
          merged[existingIndex] = {
            type: 'tool_call',
            toolCall: { ...current, ...incoming },
          }
        }
      } else if (
        entry.type === 'assistant_message' &&
        existing?.type === 'assistant_message'
      ) {
        // History may know more than the streamed snapshot. A resync after an
        // SSE gap can deliver the full text in the SAME chunk count (the live
        // entry already had one message chunk), so compare content length
        // too, not just chunk count — otherwise a truncated reply sticks.
        const chunkText = (chunks: AssistantChunk[]) =>
          chunks.map(chunk => chunk.text).join('')
        const longer =
          entry.chunks.length > existing.chunks.length ||
          chunkText(entry.chunks).length > chunkText(existing.chunks).length
        if (longer) merged[existingIndex] = entry
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
  // Echoes satisfy pendings one-for-one: sending "继续" twice while the
  // terminal is busy must keep the second queued bubble on screen, while a
  // repeat of an OLD message still adopts its (older) echo correctly.
  const echoCount = new Map<string, number>()
  for (const row of page) {
    if (row.type === 'user_message') {
      echoCount.set(row.content, (echoCount.get(row.content) ?? 0) + 1)
    }
  }
  const usedEcho = new Set<number>()
  const pendingOrdinal = new Map<string, number>()
  // An adopted echo re-enters the output through the page rows below (the
  // anchored assembly keeps the page copy and filters base's twin away by
  // key), so the optimistic photos must ride on THAT copy or they vanish.
  const enhancedPage = new Map<number, ThreadEntry>()
  const onScreenKeys = new Set(onScreen.map(entryKey))
  const base: ThreadEntry[] = []
  for (const entry of onScreen) {
    if (
      entry.type !== 'user_message' ||
      !(entry.pending === true || entry.id.startsWith('pending-'))
    ) {
      base.push(entry)
      continue
    }
    const ordinal = pendingOrdinal.get(entry.content) ?? 0
    pendingOrdinal.set(entry.content, ordinal + 1)
    if (ordinal >= (echoCount.get(entry.content) ?? 0)) {
      // No echo in this page left for this bubble: still queued.
      base.push(entry)
      continue
    }
    // Adopt the NEWEST unclaimed row with this text that is not already on
    // screen under another id; otherwise the merge below would dedupe one of
    // the two copies away.
    const exactIndex = page.findIndex(row => entryKey(row) === entryKey(entry))
    if (exactIndex >= 0) {
      const echo = page[exactIndex]
      if (echo?.type === 'user_message' && entry.images) {
        enhancedPage.set(exactIndex, { ...echo, images: entry.images })
      }
      if (echo) base.push(echo)
      usedEcho.add(exactIndex)
      continue
    }
    let echoIndex = -1
    for (let i = page.length - 1; i >= 0; i--) {
      const candidate = page[i]
      if (
        !usedEcho.has(i) &&
        candidate?.type === 'user_message' &&
        candidate.content === entry.content &&
        !onScreenKeys.has(entryKey(candidate))
      ) {
        echoIndex = i
        break
      }
    }
    if (echoIndex >= 0) {
      usedEcho.add(echoIndex)
      const echo = page[echoIndex]
      // Keep the optimistic attachments: history carries the text but not the
      // photos the browser sent with it — same rule as mergeThreadEntries.
      if (echo && echo.type === 'user_message' && entry.images) {
        enhancedPage.set(echoIndex, { ...echo, images: entry.images })
      }
      if (echo) base.push(echo)
    }
    // An older row with the same text is not evidence that this queued
    // message was delivered. Keep it until a distinct echo appears.
    else base.push(entry)
  }
  if (base.length === 0) return page

  const pageRows = page.map((row, index) => enhancedPage.get(index) ?? row)
  const anchor = base.findIndex(entry => pageKeys.has(entryKey(entry)))
  if (anchor < 0) {
    // Disjoint: assume the on-screen rows are a newer live tail (they are
    // written to the store before broadcast, so a real overlap would exist).
    return [
      ...pageRows.filter(
        entry => !onScreen.some(e => entryKey(e) === entryKey(entry)),
      ),
      ...base,
    ]
  }
  const olderOnly = base.slice(0, anchor)
  const newerOnly = base
    .slice(anchor)
    .filter(entry => !pageKeys.has(entryKey(entry)))
  return [...olderOnly, ...pageRows, ...newerOnly]
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
      // The server fills the flat text from text blocks, but tolerate rows
      // where it is empty and the prose only lives in blocks — dropping them
      // silently made user messages vanish from the rendered history.
      const userText =
        message.text.trim().length > 0
          ? message.text
          : message.blocks
              .filter(block => block.type === 'text')
              .map(block => (block.type === 'text' ? block.text : ''))
              .join('\n')
              .trim()
      // Photos arrive as base64 image blocks; without collecting them the
      // thumbnails disappeared as soon as the page reloaded history.
      const images = message.blocks.flatMap(block =>
        block.type === 'image' && block.data
          ? [{ mimeType: block.mimeType ?? 'image/png', data: block.data }]
          : [],
      )
      if (userText.length > 0 || images.length > 0) {
        // The REPL writes its own slash-command scaffolding into the
        // transcript: a caveat, a <command-name> echo, and stdout wrappers.
        // Show the command the person actually ran, not those tags.
        const commandName = userText.match(
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
          !/^\s*<(task-notification|system-reminder)\b/i.test(userText) &&
          !/<local-command-caveat>|<command-message>|<command-args>|<\/?local-command-std(out|err)>/.test(
            userText,
          )
        ) {
          entries.push({
            type: 'user_message',
            id: `user-${message.id}`,
            content: userText,
            ...(images.length > 0 ? { images } : {}),
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
            // Include the row id: a per-page counter made "history-tool-0"
            // collide across pages, and the older page's card was deduped away.
            id: block.id ?? `history-tool-${message.id}-${toolIndex++}`,
            title: block.name,
            // A paged transcript may not include the matching tool result.
            // Do not claim it completed until the result is actually seen.
            status: 'unknown',
            rawInput: input,
          },
        })
      }
    }
    // Some producers put assistant prose only in the flat text field; without
    // this fallback those messages disappeared from history entirely.
    if (chunks.length === 0 && message.text.trim().length > 0) {
      chunks.push({ type: 'message', text: message.text })
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
