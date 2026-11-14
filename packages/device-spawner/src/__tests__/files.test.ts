import { afterEach, describe, expect, test } from 'bun:test'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join, parse } from 'node:path'
import type { SpawnerConfig } from '../config'
import { copyPaths, listDirectory, uploadFiles } from '../files'

const paths: string[] = []
function fixture() {
  const base = mkdtempSync(join(tmpdir(), 'ccb-files-test-'))
  paths.push(base)
  const root = join(base, 'project')
  mkdirSync(root)
  return { base, root, config: { allowedRoots: [root] } as SpawnerConfig }
}
afterEach(() => {
  for (const path of paths.splice(0))
    rmSync(path, { recursive: true, force: true })
})

describe('directory selection and file intake', () => {
  test('shows configured roots and accepts a drive root and Windows case differences', async () => {
    const { root, config } = fixture()
    expect((await listDirectory(config, undefined)).roots?.[0]?.path).toBe(root)
    expect(
      (
        await listDirectory(
          { ...config, allowedRoots: [parse(root).root] },
          root,
        )
      ).path,
    ).toBe(root)
    if (process.platform === 'win32') {
      expect(
        (
          await listDirectory(
            { ...config, allowedRoots: [root.toUpperCase()] },
            root,
          )
        ).path,
      ).toBe(root)
    }
  })

  test('rejects malformed base64, while allowing an empty file', async () => {
    const { root, config } = fixture()
    await expect(
      uploadFiles(config, [{ name: 'bad', data: '!!!' }], root),
    ).rejects.toThrow('base64')
    expect(existsSync(join(root, 'bad'))).toBe(false)
    await uploadFiles(config, [{ name: 'empty', data: '' }], root)
    expect(readFileSync(join(root, 'empty')).length).toBe(0)
  })

  test('accepts a valid file at the 20 MB size limit', async () => {
    const { root, config } = fixture()
    const data = Buffer.alloc(20 * 1024 * 1024, 97)
    const result = await uploadFiles(
      config,
      [{ name: 'large.bin', data: data.toString('base64') }],
      root,
    )
    expect(result.files[0]?.bytes).toBe(data.length)
    expect(readFileSync(join(root, 'large.bin')).equals(data)).toBe(true)
  })

  test('does not follow a linked upload folder outside the session', async () => {
    const { base, root, config } = fixture()
    const outside = join(base, 'outside')
    mkdirSync(outside)
    symlinkSync(
      outside,
      join(root, 'link'),
      process.platform === 'win32' ? 'junction' : 'dir',
    )
    await expect(
      uploadFiles(config, [{ name: 'link/file', data: 'b2s=' }], root),
    ).rejects.toThrow('之外')
    expect(existsSync(join(outside, 'file'))).toBe(false)
    await expect(listDirectory(config, join(root, 'link'))).rejects.toThrow(
      '允许的根目录',
    )
  })

  test('rejects relative copy sources before resolving them', async () => {
    const { root, config } = fixture()
    await expect(copyPaths(config, root, ['relative.txt'])).rejects.toThrow(
      '路径不合法',
    )
  })
})
