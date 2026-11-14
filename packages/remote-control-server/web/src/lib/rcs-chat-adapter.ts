import type { SetStateAction } from 'react'
import type { TranscriptUsage } from '../api/devices'
import {
  apiFetchSession,
  apiFetchSessionHistory,
  apiBind,
  apiSendEvent,
  apiSendControl,
  apiInterrupt,
  getUuid,
} from '../api/client'
import { generateMessageUuid } from './utils'
import { findAssistantReplayTwin, mergeThreadEntries } from './device-history'
import { debugLog } from './debug-log'
import {
  applyAgentProgress,
  parseTaskSnapshot,
  parseWorkflowSnapshot,
  type ExecutionTask,
  type WorkflowRun,
} from './execution-state'
import type { SessionEvent, EventPayload } from '../types'
import type {
  ThreadEntry,
  ToolCallData,
  ToolCallStatus,
  UserMessageEntry,
  AssistantMessageEntry,
  ToolCallEntry,
  UserMessageImage,
  PendingPermission,
  PermissionOption,
} from './types'

// SSE Event Bus — connection management for the bridge event stream.
type SSEEventHandler = (event: SessionEvent) => void

/** Reject after `ms` so a stalled request cannot freeze the chat forever. */
function withTimeout<T>(
  promise: Promise<T>,
  ms: number,
  message: string,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(message)), ms)
    promise.then(
      value => {
        clearTimeout(timer)
        resolve(value)
      },
      error => {
        clearTimeout(timer)
        reject(error)
      },
    )
  })
}

/**
 * Cheap text probe for the bridge's connect banner. Stringifying every event
 * (including multi-KB payloads) just to run one regex was measurable on a
 * history replay.
 */
function eventText(payload: EventPayload): string {
  const record = payload as Record<string, unknown>
  const parts: string[] = []
  for (const candidate of [record.content, record.message, record.raw]) {
    if (typeof candidate === 'string') {
      parts.push(candidate)
    } else if (Array.isArray(candidate)) {
      for (const item of candidate) {
        if (typeof item === 'string') parts.push(item)
        else if (
          item &&
          typeof item === 'object' &&
          typeof (item as Record<string, unknown>).text === 'string'
        ) {
          parts.push(String((item as Record<string, unknown>).text))
        }
      }
    } else if (candidate && typeof candidate === 'object') {
      const value = candidate as Record<string, unknown>
      if (typeof value.content === 'string') parts.push(value.content)
      else if (typeof value.text === 'string') parts.push(value.text)
    }
  }
  return parts.join('\n')
}

/**
 * The REPL emits its own bookkeeping as user messages when a slash command
 * runs: a caveat, the <command-name> echo, and the stdout/stderr body. They
 * are not human messages and must not become bubbles. Returns null for
 * scaffolding to drop, or { output } when the payload is command output.
 */
function classifyLocalCommandText(
  content: string,
  synthetic: boolean,
):
  | { kind: 'drop' }
  | { kind: 'text'; text: string }
  | { kind: 'output'; text: string } {
  const stdout = content.match(
    /<local-command-std(out|err)>([\s\S]*?)<\/local-command-std(out|err)>/,
  )
  if (stdout) {
    const text = (stdout[2] || '').trim()
    return text ? { kind: 'output', text } : { kind: 'drop' }
  }
  // Show the command the person actually ran, not the REPL's scaffolding —
  // same rule as the transcript path (device-history), so a reload does not
  // turn a dropped row into a bubble or vice versa.
  const commandName = content.match(/<command-name>([^<]*)<\/command-name>/)
  if (commandName?.[1]?.trim()) {
    return { kind: 'text', text: commandName[1].trim() }
  }
  if (
    synthetic ||
    /^\s*<(task-notification|system-reminder)\b/i.test(content) ||
    /<local-command-caveat>|<command-name>|<command-message>|<command-args>/.test(
      content,
    )
  ) {
    return { kind: 'drop' }
  }
  // Keep genuine prose verbatim — the transcript path does not strip tags
  // either, and a live-stripped copy made the same message differ from its
  // history twin (broke echo adoption, duplicated rows after resync).
  const text = content.trim()
  return text ? { kind: 'text', text } : { kind: 'drop' }
}

/** CLI permission options, normalised for the UI buttons. */
function permissionOptions(req: Record<string, unknown>): PermissionOption[] {
  const raw = Array.isArray(req.options)
    ? (req.options as Array<Record<string, unknown>>)
    : []
  return raw
    .filter(o => o && typeof o === 'object')
    .map(o => ({
      optionId: String(o.optionId ?? o.option_id ?? o.id ?? ''),
      name: String(o.name ?? o.label ?? ''),
      kind: (typeof o.kind === 'string'
        ? o.kind
        : 'allow_once') as PermissionOption['kind'],
    }))
    .filter(o => o.optionId)
}

/**
 * Extract the request a resolution event refers to. Three wire shapes end a
 * permission request: the CLI's `control_response` (payload.response.request_id),
 * the web's own `permission_response` answer (payload.request_id), and the CLI's
 * `control_cancel_request` when the terminal resolved it locally
 * (payload.request_id). Missing any of them makes a reload replay the old
 * control_request and pop the already-answered card again.
 */
function resolvedRequestId(payload: EventPayload): string | undefined {
  const root = payload as unknown as Record<string, unknown>
  const record =
    root.raw && typeof root.raw === 'object' && !Array.isArray(root.raw)
      ? (root.raw as Record<string, unknown>)
      : root
  if (typeof record.request_id === 'string' && record.request_id) {
    return record.request_id
  }
  const response = record.response as Record<string, unknown> | undefined
  if (
    response &&
    typeof response.request_id === 'string' &&
    response.request_id
  ) {
    return response.request_id
  }
  return undefined
}

/**
 * Reconnect cursor, persisted per session. EventSource sends Last-Event-ID on
 * its own retries, but a page reload starts a fresh source with none — which
 * used to replay the whole in-memory bus (up to thousands of events) on every
 * refresh, on top of the history the device view had already loaded.
 */
function cursorStorageKey(sessionId: string): string {
  return `rcs.seq.${sessionId}`
}

function readStoredCursor(sessionId: string): number {
  try {
    const raw = sessionStorage.getItem(cursorStorageKey(sessionId))
    const value = raw ? Number(raw) : 0
    return Number.isFinite(value) && value > 0 ? value : 0
  } catch {
    return 0
  }
}

function storeCursor(sessionId: string, seqNum: number): void {
  try {
    if (seqNum > 0)
      sessionStorage.setItem(cursorStorageKey(sessionId), String(seqNum))
    else sessionStorage.removeItem(cursorStorageKey(sessionId))
  } catch {
    // Private mode: the in-memory cursor still covers this page's lifetime.
  }
}

class SSEBus {
  private listeners: Set<SSEEventHandler> = new Set()
  private eventSource: EventSource | null = null
  private onError: ((message: string) => void) | null = null
  private onOpen: (() => void) | null = null
  private lastProbe = 0
  /** Server sequence cursor: a reconnect may replay events we already have. */
  private lastSeqNum = 0
  private seqSessionId = ''
  /** The server admitted it cannot replay the gap; the transcript is stale. */
  private onResync: (() => void) | null = null
  private onConnectionChange: ((connected: boolean) => void) | null = null

  onEvent(handler: SSEEventHandler): () => void {
    this.listeners.add(handler)
    return () => this.listeners.delete(handler)
  }

  connect(
    sessionId: string,
    onSseError?: (message: string) => void,
    onStreamOpen?: () => void,
    onResync?: () => void,
    onConnectionChange?: (connected: boolean) => void,
  ): EventSource {
    this.disconnect()
    this.onError = onSseError ?? null
    this.onOpen = onStreamOpen ?? null
    this.onResync = onResync ?? null
    this.onConnectionChange = onConnectionChange ?? null
    this.lastProbe = 0
    // Keep the cursor across reconnects of the same session; reset it when the
    // bus is reused for a different one. A reload restores it from storage.
    if (this.seqSessionId !== sessionId) {
      this.lastSeqNum = readStoredCursor(sessionId)
      this.seqSessionId = sessionId
    } else {
      this.lastSeqNum = Math.max(this.lastSeqNum, readStoredCursor(sessionId))
    }
    const uuid = getUuid()
    const since = this.lastSeqNum > 0 ? `&since=${this.lastSeqNum}` : ''
    const url = `/web/sessions/${sessionId}/events?uuid=${encodeURIComponent(uuid)}${since}`
    const es = new EventSource(url)
    this.eventSource = es

    es.addEventListener('message', (e: MessageEvent) => {
      if (this.eventSource !== es) return
      try {
        const data = JSON.parse(e.data) as SessionEvent
        // Auto-replay after a reconnect re-sends everything since the cursor;
        // without this the duplicate assistant text/tool cards piled up.
        if (typeof data.seqNum === 'number') {
          if (data.seqNum <= this.lastSeqNum) return
          // A jump means the server could not replay everything. Applying the
          // rest would leave a permanent hole, so ask for a history reload.
          const gap = this.lastSeqNum > 0 && data.seqNum > this.lastSeqNum + 1
          this.lastSeqNum = data.seqNum
          storeCursor(sessionId, this.lastSeqNum)
          if (gap) this.onResync?.()
        }
        for (const handler of this.listeners) {
          handler(data)
        }
      } catch {
        // ignore parse errors
      }
    })

    // The server could not bridge the gap from our cursor (evicted buffer or
    // a restarted process). Trust its cursor, then re-read the durable store.
    es.addEventListener('resync', (e: MessageEvent) => {
      if (this.eventSource !== es) return
      let cursor = 0
      try {
        const data = JSON.parse(e.data || '{}') as { seqNum?: unknown }
        if (typeof data.seqNum === 'number') cursor = data.seqNum
      } catch {
        // Keep 0: the next event after the reload re-establishes the cursor.
      }
      this.lastSeqNum = cursor
      storeCursor(sessionId, cursor)
      this.onResync?.()
    })

    es.onopen = () => {
      if (this.eventSource !== es) return
      this.onConnectionChange?.(true)
      // The server has subscribed to the bus before answering, so anything
      // produced earlier is only in the durable store. Let the view catch up.
      this.onOpen?.()
    }

    // A *closed* session answers 409 forever, and the browser's built-in
    // retry would hammer it. A transient network blip also fires onerror but
    // should reconnect. Probe the session before deciding which this is.
    es.onerror = () => {
      if (this.eventSource !== es) return
      this.onConnectionChange?.(false)
      const now = Date.now()
      if (now - this.lastProbe < 2000) return
      this.lastProbe = now
      // A prolonged network outage says nothing about whether the terminal
      // closed. Let EventSource keep retrying until the server confirms it.
      void (async () => {
        try {
          const res = await fetch(
            `/web/sessions/${sessionId}?uuid=${encodeURIComponent(uuid)}`,
          )
          if (this.eventSource !== es) return
          if (res.status === 404 || res.status === 409) {
            this.fail(es, sessionId, '会话连接已断开，终端可能已关闭')
          }
          // Other statuses are transient; EventSource retries on its own.
        } catch {
          // Network is down; the browser retries with backoff.
        }
      })()
    }
    return es
  }

  private fail(es: EventSource, sessionId: string, message: string): void {
    debugLog('sse', `SSE 连接关闭: ${sessionId} — ${message}`)
    es.close()
    if (this.eventSource === es) this.eventSource = null
    this.onError?.(message)
    this.onError = null
  }

  /** True while `es` is still the connection this bus owns. */
  owns(es: EventSource | null): boolean {
    return es !== null && this.eventSource === es
  }

  disconnect(): void {
    if (this.eventSource) {
      this.eventSource.close()
      this.eventSource = null
    }
    this.onError = null
    this.onOpen = null
    this.onResync = null
    this.onConnectionChange = null
  }
}

// 全局 SSE bus 实例
export const sseBus = new SSEBus()

// =============================================================================
// RCS Chat Adapter — 将 SSE 事件转为 ThreadEntry
// =============================================================================

function mapToolStatus(status: string): ToolCallStatus {
  if (status === 'completed') return 'complete'
  if (status === 'failed') return 'error'
  return 'running'
}

function extractEventText(payload: EventPayload): string {
  if (typeof payload.content === 'string') return payload.content
  if (payload.message && typeof payload.message === 'object') {
    const msg = payload.message as Record<string, unknown>
    if (typeof msg.content === 'string') return msg.content
    if (Array.isArray(msg.content)) {
      return (msg.content as Array<Record<string, unknown>>)
        .filter(b => b.type === 'text' && typeof b.text === 'string')
        .map(b => b.text as string)
        .join('')
    }
  }
  return ''
}

/** Preserve image blocks from terminal echoes and history on other devices. */
function extractEventImages(payload: EventPayload): UserMessageImage[] {
  const record = payload as Record<string, unknown>
  const raw =
    record.raw && typeof record.raw === 'object'
      ? (record.raw as Record<string, unknown>)
      : undefined
  const message =
    record.message && typeof record.message === 'object'
      ? (record.message as Record<string, unknown>)
      : undefined
  const rawMessage =
    raw?.message && typeof raw.message === 'object'
      ? (raw.message as Record<string, unknown>)
      : undefined
  const blocks = [
    message?.content,
    rawMessage?.content,
    raw?.content,
    record.content,
  ].find(value => Array.isArray(value))
  if (!Array.isArray(blocks)) return []
  const images: UserMessageImage[] = []
  for (const block of blocks) {
    if (!block || typeof block !== 'object' || block.type !== 'image') continue
    const source = block.source
    if (
      !source ||
      typeof source !== 'object' ||
      source.type !== 'base64' ||
      typeof source.data !== 'string'
    )
      continue
    const mimeType = source.media_type ?? source.mediaType
    if (typeof mimeType !== 'string' || !mimeType.startsWith('image/')) continue
    images.push({ mimeType, data: source.data })
  }
  return images
}

/** SDK tool_result blocks (top-level tool_call_id only exists on legacy v1). */
function toolResultBlocks(
  p: Record<string, unknown>,
): Array<Record<string, unknown>> {
  const msg = p.message as Record<string, unknown> | undefined
  const blocks = Array.isArray(msg?.content)
    ? (msg.content as Array<Record<string, unknown>>)
    : []
  const found = blocks.filter(b => b && b.type === 'tool_result')
  if (found.length > 0) return found
  // Legacy flat shape.
  if (typeof p.tool_call_id === 'string' && p.tool_call_id) {
    return [
      {
        tool_use_id: p.tool_call_id,
        content: p.content ?? p.output ?? '',
        is_error: p.is_error === true,
      },
    ]
  }
  return []
}

function blockText(value: unknown): string {
  if (typeof value === 'string') return value
  if (Array.isArray(value)) {
    return (value as Array<Record<string, unknown>>)
      .filter(b => b && b.type === 'text')
      .map(b => String(b.text ?? ''))
      .join('')
  }
  return ''
}

/** Fold tool_result blocks into their tool cards (live stream and history). */
function applyToolResult(
  entries: ThreadEntry[],
  p: Record<string, unknown>,
): ThreadEntry[] {
  let next = entries
  for (const block of toolResultBlocks(p)) {
    const id = typeof block.tool_use_id === 'string' ? block.tool_use_id : ''
    if (!id) continue
    const idx = findToolCallIndex(next, id)
    if (idx < 0) continue
    const entry = next[idx] as ToolCallEntry
    const output = blockText(block.content)
    const failed = block.is_error === true
    next = next.map((e, i) =>
      i === idx
        ? {
            type: 'tool_call',
            toolCall: {
              ...entry.toolCall,
              status: (failed ? 'error' : 'complete') as ToolCallStatus,
              rawOutput: { output },
            },
          }
        : e,
    )
  }
  return next
}

/** Embedded tool_use blocks of an assistant message, in SDK order. */
function toolUseBlocks(
  payload: Record<string, unknown>,
): Array<Record<string, unknown>> {
  const msg = payload.message as Record<string, unknown> | undefined
  if (!msg || !Array.isArray(msg.content)) return []
  return (msg.content as Array<Record<string, unknown>>).filter(
    b => b && b.type === 'tool_use',
  )
}

function findToolCallIndex(entries: ThreadEntry[], toolCallId: string): number {
  for (let i = entries.length - 1; i >= 0; i--) {
    const entry = entries[i]
    if (
      entry &&
      entry.type === 'tool_call' &&
      entry.toolCall.id === toolCallId
    ) {
      return i
    }
  }
  return -1
}

export type BridgeSessionInfo = {
  model?: string
  permissionMode?: string
  /** Current session effort ('auto' when the user has not pinned a level). */
  effort?: string
  /** Whether the current model can enable Ultracode at all. */
  ultracodeAvailable?: boolean
  ultracodeActive?: boolean
  /** Context window of the current model, for the context bar. */
  contextWindow?: number
  commands: Array<{ name: string; description: string }>
  models: Array<{ id: string; name: string; description?: string }>
}

/** Token usage of the live session, mirroring the terminal status line. */
export type SessionUsage = {
  /** Latest assistant message's usage — this is the current context size. */
  current: {
    input: number
    output: number
    cacheCreation: number
    cacheRead: number
  } | null
  /** Sum of the newest usage per assistant message (stream chunks repeat). */
  totals: { input: number; output: number }
  /** Conversation average: cache_read / (input + cache_creation + cache_read). */
  cacheHitRate: number | null
}

export type BridgeControlResponse = {
  requestId?: string
  subtype: string
  error?: string
}

export class RCSChatAdapter {
  private sessionId: string
  private setEntries: React.Dispatch<SetStateAction<ThreadEntry[]>>
  private unsub: (() => void) | null = null
  /** The EventSource this adapter opened on the shared bus. */
  private es: EventSource | null = null
  private onStatusChange?: (status: string, since?: number) => void
  private onError?: (error: string) => void
  private onPermissionRequest?: (permission: PendingPermission) => void
  private onPermissionResolved?: (requestId: string) => void
  private onSessionInfo?: (info: BridgeSessionInfo) => void
  private onControlResponse?: (response: BridgeControlResponse) => void
  private onBridgeState?: (connected: boolean) => void
  private onTurnComplete?: () => void
  private onUsage?: (usage: SessionUsage) => void
  private onStreamOpen?: () => void
  private onStreamState?: (connected: boolean) => void
  private onTasks?: (tasks: ExecutionTask[]) => void
  private onWorkflows?: (runs: WorkflowRun[]) => void
  private onAgentProgress?: (payload: unknown) => void
  private historyMode: 'full' | 'metadata'
  private disposed = false
  /**
   * Newest usage per assistant message id. Stream chunks repeat a message's
   * usage (cumulative within that message), so totals must sum one value per
   * message, not one per chunk.
   */
  private usageByMessage = new Map<string, SessionUsage['current'] & object>()
  private usageOrder: string[] = []
  // Direct handleEvent() calls (and tests) work without init(); init() flips
  // this off while the history snapshot is being merged.
  private historyReady = true
  private bufferedEvents: SessionEvent[] = []

  constructor(
    sessionId: string,
    setEntries: React.Dispatch<SetStateAction<ThreadEntry[]>>,
    options?: {
      onStatusChange?: (status: string, since?: number) => void
      onError?: (error: string) => void
      onPermissionRequest?: (permission: PendingPermission) => void
      /** The request was answered elsewhere (the terminal, another device). */
      onPermissionResolved?: (requestId: string) => void
      onSessionInfo?: (info: BridgeSessionInfo) => void
      onControlResponse?: (response: BridgeControlResponse) => void
      onBridgeState?: (connected: boolean) => void
      onTurnComplete?: () => void
      /** Live token usage, after each assistant chunk that carries it. */
      onUsage?: (usage: SessionUsage) => void
      /** Fired once the live SSE stream is actually open. */
      onStreamOpen?: () => void
      onStreamState?: (connected: boolean) => void
      onTasks?: (tasks: ExecutionTask[]) => void
      onWorkflows?: (runs: WorkflowRun[]) => void
      onAgentProgress?: (payload: unknown) => void
      /**
       * 'metadata' still reads the bridge's system/init and bridge_state so
       * the pickers work, but leaves the transcript to the caller.
       */
      historyMode?: 'full' | 'metadata'
    },
  ) {
    this.sessionId = sessionId
    this.setEntries = setEntries
    this.onStatusChange = options?.onStatusChange
    this.onError = options?.onError
    this.onPermissionRequest = options?.onPermissionRequest
    this.onPermissionResolved = options?.onPermissionResolved
    this.onSessionInfo = options?.onSessionInfo
    this.onControlResponse = options?.onControlResponse
    this.onBridgeState = options?.onBridgeState
    this.onTurnComplete = options?.onTurnComplete
    this.onUsage = options?.onUsage
    this.onStreamOpen = options?.onStreamOpen
    this.onStreamState = options?.onStreamState
    this.onTasks = options?.onTasks
    this.onWorkflows = options?.onWorkflows
    this.onAgentProgress = options?.onAgentProgress
    this.historyMode = options?.historyMode ?? 'full'
  }

  /** Server-published bridge_state, possibly wrapped as {content, raw}. */
  private emitBridgeState(payload: EventPayload): void {
    const root = payload as unknown as Record<string, unknown>
    const record =
      root.raw && typeof root.raw === 'object' && !Array.isArray(root.raw)
        ? (root.raw as Record<string, unknown>)
        : root
    this.onBridgeState?.(record.connected !== false)
  }

  /**
   * Parse the CLI's system/init message. The wire type carries the current
   * model, permission mode and command names; self-hosted builds add
   * `models` and `command_details` (see src/utils/messages/systemInit.ts).
   */
  private handleInit(payload: EventPayload): void {
    const root = payload as unknown as Record<string, unknown>
    // The bridge transport wraps SDK messages as {content, raw}; the init
    // message itself lives under `raw`.
    const record =
      root.raw && typeof root.raw === 'object' && !Array.isArray(root.raw)
        ? (root.raw as Record<string, unknown>)
        : root
    if (record.type !== 'system' || record.subtype !== 'init') return

    const commands: Array<{ name: string; description: string }> = []
    // Older CLI builds advertise local-jsx commands as bridge-safe; they
    // cannot render in the web and only produce an error. Hide them even
    // before the CLI is restarted with the fixed filter.
    const unsupported = new Set(['plan', 'local-vault', 'local-memory'])
    if (Array.isArray(record.command_details)) {
      for (const item of record.command_details as Array<
        Record<string, unknown>
      >) {
        if (typeof item?.name === 'string') {
          commands.push({
            name: item.name,
            description:
              typeof item.description === 'string' ? item.description : '',
          })
        }
      }
    } else if (Array.isArray(record.slash_commands)) {
      for (const name of record.slash_commands as string[]) {
        if (typeof name === 'string') commands.push({ name, description: '' })
      }
    }

    const models: Array<{ id: string; name: string; description?: string }> = []
    if (Array.isArray(record.models)) {
      for (const item of record.models as Array<Record<string, unknown>>) {
        if (typeof item?.id === 'string') {
          models.push({
            id: item.id,
            name: typeof item.name === 'string' ? item.name : item.id,
            description:
              typeof item.description === 'string'
                ? item.description
                : undefined,
          })
        }
      }
    }

    this.onSessionInfo?.({
      model: typeof record.model === 'string' ? record.model : undefined,
      permissionMode:
        typeof record.permissionMode === 'string'
          ? record.permissionMode
          : undefined,
      effort: typeof record.effort === 'string' ? record.effort : undefined,
      ultracodeAvailable:
        typeof record.ultracode_available === 'boolean'
          ? record.ultracode_available
          : undefined,
      ultracodeActive:
        typeof record.ultracode_active === 'boolean'
          ? record.ultracode_active
          : undefined,
      contextWindow:
        typeof record.context_window === 'number' && record.context_window > 0
          ? record.context_window
          : undefined,
      commands: commands.filter(command => !unsupported.has(command.name)),
      models,
    })
  }

  /**
   * 初始化：先连 SSE（缓冲实时事件），再绑定/加载历史。
   *
   * History is a snapshot; subscribing afterwards loses every event that
   * happens in between (notably a permission request, which blocks the
   * terminal until the browser answers). Buffer live events until the merge
   * finishes, then replay them in order.
   */
  async init(): Promise<void> {
    this.disposed = false
    this.historyReady = false
    this.bufferedEvents = []
    this.connectSSE()
    try {
      // A stalled bind/history fetch used to leave historyReady false forever:
      // the live view stayed empty while bufferedEvents grew without bound.
      await withTimeout(apiBind(this.sessionId), 10_000, '连接会话超时')
    } catch {
      // may already be bound, or offline — carry on with the live stream
    }
    if (this.disposed) return
    try {
      await withTimeout(this.loadHistory(), 12_000, '加载历史超时')
    } catch (error) {
      // A failed history fetch must not disable live messaging.
      this.onError?.(error instanceof Error ? error.message : '加载历史失败')
    }
    if (this.disposed) return
    this.historyReady = true
    const buffered = this.bufferedEvents
    this.bufferedEvents = []
    for (const event of buffered) this.handleEvent(event)
  }

  /** 加载历史事件并转为 ThreadEntry */
  async loadHistory(): Promise<void> {
    const { events } = await apiFetchSessionHistory(this.sessionId)
    if (!events || events.length === 0) return

    const historyEntries: ThreadEntry[] = []
    const agentProgress: unknown[] = []
    /**
     * Tool results seen in the replay. They are applied to the LIVE transcript
     * too: a resync can contain the completion of a tool whose tool_use event
     * fell out of the surviving bus window, and applying the result only to
     * the freshly built history would leave the on-screen card spinning with
     * no output (the reported "bash output missing after a reconnect").
     */
    const toolResultPayloads: Array<Record<string, unknown>> = []
    let currentAssistant: AssistantMessageEntry | null = null
    // Permission requests are state, not transcript: replay the ones that
    // were never answered (the terminal is still blocked on them).
    const pendingPermissions: Array<{
      requestId: string
      toolName: string
      toolInput: Record<string, unknown>
      description: string
    }> = []
    const answeredPermissions = new Set<string>()

    const flushAssistant = () => {
      if (currentAssistant) {
        historyEntries.push(currentAssistant)
        currentAssistant = null
      }
    }

    for (const event of events) {
      const payload = event.payload || ({} as EventPayload)

      if (event.type === 'user') {
        if (event.direction === 'outbound') continue // skip echoed user messages
        flushAssistant()
        // Tool results arrive as user turns with no text; fold them into the
        // tool cards instead of dropping them.
        const asRecord = payload as Record<string, unknown>
        if (toolResultBlocks(asRecord).length > 0) {
          toolResultPayloads.push(asRecord)
          const updated = applyToolResult(historyEntries, asRecord)
          historyEntries.length = 0
          historyEntries.push(...updated)
          continue
        }
        const text = extractEventText(payload)
        const images = extractEventImages(payload)
        if (text || images.length) {
          const synthetic =
            (payload as Record<string, unknown>).isSynthetic === true ||
            (payload.raw as Record<string, unknown> | undefined)
              ?.isSynthetic === true
          const classified =
            !text && images.length && !synthetic
              ? { kind: 'text' as const, text: '' }
              : classifyLocalCommandText(text, synthetic)
          if (classified.kind === 'drop') continue
          const id = payload.uuid
            ? `user-${payload.uuid}`
            : event.id || `hist-user-${historyEntries.length}`
          if (classified.kind === 'output') {
            historyEntries.push({
              type: 'command_output',
              id: `command-${payload.uuid || event.id || historyEntries.length}`,
              text: classified.text,
            })
          } else {
            historyEntries.push({
              type: 'user_message',
              id,
              content: classified.text,
              ...(images.length ? { images } : {}),
            })
          }
        }
      } else if (event.type === 'assistant') {
        flushAssistant()
        const text = extractEventText(payload)
        const toolParts: ThreadEntry[] = []

        const msg = payload.message as Record<string, unknown> | undefined
        if (msg && typeof msg === 'object' && Array.isArray(msg.content)) {
          for (const block of msg.content as Array<Record<string, unknown>>) {
            if (block.type === 'tool_use') {
              toolParts.push({
                type: 'tool_call',
                toolCall: {
                  id:
                    (block.id as string) ||
                    `hist-tool-${historyEntries.length}`,
                  title: (block.name as string) || 'tool',
                  status: 'unknown',
                  rawInput: (block.input as Record<string, unknown>) || {},
                },
              })
            }
          }
        }

        if (text || toolParts.length > 0) {
          currentAssistant = {
            type: 'assistant_message',
            id:
              (typeof payload.uuid === 'string' &&
                `assistant-${payload.uuid}`) ||
              event.id ||
              `hist-asst-${historyEntries.length}`,
            chunks: text ? [{ type: 'message', text }] : [],
          }
          historyEntries.push(currentAssistant)
          // Push tool calls after assistant message
          for (const tp of toolParts) {
            historyEntries.push(tp)
          }
          currentAssistant = null // Tool calls are separate entries
        }
      } else if (event.type === 'tool_use') {
        const p = payload as Record<string, unknown>
        const tc: ToolCallEntry = {
          type: 'tool_call',
          toolCall: {
            id:
              (p.tool_call_id as string) ||
              `hist-tool-${historyEntries.length}`,
            title: (p.tool_name as string) || 'tool',
            status: 'unknown',
            rawInput: (p.tool_input as Record<string, unknown>) || {},
          },
        }
        historyEntries.push(tc)
      } else if (event.type === 'tool_result') {
        const result = payload as Record<string, unknown>
        toolResultPayloads.push(result)
        const updated = applyToolResult(historyEntries, result)
        historyEntries.length = 0
        historyEntries.push(...updated)
      } else if (event.type === 'system') {
        const record = payload as Record<string, unknown>
        if (record.subtype === 'local_command') {
          const raw = typeof record.content === 'string' ? record.content : ''
          const text = raw.replace(/<\/?local-command-[^>]*>/g, '').trim()
          if (text) {
            historyEntries.push({
              type: 'command_output',
              id: `command-${record.uuid || event.id || historyEntries.length}`,
              text,
            })
          }
        }
        this.handleInit(payload)
      } else if (event.type === 'bridge_state') {
        this.emitBridgeState(payload)
      } else if (
        event.type === 'task_state' ||
        event.type === 'workflow_state'
      ) {
        this.applyExecutionSnapshot(event.type, payload)
      } else if (event.type === 'agent_progress') {
        agentProgress.push(payload)
        this.onAgentProgress?.(payload)
      } else if (
        event.type === 'control_request' ||
        event.type === 'permission_request'
      ) {
        const request = payload.request as Record<string, unknown> | undefined
        if (request && request.subtype === 'can_use_tool') {
          pendingPermissions.push({
            requestId: String(payload.request_id ?? ''),
            toolName:
              typeof request.tool_name === 'string'
                ? request.tool_name
                : 'unknown',
            toolInput: (request.input ?? request.tool_input ?? {}) as Record<
              string,
              unknown
            >,
            description:
              typeof request.description === 'string'
                ? request.description
                : '',
          })
        }
      } else if (
        event.type === 'control_response' ||
        event.type === 'permission_response' ||
        event.type === 'control_cancel_request'
      ) {
        // Any resolution signal means the request is spent. Missing the web's
        // own answer (permission_response) or a terminal-side resolution
        // (control_cancel_request) made a reload replay the old
        // control_request and pop the already-answered card again.
        const requestId = resolvedRequestId(payload)
        if (requestId) {
          answeredPermissions.add(requestId)
          this.onPermissionResolved?.(requestId)
        }
      }
    }

    flushAssistant()
    for (const permission of pendingPermissions) {
      if (!answeredPermissions.has(permission.requestId)) {
        this.onPermissionRequest?.(permission)
      }
    }
    if (this.historyMode === 'metadata') {
      if (agentProgress.length)
        this.setEntries(prev =>
          agentProgress.reduce<ThreadEntry[]>(
            (items, event) => applyAgentProgress(items, event),
            prev,
          ),
        )
      return
    }
    if (this.disposed) return
    // The parent may already have loaded the server-cached transcript (the
    // device history survives terminal restarts). Merge instead of replacing
    // so the bridge replay only fills the gaps.
    this.setEntries(prev => {
      const merged = mergeThreadEntries(
        prev,
        historyEntries,
        this.optimisticUserIds,
      )
      // Complete cards that only exist in the live transcript: their
      // tool_use event may predate the surviving replay window.
      const withResults = toolResultPayloads.reduce<ThreadEntry[]>(
        (items, result) => applyToolResult(items, result),
        merged,
      )
      return agentProgress.reduce<ThreadEntry[]>(
        (items, event) => applyAgentProgress(items, event),
        withResults,
      )
    })
  }

  /**
   * Fold the usage of an assistant message into the session ledger and
   * publish the aggregate. Each streamed chunk of one message repeats that
   * message's (cumulative) usage; only the newest value per message counts.
   */
  private trackUsage(assistantId: string, payload: EventPayload): void {
    const message = (payload as Record<string, unknown>).message as
      | Record<string, unknown>
      | undefined
    const usage = message?.usage
    if (!usage || typeof usage !== 'object') return
    const record = usage as Record<string, unknown>
    this.recordUsage(assistantId, {
      input: Number(record.input_tokens) || 0,
      output: Number(record.output_tokens) || 0,
      cacheCreation: Number(record.cache_creation_input_tokens) || 0,
      cacheRead: Number(record.cache_read_input_tokens) || 0,
    })
    this.publishUsageLedger()
  }

  /**
   * Seed the ledger from history rows that carry billed usage. The terminal
   * hydrates its status line from the transcript the same way; without this a
   * reload showed Tokens 0 (cache —) until the next reply. Ids match the live
   * stream's uuid space, so a later live event for the same message simply
   * overwrites its seed — nothing is counted twice.
   */
  seedUsageFromHistory(
    messages: { id: string; usage?: TranscriptUsage }[],
  ): void {
    let touched = false
    for (const message of messages) {
      if (!message.usage) continue
      this.recordUsage(`assistant-${message.id}`, {
        input: message.usage.inputTokens,
        output: message.usage.outputTokens,
        cacheCreation: message.usage.cacheCreationInputTokens,
        cacheRead: message.usage.cacheReadInputTokens,
      })
      touched = true
    }
    if (touched) this.publishUsageLedger()
  }

  private recordUsage(
    id: string,
    entry: {
      input: number
      output: number
      cacheCreation: number
      cacheRead: number
    },
  ): void {
    if (!this.usageByMessage.has(id)) this.usageOrder.push(id)
    this.usageByMessage.set(id, entry)
    // Bound the ledger: totals drift slightly once old messages drop off,
    // which is the same trade-off the terminal status line makes.
    while (this.usageOrder.length > 200) {
      const dropped = this.usageOrder.shift()
      if (dropped) this.usageByMessage.delete(dropped)
    }
  }

  private publishUsageLedger(): void {
    let input = 0
    let output = 0
    let cacheCreation = 0
    let cacheRead = 0
    for (const item of this.usageByMessage.values()) {
      input += item.input
      output += item.output
      cacheCreation += item.cacheCreation
      cacheRead += item.cacheRead
    }
    // The newest ledger row is the latest context size the model saw — the
    // context bar should be right immediately after a reload, not only after
    // the next reply.
    const newestId = this.usageOrder[this.usageOrder.length - 1]
    const current = newestId ? this.usageByMessage.get(newestId) : undefined
    if (!current) return
    // Conversation average, matching the terminal status line: cumulative
    // cache reads over every prompt token billed this session.
    const readTotal = input + cacheCreation + cacheRead
    this.onUsage?.({
      current,
      // `in` is the uncached prompt only, matching the terminal's totals; the
      // cache share is reported separately as a rate.
      totals: { input, output },
      cacheHitRate: readTotal > 0 ? cacheRead / readTotal : null,
    })
  }

  /** 连接 SSE 事件流 */
  connectSSE(): void {
    if (this.disposed) return
    this.es = sseBus.connect(
      this.sessionId,
      message => {
        // The bridge is gone; a dead SSE must also flip the connection state
        // so the parent view can switch back / stop pretending to be live.
        this.onBridgeState?.(false)
        this.onError?.(message)
      },
      () => {
        if (!this.disposed) this.onStreamOpen?.()
      },
      () => this.requestResync(),
      connected => this.onStreamState?.(connected),
    )
    this.unsub = sseBus.onEvent(event => this.handleEvent(event))
  }

  /** Guards a history reload from stacking up behind a burst of events. */
  private resyncing = false

  /**
   * Ids of the optimistic user bubbles this adapter itself created in
   * sendMessage. The echo's adoption loop may only rewrite bubbles that are
   * still ours and un-echoed: matching ANY same-text row let the second echo
   * of a repeated reply (“好”) swallow the first echo's bubble, and a later
   * resync re-appended it out of order. Cleared pending state alone is not a
   * discriminator — clearStalePending strips it legitimately — so the id set
   * is what survives that pass.
   */
  private optimisticUserIds = new Set<string>()

  /**
   * Any sign that the CLI is consuming its queue — an assistant chunk, a tool
   * call, a finished turn, a local command — proves earlier messages were
   * delivered. Slash commands never echo as user events and a server restart
   * wipes the echo entirely, so without this the "排队中" state could stick
   * forever.
   */
  private clearStalePending(): void {
    this.setEntries(prev => {
      let changed = false
      const next = prev.map(entry => {
        if (entry.type === 'user_message' && entry.pending) {
          changed = true
          return { ...entry, pending: false }
        }
        return entry
      })
      return changed ? next : prev
    })
  }

  /**
   * The stream lost events that no replay can recover. Re-read the stored
   * transcript; `mergeThreadEntries` is id-based, so whatever the live stream
   * already applied is not duplicated. In the controlled device view the
   * parent owns the transcript, so ask it to sync instead.
   */
  private requestResync(): void {
    if (this.disposed || !this.historyReady) return
    debugLog('sse', `事件流断档，重新同步历史: ${this.sessionId}`)
    if (this.historyMode === 'metadata') {
      this.onStreamOpen?.()
      return
    }
    if (this.resyncing) return
    this.resyncing = true
    // Buffer live events for the duration of the fetch, exactly like init():
    // merging while the stream keeps appending pushed the re-read older rows
    // AFTER the newer live tail (the merge is insertion-only), permanently
    // reordering the transcript.
    this.historyReady = false
    void (async () => {
      try {
        await this.loadHistory()
        // Anything still pending after the replay was never echoed (restart,
        // uuid rewrite): stop claiming it is queued.
        this.clearStalePending()
      } catch (error) {
        this.onError?.(
          error instanceof Error ? error.message : '重新加载历史失败',
        )
      } finally {
        this.resyncing = false
        if (!this.disposed) {
          this.historyReady = true
          const buffered = this.bufferedEvents
          this.bufferedEvents = []
          for (const event of buffered) this.handleEvent(event)
        } else {
          this.historyReady = true
        }
      }
    })()
  }

  /** 断开 SSE */
  disconnect(): void {
    this.disposed = true
    if (this.unsub) {
      this.unsub()
      this.unsub = null
    }
    // The bus is shared and session-scoped. React may mount the next session's
    // adapter before this stale one's cleanup runs, so only the adapter whose
    // stream is still live may close it — otherwise switching between two
    // running terminals left the new one with a dead, silent SSE.
    if (this.es && sseBus.owns(this.es)) sseBus.disconnect()
    this.es = null
  }

  /** 处理 SSE 事件 */
  private applyExecutionSnapshot(type: string, payload: unknown): void {
    if (type === 'task_state') {
      const tasks = parseTaskSnapshot(payload)
      if (tasks) this.onTasks?.(tasks)
    } else if (type === 'workflow_state') {
      const runs = parseWorkflowSnapshot(payload)
      if (runs) this.onWorkflows?.(runs)
    }
  }

  handleEvent(event: SessionEvent): void {
    if (this.disposed) return
    if (!this.historyReady) {
      this.bufferedEvents.push(event)
      return
    }
    const type = event.type
    const payload = event.payload || ({} as EventPayload)

    // Skip bridge init noise. Only the short connect banner matters here;
    // serialising a whole history event was pure overhead on every event.
    if (/Remote Control connecting/i.test(eventText(payload))) return

    switch (type) {
      // Messages entered in the terminal also need to appear in the browser.
      // The bridge echoes browser messages with the original UUID, so avoid
      // adding those a second time.
      case 'user': {
        if (event.direction === 'outbound') break
        // The SDK delivers tool results as user turns; they carry no text and
        // used to be dropped, leaving every tool card spinning forever.
        const asRecord = payload as Record<string, unknown>
        if (toolResultBlocks(asRecord).length > 0) {
          // Same proof of queue consumption as a tool_use event — and the only
          // signal that still arrives when the page reattached mid-tool-run
          // (the tool_use event already passed before the resubscribe).
          this.clearStalePending()
          this.setEntries(prev => applyToolResult(prev, asRecord))
          break
        }
        const content = extractEventText(payload)
        const images = extractEventImages(payload)
        if (!content && !images.length) break
        const synthetic =
          (payload as Record<string, unknown>).isSynthetic === true ||
          (payload.raw as Record<string, unknown> | undefined)?.isSynthetic ===
            true
        const classified =
          !content && images.length && !synthetic
            ? { kind: 'text' as const, text: '' }
            : classifyLocalCommandText(content, synthetic)
        if (classified.kind === 'drop') break
        if (classified.kind === 'output') {
          const outputId = `command-${
            typeof payload.uuid === 'string'
              ? payload.uuid
              : event.id || generateMessageUuid()
          }`
          this.setEntries(prev =>
            prev.some(
              entry => entry.type === 'command_output' && entry.id === outputId,
            )
              ? prev
              : [
                  ...prev,
                  {
                    type: 'command_output',
                    id: outputId,
                    text: classified.text,
                  },
                ],
          )
          break
        }
        const cleanContent = classified.text
        const id = payload.uuid
          ? `user-${payload.uuid}`
          : event.id || `user-${generateMessageUuid()}`
        // Snapshot the id set: the updater may run twice under StrictMode, and
        // draining the live set inside invocation 1 made invocation 2 take the
        // append branch — nondeterministic output, duplicate bubble.
        const adoptableIds = new Set(this.optimisticUserIds)
        this.setEntries(prev => {
          // The echo means the CLI has actually taken the message off its
          // queue — clear the "排队中" state on the optimistic bubble.
          const echoed = prev.findIndex(
            entry => entry.type === 'user_message' && entry.id === id,
          )
          if (echoed >= 0) {
            const entry = prev[echoed] as UserMessageEntry
            // Idempotent side effect only: control flow above never reads the
            // live set, so a StrictMode double-invocation stays deterministic.
            this.optimisticUserIds.delete(entry.id)
            if (!entry.pending && (entry.images?.length || !images.length))
              return prev
            const next = [...prev]
            next[echoed] = {
              ...entry,
              pending: false,
              ...(!entry.images?.length && images.length ? { images } : {}),
            }
            return next
          }
          // A message shown optimistically with a local id adopts the echo
          // instead of adding a second bubble. Both the bridge composer
          // (`user-…`, pending:true) and the device view (`pending-…`) take
          // this path: the echo's uuid can differ from the one the browser
          // generated, and matching only `pending-` ids split one prompt into
          // two bubbles. Pending state is not required — clearStalePending may
          // have already cleared it — but the candidate MUST still be one of
          // ours and un-echoed; an already-adopted echo is not, and letting a
          // later same-text echo replace it deleted the older bubble when the
          // person repeated a short reply.
          const isAdoptable = (entry: ThreadEntry): boolean =>
            entry.type === 'user_message' &&
            entry.id !== id &&
            entry.content === cleanContent &&
            (cleanContent !== '' ||
              (entry.images?.length === images.length &&
                entry.images?.every(
                  (image, index) => image.data === images[index]?.data,
                ))) &&
            (entry.pending === true ||
              entry.id.startsWith('pending-') ||
              adoptableIds.has(entry.id))
          let pending = -1
          for (let i = prev.length - 1; i >= 0; i -= 1) {
            if (isAdoptable(prev[i])) {
              pending = i
              break
            }
          }
          if (pending >= 0) {
            const next = [...prev]
            const optimistic = prev[pending] as UserMessageEntry
            // Idempotent side effect (see the exact-id branch): the decision
            // came from the snapshot, not the live set.
            this.optimisticUserIds.delete(optimistic.id)
            // Adopt the real id but keep the images: the echo's text is the
            // same string, and dropping the optimistic entry's attachments
            // made sent photos vanish from the transcript.
            next[pending] = {
              type: 'user_message',
              id,
              content: cleanContent,
              ...(optimistic.images?.length
                ? { images: optimistic.images }
                : images.length
                  ? { images }
                  : {}),
            }
            return next
          }
          return [
            ...prev,
            {
              type: 'user_message',
              id,
              content: cleanContent,
              ...(images.length ? { images } : {}),
            },
          ]
        })
        break
      }

      // ---- 助手消息 ----
      case 'assistant': {
        this.clearStalePending()
        const content =
          typeof payload.content === 'string' ? payload.content : ''
        // Stable id: when the CLI streams one message in several events they
        // share a uuid, so chunks append; a new uuid is a new message. A
        // stable id also lets mergeThreadEntries dedupe against history.
        const assistantId = `assistant-${
          typeof payload.uuid === 'string'
            ? payload.uuid
            : event.id || generateMessageUuid()
        }`
        this.trackUsage(assistantId, payload)
        this.setEntries(prev => {
          // Thinking blocks are part of the assistant message, not a separate
          // event. Reading only the server-extracted text (a string) dropped
          // them from the live view while the transcript/history path showed
          // them — the same conversation looked different after a reload.
          const message = payload.message as Record<string, unknown> | undefined
          const messageBlocks =
            message && Array.isArray(message.content)
              ? (message.content as Array<Record<string, unknown>>)
              : []
          const thoughts: string[] = []
          for (const block of messageBlocks) {
            if (block?.type !== 'thinking') continue
            const text =
              typeof block.thinking === 'string'
                ? block.thinking
                : typeof block.text === 'string'
                  ? block.text
                  : ''
            if (text.trim()) thoughts.push(text)
          }

          const existingIndex = prev.findIndex(
            entry =>
              entry.type === 'assistant_message' && entry.id === assistantId,
          )

          if (existingIndex < 0) {
            // A bridge reattach (server restart, terminal resume) replays the
            // transcript tail under fresh uuids; without this fallback the
            // whole old turn renders again at the bottom of the chat.
            const replayTwin = findAssistantReplayTwin(prev, {
              type: 'assistant_message',
              id: assistantId,
              chunks: [{ type: 'message', text: content }],
            })
            if (replayTwin >= 0) return prev
            if (!thoughts.length && !content.trim()) return prev
            const newEntry: AssistantMessageEntry = {
              type: 'assistant_message',
              id: assistantId,
              chunks: [
                ...thoughts.map(text => ({ type: 'thought' as const, text })),
                ...(content.trim()
                  ? [{ type: 'message' as const, text: content }]
                  : []),
              ],
            }
            return [...prev, newEntry]
          }

          // Same message seen again: a bridge reattach replay, or a growing
          // message. Merge only what is new — replacing wholesale would let a
          // replay truncate text the stream had already shown.
          const existing = prev[existingIndex] as AssistantMessageEntry
          const chunks: AssistantMessageEntry['chunks'] = [...existing.chunks]
          let changed = false
          for (const text of thoughts) {
            if (
              chunks.some(
                chunk => chunk.type === 'thought' && chunk.text === text,
              )
            )
              continue
            chunks.push({ type: 'thought', text })
            changed = true
          }
          if (content.trim()) {
            const last = chunks[chunks.length - 1]
            if (last?.type === 'message') {
              if (content !== last.text) {
                chunks[chunks.length - 1] = content.startsWith(last.text)
                  ? { type: 'message', text: content }
                  : { type: 'message', text: last.text + content }
                changed = true
              }
            } else if (
              !chunks.some(
                chunk => chunk.type === 'message' && chunk.text === content,
              )
            ) {
              chunks.push({ type: 'message', text: content })
              changed = true
            }
          }
          if (!changed) return prev
          const next = [...prev]
          next[existingIndex] = { ...existing, chunks }
          return next
        })

        // Check for embedded tool_use blocks
        const msg = payload.message as Record<string, unknown> | undefined
        // Every streamed chunk repeats the message's full content, so only add
        // tool cards this message has not produced yet.
        const blocks = msg
          ? toolUseBlocks(payload as Record<string, unknown>)
          : []
        for (const block of blocks) {
          const toolCallId =
            (block.id as string) ||
            `call-${Date.now()}-${Math.random().toString(16).slice(2, 8)}`
          const rawInput = (block.input as Record<string, unknown>) || {}
          this.setEntries(prev => {
            const idx = findToolCallIndex(prev, toolCallId)
            if (idx >= 0) {
              // The final assistant message carries the complete input; the
              // first partial chunk often only had a fragment of it.
              const existing = prev[idx] as ToolCallEntry
              const hasInput = Object.keys(rawInput).length > 0
              if (!hasInput) return prev
              const next = [...prev]
              next[idx] = {
                type: 'tool_call',
                toolCall: { ...existing.toolCall, rawInput },
              }
              return next
            }
            return [
              ...prev,
              {
                type: 'tool_call',
                toolCall: {
                  id: toolCallId,
                  title: (block.name as string) || 'tool',
                  status: 'running',
                  rawInput,
                },
              },
            ]
          })
        }
        break
      }

      // ---- 工具调用 ----
      case 'tool_use': {
        this.clearStalePending()
        const p = payload as Record<string, unknown>
        const toolCallId = (p.tool_call_id as string) || `call-${Date.now()}`
        const toolData: ToolCallData = {
          id: toolCallId,
          title: (p.tool_name as string) || 'tool',
          status: 'running',
          rawInput: (p.tool_input as Record<string, unknown>) || {},
        }
        this.setEntries(prev =>
          findToolCallIndex(prev, toolCallId) >= 0
            ? prev
            : [...prev, { type: 'tool_call', toolCall: toolData }],
        )
        break
      }

      // ---- 工具结果 ----
      case 'tool_result': {
        const p = payload as Record<string, unknown>
        this.clearStalePending()
        this.setEntries(prev => applyToolResult(prev, p))
        break
      }

      // ---- 权限请求 ----
      case 'control_request':
      case 'permission_request': {
        const req = payload.request as Record<string, unknown> | undefined
        if (req && req.subtype === 'can_use_tool') {
          const requestId = payload.request_id || ''
          const toolName = (req.tool_name as string) || 'unknown'
          const toolInput = (req.input || req.tool_input || {}) as Record<
            string,
            unknown
          >
          const description = (req.description as string) || ''

          // Update tool call status. The request carries no tool_use id, so
          // match by name: with parallel calls the permission can belong to an
          // earlier tool, and "the newest running card" was the wrong one.
          this.setEntries(prev => {
            const candidates = prev
              .map((entry, index) => ({ entry, index }))
              .filter(
                item =>
                  item.entry.type === 'tool_call' &&
                  item.entry.toolCall.status === 'running' &&
                  (toolName === 'unknown' ||
                    item.entry.toolCall.title.toLowerCase() ===
                      toolName.toLowerCase()),
              )
            const match = candidates[candidates.length - 1]
            if (match && match.entry.type === 'tool_call') {
              // Hoisted so the narrowing survives the map callback below.
              const matched = match.entry
              return prev.map((e, i) =>
                i === match.index
                  ? {
                      type: 'tool_call' as const,
                      toolCall: {
                        ...matched.toolCall,
                        status: 'waiting_for_confirmation' as ToolCallStatus,
                        permissionRequest: {
                          requestId,
                          options: permissionOptions(req),
                        },
                      },
                    }
                  : e,
              )
            }
            return prev
          })

          // Notify parent
          this.onPermissionRequest?.({
            requestId,
            toolName,
            toolInput,
            description,
            options: permissionOptions(req),
          })
        }
        break
      }

      // ---- 会话状态 ----
      case 'session_status': {
        if (typeof payload.status === 'string') {
          this.onStatusChange?.(
            payload.status,
            typeof payload.since === 'number' ? payload.since : undefined,
          )
        }
        break
      }

      // ---- 错误 ----
      case 'error': {
        this.clearStalePending()
        const errorMsg = String(
          payload.message || payload.content || 'Unknown error',
        )
        this.onError?.(errorMsg)
        this.onTurnComplete?.()
        break
      }

      // A result closes the turn: stop the stop/spinner even if the assistant
      // message arrived without a trailing tool result.
      case 'result':
      case 'result_success': {
        this.clearStalePending()
        this.onTurnComplete?.()
        break
      }

      case 'system': {
        // Slash commands (and the CLI's "not available over Remote Control"
        // replies) arrive as system/local_command. They used to be dropped,
        // so a command looked like it did nothing.
        const record = payload as Record<string, unknown>
        if (record.subtype === 'local_command') {
          // A local command is executed without any user echo; without this
          // its optimistic bubble would show 排队中 forever.
          this.clearStalePending()
          const raw = typeof record.content === 'string' ? record.content : ''
          const text = raw.replace(/<\/?local-command-[^>]*>/g, '').trim()
          if (text) {
            const id = `command-${
              typeof record.uuid === 'string'
                ? record.uuid
                : event.id || generateMessageUuid()
            }`
            this.setEntries(prev =>
              prev.some(
                entry => entry.type === 'command_output' && entry.id === id,
              )
                ? prev
                : [...prev, { type: 'command_output', id, text }],
            )
          }
        }
        this.handleInit(payload)
        break
      }

      case 'bridge_state': {
        this.emitBridgeState(payload)
        break
      }

      case 'control_response': {
        const root = payload as unknown as Record<string, unknown>
        // The bridge transport wraps messages as {content, raw}; server
        // control responses are published directly. Accept both.
        const record =
          root.raw && typeof root.raw === 'object' && !Array.isArray(root.raw)
            ? (root.raw as Record<string, unknown>)
            : root
        const response = record.response as Record<string, unknown> | undefined
        if (response) {
          // Answering on the PC (or another device) also produces a
          // control_response. Without this the permission card stayed on
          // screen until a reload, long after the terminal had moved on.
          if (typeof response.request_id === 'string') {
            this.onPermissionResolved?.(response.request_id)
          }
          if (typeof response.error === 'string' && response.error) {
            debugLog(
              'control',
              `control_response error (${String(response.subtype)}): ${response.error}`,
            )
          }
          this.onControlResponse?.({
            requestId:
              typeof response.request_id === 'string'
                ? response.request_id
                : undefined,
            subtype:
              typeof response.subtype === 'string'
                ? response.subtype
                : 'unknown',
            error:
              typeof response.error === 'string' ? response.error : undefined,
          })
        }
        break
      }

      case 'task_state':
      case 'workflow_state':
        this.applyExecutionSnapshot(type, payload)
        break
      case 'agent_progress':
        this.onAgentProgress?.(payload)
        this.setEntries(prev => applyAgentProgress(prev, payload))
        break

      // ---- 权限已在别处解决 ----
      case 'permission_response':
      case 'control_cancel_request': {
        // The web answered (permission_response) or the terminal resolved the
        // request locally (control_cancel_request). Dismiss the card on every
        // connected client; ignoring these left a stale prompt that a reload
        // then re-raised from history.
        const requestId = resolvedRequestId(payload)
        if (requestId) this.onPermissionResolved?.(requestId)
        break
      }

      // ---- 忽略的事件类型 ----
      case 'partial_assistant':
      case 'automation_state':
      case 'status':
        break
    }
  }

  /**
   * Send a control_request to the CLI bridge. The CLI answers with a
   * control_response event, surfaced through onControlResponse.
   */
  async sendControlRequest(request: Record<string, unknown>): Promise<string> {
    const requestId = generateMessageUuid()
    await apiSendEvent(this.sessionId, {
      type: 'control_request',
      request_id: requestId,
      request,
    })
    return requestId
  }

  setModel(model: string): Promise<string> {
    return this.sendControlRequest({
      subtype: 'set_model',
      model: model === 'default' ? undefined : model,
    })
  }

  setPermissionMode(mode: string): Promise<string> {
    return this.sendControlRequest({ subtype: 'set_permission_mode', mode })
  }

  /** Change the model effort level; the CLI applies it to the live session. */
  setEffort(effort: string): Promise<string> {
    return this.sendControlRequest({ subtype: 'set_effort', effort })
  }

  /** Toggle session-only Ultracode. */
  setUltracode(enabled: boolean): Promise<string> {
    return this.sendControlRequest({ subtype: 'set_ultracode', enabled })
  }

  /** 发送用户消息 */
  async sendMessage(text: string, images?: UserMessageImage[]): Promise<void> {
    if (!text.trim() && (!images || images.length === 0)) return

    const uuid = generateMessageUuid()
    // Add user message to entries
    const userEntry: UserMessageEntry = {
      type: 'user_message',
      id: `user-${uuid}`,
      content: text,
      images: images && images.length > 0 ? images : undefined,
      // The CLI queues a message that arrives mid-turn; until it echoes the
      // message back the bubble stays muted ("排队中"), matching the terminal.
      pending: true,
    }
    this.optimisticUserIds.add(userEntry.id)
    this.setEntries(prev => [...prev, userEntry])

    // Images travel as Anthropic content blocks: the CLI's bridge inbound
    // path already normalizes them (inboundMessages.normalizeImageBlocks), and
    // the server forwards the original block array for user turns.
    const content =
      images && images.length > 0
        ? [
            ...(text.trim() ? [{ type: 'text' as const, text }] : []),
            ...images.map(image => ({
              type: 'image' as const,
              source: {
                type: 'base64' as const,
                media_type: image.mimeType,
                data: image.data,
              },
            })),
          ]
        : text
    // Send to backend; a failed POST must not leave a phantom message that
    // never reached the terminal.
    try {
      await apiSendEvent(this.sessionId, {
        type: 'user',
        uuid,
        content,
      })
    } catch (error) {
      this.optimisticUserIds.delete(userEntry.id)
      this.setEntries(prev =>
        prev.filter(
          entry =>
            !(entry.type === 'user_message' && entry.id === userEntry.id),
        ),
      )
      throw error
    }
  }

  /** 响应权限请求 */
  async respondPermission(
    requestId: string,
    approved: boolean,
    extra?: Record<string, unknown>,
  ): Promise<void> {
    await apiSendControl(this.sessionId, {
      type: 'permission_response',
      approved,
      request_id: requestId,
      ...extra,
    })

    // Update tool call status
    this.setEntries(prev =>
      prev.map(entry => {
        if (entry.type !== 'tool_call') return entry
        if (entry.toolCall.permissionRequest?.requestId !== requestId)
          return entry
        return {
          type: 'tool_call',
          toolCall: {
            ...entry.toolCall,
            status: approved ? 'running' : ('rejected' as ToolCallStatus),
            permissionRequest: undefined,
          },
        }
      }),
    )
  }

  /** 中断当前操作 */
  async interrupt(): Promise<void> {
    await apiInterrupt(this.sessionId)

    // Mark running tools as canceled — only after the interrupt really
    // landed. Marking optimistically left cards claiming 已取消 when the
    // POST failed and the turn kept running.
    this.setEntries(prev =>
      prev.map(entry => {
        if (entry.type !== 'tool_call') return entry
        if (
          entry.toolCall.status !== 'running' &&
          entry.toolCall.status !== 'waiting_for_confirmation'
        )
          return entry
        return {
          type: 'tool_call',
          toolCall: {
            ...entry.toolCall,
            status: 'canceled' as ToolCallStatus,
            permissionRequest: undefined,
          },
        }
      }),
    )
  }
}
