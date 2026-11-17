import { expect, test } from 'bun:test'
import {
  drawerGestureDirection,
  shouldCommitDrawerGesture,
} from '../lib/mobile-drawer-gesture'

test('drawer gesture leaves vertical scrolling and opposite movement alone', () => {
  expect(drawerGestureDirection(3, 4, true)).toBe('wait')
  expect(drawerGestureDirection(12, 30, true)).toBe('ignore')
  expect(drawerGestureDirection(-30, 2, true)).toBe('ignore')
  expect(drawerGestureDirection(30, 2, true)).toBe('drag')
  expect(drawerGestureDirection(-30, 2, false)).toBe('drag')
})

test('a deliberate drag or flick commits; a short slow drag snaps back', () => {
  expect(shouldCommitDrawerGesture(100, 320, 500)).toBe(true)
  expect(shouldCommitDrawerGesture(35, 320, 50)).toBe(true)
  expect(shouldCommitDrawerGesture(35, 320, 500)).toBe(false)
  expect(shouldCommitDrawerGesture(0, 0, 0)).toBe(false)
})
