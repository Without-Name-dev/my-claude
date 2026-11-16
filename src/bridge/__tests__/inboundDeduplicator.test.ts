import { describe, expect, test } from 'bun:test'
import { createInboundDeduplicator } from '../inboundDeduplicator'

describe('inbound delivery across bridge reconnects', () => {
  test('retries after an in-flight delivery is cancelled', async () => {
    const run = createInboundDeduplicator()
    let release!: (value: boolean) => void
    const first = run(
      'message',
      () =>
        new Promise(resolve => {
          release = resolve
        }),
    )
    let delivered = 0
    const replay = run('message', async () => {
      delivered++
      return true
    })
    release(false)
    await Promise.all([first, replay])
    await run('message', async () => {
      delivered++
      return true
    })
    expect(delivered).toBe(1)
  })

  test('coalesces concurrent duplicate deliveries', async () => {
    const run = createInboundDeduplicator()
    let release!: (value: boolean) => void
    const first = run(
      'message',
      () =>
        new Promise(resolve => {
          release = resolve
        }),
    )
    let delivered = 0
    const replay = run('message', async () => {
      delivered++
      return true
    })
    release(true)
    await Promise.all([first, replay])
    expect(delivered).toBe(0)
  })
})
