import { beforeEach, describe, expect, spyOn, test } from 'bun:test'
import { RCSChatAdapter, type BridgeSessionInfo } from '../lib/rcs-chat-adapter'
import type { SessionEvent } from '../types'

function initEvent(raw: Record<string, unknown>): SessionEvent {
  return {
    id: 'e1',
    type: 'system',
    payload: { content: '', raw },
    direction: 'inbound',
    seqNum: 1,
  }
}

beforeEach(() => {
  // api() needs an identity; a minimal localStorage is enough.
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'test-identity', setItem: () => {} },
  })
})

describe('bridge session controls', () => {
  test('parses the wrapped system/init message', () => {
    let info: BridgeSessionInfo | undefined
    const adapter = new RCSChatAdapter('session_x', () => {}, {
      onSessionInfo: next => {
        info = next
      },
    })

    adapter.handleEvent(
      initEvent({
        type: 'system',
        subtype: 'init',
        model: 'deepseek/deepseek-flash',
        permissionMode: 'plan',
        slash_commands: ['clear', 'compact'],
        models: [{ id: 'default', name: 'Default' }],
        command_details: [
          { name: 'clear', description: 'Clear context' },
          { name: 'compact', description: 'Compact context' },
        ],
      }),
    )

    expect(info?.model).toBe('deepseek/deepseek-flash')
    expect(info?.permissionMode).toBe('plan')
    expect(info?.models).toEqual([{ id: 'default', name: 'Default' }])
    expect(info?.commands).toHaveLength(2)
    expect(info?.commands[0]).toEqual({
      name: 'clear',
      description: 'Clear context',
    })
  })

  test('surfaces bridge_state without polling', () => {
    let connected: boolean | undefined
    const adapter = new RCSChatAdapter('session_x', () => {}, {
      onBridgeState: next => {
        connected = next
      },
    })
    adapter.handleEvent({
      id: 'e2',
      type: 'bridge_state',
      payload: { connected: false },
      direction: 'inbound',
      seqNum: 2,
    })
    expect(connected).toBe(false)
  })

  test('sends set_model and set_permission_mode as control requests', async () => {
    const bodies: Array<Record<string, unknown>> = []
    const spy = spyOn(globalThis, 'fetch').mockImplementation((async (
      _input,
      init,
    ) => {
      bodies.push(JSON.parse(String(init?.body)))
      return Response.json({ ok: true })
    }) as typeof fetch)

    try {
      const adapter = new RCSChatAdapter('session_x', () => {})
      await adapter.setModel('opus')
      await adapter.setPermissionMode('acceptEdits')
    } finally {
      spy.mockRestore()
    }

    expect(bodies).toHaveLength(2)
    const first = bodies[0] as {
      type: string
      request: { subtype: string; model?: string }
    }
    const second = bodies[1] as {
      type: string
      request: { subtype: string; mode?: string }
    }
    expect(first.type).toBe('control_request')
    expect(first.request.subtype).toBe('set_model')
    expect(first.request.model).toBe('opus')
    expect(second.request.subtype).toBe('set_permission_mode')
    expect(second.request.mode).toBe('acceptEdits')
  })
})
