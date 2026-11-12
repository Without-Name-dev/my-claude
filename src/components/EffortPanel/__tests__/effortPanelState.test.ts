import { describe, expect, test } from 'bun:test'
import {
  CANCEL_MESSAGE,
  type ApplyFn,
  END_POSITION,
  HOME_POSITION,
  PANEL_POSITIONS,
  type PanelPosition,
  computeConfirmOutcome,
  getInitialCursor,
  moveLeft,
  moveRight,
} from '../effortPanelState.js'

describe('effortPanelState', () => {
  test('PANEL_POSITIONS 顺序为 low → max（ultracode 是独立开关，不再是档位）', () => {
    expect(PANEL_POSITIONS).toEqual(['low', 'medium', 'high', 'xhigh', 'max'])
  })

  test('moveLeft 在 low 处保持 low', () => {
    expect(moveLeft('low')).toBe('low')
  })

  test('moveLeft 正常左移', () => {
    expect(moveLeft('high')).toBe('medium')
    expect(moveLeft('max')).toBe('xhigh')
  })

  test('moveRight 在 max 处保持 max', () => {
    expect(moveRight('max')).toBe('max')
  })

  test('moveRight 正常右移', () => {
    expect(moveRight('medium')).toBe('high')
    expect(moveRight('xhigh')).toBe('max')
  })

  test('HOME_POSITION 等于 low', () => {
    expect(HOME_POSITION).toBe('low')
  })

  test('END_POSITION 等于 max', () => {
    expect(END_POSITION).toBe('max')
  })

  test('getInitialCursor：env override 为合法档位时返回 env 值', () => {
    expect(
      getInitialCursor({
        envOverride: 'high',
        appStateEffort: 'medium',
        displayed: 'high',
      }),
    ).toBe('high')
  })

  test('getInitialCursor：env 为 null（unset）时用 displayed', () => {
    expect(
      getInitialCursor({
        envOverride: null,
        appStateEffort: undefined,
        displayed: 'medium',
      }),
    ).toBe('medium')
  })

  test('getInitialCursor：env undefined 时用 displayed', () => {
    expect(
      getInitialCursor({
        envOverride: undefined,
        appStateEffort: 'high',
        displayed: 'high',
      }),
    ).toBe('high')
  })

  test('getInitialCursor：env 是数值（ant-only）时落回 displayed', () => {
    // 数值不是合法 PanelPosition，回退
    expect(
      getInitialCursor({
        envOverride: 75,
        appStateEffort: 'medium',
        displayed: 'medium',
      }),
    ).toBe('medium')
  })

  test('PanelPosition 类型编译期检查（隐式）', () => {
    const p: PanelPosition = 'xhigh'
    expect(p).toBe('xhigh')
  })
})

describe('computeConfirmOutcome', () => {
  const mockApply: ApplyFn = cursor => ({
    message: `applied:${cursor}`,
    effortUpdate: { value: cursor },
  })

  test('low → kind=apply，message 来自 applyFn，effortUpdate 透传', () => {
    const out = computeConfirmOutcome('low', mockApply)
    expect(out.kind).toBe('apply')
    expect(out.message).toBe('applied:low')
    expect(out.effortUpdate?.value).toBe('low')
  })

  test('high → apply 路径正常', () => {
    const out = computeConfirmOutcome('high', mockApply)
    expect(out.kind).toBe('apply')
  })

  test('applyFn 返回无 effortUpdate 时，outcome.effortUpdate 为 undefined', () => {
    const noUpdate: ApplyFn = c => ({ message: `applied:${c}` })
    const out = computeConfirmOutcome('medium', noUpdate)
    expect(out.kind).toBe('apply')
    expect(out.effortUpdate).toBeUndefined()
  })
})

test('常量字符串', () => {
  expect(CANCEL_MESSAGE).toBe('Effort unchanged.')
})
