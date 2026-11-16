import { expect, test } from 'bun:test'
import type Anthropic from '@anthropic-ai/sdk'
import { APIConnectionError } from '@anthropic-ai/sdk'

test('server provider retries a reset with a fresh connection', async () => {
  // Keep the process-wide keep-alive setting isolated from other test files.
  if (process.env.CCB_RETRY_E2E !== '1') {
    const child = Bun.spawn([process.execPath, 'test', import.meta.path], {
      env: { ...process.env, CCB_RETRY_E2E: '1' },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    const [code, output] = await Promise.all([
      child.exited,
      new Response(child.stderr).text(),
    ])
    if (code !== 0) throw new Error(`Retry integration failed: ${output}`)
    return
  }

  const { setServerProvider } = await import(
    '../../../utils/model/serverProviders'
  )
  const { getProxyFetchOptions, _resetKeepAliveForTesting } = await import(
    '../../../utils/proxy'
  )
  const { withRetry } = await import('../withRetry')
  _resetKeepAliveForTesting()
  setServerProvider({
    id: 'fixture',
    name: 'Fixture',
    protocol: 'anthropic',
    baseUrl: 'http://127.0.0.1',
    apiKey: 'fixture',
    models: ['fixture-model'],
    model: 'fixture-model',
    revision: 1,
  })
  const keepAlive: Array<boolean | undefined> = []
  try {
    const retry = withRetry(
      async () => {
        keepAlive.push(
          getProxyFetchOptions({
            url: 'http://127.0.0.1',
            forAnthropicAPI: false,
          }).keepalive,
        )
        return {} as Anthropic
      },
      async (_client, attempt) => {
        if (attempt === 1) {
          throw new APIConnectionError({
            cause: Object.assign(new Error('connection reset'), {
              code: 'ECONNRESET',
            }),
          })
        }
        return 'ok'
      },
      {
        maxRetries: 1,
        model: 'fixture-model',
        thinkingConfig: { type: 'disabled' },
      },
    )
    let result = await retry.next()
    while (!result.done) result = await retry.next()
    expect(result.value).toBe('ok')
    expect(keepAlive).toEqual([undefined, false])
  } finally {
    setServerProvider(null)
    _resetKeepAliveForTesting()
  }
}, 30_000)
