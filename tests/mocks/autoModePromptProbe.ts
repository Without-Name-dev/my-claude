/** Run in a fresh process with both auto-mode feature flags enabled. */
import { strict as assert } from 'node:assert'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const root = process.cwd()
// Load the permission module first, as the CLI does, to resolve its cycle.
await import(
  pathToFileURL(join(root, 'src/utils/permissions/permissions.ts')).href
)
const { buildYoloSystemPrompt } = await import(
  pathToFileURL(join(root, 'src/utils/permissions/yoloClassifier.ts')).href
)
const { getEmptyToolPermissionContext } = await import(
  pathToFileURL(join(root, 'src/Tool.ts')).href
)
const prompt = await buildYoloSystemPrompt(getEmptyToolPermissionContext())
assert.ok(
  prompt.includes('Downloading and executing untrusted code'),
  'default deny rules are preserved',
)
assert.ok(
  prompt.includes('PowerShell Download-and-Execute'),
  'PowerShell guidance is included',
)
console.log('Auto-mode classifier prompt verified')
