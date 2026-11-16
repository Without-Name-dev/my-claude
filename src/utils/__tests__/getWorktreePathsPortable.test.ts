import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  getWorktreePathsPortable,
  hasPotentialGitRepository,
} from '../getWorktreePathsPortable'

describe('portable Git repository discovery', () => {
  let root: string
  beforeAll(async () => {
    root = await mkdtemp(join(tmpdir(), 'ccb-worktree-discovery-'))
  })
  afterAll(async () => {
    await rm(root, { recursive: true, force: true })
  })

  test('skips Git for a directory outside a repository', async () => {
    if (
      process.env.GIT_DIR ||
      process.env.GIT_WORK_TREE ||
      process.env.GIT_COMMON_DIR ||
      process.env.GIT_OBJECT_DIRECTORY
    )
      return
    const path = join(root, 'plain')
    await mkdir(path)
    expect(await hasPotentialGitRepository(path)).toBe(false)
    expect(await getWorktreePathsPortable(path)).toEqual([])
  })

  test('recognizes repository directories and linked worktree files in ancestors', async () => {
    const repository = join(root, 'repo')
    const nested = join(repository, 'nested')
    await mkdir(nested, { recursive: true })
    await mkdir(join(repository, '.git'))
    expect(await hasPotentialGitRepository(nested)).toBe(true)
    const linked = join(root, 'linked')
    await mkdir(linked)
    await writeFile(join(linked, '.git'), 'gitdir: elsewhere')
    expect(await hasPotentialGitRepository(linked)).toBe(true)
  })

  test('recognizes a bare repository', async () => {
    const bare = join(root, 'bare')
    await mkdir(join(bare, 'objects'), { recursive: true })
    await writeFile(join(bare, 'HEAD'), 'ref: refs/heads/main\n')
    expect(await hasPotentialGitRepository(bare)).toBe(true)
  })

  test('still lists real worktrees from a nested repository directory', async () => {
    const paths = await getWorktreePathsPortable(process.cwd())
    expect(paths.length).toBeGreaterThan(0)
  }, 15_000)
})
