import { afterAll, beforeEach, describe, expect, test } from 'bun:test'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  ensureSessionSettings,
  updateSessionSettings,
  validateSettings,
} from '../device/session-settings'
import { flushOutbox, queueMessage } from '../device/operations'
import {
  findOutboxEntry,
  getSessionSettings,
  listOutbox,
  loadState,
  resetStateForTest,
  setMapping,
} from '../device/state'
import {
  handleWebSocketClose,
  handleWebSocketOpen,
  ingestBridgeMessage,
} from '../transport/ws-handler'
import { removeEventBus } from '../transport/event-bus'
import { resetConversationStoreForTest } from '../device/conversation-store'

let directory = ''
const directories: string[] = []
const device = 'dev_settings'
const local = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee'
let sequence = 0
beforeEach(async () => {
  resetConversationStoreForTest()
  directory = await mkdtemp(join(tmpdir(), 'settings-test-'))
  directories.push(directory)
  process.env.RCS_DATA_DIR = directory
  resetStateForTest()
  await loadState()
})
afterAll(async () => {
  resetStateForTest()
  resetConversationStoreForTest()
  for (const dir of directories) await rm(dir, { recursive: true, force: true })
})
const tick = () => new Promise(resolve => setTimeout(resolve, 20))

describe('device session settings', () => {
  test('a resumed session waits for its own settings before a queued message is sent', async () => {
    const other = 'bbbbbbbb-cccc-4ddd-8eee-ffffffffffff'
    const bridge = `session_settings_race${++sequence}`
    await updateSessionSettings(device, local, { model: 'model-a' })
    await updateSessionSettings(device, other, { model: 'model-b' })
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const sent: Record<string, unknown>[] = []
    const ws = {
      readyState: 1,
      send: (frame: string) => sent.push(JSON.parse(frame)),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      const oldSettings = ensureSessionSettings(device, local, bridge).catch(
        () => {},
      )
      for (let i = 0; i < 100 && sent.length === 0; i++) await tick()
      expect((sent[0]?.request as Record<string, unknown>)?.model).toBe(
        'model-a',
      )
      ingestBridgeMessage(bridge, {
        type: 'local_session_changed',
        local_session_id: other,
      })
      const queued = queueMessage(device, other, 'new-session-message')
      const flush = flushOutbox(device, other, bridge)
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: { subtype: 'success', request_id: sent[0]!.request_id },
      })
      await oldSettings
      for (let i = 0; i < 100 && sent.length < 2; i++) await tick()
      expect((sent[1]?.request as Record<string, unknown>)?.model).toBe(
        'model-b',
      )
      expect(sent.some(row => row.type === 'user')).toBe(false)
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: { subtype: 'success', request_id: sent[1]!.request_id },
      })
      await flush
      for (let i = 0; i < 100 && !sent.some(row => row.type === 'user'); i++)
        await tick()
      expect(sent.filter(row => row.type === 'user')).toHaveLength(1)
      expect(findOutboxEntry(device, other, queued.id)?.state).toBe(
        'unconfirmed',
      )
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })
  test('the same bridge restores a different resumed session even when revisions match', async () => {
    const other = 'bbbbbbbb-cccc-4ddd-8eee-ffffffffffff'
    const bridge = `session_settings_resume${++sequence}`
    await updateSessionSettings(device, local, {
      model: 'model-a',
      effort: 'low',
      ultracode: false,
    })
    await updateSessionSettings(device, other, {
      model: 'model-b',
      effort: 'max',
      ultracode: true,
    })
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const requests: Array<Record<string, unknown>> = []
    const ws = {
      readyState: 1,
      send: (frame: string) => {
        const row = JSON.parse(frame) as Record<string, unknown>
        if (row.type !== 'control_request') return
        requests.push(row)
        queueMicrotask(() =>
          ingestBridgeMessage(bridge, {
            type: 'control_response',
            response: { subtype: 'success', request_id: row.request_id },
          }),
        )
      },
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      await ensureSessionSettings(device, local, bridge)
      const previousCount = requests.length
      // New-session init can race ahead of the device's mapping update.
      ingestBridgeMessage(bridge, {
        type: 'system',
        subtype: 'init',
        session_id: other,
        model: 'stale',
        effort: 'high',
        ultracode_active: false,
      })
      expect(getSessionSettings(device, local)?.values.model).toBe('model-a')
      ingestBridgeMessage(bridge, {
        type: 'local_session_changed',
        local_session_id: other,
      })
      await ensureSessionSettings(device, other, bridge)
      const restored = requests
        .slice(previousCount)
        .map(row => row.request as Record<string, unknown>)
      expect(restored).toEqual([
        { subtype: 'set_model', model: 'model-b' },
        { subtype: 'set_effort', effort: 'max' },
        { subtype: 'set_ultracode', enabled: true },
      ])
      expect(getSessionSettings(device, other)?.values).toEqual({
        model: 'model-b',
        effort: 'max',
        ultracode: true,
      })
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })
  test('offline edits survive restart and are isolated by device and session', async () => {
    await updateSessionSettings(device, local, {
      model: 'model-one',
      effort: 'xhigh',
      ultracode: true,
    })
    resetStateForTest()
    await loadState()
    expect(getSessionSettings(device, local)?.values).toEqual({
      model: 'model-one',
      effort: 'xhigh',
      ultracode: true,
    })
    expect(getSessionSettings(device, local)?.pending).toEqual([
      'model',
      'effort',
      'ultracode',
    ])
    expect(getSessionSettings('dev_other', local)).toBeUndefined()
  })

  test('only validated settings can be accepted', () => {
    expect(() => validateSettings({ effort: 'bad' })).toThrow()
    expect(() => validateSettings({ apiKey: 'secret' })).toThrow()
    expect(
      validateSettings({ permissionMode: 'auto', ultracode: false }),
    ).toEqual({ permissionMode: 'auto', ultracode: false })
  })

  test('a terminal change is persisted for the next offline visit', async () => {
    const bridge = `session_settings${++sequence}`
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const ws = {
      readyState: 1,
      send: () => {},
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      await tick()
      ingestBridgeMessage(bridge, {
        type: 'system',
        subtype: 'init',
        model: 'runtime-model',
        model_selection: 'rcs-provider:test:model',
        permissionMode: 'plan',
        effort: 'max',
        ultracode_active: true,
      })
      await tick()
      expect(getSessionSettings(device, local)?.values).toEqual({
        model: 'rcs-provider:test:model',
        permissionMode: 'plan',
        effort: 'max',
        ultracode: true,
      })
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })

  test('a terminal model change is not overwritten by a simultaneous web effort change', async () => {
    const bridge = `session_settings${++sequence}`
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const sent: Record<string, unknown>[] = []
    const ws = {
      readyState: 1,
      send: (frame: string) => sent.push(JSON.parse(frame)),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      await tick()
      ingestBridgeMessage(bridge, {
        type: 'system',
        subtype: 'init',
        model: 'model-a',
        effort: 'low',
      })
      const update = updateSessionSettings(device, local, { effort: 'high' })
      for (let i = 0; i < 100 && sent.length < 1; i++) await tick()
      ingestBridgeMessage(bridge, {
        type: 'system',
        subtype: 'init',
        model: 'model-b',
        effort: 'low',
      })
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: { subtype: 'success', request_id: sent[0]!.request_id },
      })
      for (let i = 0; i < 100 && sent.length < 2; i++) await tick()
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: { subtype: 'success', request_id: sent[1]!.request_id },
      })
      await update
      expect(
        sent.map(row => (row.request as Record<string, unknown>).subtype),
      ).toEqual(['set_effort', 'set_effort'])
      expect(getSessionSettings(device, local)?.values).toEqual({
        model: 'model-b',
        effort: 'high',
      })
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })

  test('terminal confirms every setting before a queued message is delivered', async () => {
    const bridge = `session_settings${++sequence}`
    await updateSessionSettings(device, local, {
      model: 'model-one',
      permissionMode: 'auto',
      effort: 'high',
      ultracode: true,
    })
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const queued = queueMessage(device, local, 'hello')
    const sent: Record<string, unknown>[] = []
    const ws = {
      readyState: 1,
      send: (frame: string) => sent.push(JSON.parse(frame)),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      const flush = flushOutbox(device, local, bridge)
      for (const subtype of [
        'set_model',
        'set_permission_mode',
        'set_effort',
        'set_ultracode',
      ]) {
        for (
          let i = 0;
          i < 100 &&
          !sent.some(
            row =>
              (row.request as Record<string, unknown> | undefined)?.subtype ===
              subtype,
          );
          i++
        )
          await tick()
        const request = sent.find(
          row =>
            (row.request as Record<string, unknown> | undefined)?.subtype ===
            subtype,
        )
        expect(request).toBeDefined()
        expect(sent.some(row => row.type === 'user')).toBe(false)
        ingestBridgeMessage(bridge, {
          type: 'control_response',
          response: { subtype: 'success', request_id: request!.request_id },
        })
      }
      await flush
      expect(sent.filter(row => row.type === 'user')).toHaveLength(1)
      expect(findOutboxEntry(device, local, queued.id)?.state).toBe(
        'unconfirmed',
      )
      ingestBridgeMessage(bridge, {
        type: 'user',
        uuid: queued.id,
        message: { role: 'user', content: 'hello' },
      })
      expect(findOutboxEntry(device, local, queued.id)?.state).toBe('sent')
      expect(getSessionSettings(device, local)?.pending).toEqual([])
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })

  test('a rejected setting keeps queued messages and exposes the error', async () => {
    const bridge = `session_settings${++sequence}`
    await updateSessionSettings(device, local, { model: 'bad-model' })
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const queued = queueMessage(device, local, 'hello')
    const sent: Record<string, unknown>[] = []
    const ws = {
      readyState: 1,
      send: (frame: string) => sent.push(JSON.parse(frame)),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      const flush = flushOutbox(device, local, bridge)
      for (let i = 0; i < 100 && !sent.length; i++) await tick()
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: {
          subtype: 'error',
          request_id: sent[0]!.request_id,
          error: '模型不可用',
        },
      })
      await flush
      expect(sent.some(row => row.type === 'user')).toBe(false)
      expect(
        listOutbox(device, local).find(row => row.id === queued.id)?.state,
      ).toBe('queued')
      expect(getSessionSettings(device, local)?.error).toBe('模型不可用')
      await expect(
        ensureSessionSettings(device, local, 'not-connected'),
      ).rejects.toThrow()
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })

  test('correcting a rejected model in the terminal restores settings and releases queued messages', async () => {
    const bridge = `session_settings${++sequence}`
    await updateSessionSettings(device, local, { model: 'missing-model' })
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const queued = queueMessage(device, local, 'hello')
    const sent: Record<string, unknown>[] = []
    const ws = {
      readyState: 1,
      send: (frame: string) => sent.push(JSON.parse(frame)),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      const flush = flushOutbox(device, local, bridge)
      for (let i = 0; i < 100 && !sent.length; i++) await tick()
      ingestBridgeMessage(bridge, {
        type: 'system',
        subtype: 'init',
        model: 'original-model',
      })
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: {
          subtype: 'error',
          request_id: sent[0]!.request_id,
          error: '模型不可用',
        },
      })
      await flush
      await tick()
      ingestBridgeMessage(bridge, {
        type: 'system',
        subtype: 'init',
        model: 'fixed-model',
      })
      for (let i = 0; i < 100 && sent.length < 2; i++) await tick()
      expect((sent[1]!.request as Record<string, unknown>).model).toBe(
        'fixed-model',
      )
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: { subtype: 'success', request_id: sent[1]!.request_id },
      })
      for (let i = 0; i < 100 && !sent.some(row => row.type === 'user'); i++)
        await tick()
      expect(findOutboxEntry(device, local, queued.id)?.state).toBe(
        'unconfirmed',
      )
      ingestBridgeMessage(bridge, {
        type: 'user',
        uuid: queued.id,
        message: { role: 'user', content: 'hello' },
      })
      expect(findOutboxEntry(device, local, queued.id)?.state).toBe('sent')
      expect(getSessionSettings(device, local)?.values.model).toBe(
        'fixed-model',
      )
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })

  test('does not send a queued message to a bridge replaced during settings confirmation', async () => {
    const bridge = `session_settings${++sequence}`
    await updateSessionSettings(device, local, { model: 'model-one' })
    setMapping({
      deviceId: device,
      localSessionId: local,
      bridgeSessionId: bridge,
      cwd: 'C:\\work',
      updatedAt: Date.now(),
    })
    const queued = queueMessage(device, local, 'hello')
    const sent: Record<string, unknown>[] = []
    const ws = {
      readyState: 1,
      send: (frame: string) => sent.push(JSON.parse(frame)),
      close: () => {},
    } as unknown as Parameters<typeof handleWebSocketOpen>[0]
    const token = handleWebSocketOpen(ws, bridge)
    try {
      const flush = flushOutbox(device, local, bridge)
      for (let i = 0; i < 100 && !sent.length; i++) await tick()
      setMapping({
        deviceId: device,
        localSessionId: local,
        bridgeSessionId: 'session_replacement',
        cwd: 'C:\\work',
        updatedAt: Date.now(),
      })
      ingestBridgeMessage(bridge, {
        type: 'control_response',
        response: { subtype: 'success', request_id: sent[0]!.request_id },
      })
      await flush
      expect(sent.some(row => row.type === 'user')).toBe(false)
      expect(findOutboxEntry(device, local, queued.id)?.state).toBe('queued')
    } finally {
      handleWebSocketClose(ws, bridge, 1000, '', token)
      removeEventBus(bridge)
    }
  })
})
