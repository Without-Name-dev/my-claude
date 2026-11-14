/**
 * Device Spawner protocol (v1).
 *
 * A Spawner is a resident process on each operator machine. It connects out
 * to the RCS over WebSocket, lists local Claude sessions, and — only when the
 * server asks — opens a visible interactive terminal for one historical
 * session. Conversation content itself never flows through the Spawner: the
 * terminal's REPL Bridge talks to the RCS directly.
 *
 * Honesty rules encoded in these types:
 * - "online" means the Spawner WebSocket is up, nothing about terminals.
 * - "processAlive" and "bridgeSessionId" are separate evidence with their own
 *   verification timestamps; the UI must not derive one from the other.
 * - An `open_session` result is an acknowledgement, not a success. Progress
 *   arrives as `operation` events until `ready` or `failed`.
 */

export const DEVICE_PROTOCOL_VERSION = 1

export const DEVICE_METHODS = [
  'list_projects',
  'list_sessions',
  'read_session',
  'list_directory',
  'upload_files',
  'copy_paths',
  'open_session',
  'open_new_session',
  'close_session',
  'verify_state',
] as const

export type DeviceMethod = (typeof DEVICE_METHODS)[number]

export type DeviceCapability = DeviceMethod | 'heartbeat'

export type ProjectSummary = {
  /** Stable sanitized project id (the CLI's project directory name). */
  projectId: string
  /** Absolute working directory the project represents. */
  path: string
  name: string
  sessionCount: number
  lastActivityAt: number | null
}

export type SessionSummary = {
  localSessionId: string
  projectId: string
  cwd: string
  title: string
  summary: string
  updatedAt: number
  createdAt?: number
  messageCount?: number
  fileSize?: number
  gitBranch?: string
}

export type TranscriptBlock =
  | { type: 'text'; text: string }
  | { type: 'thinking'; text: string }
  | { type: 'tool_use'; id?: string; name: string; input?: unknown }
  | { type: 'tool_result'; toolUseId?: string; text: string; isError?: boolean }
  | { type: 'image'; mimeType?: string; data?: string }

export type TranscriptMessage = {
  /** Stable id: the transcript entry uuid when present. */
  id: string
  role: 'user' | 'assistant'
  text: string
  timestamp?: string
  blocks: TranscriptBlock[]
  isMeta?: boolean
  /**
   * Billed usage of this assistant message, when the source carries it. The
   * web's token/cache-rate row seeds from history with the same numbers the
   * terminal's status line hydrates from the transcript, so a reload shows
   * the same rate instead of restarting from zero.
   */
  usage?: TranscriptUsage
}

export type TranscriptUsage = {
  inputTokens: number
  outputTokens: number
  cacheReadInputTokens: number
  cacheCreationInputTokens: number
}

/**
 * Content of an outbound user event on the bridge wire. Plain text stays a
 * string; a message with photos travels as Anthropic content blocks, which
 * the CLI's inbound path normalizes (inboundMessages.normalizeImageBlocks).
 */
export type UserContentBlock =
  | { type: 'text'; text: string }
  | {
      type: 'image'
      source: { type: 'base64'; media_type: string; data: string }
    }

export type UserContent = string | UserContentBlock[]

/** One child of a directory the operator browsed on their own machine. */
export type DirectoryEntry = {
  name: string
  /** Absolute path the server can echo back on the next request. */
  path: string
  type: 'dir' | 'file'
  /** Files only; bytes. */
  size?: number
  modifiedAt?: number
}

/** A starting point when no path has been chosen yet. */
export type DirectoryRoot = {
  name: string
  path: string
  kind: 'drive' | 'home' | 'volume'
}

export type DirectoryListing = {
  /** The directory that was listed; absent when `roots` is returned. */
  path?: string
  parent?: string
  entries: DirectoryEntry[]
  roots?: DirectoryRoot[]
  /** The listing was cut at the entry cap. */
  truncated?: boolean
}

/**
 * A file pushed from the browser (usually a phone) onto the operator machine.
 * `name` is relative and may contain directories; it is validated by the
 * Spawner and always written below one fresh upload folder.
 */
export type UploadFileInput = {
  name: string
  /** Base64, no data: prefix. */
  data: string
}

export type UploadedFile = {
  name: string
  /** Absolute path on the operator machine; safe to reference in a message. */
  path: string
  bytes: number
}

export type UploadResult = {
  /** The session directory the files were written into. */
  root: string
  files: UploadedFile[]
}

export type CopiedEntry = {
  source: string
  /** Where the entry now lives (the source itself when it was already inside). */
  path: string
  type: 'dir' | 'file'
  /** False when the source already lived inside the session directory. */
  copied: boolean
}

export type CopyResult = {
  entries: CopiedEntry[]
}

export type SessionRuntimeState = {
  localSessionId: string
  cwd: string
  /** A process that the registry says is running this session. */
  processAlive: boolean
  pid?: number
  procStart?: string
  /** Bridge session id reported by the CLI's own session registry. */
  bridgeSessionId?: string
  /** When the Spawner last confirmed the above (epoch ms). */
  verifiedAt: number
  /** How the state was determined, for debugging and honesty in the UI. */
  source: 'registry' | 'managed' | 'scan'
}

export type OpenOperationState =
  | 'accepted'
  | 'starting'
  | 'terminal_spawned'
  | 'bridge_connecting'
  | 'ready'
  | 'failed'
  | 'waiting_for_user'

export type OpenFailureCode =
  | 'session_not_found'
  | 'cwd_mismatch'
  | 'config_dir_mismatch'
  | 'already_running_without_bridge'
  | 'spawn_failed'
  | 'bridge_timeout'
  | 'process_exited'
  | 'busy'
  | 'unsupported'

export type DeviceMethodParams = {
  list_projects: Record<string, never>
  list_sessions: {
    projectId?: string
    projectPath?: string
    limit?: number
    offset?: number
  }
  read_session: {
    localSessionId: string
    cwd?: string
    /** Return at most this many messages (tail-first when `before` is set). */
    limit?: number
    /** Opaque cursor from a previous response; returns messages before it. */
    before?: string
  }
  list_directory: {
    /** Absolute directory to list; omitted returns the roots (drives, home). */
    path?: string
  }
  upload_files: {
    files: UploadFileInput[]
    /**
     * The session's working directory. Uploads land here (never in a global
     * cache folder) so the terminal can use them as project files.
     */
    cwd: string
  }
  copy_paths: {
    cwd: string
    /** Absolute paths (files or directories) to bring into `cwd`. */
    paths: string[]
  }
  open_session: {
    localSessionId: string
    cwd: string
    configDir: string
    operationId: string
    /**
     * The user explicitly chose to replace a terminal that is running without
     * remote control. Only then may the Spawner stop that process; without
     * this flag an alive process is reported and left alone.
     */
    force?: boolean
  }
  open_new_session: {
    /** Fresh terminal: no --resume; the CLI mints a new session id. */
    cwd: string
    configDir: string
    operationId: string
  }
  close_session: {
    localSessionId: string
    cwd?: string
    /** Bridge id the server associates with the session, for matching. */
    bridgeSessionId?: string
  }
  verify_state: {
    localSessionId: string
    cwd?: string
    /**
     * The bridge id the server already associates with this local session.
     * After a Spawner restart this is the only reliable way to find the
     * process: the CLI's PID file can carry a different sessionId after
     * `--resume`, while the bridge id is the one it actually connected with.
     */
    bridgeSessionId?: string
  }
}

export type DeviceMethodResults = {
  list_projects: { projects: ProjectSummary[] }
  list_sessions: { sessions: SessionSummary[]; hasMore: boolean }
  read_session: {
    messages: TranscriptMessage[]
    /** Cursor for the next older page; absent when the start is reached. */
    before?: string
    hasMore: boolean
    total?: number
  }
  list_directory: DirectoryListing
  upload_files: UploadResult
  copy_paths: CopyResult
  open_session: { accepted: true }
  open_new_session: { accepted: true }
  close_session: {
    closed: boolean
    pid?: number
    /** Why it was not closed (already gone is still "closed"). */
    detail?: string
  }
  verify_state: { state: SessionRuntimeState }
}

export type DeviceEvent =
  | {
      type: 'operation'
      operationId: string
      localSessionId: string
      state: OpenOperationState
      detail?: string
      failureCode?: OpenFailureCode
      bridgeSessionId?: string
      pid?: number
    }
  | { type: 'session_state'; states: SessionRuntimeState[] }
  | { type: 'catalog_changed'; reason?: string }

export type DeviceClientMessage =
  | {
      type: 'hello'
      protocol: number
      deviceId: string
      name: string
      configDir: string
      platform: string
      version: string
      capabilities: DeviceCapability[]
      /**
       * Enrollment secret on first contact. Later connections authenticate
       * with the device token as the WebSocket bearer and may omit it.
       */
      secret?: string
    }
  | { type: 'heartbeat'; ts: number; running?: SessionRuntimeState[] }
  | { type: 'response'; id: string; ok: true; result: unknown }
  | {
      type: 'response'
      id: string
      ok: false
      error: { code: string; message: string }
    }
  | { type: 'event'; event: DeviceEvent }

export type DeviceServerMessage =
  | { type: 'ready'; deviceId: string; serverTime: number }
  | { type: 'request'; id: string; method: DeviceMethod; params: unknown }
  | { type: 'heartbeat_ack'; ts: number }
  | { type: 'error'; code: string; message: string }

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function stringField(
  value: Record<string, unknown>,
  key: string,
  max = 4096,
): string | undefined {
  const raw = value[key]
  return typeof raw === 'string' && raw.length > 0 && raw.length <= max
    ? raw
    : undefined
}

export function optionalStringField(
  value: Record<string, unknown>,
  key: string,
  max = 4096,
): string | undefined {
  return stringField(value, key, max)
}

export function numberField(
  value: Record<string, unknown>,
  key: string,
): number | undefined {
  const raw = value[key]
  return typeof raw === 'number' && Number.isFinite(raw) ? raw : undefined
}

/** Session ids are UUIDs from the CLI; reject anything else. */
export function isLocalSessionId(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
    value,
  )
}

/** Bridge ids use the CLI's `session_<hex>` compat form. */
export function isBridgeSessionId(value: string): boolean {
  return /^session_[0-9a-z]{8,64}$/i.test(value)
}

export function isDeviceId(value: string): boolean {
  return /^[A-Za-z0-9_-]{1,128}$/.test(value)
}

export function parseDeviceMethod(value: unknown): DeviceMethod | undefined {
  return typeof value === 'string' &&
    (DEVICE_METHODS as readonly string[]).includes(value)
    ? (value as DeviceMethod)
    : undefined
}
