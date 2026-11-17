import { expect, test } from 'bun:test'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

test('startup restores server settings instead of an older local selection', async () => {
  // Bootstrap and configuration are process singletons; isolate them from the suite.
  if (process.env.CCB_RESTORE_E2E !== '1') {
    const child = Bun.spawn([process.execPath, 'test', import.meta.path], {
      env: { ...process.env, CCB_RESTORE_E2E: '1' },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    const [code, output] = await Promise.all([
      child.exited,
      new Response(child.stderr).text(),
    ])
    if (code !== 0) throw new Error(`Restore integration failed: ${output}`)
    return
  }
  const dir = mkdtempSync(join(tmpdir(), 'ccb-restore-e2e-'))
  const sid = '11111111-1111-4111-8111-111111111111'
  process.env.CLAUDE_CONFIG_DIR = dir
  process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = 'test-token'
  process.env.CLAUDE_BRIDGE_DEVICE_ID = 'dev-test'
  delete process.env.CLAUDE_CODE_EFFORT_LEVEL
  const server = Bun.serve({
    hostname: '127.0.0.1',
    port: 0,
    fetch(request) {
      const path = new URL(request.url).pathname
      if (path.endsWith('/settings'))
        return Response.json({
          settings: {
            values: {
              model: 'rcs-provider:new:target-model',
              effort: 'xhigh',
              ultracode: false,
            },
          },
        })
      if (path.endsWith('/provider-credentials/new'))
        return Response.json({
          provider: {
            id: 'new',
            name: 'new',
            protocol: 'anthropic',
            baseUrl: 'https://example.test',
            apiKey: 'test-key',
            models: ['target-model'],
            revision: 1,
          },
        })
      return new Response('not found', { status: 404 })
    },
  })
  process.env.CLAUDE_BRIDGE_BASE_URL = server.url.origin
  try {
    const { saveSessionProvider } = await import(
      '../model/serverProviderSession.js'
    )
    const { getServerProvider } = await import('../model/serverProviders.js')
    const { processResumedConversation } = await import('../sessionRestore.js')
    const { getDefaultAppState } = await import('../../state/AppStateStore.js')
    saveSessionProvider(sid, { id: 'old', model: 'old-model' })
    const result = await processResumedConversation(
      { messages: [], sessionId: sid },
      { forkSession: false },
      {
        modeApi: null,
        mainThreadAgentDefinition: undefined,
        agentDefinitions: { allAgents: [], activeAgents: [] },
        currentCwd: dir,
        cliAgents: [],
        initialState: getDefaultAppState(),
      },
    )
    expect(result.initialState.mainLoopModel).toBe('target-model')
    expect(result.initialState.effortValue).toBe('xhigh')
    expect(result.initialState.ultracodeActive).toBe(false)
    expect(getServerProvider()?.id).toBe('new')
  } finally {
    server.stop(true)
    rmSync(dir, { recursive: true, force: true })
  }
}, 30_000)
