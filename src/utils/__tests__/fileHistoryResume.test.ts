import { expect, test } from 'bun:test'
import {
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  rm,
  writeFile,
} from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { LogOption } from '../../types/logs.js'
import { copyFileHistoryForResume } from '../fileHistory.js'

test('resume preserves existing file history; a fork copies it into its own session', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'ccb-history-resume-'))
  const originalDir = process.env.CLAUDE_CONFIG_DIR
  const originalDisabled = process.env.CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING
  const originalSdk = process.env.CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING
  process.env.CLAUDE_CONFIG_DIR = dir
  delete process.env.CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING
  process.env.CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING = '1'
  const source = '11111111-1111-4111-8111-111111111111'
  const fork = '22222222-2222-4222-8222-222222222222'
  try {
    const sourceDir = join(dir, 'file-history', source)
    await mkdir(sourceDir, { recursive: true })
    await writeFile(join(sourceDir, 'backup'), 'original contents')
    const log = {
      messages: [{ sessionId: source }],
      fileHistorySnapshots: [
        { trackedFileBackups: { file: { backupFileName: 'backup' } } },
      ],
    } as unknown as LogOption
    await copyFileHistoryForResume(log, source)
    expect(await readdir(join(dir, 'file-history'))).toEqual([source])
    await copyFileHistoryForResume(log, fork)
    expect(
      await readFile(join(dir, 'file-history', fork, 'backup'), 'utf8'),
    ).toBe('original contents')
  } finally {
    if (originalDir === undefined) delete process.env.CLAUDE_CONFIG_DIR
    else process.env.CLAUDE_CONFIG_DIR = originalDir
    if (originalDisabled === undefined)
      delete process.env.CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING
    else process.env.CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING = originalDisabled
    if (originalSdk === undefined)
      delete process.env.CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING
    else process.env.CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING = originalSdk
    await rm(dir, { recursive: true, force: true })
  }
})
