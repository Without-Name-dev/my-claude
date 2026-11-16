import { expect, test } from 'bun:test'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'

test('server-managed provider is selected and receives a direct streaming request', async () => {
  // Keep global configuration and module caches isolated from other suites.
  if (process.env.CCB_PROVIDER_E2E !== '1') {
    const child = Bun.spawn([process.execPath, 'test', import.meta.path], {
      env: { ...process.env, CCB_PROVIDER_E2E: '1' },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    const [code, output] = await Promise.all([
      child.exited,
      new Response(child.stderr).text(),
    ])
    if (code !== 0) throw new Error(`Provider integration failed: ${output}`)
    return
  }

  const dir = mkdtempSync(join(tmpdir(), 'ccb-provider-e2e-'))
  process.env.RCS_DATA_DIR = dir
  process.env.CLAUDE_CONFIG_DIR = dir
  process.env.RCS_API_KEYS = 'fixture-rcs-key'
  process.env.RCS_WEB_ACCESS_KEY = ''
  delete process.env.HTTP_PROXY
  delete process.env.HTTPS_PROXY
  delete process.env.http_proxy
  delete process.env.https_proxy
  process.env.NO_PROXY = '127.0.0.1,localhost'

  const { default: routes } = await import(
    '../../../../packages/remote-control-server/src/routes/web/providers'
  )
  const { resetProviderStoreForTest } = await import(
    '../../../../packages/remote-control-server/src/device/provider-store'
  )
  const {
    fetchServerProviders,
    getServerProvider,
    selectServerProvider,
    rememberServerProvider,
    initializeNewSessionProvider,
    restoreSessionProvider,
    setServerProvider,
  } = await import('../serverProviders')
  const { getOpenAIClient } = await import(
    '../../../services/api/openai/client'
  )
  const { getAnthropicClient } = await import('../../../services/api/client')
  const { saveSessionProvider } = await import('../serverProviderSession')
  const received: Array<{
    path: string
    authorization: string | null
    model: unknown
  }> = []
  const provider = Bun.serve({
    hostname: '127.0.0.1',
    port: 0,
    async fetch(request) {
      const body = (await request.json()) as { model?: unknown }
      received.push({
        path: new URL(request.url).pathname,
        authorization: request.headers.get('authorization'),
        model: body.model,
      })
      return new Response(
        'data: {"id":"fixture","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"MOCK_OK"},"finish_reason":null}]}\n\n' +
          'data: {"id":"fixture","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\n' +
          'data: [DONE]\n\n',
        { headers: { 'content-type': 'text/event-stream' } },
      )
    },
  })
  const rcs = Bun.serve({
    hostname: '127.0.0.1',
    port: 0,
    fetch(request) {
      const url = new URL(request.url)
      if (!url.pathname.startsWith('/web/'))
        return new Response(null, { status: 404 })
      url.pathname = url.pathname.slice(4)
      return routes.fetch(new Request(url, request))
    },
  })
  process.env.CLAUDE_BRIDGE_BASE_URL = rcs.url.origin
  process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = 'fixture-rcs-key'
  try {
    const create = await fetch(`${rcs.url.origin}/web/providers?uuid=fixture`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        name: 'Mock Provider',
        protocol: 'openai',
        baseUrl: `${provider.url.origin}/v1`,
        apiKey: 'fixture-provider-key',
        models: ['model-a'],
      }),
    })
    expect(create.status).toBe(201)
    const created = (await create.json()) as { provider: { id: string } }
    const list = await fetchServerProviders()
    expect(list.map(item => item.id)).toEqual([created.provider.id])
    expect(JSON.stringify(list)).not.toContain('fixture-provider-key')
    await selectServerProvider(created.provider.id, 'model-a', 'session-a')
    await rememberServerProvider(getServerProvider()!)
    // The preference is durable and is only a provider/model identifier.
    resetProviderStoreForTest()
    setServerProvider(null)
    expect((await initializeNewSessionProvider('new-session'))?.model).toBe(
      'model-a',
    )
    expect((await restoreSessionProvider('session-a'))?.model).toBe('model-a')
    const second = await fetch(`${rcs.url.origin}/web/providers?uuid=fixture`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        name: 'Second',
        protocol: 'openai',
        baseUrl: `${provider.url.origin}/v1`,
        apiKey: 'fixture-second-key',
        models: ['model-b'],
        modelDetails: [{ model: 'model-b', displayName: 'B', context1m: true }],
      }),
    })
    const secondId = ((await second.json()) as { provider: { id: string } })
      .provider.id
    const secondProvider = await selectServerProvider(
      secondId,
      'model-b',
      'session-b',
    )
    await rememberServerProvider(secondProvider)
    setServerProvider(null)
    expect((await initializeNewSessionProvider('new-session-b'))?.model).toBe(
      'model-b',
    )
    expect((await restoreSessionProvider('session-a'))?.model).toBe('model-a')
    // Restoring another session does not alter the next session's preference.
    expect((await initializeNewSessionProvider('new-session-c'))?.model).toBe(
      'model-b',
    )
    setServerProvider(null)
    expect((await restoreSessionProvider('session-a'))?.id).toBe(
      created.provider.id,
    )
    const stream = await getOpenAIClient({
      maxRetries: 0,
    }).chat.completions.create({
      model: 'model-a',
      messages: [{ role: 'user', content: 'test' }],
      stream: true,
    })
    let result = ''
    for await (const part of stream)
      result += part.choices[0]?.delta.content ?? ''
    expect(result).toBe('MOCK_OK')
    expect(received).toEqual([
      {
        path: '/v1/chat/completions',
        authorization: 'Bearer fixture-provider-key',
        model: 'model-a',
      },
    ])
    await expect(selectServerProvider('missing', 'model-a')).rejects.toThrow()
    expect(getServerProvider()?.id).toBe(created.provider.id)
    saveSessionProvider('bad-session', { id: 'missing', model: 'model-a' })
    await expect(restoreSessionProvider('bad-session')).rejects.toThrow()
    expect(getServerProvider()?.id).toBe(created.provider.id)
    const unauthorized = await fetch(
      `${rcs.url.origin}/web/provider-preference`,
    )
    expect(unauthorized.status).toBe(401)
    const deletion = await fetch(
      `${rcs.url.origin}/web/providers/${secondId}?uuid=fixture`,
      { method: 'DELETE' },
    )
    expect(deletion.status).toBe(200)
    await expect(
      initializeNewSessionProvider('deleted-provider'),
    ).rejects.toThrow()
    // A missing new-session provider must not switch an existing session.
    expect(getServerProvider()?.id).toBe(created.provider.id)

    // An Anthropic-compatible server profile is not the SSH tunnel to the
    // official API. It must also respect NO_PROXY under Bun.
    const anthropicFixture = Bun.serve({
      hostname: '127.0.0.1',
      port: 0,
      fetch() {
        return Response.json({
          id: 'msg_fixture',
          type: 'message',
          role: 'assistant',
          model: 'fixture',
          content: [{ type: 'text', text: 'OK' }],
          stop_reason: 'end_turn',
          stop_sequence: null,
          usage: { input_tokens: 1, output_tokens: 1 },
        })
      },
    })
    try {
      process.env.HTTPS_PROXY = 'http://127.0.0.1:1'
      process.env.NO_PROXY = '127.0.0.1,localhost'
      process.env.ANTHROPIC_UNIX_SOCKET = join(dir, 'missing-ssh-socket')
      setServerProvider({
        id: 'anthropic-fixture',
        name: 'fixture',
        protocol: 'anthropic',
        baseUrl: anthropicFixture.url.origin,
        apiKey: 'fixture-key',
        models: ['fixture'],
        model: 'fixture',
        revision: 1,
      })
      const sdk = await getAnthropicClient({ maxRetries: 0 })
      const result = await sdk.messages.create({
        model: 'fixture',
        max_tokens: 8,
        messages: [{ role: 'user', content: 'hello' }],
      })
      expect(result.content[0]?.type).toBe('text')
    } finally {
      anthropicFixture.stop(true)
      delete process.env.ANTHROPIC_UNIX_SOCKET
      delete process.env.HTTPS_PROXY
    }
  } finally {
    setServerProvider(null)
    provider.stop(true)
    rcs.stop(true)
    resetProviderStoreForTest()
    if (
      dirname(resolve(dir)) === resolve(tmpdir()) &&
      basename(dir).startsWith('ccb-provider-e2e-')
    ) {
      rmSync(dir, { recursive: true, force: true })
    }
  }
}, 45_000)
