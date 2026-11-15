import { expect, test } from 'bun:test'
import { createSerialInboundQueue } from '../serialInboundQueue.js'

test('slow attachment resolution cannot reorder later prompts', async () => {
  let release!: () => void
  const attachment = new Promise<void>(resolve => {
    release = resolve
  })
  const delivered: string[] = []
  const enqueue = createSerialInboundQueue(async (message: string) => {
    if (message === 'with attachment') await attachment
    delivered.push(message)
  })
  const first = enqueue('with attachment')
  const second = enqueue('plain text')
  await Promise.resolve()
  expect(delivered).toEqual([])
  release()
  await Promise.all([first, second])
  expect(delivered).toEqual(['with attachment', 'plain text'])
})

test('a rejected preparation does not block later messages', async () => {
  const delivered: string[] = []
  const enqueue = createSerialInboundQueue(async (message: string) => {
    if (message === 'broken') throw new Error('failed')
    delivered.push(message)
  })
  const first = enqueue('broken')
  const second = enqueue('next')
  await expect(first).rejects.toThrow('failed')
  await second
  expect(delivered).toEqual(['next'])
})
