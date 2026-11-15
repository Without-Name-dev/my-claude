#!/usr/bin/env bun
// Packaged CCB launcher. Installed next to dist/cli.js and bun/bun.exe.
// This is deliberately separate from the user's independently installed Claude.
import { copyFileSync, existsSync, mkdirSync, readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { resolveManualBridge } from './cc-manual-bridge.ts'

const CLI = join(import.meta.dir, 'dist', 'cli.js')
if (!existsSync(CLI)) {
  console.error(
    `cc: CLI not found at ${CLI}\n    re-copy the repo's dist/ into ${import.meta.dir}`,
  )
  process.exit(1)
}

const CCB_CONFIG = 'D:/Claude Code/claude-config'
const configDir = process.env.CLAUDE_CONFIG_DIR || CCB_CONFIG
try {
  mkdirSync(configDir, { recursive: true })
} catch {
  // The CLI reports an unusable config directory itself.
}

const source = join(configDir, '.claude.json')
if (existsSync(source)) {
  try {
    const backups = join(configDir, 'backups')
    mkdirSync(backups, { recursive: true })
    const target = join(
      backups,
      `cc-launch-${new Date().toISOString().slice(0, 10)}.json`,
    )
    if (!existsSync(target)) copyFileSync(source, target)
  } catch {
    // A failed backup must never stop the CLI from starting.
  }
}
process.env.CLAUDE_CONFIG_DIR = configDir

// A manually opened `cc` should use the same self-hosted Bridge as the
// Spawner, but only for the exact CLI and profile enrolled on this machine.
// Non-interactive commands and explicit opt-outs keep their original behavior.
let spawnerConfig
let autoConnectEnabled
try {
  const base = process.env.LOCALAPPDATA || join(homedir(), '.local', 'share')
  const path =
    process.env.SPAWNER_CONFIG || join(base, 'CCRemote', 'spawner.json')
  spawnerConfig = JSON.parse(readFileSync(path, 'utf8'))
} catch {
  // A machine without a configured Spawner remains a normal CLI.
}
try {
  const settings = JSON.parse(readFileSync(source, 'utf8'))
  if (typeof settings.remoteControlAtStartup === 'boolean') {
    autoConnectEnabled = settings.remoteControlAtStartup
  }
} catch {
  // The CLI handles a missing or unreadable config as usual.
}
const bridge = resolveManualBridge({
  args: process.argv.slice(2),
  isTerminal: Boolean(process.stdout.isTTY),
  cliPath: CLI,
  configDir,
  spawnerConfig,
  existingBaseUrl: process.env.CLAUDE_BRIDGE_BASE_URL,
  existingToken: process.env.CLAUDE_BRIDGE_OAUTH_TOKEN,
  disabled: process.env.CCB_DISABLE_AUTO_BRIDGE === '1',
  autoConnectEnabled,
})
if (bridge.baseUrl && bridge.token) {
  process.env.CLAUDE_BRIDGE_BASE_URL = bridge.baseUrl
  process.env.CLAUDE_BRIDGE_OAUTH_TOKEN = bridge.token
}

// Load the CLI in this process: starting another Bun adds noticeable latency.
process.argv = [process.argv[0], CLI, ...bridge.args]
await import(pathToFileURL(CLI).href)
