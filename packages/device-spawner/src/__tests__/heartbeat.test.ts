import { expect, test } from 'bun:test'
import { createHeartbeatTick, heartbeatInterval } from '../heartbeat'

test('keeps sending liveness while discovery is slow and ignores stale snapshots', async () => {
  let connection = 'first'
  let resolve!: (value: string[]) => void
  const messages: Array<string[] | undefined> = []
  let scans = 0
  const tick = createHeartbeatTick({
    currentConnection: () => connection,
    isCurrentConnection: value => value === connection,
    send: running => {
      messages.push(running)
      return true
    },
    collect: () => {
      scans++
      return new Promise<string[]>(done => {
        resolve = done
      })
    },
    onError: () => {
      throw new Error('unexpected discovery error')
    },
  })
  tick()
  tick()
  expect(messages).toEqual([undefined, undefined])
  expect(scans).toBe(1)
  connection = 'second'
  resolve(['stale'])
  await new Promise(done => setTimeout(done, 0))
  expect(messages).toEqual([undefined, undefined])
})

test('heartbeat interval remains within the server timeout', () => {
  expect(heartbeatInterval(1_000)).toBe(5_000)
  expect(heartbeatInterval(15_000)).toBe(15_000)
  expect(heartbeatInterval(120_000)).toBe(25_000)
})
