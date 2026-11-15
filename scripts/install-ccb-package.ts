/** Install a split Bun build without deleting chunks needed by running CLIs. */
import {
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  writeFileSync,
} from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { randomUUID } from 'node:crypto'

const [sourceArg, targetArg] = process.argv.slice(2)
if (!sourceArg || !targetArg) {
  throw new Error(
    'Usage: bun scripts/install-ccb-package.ts <build-dir> <package-dist>',
  )
}
const source = resolve(sourceArg)
const target = resolve(targetArg)
if (source === target)
  throw new Error('Build and package directories must differ')

function releaseContents(relativePath: string, bytes: Buffer): Buffer {
  if (!relativePath.endsWith('.js')) return bytes
  return Buffer.from(
    bytes
      .toString('utf8')
      .replace(
        /^[ \t]*\/\/[#@][ \t]*sourceMappingURL=[^\r\n]*\.map[ \t]*(?:\r?\n|$)/gm,
        '',
      ),
  )
}

const files = readdirSync(source, { recursive: true, withFileTypes: true })
  .filter(entry => entry.isFile())
  .map(entry => {
    // parentPath is supported by Bun and preserves nested build output.
    const parent = entry.parentPath
    return join(parent.slice(source.length + 1), entry.name)
  })
  .filter(path => !path.endsWith('.map'))
if (!files.includes('cli.js')) throw new Error('Build is missing cli.js')

// Install content-addressed chunks before making the new entrypoint visible.
// Existing chunks must never be overwritten: an older process may import one
// later, including after a long-running tool call finishes.
mkdirSync(target, { recursive: true })
for (const file of files) {
  if (file === 'cli.js') continue
  const output = join(target, file)
  const bytes = releaseContents(file, readFileSync(join(source, file)))
  mkdirSync(dirname(output), { recursive: true })
  try {
    writeFileSync(output, bytes, { flag: 'wx' })
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error
    if (!releaseContents(file, readFileSync(output)).equals(bytes)) {
      throw new Error(
        `Existing package chunk differs: ${file}; package entrypoint unchanged`,
      )
    }
  }
}

const temporaryEntry = join(target, `.cli-${randomUUID()}.tmp`)
writeFileSync(
  temporaryEntry,
  releaseContents('cli.js', readFileSync(join(source, 'cli.js'))),
  { flag: 'wx' },
)
renameSync(temporaryEntry, join(target, 'cli.js'))
console.log(
  'CCB package updated; existing chunks retained for running sessions.',
)
