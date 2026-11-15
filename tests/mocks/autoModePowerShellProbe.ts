/** Executed in a fresh Bun process so the classifier mock cannot leak. */
import { mock } from 'bun:test'
import { strict as assert } from 'node:assert'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const root = process.cwd()
let shouldBlock = false
let classifierCalls = 0
mock.module(join(root, 'src/utils/permissions/yoloClassifier.ts'), () => ({
  YOLO_CLASSIFIER_TOOL_NAME: 'classify_result',
  buildTranscriptForClassifier: () => '',
  buildTranscriptEntries: () => [],
  buildYoloSystemPrompt: async () => '',
  getDefaultExternalAutoModeRules: () => ({
    allow: [],
    soft_deny: [],
    environment: [],
  }),
  buildDefaultExternalSystemPrompt: () => '',
  getAutoModeClassifierErrorDumpPath: () => '',
  getAutoModeClassifierTranscript: () => null,
  httpStatusOf: () => undefined,
  classifyYoloAction: async () => {
    classifierCalls += 1
    return { shouldBlock, reason: 'fixture' }
  },
  formatActionForClassifier: () => ({ role: 'assistant', content: [] }),
}))

const { hasPermissionsToUseTool } = await import(
  pathToFileURL(join(root, 'src/utils/permissions/permissions.ts')).href
)
const { getEmptyToolPermissionContext } = await import(
  pathToFileURL(join(root, 'src/Tool.ts')).href
)
const pc = { ...getEmptyToolPermissionContext(), mode: 'auto' as const }
const state = { toolPermissionContext: pc }
let reason: { type: string; classifierApprovable?: boolean } = { type: 'other' }
const tool = {
  name: 'PowerShell',
  aliases: [],
  inputSchema: { parse: (value: unknown) => value },
  checkPermissions: async () => ({
    behavior: 'ask',
    message: 'requires approval',
    decisionReason: reason,
  }),
  toAutoClassifierInput: (value: { command: string }) => value.command,
}
const context = {
  abortController: new AbortController(),
  getAppState: () => state,
  setAppState: () => {},
  options: { tools: [tool], isNonInteractiveSession: false },
  messages: [],
}
const message = { message: { id: 'fixture' } }
const check = () =>
  hasPermissionsToUseTool(
    tool as never,
    { command: 'Write-Output test' },
    context as never,
    message as never,
    'test',
  )

assert.equal((await check()).behavior, 'allow')
assert.equal(
  classifierCalls,
  1,
  'ordinary PowerShell commands reach the classifier',
)
shouldBlock = true
assert.equal((await check()).behavior, 'deny')
assert.equal(classifierCalls, 2, 'classifier can still block an action')
pc.alwaysAskRules = { ...pc.alwaysAskRules, localSettings: ['PowerShell'] }
assert.equal((await check()).behavior, 'ask')
assert.equal(classifierCalls, 2, 'explicit ask rule is never auto-approved')
pc.alwaysAskRules = {}
pc.alwaysDenyRules = { ...pc.alwaysDenyRules, localSettings: ['PowerShell'] }
assert.equal((await check()).behavior, 'deny')
assert.equal(classifierCalls, 2, 'explicit deny rule is never auto-approved')
pc.alwaysDenyRules = {}
reason = { type: 'safetyCheck', classifierApprovable: false }
assert.equal((await check()).behavior, 'ask')
assert.equal(
  classifierCalls,
  2,
  'non-classifier safety checks stay interactive',
)
console.log('PowerShell auto approval and guardrails verified')
