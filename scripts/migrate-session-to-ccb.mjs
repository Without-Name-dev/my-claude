#!/usr/bin/env bun
/**
 * Move or continuously mirror one Claude Code session between config
 * directories.
 *
 * The default pair is the official CLI config (~/.claude) and ccb's isolated
 * config (D:\Claude Code\claude-config), but both are flags. A session is more
 * than its transcript: the per-session sidecar folder holds subagent logs and
 * the custom title, and history.jsonl holds the up-arrow prompt history, so
 * all three are migrated.
 *
 * Usage:
 *   bun scripts/migrate-session-to-ccb.mjs --latest
 *   bun scripts/migrate-session-to-ccb.mjs --session <uuid>
 *   bun scripts/migrate-session-to-ccb.mjs --session <uuid> --sync      # top up once
 *   bun scripts/migrate-session-to-ccb.mjs --session <uuid> --watch 5   # keep mirroring
 *
 * Flags:
 *   --source-config <dir>  default %USERPROFILE%\.claude
 *   --target-config <dir>  default D:\Claude Code\claude-config
 *   --session <uuid>       pick this session from the source config
 *   --file <path>          explicit .jsonl
 *   --latest               newest session by mtime (default when nothing given)
 *   --new-id               give the copy a fresh session id (one-shot only)
 *   --overwrite            replace an existing copy with the same id
 *   --sync                 append entries written since the last migration
 *   --watch [seconds]      imply --sync and repeat every N seconds (default 5)
 *   --set-last             point the target project's lastSessionId at the copy
 *   --dry-run              print the plan, write nothing
 *
 * Nothing in the source config is modified. If the target grows past the
 * source (i.e. the copy was resumed in ccb), syncing stops instead of
 * overwriting the newer side.
 */

import { createHash, randomUUID } from 'node:crypto'
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  readdirSync,
  readSync,
  closeSync,
  renameSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { homedir } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'
import { createInterface } from 'node:readline'

const args = process.argv.slice(2)
const flag = name => args.includes(`--${name}`)
const value = name => {
  const index = args.indexOf(`--${name}`)
  return index >= 0 ? args[index + 1] : undefined
}

const sourceConfig = resolve(
  value('source-config') ?? join(homedir(), '.claude'),
)
const targetConfig = resolve(
  value('target-config') ?? 'D:\\Claude Code\\claude-config',
)
const dryRun = flag('dry-run')
const newId = flag('new-id')
const overwrite = flag('overwrite')
const setLast = flag('set-last')
const watchIndex = args.indexOf('--watch')
const watchSeconds = watchIndex >= 0 ? Number(args[watchIndex + 1]) || 5 : 0
const syncMode = flag('sync') || watchIndex >= 0

if (sourceConfig === targetConfig)
  fail('source and target config are the same directory')
if (!existsSync(sourceConfig)) fail(`source config not found: ${sourceConfig}`)

function fail(message) {
  console.error(`error: ${message}`)
  process.exit(1)
}

function listSessions(config) {
  const projectsDir = join(config, 'projects')
  if (!existsSync(projectsDir)) return []
  return readdirSync(projectsDir, { withFileTypes: true })
    .filter(entry => entry.isDirectory())
    .flatMap(entry =>
      readdirSync(join(projectsDir, entry.name))
        .filter(name => name.endsWith('.jsonl'))
        .map(name => ({
          id: name.slice(0, -'.jsonl'.length),
          slug: entry.name,
          path: join(projectsDir, entry.name, name),
          mtime: statSync(join(projectsDir, entry.name, name)).mtimeMs,
        })),
    )
}

function pickSource() {
  const explicit = value('file')
  if (explicit) {
    const path = resolve(explicit)
    if (!existsSync(path)) fail(`file not found: ${path}`)
    return { path, id: basename(path, '.jsonl'), slug: basename(dirname(path)) }
  }
  const sessions = listSessions(sourceConfig)
  if (sessions.length === 0) fail(`no sessions under ${sourceConfig}`)
  const wanted = value('session')
  if (wanted) {
    const found = sessions.find(session => session.id === wanted)
    if (!found) fail(`session ${wanted} not found in ${sourceConfig}`)
    return found
  }
  return sessions.sort((a, b) => b.mtime - a.mtime)[0]
}

/** Read the first lines that carry the cwd and the title without loading the transcript. */
async function readHeader(path) {
  const { createReadStream } = await import('node:fs')
  const header = { cwd: undefined, title: undefined, sessionId: undefined }
  const lines = createInterface({
    input: createReadStream(path),
    crlfDelay: Infinity,
  })
  let seen = 0
  for await (const line of lines) {
    seen += 1
    if (!line.trim()) continue
    let entry
    try {
      entry = JSON.parse(line)
    } catch {
      continue
    }
    header.sessionId ??= entry.sessionId
    header.cwd ??= entry.cwd
    if (entry.type === 'custom-title' && entry.customTitle)
      header.title = entry.customTitle
    if (!header.title && entry.type === 'ai-title' && entry.aiTitle)
      header.title = entry.aiTitle
    if (header.cwd && header.sessionId) break
    if (seen > 500) break
  }
  lines.close()
  return header
}

async function sha256(path) {
  const { createReadStream } = await import('node:fs')
  const hash = createHash('sha256')
  for await (const chunk of createReadStream(path)) hash.update(chunk)
  return hash.digest('hex')
}

/** Does the first 64 KB (what the session list reads) contain a cwd field? */
async function headContains(path, needle) {
  const { open } = await import('node:fs/promises')
  const handle = await open(path, 'r')
  try {
    const buf = Buffer.alloc(65536)
    const { bytesRead } = await handle.read(buf, 0, buf.length, 0)
    return buf.toString('utf8', 0, bytesRead).includes(needle)
  } finally {
    await handle.close()
  }
}

/** Byte offset just after the first newline, or the end of file. */
function firstLineEnd(path) {
  const fd = openSync(path, 'r')
  try {
    const buf = Buffer.alloc(8192)
    let offset = 0
    for (;;) {
      const read = readSync(fd, buf, 0, buf.length, offset)
      if (read <= 0) return statSync(path).size
      const nl = buf.subarray(0, read).indexOf(0x0a)
      if (nl >= 0) return offset + nl + 1
      offset += read
      if (offset > 1 << 20) {
        // A first line this long is not a metadata line; treat it as one block.
        return statSync(path).size
      }
    }
  } finally {
    closeSync(fd)
  }
}

/** Compare `length` bytes of two files starting at their respective offsets. */
function samePrefix(aPath, aOffset, bPath, bOffset, length) {
  const chunkSize = 1 << 20
  const aFd = openSync(aPath, 'r')
  const bFd = openSync(bPath, 'r')
  try {
    const aBuf = Buffer.alloc(chunkSize)
    const bBuf = Buffer.alloc(chunkSize)
    let done = 0
    while (done < length) {
      const want = Math.min(chunkSize, length - done)
      const aRead = readSync(aFd, aBuf, 0, want, aOffset + done)
      const bRead = readSync(bFd, bBuf, 0, want, bOffset + done)
      if (aRead !== want || bRead !== want) return false
      if (!aBuf.subarray(0, want).equals(bBuf.subarray(0, want))) return false
      done += want
    }
    return true
  } finally {
    closeSync(aFd)
    closeSync(bFd)
  }
}

function directorySize(path) {
  let total = 0
  for (const entry of readdirSync(path, { withFileTypes: true })) {
    const child = join(path, entry.name)
    total += entry.isDirectory() ? directorySize(child) : statSync(child).size
  }
  return total
}

/** Copy a directory, reusing files that already match in size. */
function syncDirectory(src, dst) {
  mkdirSync(dst, { recursive: true })
  let copied = 0
  for (const entry of readdirSync(src, { withFileTypes: true })) {
    const s = join(src, entry.name)
    const d = join(dst, entry.name)
    if (entry.isDirectory()) {
      copied += syncDirectory(s, d)
    } else if (entry.isFile()) {
      if (!existsSync(d) || statSync(d).size !== statSync(s).size) {
        copyFileSync(s, d)
        copied += 1
      }
    }
  }
  return copied
}

const source = pickSource()
const header = await readHeader(source.path)
if (!header.cwd) fail(`could not read cwd from ${source.path}`)
const sessionId = header.sessionId ?? source.id
if (sessionId !== source.id)
  fail(`session id mismatch: file ${source.id}, entries ${sessionId}`)
const targetId = newId ? randomUUID() : sessionId
const targetProjectDir = join(targetConfig, 'projects', source.slug)
const targetFile = join(targetProjectDir, `${targetId}.jsonl`)
const sourceSidecar = join(dirname(source.path), sessionId)
const targetSidecar = join(targetProjectDir, targetId)
const sourceHistory = join(sourceConfig, 'history.jsonl')
const targetHistory = join(targetConfig, 'history.jsonl')
const statePath = `${targetFile}.sync.json`

/** Checkpoint so a 200 MB transcript is not re-compared on every tick. */
function readState() {
  try {
    const raw = JSON.parse(readFileSync(statePath, 'utf8'))
    if (raw.sourcePath === source.path && raw.targetPath === targetFile)
      return raw
  } catch {
    // Missing or stale checkpoint: fall back to a full prefix comparison.
  }
  return null
}

function writeState(state) {
  writeFileSync(
    statePath,
    `${JSON.stringify({ sourcePath: source.path, targetPath: targetFile, ...state }, null, 2)}\n`,
  )
}

/** Source offset that corresponds to the end of the copied target tail. */
function checkpointForTarget() {
  return (
    firstLineEnd(source.path) +
    (statSync(targetFile).size - firstLineEnd(targetFile))
  )
}

/** Append prompt-history lines for this session that the target does not have. */
function syncHistory() {
  if (!existsSync(sourceHistory)) return 0
  const existingText = existsSync(targetHistory)
    ? readFileSync(targetHistory, 'utf8')
    : ''
  const existing = new Set(existingText.split('\n').filter(Boolean))
  const additions = []
  for (const line of readFileSync(sourceHistory, 'utf8').split('\n')) {
    if (!line.trim()) continue
    let entry
    try {
      entry = JSON.parse(line)
    } catch {
      continue
    }
    if (entry.sessionId !== sessionId) continue
    if (targetId !== sessionId) entry.sessionId = targetId
    const rewritten = JSON.stringify(entry)
    if (existing.has(rewritten)) continue
    existing.add(rewritten)
    additions.push(rewritten)
  }
  if (additions.length === 0) return 0
  const prefix = existingText && !existingText.endsWith('\n') ? '\n' : ''
  writeFileSync(targetHistory, prefix + additions.join('\n') + '\n', {
    flag: 'a',
  })
  return additions.length
}

function ensureProjectEntry() {
  const targetStatePath = join(targetConfig, '.claude.json')
  const projectKey = header.cwd.replace(/\\/g, '/')
  const state = existsSync(targetStatePath)
    ? JSON.parse(readFileSync(targetStatePath, 'utf8'))
    : {}
  state.projects ??= {}
  let changed = false
  if (!state.projects[projectKey]) {
    state.projects[projectKey] = {}
    changed = true
  }
  if (setLast && state.projects[projectKey].lastSessionId !== targetId) {
    state.projects[projectKey].lastSessionId = targetId
    changed = true
  }
  if (!changed) return
  const backupDir = join(targetConfig, 'backups')
  mkdirSync(backupDir, { recursive: true })
  if (existsSync(targetStatePath)) {
    copyFileSync(
      targetStatePath,
      join(backupDir, `.claude.json.before-migrate-${Date.now()}`),
    )
  }
  const temp = `${targetStatePath}.tmp`
  writeFileSync(temp, `${JSON.stringify(state, null, 2)}\n`)
  renameSync(temp, targetStatePath)
}

/** Re-run the CLI's own metadata extraction so the result is what the list shows. */
async function verifyCatalog(path) {
  const { readSessionLite } = await import(
    '../src/utils/sessionStoragePortable.ts'
  )
  const { parseSessionInfoFromLite } = await import(
    '../src/utils/listSessionsImpl.ts'
  )
  const lite = await readSessionLite(path)
  const info = lite ? parseSessionInfoFromLite(targetId, lite) : null
  return info
}

/** Full copy: used for the first migration and when a sync cannot append. */
async function fullMigration({ reason } = {}) {
  console.log(`source : ${source.path}`)
  console.log(
    `        ${(statSync(source.path).size / 1024 / 1024).toFixed(1)} MB, title: ${header.title ?? '(none)'}`,
  )
  console.log(`target : ${targetFile}`)
  console.log(`cwd    : ${header.cwd}`)
  console.log(
    `id     : ${sessionId}${targetId !== sessionId ? ` -> ${targetId}` : ''}`,
  )
  if (existsSync(sourceSidecar)) {
    console.log(
      `sidecar: ${(directorySize(sourceSidecar) / 1024 / 1024).toFixed(1)} MB (subagents/title)`,
    )
  }

  if (dryRun) {
    console.log('dry run: nothing written')
    return
  }
  if (reason) console.log(`note   : ${reason}`)

  mkdirSync(targetProjectDir, { recursive: true })
  // listSessions only reads the first 64 KB for cwd. Sessions whose opening
  // entries are big file-history snapshots have no cwd in that window, and the
  // web then cannot tell which project directory to resume in. Put cwd on the
  // first metadata line, where the extractor looks for it.
  const headHasCwd = await headContains(source.path, '"cwd"')
  let lineCount = 0
  if (!headHasCwd || targetId !== sessionId) {
    const { createReadStream, createWriteStream } = await import('node:fs')
    const output = createWriteStream(targetFile)
    const lines = createInterface({
      input: createReadStream(source.path),
      crlfDelay: Infinity,
    })
    let first = true
    for await (const line of lines) {
      if (!line.trim()) continue
      let entry
      try {
        entry = JSON.parse(line)
      } catch {
        output.write(`${line}\n`)
        lineCount += 1
        first = false
        continue
      }
      if (first) {
        first = false
        if (!headHasCwd && !('cwd' in entry)) entry.cwd = header.cwd
      }
      if (targetId !== sessionId) {
        if (entry.sessionId === sessionId) entry.sessionId = targetId
        if (entry.session_id === sessionId) entry.session_id = targetId
      }
      output.write(`${JSON.stringify(entry)}\n`)
      lineCount += 1
    }
    if (first) {
      output.write(
        `${JSON.stringify({ type: 'mode', mode: 'normal', sessionId: targetId, cwd: header.cwd })}\n`,
      )
      lineCount = 1
    }
    lines.close()
    await new Promise((resolve, reject) =>
      output.end(err => (err ? reject(err) : resolve())),
    )
  } else {
    copyFileSync(source.path, targetFile)
  }

  if (existsSync(sourceSidecar)) syncDirectory(sourceSidecar, targetSidecar)
  const prompts = syncHistory()
  if (prompts > 0) console.log(`history: +${prompts} prompt(s)`)
  ensureProjectEntry()
  if (targetId === sessionId) {
    writeState({
      sourceOffset: checkpointForTarget(),
      targetSize: statSync(targetFile).size,
    })
  }

  const targetSize = statSync(targetFile).size
  const sourceSize = statSync(source.path).size
  if (!headHasCwd && targetId === sessionId) {
    console.log(
      `verify : ${lineCount} entries, cwd added to the first metadata line (+${targetSize - sourceSize} bytes)`,
    )
  } else if (targetId === sessionId) {
    const identical = (await sha256(source.path)) === (await sha256(targetFile))
    console.log(`verify : ${identical ? 'byte-identical' : 'HASH MISMATCH'}`)
    if (!identical) process.exit(1)
  } else {
    console.log(
      `verify : ${lineCount} entries, ${(targetSize / 1024 / 1024).toFixed(1)} MB (source ${(sourceSize / 1024 / 1024).toFixed(1)} MB)`,
    )
  }

  const info = await verifyCatalog(targetFile)
  console.log(
    `catalog: cwd=${info?.cwd ?? '(missing)'} title=${info?.customTitle ?? info?.summary ?? '(none)'}`,
  )
  if (!info?.cwd) {
    console.error(
      'error: the CLI session list still cannot resolve cwd for this session',
    )
    process.exit(1)
  }
  console.log('')
  console.log('done. Resume it with:')
  console.log(`  cc --resume ${targetId}`)
  console.log('It also shows up in the RCS web session list for this project.')
}

/**
 * Append whatever the source gained since the last copy.
 *
 * The first line of the target differs (it carries the injected cwd), so the
 * comparison starts after each file's first newline. If the target is no
 * longer a prefix of the source, someone resumed the copy in ccb and syncing
 * must stop rather than truncate their new messages.
 */
function incrementalSync() {
  const sourceSize = statSync(source.path).size
  const targetSize = statSync(targetFile).size
  if (targetId !== sessionId) {
    return { diverged: true, reason: 'a --new-id copy cannot be kept in sync' }
  }

  // Fast path: the checkpoint says exactly how much of the source is already
  // in the target. This keeps 5-second ticks to two stats plus the new bytes,
  // instead of re-reading a 200 MB transcript.
  const state = readState()
  let appendFrom
  if (
    state &&
    state.targetSize === targetSize &&
    state.sourceOffset <= sourceSize
  ) {
    appendFrom = state.sourceOffset
  } else if (state && targetSize > state.targetSize) {
    return {
      diverged: true,
      reason: 'the copy grew outside this sync (it was resumed in ccb)',
    }
  } else {
    const sourceStart = firstLineEnd(source.path)
    const targetStart = firstLineEnd(targetFile)
    const targetTail = targetSize - targetStart
    if (targetTail > sourceSize - sourceStart) {
      return {
        diverged: true,
        reason: 'the copy is longer than the source (it was resumed in ccb)',
      }
    }
    if (
      !samePrefix(source.path, sourceStart, targetFile, targetStart, targetTail)
    ) {
      return {
        diverged: true,
        reason: 'the copy no longer matches the source transcript',
      }
    }
    appendFrom = sourceStart + targetTail
  }

  const pending = sourceSize - appendFrom
  if (pending <= 0) {
    const sidecar = existsSync(sourceSidecar)
      ? syncDirectory(sourceSidecar, targetSidecar)
      : 0
    const prompts = syncHistory()
    ensureProjectEntry()
    writeState({ sourceOffset: appendFrom, targetSize })
    return { appendedBytes: 0, sidecar, prompts }
  }

  const fd = openSync(source.path, 'r')
  let chunk
  try {
    const buf = Buffer.alloc(pending)
    const read = readSync(fd, buf, 0, pending, appendFrom)
    chunk = buf.subarray(0, read)
  } finally {
    closeSync(fd)
  }
  const lastNewline = chunk.lastIndexOf(0x0a)
  if (lastNewline < 0) {
    const sidecar = existsSync(sourceSidecar)
      ? syncDirectory(sourceSidecar, targetSidecar)
      : 0
    const prompts = syncHistory()
    writeState({ sourceOffset: appendFrom, targetSize })
    return { appendedBytes: 0, sidecar, prompts }
  }
  const payload = chunk.subarray(0, lastNewline + 1)
  writeFileSync(targetFile, payload, { flag: 'a' })
  writeState({
    sourceOffset: appendFrom + payload.length,
    targetSize: targetSize + payload.length,
  })
  const sidecar = existsSync(sourceSidecar)
    ? syncDirectory(sourceSidecar, targetSidecar)
    : 0
  const prompts = syncHistory()
  ensureProjectEntry()
  return { appendedBytes: payload.length, sidecar, prompts }
}

if (!existsSync(targetFile)) {
  await fullMigration()
} else if (newId) {
  fail('--new-id needs a target that does not exist yet')
} else if (!syncMode) {
  if (!overwrite)
    fail(
      `target already exists: ${targetFile} (use --sync, --overwrite or --new-id)`,
    )
  await fullMigration({ reason: 'overwrite requested' })
} else if (dryRun) {
  const pending = statSync(source.path).size - statSync(targetFile).size
  console.log(
    `dry run: target exists, about ${(Math.max(0, pending) / 1024).toFixed(1)} KB newer in the source`,
  )
} else {
  const first = incrementalSync()
  if (first.diverged) {
    fail(
      `stopped: ${first.reason}. Delete the target if you want a fresh copy.`,
    )
  }
  console.log(
    `synced : +${(first.appendedBytes / 1024).toFixed(1)} KB, sidecar files: ${first.sidecar}, prompts: ${first.prompts}`,
  )
}

if (watchSeconds > 0) {
  console.log(`watching ${source.path} every ${watchSeconds}s; Ctrl+C to stop`)
  let seen = { appendedBytes: -1, sidecar: -1, prompts: -1 }
  for (;;) {
    await new Promise(r => setTimeout(r, watchSeconds * 1000))
    const result = incrementalSync()
    if (result.diverged) {
      console.log(`stop    : ${result.reason}; leaving the copy alone`)
      process.exit(0)
    }
    if (
      result.appendedBytes !== seen.appendedBytes ||
      result.sidecar !== seen.sidecar ||
      result.prompts !== seen.prompts
    ) {
      if (
        result.appendedBytes > 0 ||
        result.sidecar > 0 ||
        result.prompts > 0
      ) {
        const stamp = new Date().toISOString().slice(11, 19)
        console.log(
          `[${stamp}] +${(result.appendedBytes / 1024).toFixed(1)} KB, sidecar: ${result.sidecar}, prompts: ${result.prompts}`,
        )
      }
      seen = result
    }
  }
}
