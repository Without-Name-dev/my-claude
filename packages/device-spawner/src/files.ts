/**
 * Directory browsing and file intake.
 *
 * The web UI lets the operator pick a working directory for a new terminal and
 * attach files from either side. Listing is read-only and reports what the
 * filesystem says. Uploads and copies always land in the session's own working
 * directory — the folder the terminal runs in — so the files are project
 * files, not a cache the user has to clean up later.
 */

import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { homedir } from 'node:os'
import {
  basename,
  dirname,
  extname,
  isAbsolute,
  join,
  resolve,
  sep,
} from 'node:path'
import type {
  CopyResult,
  DirectoryEntry,
  DirectoryListing,
  DirectoryRoot,
  UploadFileInput,
  UploadResult,
} from '../../remote-control-server/src/device/protocol'
import type { SpawnerConfig } from './config'

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

/** The session's working directory: every upload and copy lands here. */
function sessionDirectory(
  config: SpawnerConfig,
  cwd: string | undefined,
): string {
  const target = (cwd || '').trim()
  if (
    !target ||
    target.length > 4096 ||
    !isAbsolute(target) ||
    (!target.startsWith(sep) && !/^[A-Za-z]:[\\/]/.test(target))
  ) {
    throw new Error('会话目录不是绝对路径')
  }
  if (!underAllowedRoot(target, config.allowedRoots)) {
    throw new Error('会话目录不在允许的根目录内')
  }
  let info
  try {
    info = statSync(target)
  } catch {
    throw new Error('会话目录不存在或无法访问')
  }
  if (!info.isDirectory()) throw new Error('会话目录不是目录')
  return resolve(target)
}

/** `report.txt` next to an existing one becomes `report (2).txt`. */
function uniquePath(directory: string, name: string): string {
  const first = join(directory, name)
  if (!existsSync(first)) return first
  const extension = extname(name)
  const stem = extension ? name.slice(0, -extension.length) : name
  for (let index = 2; index < 1000; index += 1) {
    const candidate = join(directory, `${stem} (${index})${extension}`)
    if (!existsSync(candidate)) return candidate
  }
  throw new Error(`无法为 ${name} 生成不冲突的文件名`)
}

/** True when `child` is `parent` itself or lives below it. */
function insideOrEqual(parent: string, child: string): boolean {
  const base = resolve(parent)
  const value = resolve(child)
  return value === base || value.startsWith(`${base}${sep}`)
}

export async function uploadFiles(
  config: SpawnerConfig,
  files: UploadFileInput[],
  cwd: string | undefined,
): Promise<UploadResult> {
  if (!Array.isArray(files) || files.length === 0) {
    throw new Error('没有要上传的文件')
  }
  if (files.length > MAX_FILES) {
    throw new Error(`一次最多上传 ${MAX_FILES} 个文件`)
  }
  const root = sessionDirectory(config, cwd)

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

  const uploaded = prepared.map(file => {
    const parts = file.relative.split('/')
    const name = parts.pop() ?? file.relative
    // Intermediate folders are reused: a folder upload split into several
    // batches must keep writing into the same tree.
    const directory = parts.length > 0 ? join(root, ...parts) : root
    mkdirSync(directory, { recursive: true })
    const target = uniquePath(directory, name)
    if (!insideOrEqual(root, target)) throw new Error('文件名不合法')
    writeFileSync(target, file.data)
    return {
      name: [...parts, basename(target)].join('/'),
      path: target,
      bytes: file.data.length,
    }
  })

  return { root, files: uploaded }
}

const MAX_COPY_ENTRIES = 1000
const MAX_COPY_BYTES = 200 * 1024 * 1024
const MAX_COPY_PATHS = 50

type CopyStats = { entries: number; bytes: number }

/** Pre-scan so a huge directory is refused before anything is written. */
function measure(source: string, stats: CopyStats): void {
  const info = statSync(source)
  if (info.isDirectory()) {
    for (const dirent of readdirSync(source, { withFileTypes: true })) {
      if (dirent.isSymbolicLink()) continue
      if (dirent.isDirectory()) {
        measure(join(source, dirent.name), stats)
      } else if (dirent.isFile()) {
        stats.entries += 1
        stats.bytes += statSync(join(source, dirent.name)).size
      }
    }
  } else {
    stats.entries += 1
    stats.bytes += info.size
  }
  if (stats.entries > MAX_COPY_ENTRIES || stats.bytes > MAX_COPY_BYTES) {
    throw new Error(
      `要复制的内容太大（上限 ${MAX_COPY_ENTRIES} 个文件 / ${
        MAX_COPY_BYTES / 1024 / 1024
      }MB）`,
    )
  }
}

function copyTree(source: string, destination: string): void {
  const info = statSync(source)
  if (info.isDirectory()) {
    mkdirSync(destination, { recursive: true })
    for (const dirent of readdirSync(source, { withFileTypes: true })) {
      if (dirent.isSymbolicLink()) continue
      const child = join(source, dirent.name)
      if (dirent.isDirectory()) {
        copyTree(child, join(destination, dirent.name))
      } else if (dirent.isFile()) {
        copyFileSync(child, join(destination, dirent.name))
      }
    }
  } else {
    copyFileSync(source, destination)
  }
}

/**
 * Bring files or directories that already exist on this machine into the
 * session directory. Sources already inside it are returned untouched.
 */
export async function copyPaths(
  config: SpawnerConfig,
  cwd: string | undefined,
  paths: string[],
): Promise<CopyResult> {
  if (!Array.isArray(paths) || paths.length === 0) {
    throw new Error('没有要添加的文件')
  }
  if (paths.length > MAX_COPY_PATHS) {
    throw new Error(`一次最多添加 ${MAX_COPY_PATHS} 项`)
  }
  const root = sessionDirectory(config, cwd)
  const stats: CopyStats = { entries: 0, bytes: 0 }
  const entries: CopyResult['entries'] = []

  for (const raw of paths) {
    if (typeof raw !== 'string' || !raw.trim()) throw new Error('路径不合法')
    const source = resolve(raw.trim())
    if (
      source.length > 4096 ||
      !isAbsolute(source) ||
      (!source.startsWith(sep) && !/^[A-Za-z]:[\\/]/.test(source))
    ) {
      throw new Error(`路径不合法：${raw.slice(0, 120)}`)
    }
    let info
    try {
      info = statSync(source)
    } catch {
      throw new Error(`找不到：${raw.slice(0, 160)}`)
    }
    const type: 'dir' | 'file' = info.isDirectory() ? 'dir' : 'file'
    if (insideOrEqual(root, source)) {
      entries.push({ source, path: source, type, copied: false })
      continue
    }
    if (type === 'dir' && insideOrEqual(source, root)) {
      throw new Error('不能把会话目录的上级目录复制到它自己里面')
    }
    measure(source, stats)
    const destination = uniquePath(root, basename(source))
    copyTree(source, destination)
    entries.push({ source, path: destination, type, copied: true })
  }

  return { entries }
}
