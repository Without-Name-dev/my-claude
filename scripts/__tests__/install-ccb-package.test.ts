import { afterEach, expect, test } from 'bun:test'
import {
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const roots: string[] = []
afterEach(() => {
  for (const root of roots.splice(0))
    rmSync(root, { recursive: true, force: true })
})

test('updates the entrypoint while retaining old chunks for running terminals', () => {
  const root = mkdtempSync(join(tmpdir(), 'ccb-install-'))
  roots.push(root)
  const source = join(root, 'build')
  const target = join(root, 'installed')
  mkdirSync(source)
  mkdirSync(target)
  writeFileSync(join(target, 'cli.js'), 'old entry')
  writeFileSync(join(target, 'chunk-old.js'), 'old chunk')
  writeFileSync(join(source, 'cli.js'), 'new entry')
  writeFileSync(
    join(source, 'chunk-new.js'),
    'new chunk\n//# sourceMappingURL=chunk-new.js.map\n',
  )
  writeFileSync(join(source, 'chunk-new.js.map'), '{}')

  const run = () =>
    Bun.spawnSync([
      process.execPath,
      join(import.meta.dir, '..', 'install-ccb-package.ts'),
      source,
      target,
    ])
  expect(run().exitCode).toBe(0)
  expect(readFileSync(join(target, 'cli.js'), 'utf8')).toBe('new entry')
  expect(readFileSync(join(target, 'chunk-old.js'), 'utf8')).toBe('old chunk')
  expect(readFileSync(join(target, 'chunk-new.js'), 'utf8')).toBe('new chunk\n')

  // Existing builds may still contain a source-map comment. It is the same
  // chunk and must stay intact for processes that can load it later.
  writeFileSync(
    join(target, 'chunk-new.js'),
    'new chunk\n//# sourceMappingURL=chunk-new.js.map\n',
  )
  expect(run().exitCode).toBe(0)
  expect(readFileSync(join(target, 'chunk-new.js'), 'utf8')).toContain(
    'sourceMappingURL',
  )

  writeFileSync(join(source, 'chunk-new.js'), 'conflicting chunk')
  writeFileSync(join(source, 'cli.js'), 'unsafe entry')
  expect(run().exitCode).not.toBe(0)
  expect(readFileSync(join(target, 'cli.js'), 'utf8')).toBe('new entry')
})
