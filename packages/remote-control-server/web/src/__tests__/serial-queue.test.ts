import { describe, expect, test } from 'bun:test'
import { createSerialQueue } from '../lib/serial-queue'

describe('serial message queue', () => {
  test('preserves all queued messages in order after a send fails', async () => {
    const enqueue = createSerialQueue()
    const sent: string[] = []
    let release: () => void = () => {}
    const wait = new Promise<void>(resolve => {
      release = resolve
    })
    const first = enqueue(async () => {
      await wait
      sent.push('first')
    })
    const second = enqueue(async () => {
      sent.push('second')
      throw new Error('failed')
    })
    const third = enqueue(async () => {
      sent.push('third')
    })
    release()
    await first
    await expect(second).rejects.toThrow('failed')
    await third
    expect(sent).toEqual(['first', 'second', 'third'])
  })
})
