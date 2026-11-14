import { realpathSync } from 'node:fs'
import { isAbsolute, relative, resolve, sep } from 'node:path'

/** Compare real paths so junctions cannot escape an allowed directory. */
export function isWithin(parent: string, child: string): boolean {
  const base = resolve(parent)
  const value = resolve(child)
  const normalizedBase =
    process.platform === 'win32' ? base.toLowerCase() : base
  const normalizedValue =
    process.platform === 'win32' ? value.toLowerCase() : value
  const difference = relative(normalizedBase, normalizedValue)
  return (
    difference === '' ||
    (difference !== '..' &&
      !difference.startsWith(`..${sep}`) &&
      !isAbsolute(difference))
  )
}

export function underAllowedRoot(path: string, roots: string[]): boolean {
  if (roots.length === 0) return true
  try {
    const target = realpathSync(path)
    return roots.some(root => {
      try {
        return isWithin(realpathSync(root), target)
      } catch {
        return false
      }
    })
  } catch {
    return false
  }
}
