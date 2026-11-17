import { expect, test } from 'bun:test'
import { APIError } from '@anthropic-ai/sdk'
import type Anthropic from '@anthropic-ai/sdk'

test('switching server providers uses the new endpoint without changing an active turn', async () => {
  if (process.env.CCB_SWITCH_E2E !== '1') {
    const child = Bun.spawn([process.execPath, 'test', import.meta.path], {
      env: { ...process.env, CCB_SWITCH_E2E: '1' },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    const [code, output] = await Promise.all([
      child.exited,
      new Response(child.stderr).text(),
    ])
    if (code !== 0)
      throw new Error(`Provider switch integration failed: ${output}`)
    return
  }
  const { getAnthropicClient } = await import('../client')
  const { setServerProvider, runWithServerProvider } = await import(
    '../../../utils/model/serverProviders'
  )
  const { withRetry } = await import('../withRetry')
  // Real terminals often retain auth from their original provider. Neither
  // environment credential may leak into a selected server provider.
  process.env.ANTHROPIC_AUTH_TOKEN = 'stale-env-token'
  process.env.ANTHROPIC_API_KEY = 'stale-env-key'
  const calls: Array<{
    provider: string
    key: string | null
    authorization: string | null
    model: string
  }> = []
  const server = Bun.serve({
    hostname: '127.0.0.1',
    port: 0,
    async fetch(request) {
      const body = (await request.json()) as { model: string }
      calls.push({
        provider: new URL(request.url).pathname.split('/')[1]!,
        key: request.headers.get('x-api-key'),
        authorization: request.headers.get('authorization'),
        model: body.model,
      })
      return Response.json({
        id: 'msg_fixture',
        type: 'message',
        role: 'assistant',
        model: body.model,
        content: [{ type: 'text', text: 'ok' }],
        stop_reason: 'end_turn',
        stop_sequence: null,
        usage: { input_tokens: 1, output_tokens: 1 },
      })
    },
  })
  const a = {
    id: 'a',
    name: 'A',
    protocol: 'anthropic' as const,
    baseUrl: `${server.url.origin}/a`,
    apiKey: 'key-a',
    models: ['model-a'],
    model: 'model-a',
    revision: 1,
  }
  const b = {
    id: 'b',
    name: 'B',
    protocol: 'anthropic' as const,
    baseUrl: `${server.url.origin}/b`,
    apiKey: 'key-b',
    models: ['model-b'],
    model: 'model-b',
    revision: 1,
  }
  const bearer = {
    ...b,
    id: 'c',
    baseUrl: `${server.url.origin}/c`,
    authMode: 'bearer' as const,
    apiKey: 'token-c',
  }
  const send = async (model: string) => {
    const client = await getAnthropicClient({ maxRetries: 0 })
    await client.messages.create({
      model,
      max_tokens: 8,
      messages: [{ role: 'user', content: 'test' }],
    })
  }
  try {
    setServerProvider(a)
    await send('model-a')
    let release!: () => void
    const gate = new Promise<void>(resolve => {
      release = resolve
    })
    const active = runWithServerProvider(a, async () => {
      await gate
      await send('model-a')
    })
    setServerProvider(b)
    await send('model-b')
    release()
    await active
    setServerProvider(bearer)
    await send('model-b')
    expect(calls).toEqual([
      { provider: 'a', key: 'key-a', authorization: null, model: 'model-a' },
      { provider: 'b', key: 'key-b', authorization: null, model: 'model-b' },
      { provider: 'a', key: 'key-a', authorization: null, model: 'model-a' },
      {
        provider: 'c',
        key: null,
        authorization: 'Bearer token-c',
        model: 'model-b',
      },
    ])
    let attempts = 0
    const retry = withRetry(
      async () => ({}) as Anthropic,
      async () => {
        attempts += 1
        throw APIError.generate(
          401,
          { error: { type: 'authentication_error' } },
          'bad key',
          new Headers(),
        )
      },
      { maxRetries: 3, model: 'model-b', thinkingConfig: { type: 'disabled' } },
    )
    await expect(retry.next()).rejects.toThrow()
    expect(attempts).toBe(1)
  } finally {
    setServerProvider(null)
    server.stop(true)
  }
}, 30_000)
