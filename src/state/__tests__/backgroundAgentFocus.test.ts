import { expect, test } from 'bun:test'
import { InputEvent } from '@anthropic/ink'
import {
  INITIAL_STATE,
  parseMultipleKeypresses,
} from '../../../packages/@ant/ink/src/core/parse-keypress'
import { EventEmitter } from '../../../packages/@ant/ink/src/core/events/emitter'
import { handleBackgroundAgentShortcut } from '../backgroundAgentFocus'
import { exitTeammateView } from '../teammateViewHelpers'
import { createStore } from '../store'
import type { AppState } from '../AppState'

function eventFor(sequence: string): InputEvent {
  const [events] = parseMultipleKeypresses({ ...INITIAL_STATE }, sequence)
  const first = events[0]
  if (!first || first.kind !== 'key') throw new Error('invalid fixture key')
  return new InputEvent(first)
}

test('Shift+Down focuses the viewed completed agent, without firing the legacy dialog', () => {
  const state = {
    tasks: {
      a: {
        id: 'a',
        agentId: 'a',
        type: 'local_agent',
        agentType: 'verification',
        description: '验证',
        status: 'completed',
        startTime: 1,
        isBackgrounded: true,
        retain: true,
      },
    },
    viewingAgentTaskId: 'a',
    viewSelectionMode: 'viewing-agent',
    footerSelection: null,
    selectedBgAgentIndex: -1,
  } as unknown as AppState
  const store = createStore(state)
  const emitter = new EventEmitter()
  let legacyCalled = false
  emitter.on('input', event => handleBackgroundAgentShortcut(event, store))
  emitter.on('input', () => {
    legacyCalled = true
  })
  emitter.emit('input', eventFor('\x1b[1;2B'))
  expect(store.getState().footerSelection).toBe('bg_agent')
  expect(store.getState().selectedBgAgentIndex).toBe(0)
  expect(legacyCalled).toBe(false)
  // The footer Up/Enter path selects index -1, then uses the same exit action.
  exitTeammateView(store.setState)
  expect(store.getState().viewingAgentTaskId).toBeUndefined()
  expect(store.getState().tasks.a).toMatchObject({ retain: false })
})

test('ordinary arrows and Shift+Down without a visible agent reach their normal handlers', () => {
  const state = {
    tasks: {},
    footerSelection: null,
    selectedBgAgentIndex: -1,
  } as unknown as AppState
  const store = createStore(state)
  for (const sequence of ['\x1b[A', '\x1b[B', '\x1b[1;2B']) {
    const event = eventFor(sequence)
    expect(handleBackgroundAgentShortcut(event, store)).toBe(false)
    expect(event.didStopImmediatePropagation()).toBe(false)
  }
  expect(store.getState()).toBe(state)
})

test('focus starts on main and includes foreground agents while ignoring dismissed agents', () => {
  const state = {
    tasks: {
      hidden: {
        id: 'hidden',
        agentId: 'hidden',
        type: 'local_agent',
        agentType: 'x',
        isBackgrounded: true,
        evictAfter: 0,
      },
      foreground: {
        id: 'foreground',
        agentId: 'foreground',
        type: 'local_agent',
        agentType: 'x',
        isBackgrounded: false,
        startTime: 1,
      },
      active: {
        id: 'active',
        agentId: 'active',
        type: 'local_agent',
        agentType: 'x',
        isBackgrounded: true,
        startTime: 3,
      },
    },
    footerSelection: null,
    selectedBgAgentIndex: 0,
  } as unknown as AppState
  const store = createStore(state)
  const ordinaryUp = eventFor('\x1b[A')
  expect(handleBackgroundAgentShortcut(ordinaryUp, store)).toBe(false)
  expect(ordinaryUp.didStopImmediatePropagation()).toBe(false)
  expect(handleBackgroundAgentShortcut(eventFor('\x1b[1;2B'), store)).toBe(true)
  expect(store.getState().selectedBgAgentIndex).toBe(-1)
  store.setState(current => ({
    ...current,
    viewingAgentTaskId: 'foreground',
    footerSelection: null,
  }))
  expect(handleBackgroundAgentShortcut(eventFor('\x1b[1;2B'), store)).toBe(true)
  expect(store.getState().selectedBgAgentIndex).toBe(0)
})
