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

/** Do not adopt another client's message when the available photos differ. */
function sameMessageAttachments(
  a: UserMessageEntry,
  b: UserMessageEntry,
): boolean {
  const aImages = a.images ?? []
  const bImages = b.images ?? []
  // Some older transcript readers return text without attachments. Preserve
  // that fallback only for a text-bearing message; image-only sends need
  // matching photos to establish identity.
  if ((aImages.length === 0 || bImages.length === 0) && a.content !== '') {
    return true
  }
  return (
    aImages.length === bImages.length &&
    aImages.every(
      (image, index) =>
        image.data === bImages[index]?.data &&
        image.mimeType === bImages[index]?.mimeType,
    )
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
 * exact-text matching is only a fallback within the current turn. A user
 * message is a hard boundary: the same answer can legitimately recur after
 * another question, regardless of how long it is.
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
    if (entry?.type === 'user_message') break
    if (entry?.type !== 'assistant_message') continue
    if (assistantProse(entry) === prose) return index
  }
  return -1
}

/**
 * Resolve a row that exists on both sides under the same stable id. History
 * may know more than the streamed snapshot (the tool_result that completed a
 * card, the rest of a reply) but it may also know less (a page cached before
 * the result landed, a snapshot taken mid-stream). Always keep the richer
 * copy so neither direction can downgrade what the person already sees.
 */
function preferRicherEntry(
  current: ThreadEntry,
  incoming: ThreadEntry,
): ThreadEntry {
  if (incoming.type === 'user_message' && current.type === 'user_message') {
    if (incoming.pending && !current.pending) {
      return { ...current, pending: true }
    }
    // A same-id non-optimistic copy is the terminal's confirmation of this
    // bubble. Swapping the whole entry (not just clearing `pending`) also
    // drops `optimistic`, so a confirmed bubble can never later claim another
    // identical send's echo, and keeps the stable render identity.
    if ((current.pending || current.optimistic) && !incoming.pending) {
      // `incoming` carries neither flag, so dropping the live copy's flags
      // here is what confirms the bubble and frees it from later adoption.
      return {
        ...incoming,
        ...(current.clientId ? { clientId: current.clientId } : {}),
        images: current.images ?? incoming.images,
      }
    }
    return current
  }
  if (incoming.type === 'tool_call' && current.type === 'tool_call') {
    const next = incoming.toolCall
    const prev = current.toolCall
    const upgrade =
      (next.status !== 'running' &&
        next.status !== 'unknown' &&
        (prev.status === 'running' || prev.status === 'unknown')) ||
      (next.rawOutput !== undefined && prev.rawOutput === undefined)
    return upgrade
      ? { type: 'tool_call', toolCall: { ...prev, ...next } }
      : current
  }
  if (
    incoming.type === 'assistant_message' &&
    current.type === 'assistant_message'
  ) {
    const textLength = (chunks: AssistantChunk[]) =>
      chunks.map(chunk => chunk.text).join('').length
    return incoming.chunks.length > current.chunks.length ||
      textLength(incoming.chunks) > textLength(current.chunks)
      ? incoming
      : current
  }
  return current
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
            existing.optimistic === true ||
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
          entry.type === 'user_message'
            ? {
                ...entry,
                ...(optimistic.clientId
                  ? { clientId: optimistic.clientId }
                  : {}),
                ...(optimistic.images ? { images: optimistic.images } : {}),
              }
            : entry
        indexByKey.set(entryKey(entry), pendingIndex)
        adoptableIds?.delete(optimistic.id)
        continue
      }
    }
    if (existingIndex !== undefined) {
      const existing = merged[existingIndex]
      // Same id: never downgrade what is already on screen (a running tool
      // card must keep its completed result, a streamed reply must keep its
      // full text). A resync after an SSE gap can deliver the full text in
      // the SAME chunk count, so content length is compared too.
      if (existing) {
        const preferred = preferRicherEntry(existing, entry)
        if (preferred !== existing) merged[existingIndex] = preferred
      }
      continue
    }
    indexByKey.set(key, merged.length)
    merged.push(entry)
  }
  return merged
}

/** A bubble sent locally whose echo has not arrived yet. */
export function isUnconfirmedUserEntry(entry: ThreadEntry): boolean {
  return (
    entry.type === 'user_message' &&
    (entry.pending === true ||
      entry.optimistic === true ||
      entry.id.startsWith('pending-'))
  )
}

/**
 * Append a live transcript entry (assistant chunk, tool card) after the
 * conversation but before any trailing unconfirmed prompt bubbles. A prompt
 * sent mid-turn sits in the terminal's queue until the turn drains it; the
 * reply to the *current* turn still belongs above it. Blindly appending put
 * the assistant's output below the queued bubble, permanently disagreeing
 * with the terminal and every other client.
 */
export function appendLiveEntry(
  entries: ThreadEntry[],
  entry: ThreadEntry,
): ThreadEntry[] {
  let index = entries.length
  while (index > 0 && isUnconfirmedUserEntry(entries[index - 1]!)) index -= 1
  if (index === entries.length) return [...entries, entry]
  return [...entries.slice(0, index), entry, ...entries.slice(index)]
}

/**
 * Merge a contiguous oldest-first transcript page into the on-screen list
 * without reordering either side. Rows older than the first shared anchor are
 * inserted before it; rows after the anchor are the live/newer tail and stay
 * at the end. `mergeThreadEntries` is insertion-only and must not be used for
 * this direction (it appends older history after newer messages).
 */
export interface MergeHistoryPageOptions {
  /**
   * Ids of optimistic bubbles this client created that were not echoed yet
   * (survives `clearStalePending`, unlike `pending`). The caller must pass a
   * fresh snapshot per invocation: a React state updater may run twice.
   */
  adoptableIds?: Set<string>
  /**
   * Where the page sits when the two sides share no id at all. `older` is the
   * historical assumption: an on-demand backfill racing a live tail, where
   * the page is the past and the on-screen rows are the newest events.
   * `newer` is what a *newest-window* sync must pass: on-screen rows the page
   * does not contain have simply aged out of the window and belong above it.
   */
  pagePosition?: 'older' | 'newer'
}

export function mergeHistoryPage(
  onScreen: ThreadEntry[],
  page: ThreadEntry[],
  options?: MergeHistoryPageOptions,
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
  // Shared ids are resolved to the richer copy, never the page copy alone:
  // the cache can be missing a tool result or a mid-stream snapshot can be
  // shorter than what already streamed.
  const liveByKey = new Map(onScreen.map(entry => [entryKey(entry), entry]))
  // Page rows that are the reply to an adopted echo under the history uuid:
  // the live copy already renders it, so emitting both shows the answer twice.
  const skipPageRows = new Set<number>()
  const base: ThreadEntry[] = []
  for (let screenIndex = 0; screenIndex < onScreen.length; screenIndex += 1) {
    const entry = onScreen[screenIndex]!
    if (
      entry.type !== 'user_message' ||
      !(
        isUnconfirmedUserEntry(entry) ||
        options?.adoptableIds?.has(entry.id) === true
      )
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
    // Adopts the echo at `pageIndex`. The turn it started may already be on
    // screen under a different uuid (resync after a replay); that reply row
    // is then the same answer, not a second one.
    const adoptReplyTwin = (pageIndex: number): void => {
      const pageReply = page[pageIndex + 1]
      const liveReply = onScreen[screenIndex + 1]
      const prose = pageReply ? assistantProse(pageReply) : ''
      if (prose !== '' && liveReply && prose === assistantProse(liveReply)) {
        skipPageRows.add(pageIndex + 1)
      }
    }
    const exactIndex = page.findIndex(row => entryKey(row) === entryKey(entry))
    if (exactIndex >= 0) {
      const echo = page[exactIndex]
      if (echo?.type === 'user_message') {
        enhancedPage.set(exactIndex, {
          ...echo,
          ...(entry.clientId ? { clientId: entry.clientId } : {}),
          ...(entry.images ? { images: entry.images } : {}),
        })
      }
      if (echo) base.push(echo)
      usedEcho.add(exactIndex)
      options?.adoptableIds?.delete(entry.id)
      adoptReplyTwin(exactIndex)
      continue
    }
    let echoIndex = -1
    for (let i = page.length - 1; i >= 0; i--) {
      const candidate = page[i]
      if (
        !usedEcho.has(i) &&
        candidate?.type === 'user_message' &&
        candidate.content === entry.content &&
        // Carries no text at all ("", photo only): another device's photo
        // must not be claimed as this send's echo.
        sameMessageAttachments(entry, candidate) &&
        !onScreenKeys.has(entryKey(candidate))
      ) {
        echoIndex = i
        break
      }
    }
    if (echoIndex >= 0) {
      usedEcho.add(echoIndex)
      const echo = page[echoIndex]
      // Keep the optimistic attachments and render identity: history carries
      // the text but not the photos the browser sent with it — same rule as
      // mergeThreadEntries.
      if (echo && echo.type === 'user_message') {
        enhancedPage.set(echoIndex, {
          ...echo,
          ...(entry.clientId ? { clientId: entry.clientId } : {}),
          ...(entry.images ? { images: entry.images } : {}),
        })
      }
      options?.adoptableIds?.delete(entry.id)
      if (echo) base.push(echo)
      adoptReplyTwin(echoIndex)
    }
    // An older row with the same text is not evidence that this queued
    // message was delivered. Keep it until a distinct echo appears.
    else base.push(entry)
  }
  if (base.length === 0) return page

  const pageHasUserTurns = page.some(entry => entry.type === 'user_message')
  const pageRows = page
    .map((row, index) =>
      skipPageRows.has(index) ? null : (enhancedPage.get(index) ?? row),
    )
    .filter((row): row is ThreadEntry => row !== null)
    .map(row => {
      const live = liveByKey.get(entryKey(row))
      return live ? preferRicherEntry(live, row) : row
    })
    // A page row under a fresh uuid for a long reply already on screen is the
    // same turn replayed (server restart / terminal resume), not a new one.
    .filter(row => {
      if (row.type !== 'assistant_message' || liveByKey.has(entryKey(row))) {
        return true
      }
      // When a page includes user turns, identical prose can belong to a
      // different question. Only the anchored adoption above can prove that
      // such a reply is the same turn.
      if (pageHasUserTurns) return true
      return findAssistantReplayTwin(base, row) < 0
    })
  const anchor = base.findIndex(entry => pageKeys.has(entryKey(entry)))
  if (anchor < 0) {
    if (options?.pagePosition === 'newer') {
      // The page is the newest window: on-screen rows it no longer contains
      // have aged out and stay above it. Prepending the page here moved the
      // older conversation below the newest messages.
      const liveKeys = new Set(base.map(entryKey))
      return [
        ...base,
        ...pageRows.filter(entry => !liveKeys.has(entryKey(entry))),
      ]
    }
    // Disjoint backfill: the on-screen rows are the newer tail.
    return [
      ...pageRows.filter(
        entry => !onScreen.some(e => entryKey(e) === entryKey(entry)),
      ),
      ...base,
    ]
  }
  // Keep live-only entries between their surrounding shared rows. Moving all
  // of them to the tail puts a streamed message after a later history row.
  const beforeAnchor = new Map<string, ThreadEntry[]>()
  const olderOnly = base.slice(0, anchor)
  let between: ThreadEntry[] = []
  for (const entry of base.slice(anchor)) {
    const key = entryKey(entry)
    if (pageKeys.has(key)) {
      if (between.length > 0) {
        beforeAnchor.set(key, [...(beforeAnchor.get(key) ?? []), ...between])
        between = []
      }
    } else {
      between.push(entry)
    }
  }
  return [
    ...olderOnly,
    ...pageRows.flatMap(row => [
      ...(beforeAnchor.get(entryKey(row)) ?? []),
      row,
    ]),
    ...between,
  ]
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
            ...(message.pending ? { pending: true } : {}),
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
            ...(message.pending ? { pending: true } : {}),
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
