import {
  mkdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { randomUUID } from 'node:crypto'
import { join } from 'node:path'
import { getClaudeConfigHomeDir } from '../envUtils.js'

export type SavedProviderSelection = { id: string; model: string }

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
