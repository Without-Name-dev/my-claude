import { describe, expect, test } from 'bun:test'
import { join } from 'node:path'
import { resolveManualBridge } from '../manual-bridge'

const cliPath = join(process.cwd(), 'dist', 'cli.js')
const configDir = join(process.cwd(), 'test-profile')
const spawnerConfig = {
  cliPath,
  claudeConfigDir: configDir,
  serverUrl: 'http://127.0.0.1:3317',
  enrollKey: 'fixture-key',
}
const input = {
  args: [] as string[],
  isTerminal: true,
  cliPath,
  configDir,
  spawnerConfig,
}

describe('manual CLI Bridge setup', () => {
  test('enables Bridge for an interactive matching CLI and profile', () => {
    expect(resolveManualBridge(input)).toEqual({
      args: ['--remote-control'],
      baseUrl: spawnerConfig.serverUrl,
      token: spawnerConfig.enrollKey,
    })
    expect(
      resolveManualBridge({ ...input, args: ['--resume', 'session-id'] }).args,
    ).toEqual(['--resume', 'session-id', '--remote-control'])
  })
  test('does not affect print mode, commands, or other installations and profiles', () => {
    expect(
      resolveManualBridge({ ...input, args: ['-p', 'hello'] }).args,
    ).toEqual(['-p', 'hello'])
    expect(resolveManualBridge({ ...input, args: ['--version'] }).args).toEqual(
      ['--version'],
    )
    expect(
      resolveManualBridge({ ...input, args: ['mcp', 'list'] }).args,
    ).toEqual(['mcp', 'list'])
    expect(resolveManualBridge({ ...input, isTerminal: false }).args).toEqual(
      [],
    )
    expect(
      resolveManualBridge({
        ...input,
        cliPath: join(process.cwd(), 'other', 'cli.js'),
      }).args,
    ).toEqual([])
    expect(
      resolveManualBridge({
        ...input,
        configDir: join(process.cwd(), 'other-profile'),
      }).args,
    ).toEqual([])
  })
  test('preserves explicit Bridge settings and does not combine credentials from different sources', () => {
    const custom = resolveManualBridge({
      ...input,
      existingBaseUrl: 'http://example.test',
      existingToken: 'custom',
      args: ['--remote-control'],
    })
    expect(custom).toEqual({
      args: ['--remote-control'],
      baseUrl: 'http://example.test',
      token: 'custom',
    })
    expect(
      resolveManualBridge({ ...input, existingBaseUrl: 'http://example.test' }),
    ).toEqual({ args: [] })
  })
  test('honors explicit opt-out without suppressing an explicitly requested Bridge', () => {
    expect(
      resolveManualBridge({ ...input, autoConnectEnabled: false }).args,
    ).toEqual([])
    expect(
      resolveManualBridge({
        ...input,
        autoConnectEnabled: false,
        args: ['--remote-control'],
      }).token,
    ).toBe('fixture-key')
    expect(resolveManualBridge({ ...input, disabled: true })).toEqual({
      args: [],
    })
  })
})
