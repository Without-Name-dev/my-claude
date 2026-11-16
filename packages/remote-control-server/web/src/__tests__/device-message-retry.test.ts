import { expect, spyOn, test } from 'bun:test'
import { DeviceApiError, sendDeviceMessage } from '../api/devices'

test('a lost response retries the same id and payload', async () => {
  const old = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'identity', setItem() {} },
  })
  const bodies: string[] = []
  const fetchSpy = spyOn(globalThis, 'fetch').mockImplementation((async (
    _input,
    init,
  ) => {
    bodies.push(String(init?.body))
    if (bodies.length === 1) throw new TypeError('connection lost')
    return Response.json({ state: 'queued', messageId: 'same-id' })
  }) as typeof fetch)
  try {
    await sendDeviceMessage(
      'device',
      'session',
      'hello',
      undefined,
      '20d922c9-9822-447e-800c-2a4bfd762e55',
    )
    expect(bodies).toHaveLength(2)
    expect(bodies[0]).toBe(bodies[1])
  } finally {
    fetchSpy.mockRestore()
    if (old) Object.defineProperty(globalThis, 'localStorage', old)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})

test('two lost responses report uncertain delivery instead of claiming failure', async () => {
  const old = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'identity', setItem() {} },
  })
  const fetchSpy = spyOn(globalThis, 'fetch').mockRejectedValue(
    new TypeError('offline'),
  )
  try {
    let caught: unknown
    try {
      await sendDeviceMessage('device', 'session', 'hello')
    } catch (error) {
      caught = error
    }
    expect(caught).toBeInstanceOf(DeviceApiError)
    expect((caught as DeviceApiError).code).toBe('delivery_unknown')
  } finally {
    fetchSpy.mockRestore()
    if (old) Object.defineProperty(globalThis, 'localStorage', old)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})
