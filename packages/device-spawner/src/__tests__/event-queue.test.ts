import { expect, test } from 'bun:test'
import { enqueueEvent, flushEventQueue } from '../event-queue'

test('retains unsent events in order when the connection drops', () => {
  const queue = ['a', 'b', 'c']
  const delivered: string[] = []
  flushEventQueue(queue, message => {
    if (message === 'b') return false
    delivered.push(message)
    return true
  })
  expect(delivered).toEqual(['a'])
  expect(queue).toEqual(['b', 'c'])
  flushEventQueue(queue, message => {
    delivered.push(message)
    return true
  })
  expect(delivered).toEqual(['a', 'b', 'c'])
  expect(queue).toEqual([])
})

test('keeps the final operation failure even when disconnected progress fills the buffer', () => {
  const queue: string[] = []
  for (let i = 0; i < 100; i++) {
    enqueueEvent(queue, {
      type: 'operation',
      operationId: `op-${i}`,
      localSessionId: 'session',
      state: 'starting',
    })
  }
  enqueueEvent(queue, {
    type: 'operation',
    operationId: 'op-final',
    localSessionId: 'session',
    state: 'failed',
    failureCode: 'spawn_failed',
    detail: '启动失败',
  })
  expect(
    queue.some(item => JSON.parse(item).event.operationId === 'op-final'),
  ).toBe(true)
  enqueueEvent(queue, {
    type: 'operation',
    operationId: 'op-0',
    localSessionId: 'session',
    state: 'failed',
    failureCode: 'spawn_failed',
  })
  const latest = queue.filter(
    item => JSON.parse(item).event.operationId === 'op-0',
  )
  expect(latest).toHaveLength(1)
  expect(JSON.parse(latest[0]!).event.state).toBe('failed')
})

test('keeps only the newest runtime snapshot instead of crowding out operation results', () => {
  const queue: string[] = []
  enqueueEvent(queue, { type: 'session_state', states: [] })
  enqueueEvent(queue, {
    type: 'session_state',
    states: [
      {
        localSessionId: 'session',
        cwd: '',
        processAlive: false,
        verifiedAt: 1,
        source: 'registry',
      },
    ],
  })
  expect(queue).toHaveLength(1)
  expect(JSON.parse(queue[0]!).event.states).toHaveLength(1)
})
