import { describe, expect, test } from 'bun:test'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { randomUUID } from 'node:crypto'
import { DEFAULT_BUILD_FEATURES } from '../../../../scripts/defines'

describe('PowerShell auto mode', () => {
  test('build enables the classifier path while retaining approval guardrails', () => {
    expect(DEFAULT_BUILD_FEATURES).toContain('POWERSHELL_AUTO_MODE')
    const result = Bun.spawnSync({
      cmd: [
        process.execPath,
        'run',
        '--feature=TRANSCRIPT_CLASSIFIER',
        '--feature=POWERSHELL_AUTO_MODE',
        join(process.cwd(), 'tests/mocks/autoModePowerShellProbe.ts'),
      ],
      cwd: process.cwd(),
      env: { ...process.env, USER_TYPE: 'external' },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    expect(result.exitCode, result.stderr.toString()).toBe(0)
    expect(result.stdout.toString()).toContain(
      'PowerShell auto approval and guardrails verified',
    )
  }, 30000)

  test('classifier prompt keeps external defaults alongside PowerShell guidance', () => {
    const result = Bun.spawnSync({
      cmd: [
        process.execPath,
        'run',
        '--feature=TRANSCRIPT_CLASSIFIER',
        '--feature=POWERSHELL_AUTO_MODE',
        join(process.cwd(), 'tests/mocks/autoModePromptProbe.ts'),
      ],
      cwd: process.cwd(),
      env: {
        ...process.env,
        USER_TYPE: 'external',
        CLAUDE_CONFIG_DIR: join(tmpdir(), `ccb-auto-test-${randomUUID()}`),
      },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    expect(result.exitCode, result.stderr.toString()).toBe(0)
    expect(result.stdout.toString()).toContain(
      'Auto-mode classifier prompt verified',
    )
  }, 30000)
})
