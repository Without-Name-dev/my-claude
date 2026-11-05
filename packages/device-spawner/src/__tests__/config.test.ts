import { afterAll, describe, expect, test } from 'bun:test'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  configPath,
  defaultConfig,
  deviceWebSocketUrl,
  loadConfig,
} from '../config'

const dirs: string[] = []
const previousConfig = process.env.SPAWNER_CONFIG

async function tempConfig(content?: Record<string, unknown>): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'spawner-config-'))
  dirs.push(dir)
  const path = join(dir, 'spawner.json')
  if (content) await writeFile(path, JSON.stringify(content, null, 2))
  return path
}

afterAll(async () => {
  if (previousConfig === undefined) delete process.env.SPAWNER_CONFIG
  else process.env.SPAWNER_CONFIG = previousConfig
  for (const dir of dirs) await rm(dir, { recursive: true, force: true })
})

describe('spawner config', () => {
  test('defaults include a generated identity and no hard-coded secret', () => {
    const config = defaultConfig()
    expect(config.deviceId.startsWith('dev_')).toBe(true)
    expect(config.deviceSecret.length).toBeGreaterThanOrEqual(32)
    expect(config.enrolled).toBe(false)
    expect(config.heartbeatMs).toBe(15_000)
  })

  test('derives the device websocket URL', () => {
    expect(deviceWebSocketUrl('http://127.0.0.1:3000')).toBe(
      'ws://127.0.0.1:3000/desktop/ws',
    )
    expect(deviceWebSocketUrl('https://rcs.example.com/base')).toBe(
      'wss://rcs.example.com/desktop/ws',
    )
  })

  test('loads a config, fills defaults, and writes it back', async () => {
    const path = await tempConfig({
      serverUrl: 'http://127.0.0.1:9999',
      enrollKey: 'k'.repeat(32),
      deviceId: 'dev_fixed',
      deviceSecret: 's'.repeat(43),
      name: 'PC',
    })
    process.env.SPAWNER_CONFIG = path

    const config = await loadConfig()
    expect(config.serverUrl).toBe('http://127.0.0.1:9999')
    expect(config.deviceId).toBe('dev_fixed')
    expect(config.enrolled).toBe(false)
    expect(config.allowedRoots).toEqual([])

    const saved = JSON.parse(await readFile(path, 'utf8')) as Record<
      string,
      unknown
    >
    expect(saved.enrollKey).toBe('k'.repeat(32))
    expect(typeof saved.cliPath).toBe('string')
    expect(configPath()).toBe(path)
  })

  test('refuses to start without an enrollment key', async () => {
    delete process.env.SPAWNER_ENROLL_KEY
    delete process.env.RCS_API_KEY
    const path = await tempConfig({
      serverUrl: 'http://127.0.0.1:9999',
      deviceId: 'dev_fixed',
    })
    process.env.SPAWNER_CONFIG = path
    await expect(loadConfig()).rejects.toThrow(/enrollment key/i)
  })
})
