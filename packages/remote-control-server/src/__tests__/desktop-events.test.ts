import { describe, expect, test } from 'bun:test'

import { parseEvent } from '../routes/desktop'

const operationId = 'a67d8e34-1f2b-4c5d-9e0f-112233445566'

describe('device operation event parsing', () => {
  test('accepts a real local session UUID', () => {
    const event = parseEvent({
      type: 'operation',
      operationId,
      localSessionId: '11111111-2222-3333-4444-555555555555',
      state: 'ready',
    })
    expect(event).toMatchObject({ type: 'operation', state: 'ready' })
  })

  test('accepts the pending-<operationId> placeholder', () => {
    // open_new_session emits progress/failures before the CLI registers its
    // session id. Rejecting these hid early failures behind the 120s watchdog.
    const event = parseEvent({
      type: 'operation',
      operationId,
      localSessionId: `pending-${operationId}`,
      state: 'failed',
      failureCode: 'config_dir_mismatch',
      detail: 'mismatch',
    })
    expect(event).toMatchObject({
      type: 'operation',
      state: 'failed',
      failureCode: 'config_dir_mismatch',
    })
  })

  test('rejects a placeholder for a different operation id', () => {
    expect(
      parseEvent({
        type: 'operation',
        operationId,
        localSessionId: 'pending-00000000-1111-2222-3333-444444444444',
        state: 'failed',
      }),
    ).toBeUndefined()
  })

  test('still rejects arbitrary non-UUID localSessionIds', () => {
    expect(
      parseEvent({
        type: 'operation',
        operationId,
        localSessionId: '../../etc/passwd',
        state: 'ready',
      }),
    ).toBeUndefined()
  })
})
