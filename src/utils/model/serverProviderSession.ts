import {
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { randomUUID } from 'node:crypto'
import { join } from 'node:path'
import { getClaudeConfigHomeDir } from '../envUtils.js'

export type SavedProviderSelection = { id: string; model: string }

/** Legacy-server fallback: only identities are stored, and keys stay on RCS. */
export function readRecentSessionProviders(): SavedProviderSelection[] {
  const dir = join(getClaudeConfigHomeDir(), 'server-provider-sessions')
  let files: string[]
  try {
    files = readdirSync(dir).filter(name => /^[a-zA-Z0-9_-]+\.json$/.test(name))
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return []
    throw error
  }
  return files
    .flatMap(name => {
      try {
        const selection = readSessionProvider(name.slice(0, -5))
        return selection
          ? [{ selection, mtime: statSync(join(dir, name)).mtimeMs }]
          : []
      } catch {
        // An unrelated damaged or concurrently removed session is not the new session.
        return []
      }
    })
    .sort((a, b) => b.mtime - a.mtime)
    .map(entry => entry.selection)
}

function selectionPath(sessionId: string): string {
  if (!/^[a-zA-Z0-9_-]+$/.test(sessionId)) throw new Error('Invalid session ID')
  return join(
    getClaudeConfigHomeDir(),
    'server-provider-sessions',
    `${sessionId}.json`,
  )
}

/** Store only the selection, never provider credentials. */
export function saveSessionProvider(
  sessionId: string,
  selection: SavedProviderSelection | null,
): void {
  const path = selectionPath(sessionId)
  if (!selection) {
    rmSync(path, { force: true })
    return
  }
  mkdirSync(join(getClaudeConfigHomeDir(), 'server-provider-sessions'), {
    recursive: true,
  })
  const temp = `${path}.${randomUUID()}.tmp`
  try {
    writeFileSync(temp, JSON.stringify(selection), { mode: 0o600, flag: 'wx' })
    renameSync(temp, path)
  } finally {
    rmSync(temp, { force: true })
  }
}

export function readSessionProvider(
  sessionId: string,
): SavedProviderSelection | null {
  let raw: unknown
  try {
    raw = JSON.parse(readFileSync(selectionPath(sessionId), 'utf8'))
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null
    throw error
  }
  if (!raw || typeof raw !== 'object') throw new Error('会话供应商配置损坏')
  const value = raw as Record<string, unknown>
  if (
    typeof value.id !== 'string' ||
    !value.id ||
    typeof value.model !== 'string' ||
    !value.model
  ) {
    throw new Error('会话供应商配置损坏')
  }
  return { id: value.id, model: value.model }
}
