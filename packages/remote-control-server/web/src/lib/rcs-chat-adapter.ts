import type { SetStateAction } from 'react'
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
import { mergeThreadEntries } from './device-history'
import { debugLog } from './debug-log'
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
  if (
    synthetic ||
    /^\s*<(task-notification|system-reminder)\b/i.test(content) ||
    /<local-command-caveat>|<command-name>|<command-message>|<command-args>/.test(
      content,
    )
  ) {
    return { kind: 'drop' }
  }
  const text = content.replace(/<\/?[a-z][\w-]*>/gi, '').trim()
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

class SSEBus {
  private listeners: Set<SSEEventHandler> = new Set()
  private eventSource: EventSource | null = null
  private onError: ((message: string) => void) | null = null
  private onOpen: (() => void) | null = null
  private failures = 0
  private lastProbe = 0
  /** Server sequence cursor: a reconnect may replay events we already have. */
  private lastSeqNum = 0
  private seqSessionId = ''

  onEvent(handler: SSEEventHandler): () => void {
    this.listeners.add(handler)
    return () => this.listeners.delete(handler)
  }

  connect(
    sessionId: string,
    onSseError?: (message: string) => void,
    onStreamOpen?: () => void,
  ): EventSource {
    this.disconnect()
    this.onError = onSseError ?? null
    this.onOpen = onStreamOpen ?? null
    this.failures = 0
    this.lastProbe = 0
    // Keep the cursor across reconnects of the same session; reset it when the
    // bus is reused for a different one.
    if (this.seqSessionId !== sessionId) {
      this.lastSeqNum = 0
      this.seqSessionId = sessionId
    }
    const uuid = getUuid()
    const url = `/web/sessions/${sessionId}/events?uuid=${encodeURIComponent(uuid)}`
    const es = new EventSource(url)
    this.eventSource = es

    es.addEventListener('message', (e: MessageEvent) => {
      this.failures = 0
      try {
        const data = JSON.parse(e.data) as SessionEvent
        // Auto-replay after a reconnect re-sends everything since the cursor;
        // without this the duplicate assistant text/tool cards piled up.
        if (typeof data.seqNum === 'number') {
          if (data.seqNum <= this.lastSeqNum) return
          this.lastSeqNum = data.seqNum
        }
        for (const handler of this.listeners) {
          handler(data)
        }
      } catch {
        // ignore parse errors
      }
    })

    es.onopen = () => {
      this.failures = 0
      // The server has subscribed to the bus before answering, so anything
      // produced earlier is only in the durable store. Let the view catch up.
      this.onOpen?.()
    }

    // A *closed* session answers 409 forever, and the browser's built-in
    // retry would hammer it. A transient network blip also fires onerror but
    // should reconnect. Probe the session before deciding which this is.
    es.onerror = () => {
      if (this.eventSource !== es) return
      const now = Date.now()
      if (now - this.lastProbe < 2000) return
      this.lastProbe = now
      this.failures += 1
      if (this.failures >= 5) {
        this.fail(es, sessionId, '无法连接会话，终端可能已关闭')
        return
      }
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
  commands: Array<{ name: string; description: string }>
  models: Array<{ id: string; name: string; description?: string }>
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
  private onStatusChange?: (status: string) => void
  private onError?: (error: string) => void
  private onPermissionRequest?: (permission: PendingPermission) => void
  private onPermissionResolved?: (requestId: string) => void
  private onSessionInfo?: (info: BridgeSessionInfo) => void
  private onControlResponse?: (response: BridgeControlResponse) => void
  private onBridgeState?: (connected: boolean) => void
  private onTurnComplete?: () => void
  private onStreamOpen?: () => void
  private historyMode: 'full' | 'metadata'
  private disposed = false
  // Direct handleEvent() calls (and tests) work without init(); init() flips
  // this off while the history snapshot is being merged.
  private historyReady = true
  private bufferedEvents: SessionEvent[] = []

  constructor(
    sessionId: string,
    setEntries: React.Dispatch<SetStateAction<ThreadEntry[]>>,
    options?: {
      onStatusChange?: (status: string) => void
      onError?: (error: string) => void
      onPermissionRequest?: (permission: PendingPermission) => void
      /** The request was answered elsewhere (the terminal, another device). */
      onPermissionResolved?: (requestId: string) => void
      onSessionInfo?: (info: BridgeSessionInfo) => void
      onControlResponse?: (response: BridgeControlResponse) => void
      onBridgeState?: (connected: boolean) => void
      onTurnComplete?: () => void
      /** Fired once the live SSE stream is actually open. */
      onStreamOpen?: () => void
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
    this.onStreamOpen = options?.onStreamOpen
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
          const updated = applyToolResult(historyEntries, asRecord)
          historyEntries.length = 0
          historyEntries.push(...updated)
          continue
        }
        const text = extractEventText(payload)
        if (text) {
          const synthetic =
            (payload as Record<string, unknown>).isSynthetic === true ||
            (payload.raw as Record<string, unknown> | undefined)
              ?.isSynthetic === true
          const classified = classifyLocalCommandText(text, synthetic)
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
                  status: 'complete',
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
            status: 'complete',
            rawInput: (p.tool_input as Record<string, unknown>) || {},
          },
        }
        historyEntries.push(tc)
      } else if (event.type === 'tool_result') {
        const updated = applyToolResult(
          historyEntries,
          payload as Record<string, unknown>,
        )
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
      } else if (event.type === 'control_response') {
        const response = (payload as unknown as Record<string, unknown>)
          .response as Record<string, unknown> | undefined
        if (typeof response?.request_id === 'string') {
          answeredPermissions.add(response.request_id)
          this.onPermissionResolved?.(response.request_id)
        }
      }
    }

    flushAssistant()
    for (const permission of pendingPermissions) {
      if (!answeredPermissions.has(permission.requestId)) {
        this.onPermissionRequest?.(permission)
      }
    }
    if (this.historyMode === 'metadata') return
    if (this.disposed) return
    // The parent may already have loaded the server-cached transcript (the
    // device history survives terminal restarts). Merge instead of replacing
    // so the bridge replay only fills the gaps.
    this.setEntries(prev => mergeThreadEntries(prev, historyEntries))
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
    )
    this.unsub = sseBus.onEvent(event => this.handleEvent(event))
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
          this.setEntries(prev => applyToolResult(prev, asRecord))
          break
        }
        const content = extractEventText(payload)
        if (!content) break
        const synthetic =
          (payload as Record<string, unknown>).isSynthetic === true ||
          (payload.raw as Record<string, unknown> | undefined)?.isSynthetic ===
            true
        const classified = classifyLocalCommandText(content, synthetic)
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
        this.setEntries(prev => {
          if (
            prev.some(entry => entry.type === 'user_message' && entry.id === id)
          ) {
            return prev
          }
          // A message sent from the device view is shown optimistically with
          // a temporary id; adopt the echo instead of adding a second bubble.
          const pending = prev.findIndex(
            entry =>
              entry.type === 'user_message' &&
              entry.id.startsWith('pending-') &&
              entry.content === cleanContent,
          )
          if (pending >= 0) {
            const next = [...prev]
            next[pending] = { type: 'user_message', id, content: cleanContent }
            return next
          }
          return [...prev, { type: 'user_message', id, content: cleanContent }]
        })
        break
      }

      // ---- 助手消息 ----
      case 'assistant': {
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
        this.setEntries(prev => {
          // A reconnecting bridge re-flushes its initial transcript as fresh
          // inbound events. Anything whose id is already on screen is that
          // replay, not a new turn — dropping it here is what stops old
          // messages from suddenly appearing at the bottom of the chat.
          const seen = prev.some(
            entry =>
              entry.type === 'assistant_message' && entry.id === assistantId,
          )
          if (seen) return prev
          const lastEntry = prev[prev.length - 1]

          // Append only to the same message; a different uuid is a distinct
          // assistant turn (e.g. after a tool call).
          if (
            lastEntry?.type === 'assistant_message' &&
            lastEntry.id === assistantId
          ) {
            const lastChunk = lastEntry.chunks[lastEntry.chunks.length - 1]
            if (lastChunk?.type === 'message') {
              return [
                ...prev.slice(0, -1),
                {
                  ...lastEntry,
                  chunks: [
                    ...lastEntry.chunks.slice(0, -1),
                    { type: 'message', text: lastChunk.text + content },
                  ],
                },
              ]
            }
            return [
              ...prev.slice(0, -1),
              {
                ...lastEntry,
                chunks: [
                  ...lastEntry.chunks,
                  { type: 'message', text: content },
                ],
              },
            ]
          }

          // Create new AssistantMessage
          if (content && content.trim()) {
            const newEntry: AssistantMessageEntry = {
              type: 'assistant_message',
              id: assistantId,
              chunks: [{ type: 'message', text: content }],
            }
            return [...prev, newEntry]
          }
          return prev
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

          // Update tool call status
          this.setEntries(prev => {
            // Find matching tool call
            const idx = [...prev]
              .reverse()
              .findIndex(e => e.type === 'tool_call')
            if (idx >= 0) {
              const realIdx = prev.length - 1 - idx
              const entry = prev[realIdx] as ToolCallEntry
              if (entry.toolCall.status === 'running') {
                return prev.map((e, i) =>
                  i === realIdx
                    ? {
                        type: 'tool_call',
                        toolCall: {
                          ...entry.toolCall,
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
          this.onStatusChange?.(payload.status)
        }
        break
      }

      // ---- 错误 ----
      case 'error': {
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
        this.onTurnComplete?.()
        break
      }

      case 'system': {
        // Slash commands (and the CLI's "not available over Remote Control"
        // replies) arrive as system/local_command. They used to be dropped,
        // so a command looked like it did nothing.
        const record = payload as Record<string, unknown>
        if (record.subtype === 'local_command') {
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

      // ---- 忽略的事件类型 ----
      case 'partial_assistant':
      case 'permission_response':
      case 'task_state':
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
    }
    this.setEntries(prev => [...prev, userEntry])

    // Send to backend; a failed POST must not leave a phantom message that
    // never reached the terminal.
    try {
      await apiSendEvent(this.sessionId, {
        type: 'user',
        uuid,
        content: text,
        message: { content: text },
      })
    } catch (error) {
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
    // Mark running tools as canceled
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

    await apiInterrupt(this.sessionId)
  }
}
