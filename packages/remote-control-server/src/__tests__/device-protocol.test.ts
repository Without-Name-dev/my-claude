import { describe, expect, test } from 'bun:test'
import {
  isBridgeSessionId,
  isDeviceId,
  isLocalSessionId,
  parseDeviceMethod,
  stringField,
  numberField,
} from '../device/protocol'

describe('device protocol validation', () => {
  test('accepts CLI local session UUIDs only', () => {
    expect(isLocalSessionId('db094c06-6f2d-470d-aef1-dcce041193b7')).toBe(true)
    expect(isLocalSessionId('DB094C06-6F2D-470D-AEF1-DCCE041193B7')).toBe(true)
    expect(isLocalSessionId('session_badd8915d8374f999dd69d3fab4f7fce')).toBe(
      false,
    )
    expect(isLocalSessionId('../../etc/passwd')).toBe(false)
    expect(isLocalSessionId('')).toBe(false)
  })

  test('accepts the bridge compat id format', () => {
    expect(isBridgeSessionId('session_badd8915d8374f999dd69d3fab4f7fce')).toBe(
      true,
    )
    expect(isBridgeSessionId('sess_123')).toBe(false)
    expect(isBridgeSessionId('session_')).toBe(false)
  })

  test('device ids are path-free identifiers', () => {
    expect(isDeviceId('dev_abc123')).toBe(true)
    expect(isDeviceId('a b')).toBe(false)
    expect(isDeviceId('dev/../x')).toBe(false)
    expect(isDeviceId('')).toBe(false)
  })

  test('only known methods parse', () => {
    expect(parseDeviceMethod('open_session')).toBe('open_session')
    expect(parseDeviceMethod('list_projects')).toBe('list_projects')
    expect(parseDeviceMethod('exec')).toBeUndefined()
    expect(parseDeviceMethod(42)).toBeUndefined()
  })

  test('field helpers enforce bounds', () => {
    const record = { a: 'x', b: 2, c: new Array(20).join('y') }
    expect(stringField(record, 'a')).toBe('x')
    expect(stringField(record, 'missing')).toBeUndefined()
    expect(stringField(record, 'c', 5)).toBeUndefined()
    expect(numberField(record, 'b')).toBe(2)
    expect(numberField(record, 'a')).toBeUndefined()
  })
})
