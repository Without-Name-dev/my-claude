/**
 * Local project/session catalog and transcript reading.
 *
 * Reuses the CLI's own portable listing code (`listSessionsImpl`) so the
 * Spawner reports exactly what the CLI would list. Transcripts are read as
 * JSONL streams; pagination keeps memory bounded even for very large files.
 */

import { createReadStream } from 'node:fs'
import { basename } from 'node:path'
import { createInterface } from 'node:readline'
import {
  listSessionsImpl,
  type SessionInfo,
} from '../../../src/utils/listSessionsImpl'
import {
  resolveSessionFilePath,
  sanitizePath,
} from '../../../src/utils/sessionStoragePortable'
import {
  isBridgeSessionId,
  type ProjectSummary,
  type SessionSummary,
  type TranscriptBlock,
  type TranscriptMessage,
  type TranscriptUsage,
} from '../../remote-control-server/src/device/protocol'
import {
  isProcessAlive,
  listBunProcesses,
  readRegistryWithReal,
  samePath,
  startedAfterRegistration,
  type BunProcess,
  type RegistryEntry,
} from './registry'

const PROJECT_CACHE_MS = 20_000

type ProjectCache = {
  at: number
  projects: ProjectSummary[]
}

let projectCache: ProjectCache | undefined

export function invalidateCatalog(): void {
  projectCache = undefined
}

function toSummary(info: SessionInfo): SessionSummary {
  const cwd = info.cwd ?? ''
  return {
    localSessionId: info.sessionId,
    projectId: cwd ? sanitizePath(cwd) : 'unknown',
    cwd,
    title:
      info.customTitle || info.summary || info.firstPrompt || info.sessionId,
    summary: info.summary || info.firstPrompt || '',
    updatedAt: info.lastModified,
    createdAt: info.createdAt,
    fileSize: info.fileSize,
    gitBranch: info.gitBranch,
  }
}

export async function listProjects(
  configDir?: string,
): Promise<ProjectSummary[]> {
  if (projectCache && Date.now() - projectCache.at < PROJECT_CACHE_MS) {
    return projectCache.projects
  }
  const infos = await listSessionsImpl({})
  const live = configDir
    ? await fetchFreshTerminalSessions(configDir, infos.map(toSummary))
    : []
  const byPath = new Map<string, ProjectSummary>()
  const addSession = (cwd: string, lastModified: number) => {
    const existing = byPath.get(cwd)
    if (existing) {
      existing.sessionCount += 1
      existing.lastActivityAt = Math.max(
        existing.lastActivityAt ?? 0,
        lastModified,
      )
    } else {
      byPath.set(cwd, {
        projectId: sanitizePath(cwd),
        path: cwd,
        name: basename(cwd) || cwd,
        sessionCount: 1,
        lastActivityAt: lastModified,
      })
    }
  }
  for (const info of infos) {
    if (!info.cwd) continue
    addSession(info.cwd, info.lastModified)
  }
  for (const session of live) {
    addSession(session.cwd, session.updatedAt)
  }
  const projects = [...byPath.values()].sort(
    (a, b) => (b.lastActivityAt ?? 0) - (a.lastActivityAt ?? 0),
  )
  projectCache = { at: Date.now(), projects }
  return projects
}

export async function listSessions(
  options: {
    projectId?: string
    projectPath?: string
    limit?: number
    offset?: number
  },
  configDir?: string,
): Promise<{ sessions: SessionSummary[]; hasMore: boolean }> {
  const limit = Math.min(Math.max(options.limit ?? 200, 1), 500)
  const offset = Math.max(options.offset ?? 0, 0)

  let dir = options.projectPath
  if (!dir && options.projectId) {
    const projects = await listProjects(configDir)
    dir = projects.find(
      project => project.projectId === options.projectId,
    )?.path
    if (!dir) return { sessions: [], hasMore: false }
  }

  // Live terminals must be merged before applying the offset. Fetch a prefix
  // large enough to fill this page plus one item for an accurate hasMore.
  // Without this, the offset was applied by listSessionsImpl and a second
  // time after merging, silently skipping older conversations.
  const listingLimit = configDir ? offset + limit + 1 : limit + 1
  const listingOffset = configDir ? 0 : offset
  const listed = await listSessionsImpl(
    dir
      ? { dir, limit: listingLimit, offset: listingOffset }
      : { limit: listingLimit, offset: listingOffset },
  )
  const summaries = listed.map(toSummary)
  if (configDir) {
    const live = await fetchFreshTerminalSessions(configDir, summaries)
    const matching = dir ? live.filter(s => samePath(s.cwd, dir)) : live
    const merged = [...matching, ...summaries].sort(
      (a, b) =>
        b.updatedAt - a.updatedAt ||
        b.localSessionId.localeCompare(a.localSessionId),
    )
    return {
      sessions: merged.slice(offset, offset + limit),
      hasMore: merged.length > offset + limit,
    }
  }
  return {
    sessions: summaries.slice(0, limit),
    hasMore: summaries.length > limit,
  }
}

/**
 * Live `--remote-control` terminals whose session has no transcript yet.
 *
 * A brand-new session only writes its first JSONL line when the first message
 * arrives, so right after 新会话 the file-based catalog cannot see it and the
 * web lands on "找不到该本地会话". The CLI's own PID registry does know it.
 * Only terminals whose live command line has no `--resume` qualify: a resumed
 * terminal re-mints its registry sessionId while writing to the original
 * transcript, and listing that id would duplicate an existing session.
 */
export function selectFreshTerminalSessions(
  entries: RegistryEntry[],
  processes: Map<number, BunProcess>,
  listed: SessionSummary[],
): SessionSummary[] {
  const known = new Set(
    listed.map(session => session.localSessionId.toLowerCase()),
  )
  const fresh: SessionSummary[] = []
  for (const entry of entries) {
    if (!entry.cwd || known.has(entry.sessionId.toLowerCase())) continue
    if (!entry.bridgeSessionId || !isBridgeSessionId(entry.bridgeSessionId))
      continue
    const proc = processes.get(entry.pid)
    if (!proc) continue
    const isAgentRuntime =
      /^(bun|node|claude)\.exe$/i.test(proc.name) ||
      /cli\.js/i.test(proc.commandLine)
    if (!isAgentRuntime) continue
    if (/--resume\s/i.test(proc.commandLine)) continue
    if (startedAfterRegistration(proc.startedAt, entry.startedAt)) {
      continue
    }
    fresh.push({
      localSessionId: entry.sessionId,
      projectId: sanitizePath(entry.cwd),
      cwd: entry.cwd,
      title: '新会话',
      summary: '',
      updatedAt: entry.updatedAt ?? entry.startedAt ?? Date.now(),
      createdAt: entry.startedAt,
      fileSize: 0,
    })
  }
  return fresh
}

async function fetchFreshTerminalSessions(
  configDir: string,
  listed: SessionSummary[],
): Promise<SessionSummary[]> {
  try {
    const [entries, processes] = await Promise.all([
      readRegistryWithReal(configDir),
      listBunProcesses(),
    ])
    return selectFreshTerminalSessions(entries, processes, listed)
  } catch {
    return []
  }
}

async function isLiveRegistrySession(
  configDir: string,
  localSessionId: string,
): Promise<boolean> {
  try {
    const entries = await readRegistryWithReal(configDir)
    return entries.some(
      entry =>
        entry.sessionId.toLowerCase() === localSessionId.toLowerCase() &&
        isProcessAlive(entry.pid),
    )
  } catch {
    return false
  }
}

function stringifyToolResult(content: unknown): string {
  if (typeof content === 'string') return content
  if (Array.isArray(content)) {
    const parts: string[] = []
    for (const block of content) {
      if (block && typeof block === 'object' && 'text' in block) {
        const text = (block as { text?: unknown }).text
        if (typeof text === 'string') parts.push(text)
      }
    }
    return parts.join('\n')
  }
  return ''
}

function normalizeBlocks(content: unknown): TranscriptBlock[] {
  if (typeof content === 'string') {
    return content.length > 0 ? [{ type: 'text', text: content }] : []
  }
  if (!Array.isArray(content)) return []
  const blocks: TranscriptBlock[] = []
  for (const raw of content) {
    if (!raw || typeof raw !== 'object') continue
    const block = raw as Record<string, unknown>
    switch (block.type) {
      case 'text': {
        if (typeof block.text === 'string' && block.text.length > 0) {
          blocks.push({ type: 'text', text: block.text })
        }
        break
      }
      case 'thinking': {
        const text =
          typeof block.thinking === 'string'
            ? block.thinking
            : typeof block.text === 'string'
              ? block.text
              : ''
        if (text.length > 0) blocks.push({ type: 'thinking', text })
        break
      }
      case 'redacted_thinking':
        blocks.push({ type: 'thinking', text: '[已隐藏的思考]' })
        break
      case 'tool_use': {
        if (typeof block.name !== 'string') break
        blocks.push({
          type: 'tool_use',
          id: typeof block.id === 'string' ? block.id : undefined,
          name: block.name,
          input: block.input,
        })
        break
      }
      case 'tool_result': {
        blocks.push({
          type: 'tool_result',
          toolUseId:
            typeof block.tool_use_id === 'string'
              ? block.tool_use_id
              : undefined,
          text: stringifyToolResult(block.content),
          isError: block.is_error === true,
        })
        break
      }
      case 'image': {
        // Keep the base64 payload so photos survive history reloads on the
        // phone; cap the size to keep one page from ballooning.
        const source =
          block.source && typeof block.source === 'object'
            ? (block.source as Record<string, unknown>)
            : undefined
        if (
          source &&
          source.type === 'base64' &&
          typeof source.data === 'string' &&
          source.data.length > 0 &&
          source.data.length <= 6_000_000
        ) {
          blocks.push({
            type: 'image',
            mimeType:
              typeof source.media_type === 'string'
                ? source.media_type
                : 'image/png',
            data: source.data,
          })
        }
        break
      }
      default:
        break
    }
  }
  return blocks
}

function textFromBlocks(blocks: TranscriptBlock[]): string {
  return blocks
    .filter(
      (block): block is { type: 'text'; text: string } => block.type === 'text',
    )
    .map(block => block.text)
    .join('\n')
}

function normalizeEntry(
  entry: Record<string, unknown>,
  index: number,
): TranscriptMessage | undefined {
  if (entry.isSidechain === true || entry.isMeta === true) return undefined
  const message = entry.message
  if (!message || typeof message !== 'object' || Array.isArray(message)) {
    return undefined
  }
  const record = message as Record<string, unknown>
  const role = record.role
  if (role !== 'user' && role !== 'assistant') return undefined
  const blocks = normalizeBlocks(record.content)
  if (blocks.length === 0) return undefined
  const id =
    typeof entry.uuid === 'string' && entry.uuid.length > 0
      ? entry.uuid
      : `entry-${index}`
  return {
    id,
    role,
    text: textFromBlocks(blocks),
    timestamp:
      typeof entry.timestamp === 'string' ? entry.timestamp : undefined,
    // Same extraction rules as the terminal's usage hydration
    // (usageHydration.ts): assistant entries only, synthetic models skipped.
    ...(role === 'assistant' ? { usage: usageFromEntry(record) } : {}),
    blocks,
    isMeta: entry.isMeta === true,
  }
}

export type ReadSessionResult = {
  messages: TranscriptMessage[]
  before?: string
  hasMore: boolean
  total: number
}

/** Extract a transcript assistant message's billed usage, if present. */
function usageFromEntry(
  record: Record<string, unknown>,
): TranscriptUsage | undefined {
  const usage = record.usage
  if (!usage || typeof usage !== 'object' || Array.isArray(usage))
    return undefined
  if (record.model === '<synthetic>') return undefined
  const u = usage as Record<string, unknown>
  const result = {
    inputTokens: Number(u.input_tokens) || 0,
    outputTokens: Number(u.output_tokens) || 0,
    cacheReadInputTokens: Number(u.cache_read_input_tokens) || 0,
    cacheCreationInputTokens: Number(u.cache_creation_input_tokens) || 0,
  }
  if (
    result.inputTokens === 0 &&
    result.outputTokens === 0 &&
    result.cacheReadInputTokens === 0 &&
    result.cacheCreationInputTokens === 0
  ) {
    return undefined
  }
  return result
}

/**
 * Read a transcript page, newest-first by default.
 *
 * `before` is a message id: return the `limit` messages immediately older
 * than it. Streaming keeps memory at O(limit) even for multi-hundred-MB
 * transcripts.
 */
export async function readSession(options: {
  localSessionId: string
  cwd?: string
  limit?: number
  before?: string
  configDir?: string
}): Promise<ReadSessionResult> {
  const limit = Math.min(Math.max(options.limit ?? 100, 1), 200)
  const resolved = await resolveSessionFilePath(
    options.localSessionId,
    options.cwd,
  )
  if (!resolved) {
    // A brand-new terminal has no transcript until its first message; report
    // an empty conversation instead of an error the web renders as broken.
    if (
      options.configDir &&
      (await isLiveRegistrySession(options.configDir, options.localSessionId))
    ) {
      return { messages: [], hasMore: false, total: 0 }
    }
    throw new Error('找不到该会话的记录文件')
  }

  const collected: TranscriptMessage[] = []
  let total = 0
  let reachedCursor = false
  let moreBefore = false
  let firstReturnedId: string | undefined

  const stream = createReadStream(resolved.filePath, { encoding: 'utf8' })
  const lines = createInterface({
    input: stream,
    crlfDelay: Number.POSITIVE_INFINITY,
  })
  try {
    let index = 0
    for await (const line of lines) {
      if (!line.trim()) continue
      let entry: unknown
      try {
        entry = JSON.parse(line)
      } catch {
        continue
      }
      if (!entry || typeof entry !== 'object' || Array.isArray(entry)) continue
      const message = normalizeEntry(entry as Record<string, unknown>, index++)
      if (!message) continue
      total += 1

      if (options.before) {
        if (message.id === options.before) {
          reachedCursor = true
          break
        }
        collected.push(message)
        if (collected.length > limit) {
          moreBefore = true
          collected.shift()
        }
      } else {
        collected.push(message)
        if (collected.length > limit) collected.shift()
      }
    }
  } finally {
    lines.close()
    stream.close()
  }

  if (options.before && !reachedCursor) {
    // The cursor no longer exists (compaction or a deleted transcript). The
    // honest answer is "this page is not available", not a silently wrong
    // slice from the beginning.
    return { messages: [], hasMore: false, total }
  }

  firstReturnedId = collected[0]?.id
  const hasMore = options.before ? moreBefore : total > collected.length

  return {
    messages: collected,
    ...(firstReturnedId && hasMore ? { before: firstReturnedId } : {}),
    hasMore,
    total,
  }
}
