import { describe, expect, test } from 'bun:test'
import { reconcileVirtualMessageKeys } from '../virtualMessageKeys.js'

type Row = { id: string; text: string }
const key = (row: Row) => row.id

describe('virtual message row identity', () => {
  test('keeps each row associated with its own key after a middle insertion', () => {
    const rows: Row[] = [
      { id: 'start', text: 'start' },
      { id: 'answer', text: 'answer' },
      { id: 'tool', text: 'tool' },
    ]
    const original = reconcileVirtualMessageKeys([], rows, key)
    const next = [
      rows[0]!,
      { id: 'inserted', text: 'status' },
      ...rows.slice(1),
    ]
    expect(reconcileVirtualMessageKeys(original, next, key)).toEqual([
      'start',
      'inserted',
      'answer',
      'tool',
    ])
  })

  test('handles reorder, replacement, removal, and identical text with distinct ids', () => {
    const first = { id: 'a', text: 'same' }
    const second = { id: 'b', text: 'same' }
    let keys = reconcileVirtualMessageKeys([], [first, second], key)
    keys = reconcileVirtualMessageKeys(keys, [second, first], key)
    expect(keys).toEqual(['b', 'a'])
    keys = reconcileVirtualMessageKeys(
      keys,
      [second, { id: 'c', text: 'same' }],
      key,
    )
    expect(keys).toEqual(['b', 'c'])
    expect(reconcileVirtualMessageKeys(keys, [second], key)).toEqual(['b'])
  })

  test('preserves the append path and rebuilds when the key function changes', () => {
    const rows = [
      { id: 'a', text: 'first' },
      { id: 'b', text: 'second' },
    ]
    const keys = reconcileVirtualMessageKeys([], rows.slice(0, 1), key)
    expect(reconcileVirtualMessageKeys(keys, rows, key)).toBe(keys)
    expect(keys).toEqual(['a', 'b'])
    expect(
      reconcileVirtualMessageKeys(keys, rows, row => `new-${row.id}`, true),
    ).toEqual(['new-a', 'new-b'])
  })
})
