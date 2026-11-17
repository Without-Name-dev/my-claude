import { afterEach, expect, test } from 'bun:test'
import { getOpenAIClient } from '../../../services/api/openai/client.js'
import { getGrokClient } from '../../../services/api/grok/client.js'
import { getAnthropicClient } from '../../../services/api/client.js'
import { streamGeminiGenerateContent } from '../../../services/api/gemini/client.js'
import { getAPIProvider } from '../providers.js'
import {
  getServerProvider,
  fetchServerProviders,
  loadServerSessionSettings,
  runWithServerProvider,
  selectServerProvider,
  serverProviderRuntimeModel,
  resolveServerModel,
  setServerProvider,
  type ServerProvider,
} from '../serverProviders.js'

const base: ServerProvider = {
  id: 'a',
  name: 'A',
  protocol: 'openai',
  baseUrl: 'https://a.example/v1',
  apiKey: 'key-a',
  models: ['model-a'],
  model: 'model-a',
  revision: 1,
}
afterEach(() => setServerProvider(null))

test('loads this device and session settings before switching a conversation', async () => {
  const previousBase = process.env.CLAUDE_BRIDGE_BASE_URL
  const previousToken = process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
  const previousDevice = process.env.CLAUDE_BRIDGE_DEVICE_ID
  const originalFetch = globalThis.fetch
  let path = ''
  process.env.CLAUDE_BRIDGE_BASE_URL = 'http://rcs.example'
  process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = 'fixture-token'
  process.env.CLAUDE_BRIDGE_DEVICE_ID = 'dev-test'
  globalThis.fetch = Object.assign(
    async (input: RequestInfo | URL) => {
      path = new URL(String(input)).pathname
      return Response.json({
        settings: {
          values: {
            model: 'rcs-provider:p:model',
            effort: 'max',
            ultracode: true,
          },
        },
      })
    },
    { preconnect: originalFetch.preconnect },
  ) as typeof fetch
  try {
    expect(await loadServerSessionSettings('session-a')).toEqual({
      model: 'rcs-provider:p:model',
      effort: 'max',
      ultracode: true,
    })
    expect(path).toBe('/web/devices/dev-test/sessions/session-a/settings')
  } finally {
    globalThis.fetch = originalFetch
    if (previousBase === undefined) delete process.env.CLAUDE_BRIDGE_BASE_URL
    else process.env.CLAUDE_BRIDGE_BASE_URL = previousBase
    if (previousToken === undefined)
      delete process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
    else process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = previousToken
    if (previousDevice === undefined) delete process.env.CLAUDE_BRIDGE_DEVICE_ID
    else process.env.CLAUDE_BRIDGE_DEVICE_ID = previousDevice
  }
})

test('1M model budget is local and the provider receives its configured model ID', () => {
  const provider: ServerProvider = {
    ...base,
    modelDetails: [
      { model: 'model-a', displayName: 'My model', context1m: true },
    ],
  }
  setServerProvider(provider)
  expect(serverProviderRuntimeModel(provider)).toBe('model-a[1m]')
  expect(resolveServerModel('model-a[1m]')).toBe('model-a')
  expect(serverProviderRuntimeModel(base)).toBe('model-a')
})

test('retries a proxy failure directly without changing provider requests', async () => {
  const previousBase = process.env.CLAUDE_BRIDGE_BASE_URL
  const previousToken = process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
  const originalFetch = globalThis.fetch
  const proxies: unknown[] = []
  process.env.CLAUDE_BRIDGE_BASE_URL = 'http://rcs.example'
  process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = 'fixture-token'
  globalThis.fetch = Object.assign(
    async (_input: RequestInfo | URL, init?: RequestInit) => {
      proxies.push((init as RequestInit & { proxy?: unknown })?.proxy)
      return proxies.length === 1
        ? new Response(null, { status: 502 })
        : Response.json({ providers: [] })
    },
    { preconnect: originalFetch.preconnect },
  ) as typeof fetch
  try {
    expect(await fetchServerProviders()).toEqual([])
    expect(proxies).toEqual([undefined, false])
  } finally {
    globalThis.fetch = originalFetch
    if (previousBase === undefined) delete process.env.CLAUDE_BRIDGE_BASE_URL
    else process.env.CLAUDE_BRIDGE_BASE_URL = previousBase
    if (previousToken === undefined)
      delete process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
    else process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = previousToken
  }
})

test('does not retry an authentication rejection through another route', async () => {
  const previousBase = process.env.CLAUDE_BRIDGE_BASE_URL
  const previousToken = process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
  const originalFetch = globalThis.fetch
  let requests = 0
  process.env.CLAUDE_BRIDGE_BASE_URL = 'http://rcs.example'
  process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = 'fixture-token'
  globalThis.fetch = Object.assign(
    async () => {
      requests++
      return new Response(null, { status: 401 })
    },
    { preconnect: originalFetch.preconnect },
  ) as typeof fetch
  try {
    await expect(fetchServerProviders()).rejects.toThrow('401')
    expect(requests).toBe(1)
  } finally {
    globalThis.fetch = originalFetch
    if (previousBase === undefined) delete process.env.CLAUDE_BRIDGE_BASE_URL
    else process.env.CLAUDE_BRIDGE_BASE_URL = previousBase
    if (previousToken === undefined)
      delete process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
    else process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = previousToken
  }
})

test('concurrent provider snapshots use their own endpoint and credential', async () => {
  // Other suites mock the API client globally; exercise the real SDK in an
  // isolated process so this integration check is independent of test order.
  if (process.env.CCB_PROVIDER_ISOLATED_TEST !== '1') {
    const child = Bun.spawn([process.execPath, 'test', import.meta.path], {
      env: { ...process.env, CCB_PROVIDER_ISOLATED_TEST: '1' },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    const [exitCode, stderr] = await Promise.all([
      child.exited,
      new Response(child.stderr).text(),
    ])
    if (exitCode !== 0)
      throw new Error(`Provider isolation test failed: ${stderr}`)
    return
  }
  const other: ServerProvider = {
    ...base,
    id: 'b',
    name: 'B',
    baseUrl: 'https://b.example/v1',
    apiKey: 'key-b',
    model: 'model-b',
    models: ['model-b'],
  }
  const seen: Array<{ url: string; authorization: string | null }> = []
  const mockFetch: typeof fetch = Object.assign(
    async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init)
      seen.push({
        url: request.url,
        authorization: request.headers.get('authorization'),
      })
      return Response.json({
        id: 'completion',
        object: 'chat.completion',
        created: 0,
        model: 'fixture',
        choices: [],
      })
    },
    { preconnect: fetch.preconnect },
  )
  const invoke = (profile: ServerProvider) =>
    runWithServerProvider(profile, async () => {
      await Promise.resolve()
      expect(getAPIProvider()).toBe('openai')
      await getOpenAIClient({
        fetchOverride: mockFetch,
      }).chat.completions.create({ model: profile.model, messages: [] })
      expect(getServerProvider()?.id).toBe(profile.id)
    })
  setServerProvider(base)
  await Promise.all([invoke(base), invoke(other)])
  expect(
    seen.find(item => item.url.startsWith('https://a.example/'))?.authorization,
  ).toBe('Bearer key-a')
  expect(
    seen.find(item => item.url.startsWith('https://b.example/'))?.authorization,
  ).toBe('Bearer key-b')
}, 30_000)

test('a scoped previous provider survives changing the next selection', async () => {
  setServerProvider(base)
  const current = getServerProvider()
  await runWithServerProvider(current, async () => {
    setServerProvider({ ...base, id: 'new', apiKey: 'key-new' })
    await Promise.resolve()
    expect(getServerProvider()?.id).toBe('a')
  })
  expect(getServerProvider()?.id).toBe('new')
})

test('a slower earlier switch cannot overwrite a newer selection', async () => {
  const previousBase = process.env.CLAUDE_BRIDGE_BASE_URL
  const previousToken = process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
  let releaseFirst!: () => void
  let firstArrived!: () => void
  const arrived = new Promise<void>(resolve => {
    firstArrived = resolve
  })
  const waitFirst = new Promise<void>(resolve => {
    releaseFirst = resolve
  })
  const server = Bun.serve({
    hostname: '127.0.0.1',
    port: 0,
    async fetch(request) {
      const id = new URL(request.url).pathname.split('/').at(-1)!
      if (id === 'a') {
        firstArrived()
        await waitFirst
      }
      return Response.json({ provider: { ...base, id, models: ['model-a'] } })
    },
  })
  process.env.CLAUDE_BRIDGE_BASE_URL = server.url.origin
  process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = 'test-token'
  try {
    const first = selectServerProvider('a', 'model-a')
    await arrived
    await selectServerProvider('b', 'model-a')
    releaseFirst()
    await expect(first).rejects.toThrow('已有更新的模型选择')
    expect(getServerProvider()?.id).toBe('b')
  } finally {
    releaseFirst()
    server.stop(true)
    if (previousBase === undefined) delete process.env.CLAUDE_BRIDGE_BASE_URL
    else process.env.CLAUDE_BRIDGE_BASE_URL = previousBase
    if (previousToken === undefined)
      delete process.env.CLAUDE_BRIDGE_OAUTH_TOKEN
    else process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = previousToken
  }
})

test('Anthropic, Grok and Gemini profiles use their configured endpoints and credentials', async () => {
  if (process.env.CCB_PROVIDER_ISOLATED_TEST !== '1') {
    const child = Bun.spawn([process.execPath, 'test', import.meta.path], {
      env: { ...process.env, CCB_PROVIDER_ISOLATED_TEST: '1' },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    const [code, stderr] = await Promise.all([
      child.exited,
      new Response(child.stderr).text(),
    ])
    if (code !== 0) throw new Error(`Provider protocol test failed: ${stderr}`)
    return
  }
  const seen: Array<{ url: string; headers: Headers }> = []
  const mockFetch: typeof fetch = Object.assign(
    async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init)
      seen.push({ url: request.url, headers: request.headers })
      if (request.url.includes('/models/'))
        return new Response('data: [DONE]\n\n', {
          headers: { 'content-type': 'text/event-stream' },
        })
      return Response.json({
        id: 'fixture',
        type: 'message',
        role: 'assistant',
        content: [],
        model: 'm',
        stop_reason: 'end_turn',
        usage: { input_tokens: 0, output_tokens: 0 },
        choices: [],
      })
    },
    { preconnect: fetch.preconnect },
  )
  await runWithServerProvider(
    {
      ...base,
      protocol: 'anthropic',
      baseUrl: 'https://anthropic.example',
      apiKey: 'anthropic-key',
      authMode: 'bearer',
    },
    async () => {
      const client = await getAnthropicClient({
        maxRetries: 0,
        fetchOverride: mockFetch,
      })
      await client.messages.create({ model: 'm', max_tokens: 1, messages: [] })
    },
  )
  await runWithServerProvider(
    {
      ...base,
      protocol: 'grok',
      baseUrl: 'https://grok.example/v1',
      apiKey: 'grok-key',
    },
    async () => {
      await getGrokClient({ fetchOverride: mockFetch }).chat.completions.create(
        { model: 'm', messages: [] },
      )
    },
  )
  await runWithServerProvider(
    {
      ...base,
      protocol: 'gemini',
      baseUrl: 'https://gemini.example/v1beta',
      apiKey: 'gemini-key',
    },
    async () => {
      for await (const _chunk of streamGeminiGenerateContent({
        model: 'm',
        body: { contents: [] },
        signal: new AbortController().signal,
        fetchOverride: mockFetch,
      })) {
        /* consume */
      }
    },
  )
  expect(
    seen
      .find(item => item.url.startsWith('https://anthropic.example/'))
      ?.headers.get('authorization'),
  ).toBe('Bearer anthropic-key')
  expect(
    seen
      .find(item => item.url.startsWith('https://grok.example/'))
      ?.headers.get('authorization'),
  ).toBe('Bearer grok-key')
  expect(
    seen
      .find(item => item.url.startsWith('https://gemini.example/'))
      ?.headers.get('x-goog-api-key'),
  ).toBe('gemini-key')
}, 30_000)
