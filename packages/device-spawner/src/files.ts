/**
 * Directory browsing and browser-to-machine uploads.
 *
 * The web UI lets the operator pick a working directory for a new terminal and
 * attach files from either side. Listing is read-only and reports what the
 * filesystem says; uploading writes only below a fresh folder in the Spawner's
 * own data directory, never onto the path the browser suggests.
 */

import { randomBytes } from 'node:crypto'
import {
  existsSync,
  mkdirSync,
  readdirSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { homedir } from 'node:os'
import { dirname, isAbsolute, join, normalize, resolve, sep } from 'node:path'
import type {
  DirectoryEntry,
  DirectoryListing,
  DirectoryRoot,
  UploadFileInput,
  UploadResult,
} from '../../remote-control-server/src/device/protocol'
import { logDir, type SpawnerConfig } from './config'

/** A listing larger than this is not something a person scrolls through. */
const MAX_ENTRIES = 500

/** Reject absurd uploads before decoding base64 into memory. */
const MAX_FILES = 200
const MAX_FILE_BYTES = 20 * 1024 * 1024
const MAX_TOTAL_BYTES = 30 * 1024 * 1024

function underAllowedRoot(path: string, roots: string[]): boolean {
  if (roots.length === 0) return true
  const normalized = resolve(path)
  return roots.some(root => {
    const base = resolve(root)
    return normalized === base || normalized.startsWith(`${base}${sep}`)
  })
}

function windowsDrives(): string[] {
  const drives: string[] = []
  for (let code = 65; code <= 90; code += 1) {
    const root = `${String.fromCharCode(code)}:\\`
    try {
      if (existsSync(root)) drives.push(root)
    } catch {
      // A drive that cannot be probed is not offered.
    }
  }
  return drives
}

function directoryRoots(): DirectoryRoot[] {
  const roots: DirectoryRoot[] = []
  const home = homedir()
  roots.push({ name: '用户目录', path: home, kind: 'home' })
  if (process.platform === 'win32') {
    for (const drive of windowsDrives()) {
      roots.push({ name: drive.replace(/\\$/, ''), path: drive, kind: 'drive' })
    }
  } else {
    roots.push({ name: '根目录', path: sep, kind: 'volume' })
  }
  return roots
}

/** `C:\` and `/` are their own parents; stop there. */
function parentOf(path: string): string | undefined {
  const resolved = resolve(path)
  const parent = dirname(resolved)
  if (parent === resolved || parent === `.`) return undefined
  return parent
}

export async function listDirectory(
  config: SpawnerConfig,
  path: string | undefined,
): Promise<DirectoryListing> {
  if (!path) return { entries: [], roots: directoryRoots() }

  const target = path.trim()
  if (
    !target ||
    target.length > 4096 ||
    !isAbsolute(target) ||
    (!target.startsWith(sep) && !/^[A-Za-z]:[\\/]/.test(target))
  ) {
    throw new Error('目录不是绝对路径')
  }
  if (!underAllowedRoot(target, config.allowedRoots)) {
    throw new Error('这个目录不在允许的根目录内')
  }
  let stat
  try {
    stat = statSync(target)
  } catch {
    throw new Error('目录不存在或无法访问')
  }
  if (!stat.isDirectory()) throw new Error('这不是目录')

  const entries: DirectoryEntry[] = []
  let truncated = false
  let dirents
  try {
    dirents = readdirSync(target, { withFileTypes: true })
  } catch {
    throw new Error('没有权限读取这个目录')
  }

  for (const dirent of dirents) {
    if (entries.length >= MAX_ENTRIES) {
      truncated = true
      break
    }
    const child = join(target, dirent.name)
    let type: DirectoryEntry['type'] = 'file'
    let size: number | undefined
    let modifiedAt: number | undefined
    try {
      if (dirent.isDirectory()) {
        type = 'dir'
      } else if (dirent.isSymbolicLink()) {
        // Junctions on Windows report as links; follow them for display but
        // never recurse.
        const followed = statSync(child)
        type = followed.isDirectory() ? 'dir' : 'file'
        if (type === 'file') size = followed.size
      } else {
        const info = statSync(child)
        size = info.size
        modifiedAt = info.mtimeMs
      }
    } catch {
      // Permission-denied children are simply not shown.
      continue
    }
    entries.push({
      name: dirent.name,
      path: child,
      type,
      ...(size !== undefined ? { size } : {}),
      ...(modifiedAt !== undefined ? { modifiedAt } : {}),
    })
  }

  entries.sort((a, b) => {
    if (a.type !== b.type) return a.type === 'dir' ? -1 : 1
    return a.name.localeCompare(b.name, undefined, {
      numeric: true,
      sensitivity: 'base',
    })
  })

  const parent = parentOf(target)
  return {
    path: resolve(target),
    ...(parent ? { parent } : {}),
    entries,
    ...(truncated ? { truncated } : {}),
  }
}

/** Base64 expands by 4/3; checking the encoded length avoids a huge decode. */
function encodedSize(data: string): number {
  const padding = data.endsWith('==') ? 2 : data.endsWith('=') ? 1 : 0
  return Math.floor((data.length * 3) / 4) - padding
}

/**
 * Windows forbids these characters in names. Rather than reject the whole
 * upload, replace them so a phone-generated name still lands somewhere sane.
 */
function safeSegment(segment: string): string {
  // Walk code points instead of a control-character regex: the linter rejects
  // control characters in patterns, and this also never splits a surrogate.
  let replaced = ''
  for (const char of segment) {
    const code = char.codePointAt(0) ?? 0
    replaced += code < 0x20 || '<>:"|?*'.includes(char) ? '_' : char
  }
  const cleaned = replaced.replace(/[. ]+$/, '').trim()
  if (!cleaned || cleaned === '.' || cleaned === '..') return '_'
  return cleaned.slice(0, 120)
}

/**
 * Turn a browser-supplied relative name into a safe relative path. Absolute
 * paths, drive letters and `..` are removed rather than trusted.
 */
function safeRelativePath(name: string): string | undefined {
  const normalized = name.replace(/\\/g, '/').replace(/^\/+/, '')
  if (!normalized || normalized.length > 1024) return undefined
  if (/^[A-Za-z]:/.test(normalized)) return undefined
  const segments = normalized.split('/').filter(Boolean)
  if (segments.length === 0 || segments.length > 16) return undefined
  return segments.map(safeSegment).join('/')
}

function stamp(): string {
  const now = new Date()
  const pad = (value: number) => String(value).padStart(2, '0')
  return (
    `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}` +
    `-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`
  )
}

export async function uploadFiles(
  files: UploadFileInput[],
  group?: string,
): Promise<UploadResult> {
  if (!Array.isArray(files) || files.length === 0) {
    throw new Error('没有要上传的文件')
  }
  if (files.length > MAX_FILES) {
    throw new Error(`一次最多上传 ${MAX_FILES} 个文件`)
  }

  let total = 0
  const prepared: { relative: string; data: Buffer }[] = []
  for (const file of files) {
    if (typeof file?.name !== 'string' || typeof file?.data !== 'string') {
      throw new Error('上传参数不完整')
    }
    const relative = safeRelativePath(file.name)
    if (!relative) throw new Error(`文件名不合法：${file.name.slice(0, 80)}`)
    const bytes = encodedSize(file.data)
    if (bytes > MAX_FILE_BYTES) {
      throw new Error(
        `文件太大（最大 ${MAX_FILE_BYTES / 1024 / 1024}MB）：${relative}`,
      )
    }
    total += bytes
    if (total > MAX_TOTAL_BYTES) {
      throw new Error(`上传总大小超过 ${MAX_TOTAL_BYTES / 1024 / 1024}MB`)
    }
    prepared.push({ relative, data: Buffer.from(file.data, 'base64') })
  }

  // A client may split one folder into several batches; a hex-only group id
  // is the only thing it can contribute to the destination path.
  const folder =
    typeof group === 'string' && /^[a-z0-9-]{8,64}$/i.test(group)
      ? group.toLowerCase()
      : `${stamp()}-${randomBytes(3).toString('hex')}`
  const root = normalize(join(logDir(), 'uploads', folder))
  mkdirSync(root, { recursive: true })

  const uploaded = prepared.map(file => {
    const target = normalize(join(root, file.relative))
    if (!target.startsWith(root + sep)) {
      throw new Error('文件名不合法')
    }
    mkdirSync(dirname(target), { recursive: true })
    writeFileSync(target, file.data)
    return { name: file.relative, path: target, bytes: file.data.length }
  })

  return { root, files: uploaded }
}
