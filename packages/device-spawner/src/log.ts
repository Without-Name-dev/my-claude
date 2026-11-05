/**
 * Spawner logging.
 *
 * The Spawner may run without a visible console (scheduled task), so every
 * line goes to a rotating file as well as stdout. Callers must never pass
 * tokens, secrets or message bodies to this function.
 */

import { appendFileSync, mkdirSync, renameSync, statSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

const MAX_LOG_BYTES = 5 * 1024 * 1024

function logDirectory(): string {
  if (process.env.SPAWNER_LOG_DIR) return process.env.SPAWNER_LOG_DIR
  const base = process.env.LOCALAPPDATA || join(homedir(), '.local', 'share')
  return join(base, 'CCRemote')
}

function rotateIfNeeded(path: string): void {
  try {
    const stat = statSync(path)
    if (stat.size < MAX_LOG_BYTES) return
    renameSync(path, `${path}.1`)
  } catch {
    // Missing file or a rotation race: appending is still safe.
  }
}

export function log(message: string): void {
  const line = `[${new Date().toISOString()}] ${message}`
  console.log(line)
  try {
    const dir = logDirectory()
    mkdirSync(dir, { recursive: true })
    const path = join(dir, 'spawner.log')
    rotateIfNeeded(path)
    appendFileSync(path, `${line}\n`, 'utf8')
  } catch {
    // Logging must never take the Spawner down.
  }
}

export function errorLog(message: string, error?: unknown): void {
  const detail =
    error instanceof Error
      ? `: ${error.message}`
      : error === undefined
        ? ''
        : `: ${String(error)}`
  log(`ERROR ${message}${detail}`)
}
