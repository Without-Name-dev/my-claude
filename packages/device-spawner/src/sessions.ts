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
import type {
  ProjectSummary,
  SessionSummary,
  TranscriptBlock,
  TranscriptMessage,
} from '../../remote-control-server/src/device/protocol'

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

export async function listProjects(): Promise<ProjectSummary[]> {
  if (projectCache && Date.now() - projectCache.at < PROJECT_CACHE_MS) {
    return projectCache.projects
  }
  const sessions = await listSessionsImpl({})
  const byPath = new Map<string, ProjectSummary>()
  for (const info of sessions) {
    const cwd = info.cwd
    if (!cwd) continue
    const existing = byPath.get(cwd)
    if (existing) {
      existing.sessionCount += 1
      existing.lastActivityAt = Math.max(
        existing.lastActivityAt ?? 0,
        info.lastModified,
      )
    } else {
      byPath.set(cwd, {
        projectId: sanitizePath(cwd),
        path: cwd,
        name: basename(cwd) || cwd,
        sessionCount: 1,
        lastActivityAt: info.lastModified,
      })
    }
  }
  const projects = [...byPath.values()].sort(
    (a, b) => (b.lastActivityAt ?? 0) - (a.lastActivityAt ?? 0),
  )
  projectCache = { at: Date.now(), projects }
  return projects
}

export async function listSessions(options: {
  projectId?: string
  projectPath?: string
  limit?: number
  offset?: number
}): Promise<{ sessions: SessionSummary[]; hasMore: boolean }> {
  const limit = Math.min(Math.max(options.limit ?? 200, 1), 500)
  const offset = Math.max(options.offset ?? 0, 0)

  let dir = options.projectPath
  if (!dir && options.projectId) {
    const projects = await listProjects()
    dir = projects.find(
      project => project.projectId === options.projectId,
    )?.path
  }

  const sessions = await listSessionsImpl(
    dir ? { dir, limit, offset } : { limit, offset },
  )
  return {
    sessions: sessions.map(toSummary),
    hasMore: sessions.length === limit,
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
      case 'image':
        blocks.push({ type: 'image' })
        break
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
}): Promise<ReadSessionResult> {
  const limit = Math.min(Math.max(options.limit ?? 100, 1), 200)
  const resolved = await resolveSessionFilePath(
    options.localSessionId,
    options.cwd,
  )
  if (!resolved) {
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
