import { execFile as execFileCb } from 'child_process'
import { stat } from 'fs/promises'
import { dirname, join, resolve } from 'path'
import { promisify } from 'util'

const execFileAsync = promisify(execFileCb)

/** Avoid starting Git when no ancestor can contain a repository. */
export async function hasPotentialGitRepository(cwd: string): Promise<boolean> {
  // Git can be pointed at a repository outside the directory hierarchy.
  if (
    process.env.GIT_DIR ||
    process.env.GIT_WORK_TREE ||
    process.env.GIT_COMMON_DIR ||
    process.env.GIT_OBJECT_DIRECTORY
  )
    return true
  let current = resolve(cwd)
  while (true) {
    try {
      await stat(join(current, '.git'))
      return true
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') return true
    }
    // Bare repositories have no .git entry.
    try {
      const [head, objects] = await Promise.all([
        stat(join(current, 'HEAD')),
        stat(join(current, 'objects')),
      ])
      if (head.isFile() && objects.isDirectory()) return true
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') return true
    }
    const parent = dirname(current)
    if (parent === current) return false
    current = parent
  }
}

/**
 * Portable worktree detection using only child_process — no analytics,
 * no bootstrap deps, no execa. Used by listSessionsImpl.ts (SDK) and
 * anywhere that needs worktree paths without pulling in the CLI
 * dependency chain (execa → cross-spawn → which).
 */
export async function getWorktreePathsPortable(cwd: string): Promise<string[]> {
  try {
    if (!(await hasPotentialGitRepository(cwd))) return []
    const { stdout } = await execFileAsync(
      'git',
      ['worktree', 'list', '--porcelain'],
      { cwd, timeout: 5000 },
    )
    if (!stdout) return []
    return stdout
      .split('\n')
      .filter(line => line.startsWith('worktree '))
      .map(line => line.slice('worktree '.length).normalize('NFC'))
  } catch {
    return []
  }
}
