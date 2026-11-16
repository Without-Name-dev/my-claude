import { expect, test } from 'bun:test'
import {
  withDirectBridgeHost,
  withInteractiveTerminalColors,
} from '../bridge-env'

test('preserves existing proxy exclusions and bypasses only the configured bridge host', () => {
  const original = {
    NO_PROXY: 'localhost,127.0.0.1',
    no_proxy: 'other.example',
    HTTP_PROXY: 'http://proxy:8080',
  }
  const result = withDirectBridgeHost(original, 'http://rcs.example:3000')
  expect(result.NO_PROXY).toBe('localhost,127.0.0.1,other.example,rcs.example')
  expect(result.no_proxy).toBe(result.NO_PROXY)
  expect(result.HTTP_PROXY).toBe(original.HTTP_PROXY)
  expect(original.NO_PROXY).toBe('localhost,127.0.0.1')
})

test('visible terminals do not inherit a headless launcher color mode', () => {
  const parent = { NO_COLOR: '1', TERM: 'dumb', CLAUDE_CONFIG_DIR: 'profile' }
  const result = withInteractiveTerminalColors(parent)
  expect(result.NO_COLOR).toBeUndefined()
  expect(result.TERM).toBe('xterm-256color')
  expect(result.FORCE_COLOR).toBe('3')
  expect(result.CLAUDE_CONFIG_DIR).toBe('profile')
  expect(parent.NO_COLOR).toBe('1')
})

test('preserves deliberate terminal settings and explicit FORCE_COLOR', () => {
  expect(
    withInteractiveTerminalColors({ TERM: 'xterm', NO_COLOR: '1' }),
  ).toEqual({ TERM: 'xterm', NO_COLOR: '1' })
  expect(
    withInteractiveTerminalColors({
      TERM: 'dumb',
      NO_COLOR: '1',
      FORCE_COLOR: '0',
    }).FORCE_COLOR,
  ).toBe('0')
})

test('the interactive child produces actual color escapes with a headless parent', () => {
  const env = withInteractiveTerminalColors({
    ...process.env,
    TERM: 'dumb',
    NO_COLOR: '1',
    FORCE_COLOR: undefined,
  })
  const child = Bun.spawnSync({
    cmd: [
      process.execPath,
      '-e',
      "import chalk from 'chalk'; process.stdout.write(JSON.stringify({level: chalk.level, text: chalk.hex('#D77757')('x')}))",
    ],
    env,
    stdout: 'pipe',
    stderr: 'pipe',
  })
  expect(child.exitCode).toBe(0)
  const result = JSON.parse(child.stdout.toString()) as {
    level: number
    text: string
  }
  expect(result.level).toBe(3)
  expect(result.text).toContain('\u001b[')
})
