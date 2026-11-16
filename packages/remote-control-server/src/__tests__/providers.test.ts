import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Hono } from 'hono'
import providers from '../routes/web/providers'
import { resetProviderStoreForTest } from '../device/provider-store'
import { config } from '../config'

const dir = mkdtempSync(join(tmpdir(), 'rcs-providers-'))
const oldDir = process.env.RCS_DATA_DIR
const app = new Hono()
app.route('/web', providers)
beforeAll(() => {
  process.env.RCS_DATA_DIR = dir
})
afterAll(() => {
  resetProviderStoreForTest()
  if (oldDir === undefined) delete process.env.RCS_DATA_DIR
  else process.env.RCS_DATA_DIR = oldDir
  rmSync(dir, { recursive: true, force: true })
})

describe('provider management', () => {
  test('stores profiles without exposing credentials to the browser', async () => {
    const response = await app.request('/web/providers?uuid=test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        name: 'Example',
        protocol: 'openai',
        baseUrl: 'https://example.test/v1',
        apiKey: 'private-key',
        models: ['m1', 'm2'],
      }),
    })
    expect(response.status).toBe(201)
    const created = (await response.json()) as {
      provider: { id: string; hasApiKey: boolean }
    }
    expect(created.provider.hasApiKey).toBe(true)
    expect(JSON.stringify(created)).not.toContain('private-key')
    const list = await app.request('/web/providers?uuid=test')
    expect(await list.text()).not.toContain('private-key')
    const denied = await app.request(
      `/web/provider-credentials/${created.provider.id}?uuid=test`,
    )
    expect(denied.status).toBe(401)
    const uuidOnlySecret = await app.request(
      `/web/providers/${created.provider.id}/secret?uuid=test`,
    )
    expect(uuidOnlySecret.status).toBe(403)
    config.apiKeys.push('test-provider-access')
    try {
      const allowedSecret = await app.request(
        `/web/providers/${created.provider.id}/secret`,
        { headers: { Authorization: 'Bearer test-provider-access' } },
      )
      expect(allowedSecret.status).toBe(200)
      expect(allowedSecret.headers.get('cache-control')).toBe('no-store')
      expect(await allowedSecret.json()).toEqual({ apiKey: 'private-key' })
    } finally {
      config.apiKeys.splice(config.apiKeys.indexOf('test-provider-access'), 1)
    }
    if (config.apiKeys[0]) {
      const allowed = await app.request(
        `/web/provider-credentials/${created.provider.id}`,
        { headers: { Authorization: `Bearer ${config.apiKeys[0]}` } },
      )
      expect(allowed.status).toBe(200)
      expect(await allowed.text()).toContain('private-key')
    }
    const update = await app.request(
      `/web/providers/${created.provider.id}?uuid=test`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'Changed',
          protocol: 'openai',
          baseUrl: 'https://example.test/v1',
          models: ['m3'],
          modelDetails: [
            { model: 'm3', displayName: 'Friendly model', context1m: true },
          ],
        }),
      },
    )
    expect(update.status).toBe(200)
    const listed = await app.request('/web/providers?uuid=test')
    expect(await listed.json()).toMatchObject({
      providers: [
        {
          modelDetails: [
            { model: 'm3', displayName: 'Friendly model', context1m: true },
          ],
        },
      ],
    })
    expect(
      ((await update.json()) as { provider: { revision: number } }).provider
        .revision,
    ).toBe(2)
    const changedEndpoint = await app.request(
      `/web/providers/${created.provider.id}?uuid=test`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'Changed',
          protocol: 'openai',
          baseUrl: 'https://other.example/v1',
          models: ['m3'],
        }),
      },
    )
    expect(changedEndpoint.status).toBe(400)
    expect(
      (
        await app.request(`/web/providers/${created.provider.id}?uuid=test`, {
          method: 'DELETE',
        })
      ).status,
    ).toBe(200)
  })
})
