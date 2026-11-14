import { describe, expect, test } from 'bun:test'

// Absolute module URLs: the fixture runs in a child bun whose cwd depends on
// where the suite was invoked from (repo root vs package dir), and a relative
// spec that resolves from one breaks in the other.
const wsHandlerUrl = new URL('../transport/ws-handler.ts', import.meta.url).href
const eventBusUrl = new URL('../transport/event-bus.ts', import.meta.url).href

const fixture = `
  import { ingestBridgeMessage, handleWebSocketOpen, handleWebSocketClose } from '${wsHandlerUrl}';
  import { getEventBus } from '${eventBusUrl}';
  const ws = { readyState: 1, send() {}, close() {} };
  const token = handleWebSocketOpen(ws, 'session_log_test');
  const bus = getEventBus('session_log_test');
  bus.publish({ id: '1', sessionId: 'session_log_test', type: 'user', direction: 'outbound', payload: { content: 'PRIVATE_OUTBOUND_MARKER' } });
  bus.publish({ id: '2', sessionId: 'session_log_test', type: 'error', direction: 'inbound', payload: { content: 'PRIVATE_ERROR_MARKER' } });
  ingestBridgeMessage('session_log_test', { type: 'assistant', message: { content: 'PRIVATE_INBOUND_MARKER' } });
  handleWebSocketClose(ws, 'session_log_test', 1000, '', token);
`

function runFixture(debug: boolean): string {
  const env: Record<string, string | undefined> = {
    ...process.env,
    NODE_ENV: 'development',
    RCS_DEBUG: debug ? '1' : '0',
  }
  delete env.BUN_TEST
  const result = Bun.spawnSync([process.execPath, '-e', fixture], {
    cwd: process.cwd(),
    env,
    stdout: 'pipe',
    stderr: 'pipe',
  })
  expect(result.exitCode).toBe(0)
  return result.stdout.toString() + result.stderr.toString()
}

describe('transport logging', () => {
  test('does not log event metadata by default', () => {
    expect(runFixture(false)).not.toContain('[RC-DEBUG]')
  })

  test('debug metadata never includes message or error payloads', () => {
    const output = runFixture(true)
    expect(output).toContain('[RC-DEBUG]')
    expect(output).not.toContain('PRIVATE_OUTBOUND_MARKER')
    expect(output).not.toContain('PRIVATE_INBOUND_MARKER')
    expect(output).not.toContain('PRIVATE_ERROR_MARKER')
  })
})
