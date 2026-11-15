import { expect, test } from 'bun:test'
import { BoundedUUIDSet, handleIngressMessage } from '../bridgeMessaging.js'

test('failed inbound preparation permits redelivery with the same uuid', async () => {
  const posted = new BoundedUUIDSet(10)
  const received = new BoundedUUIDSet(10)
  const message = JSON.stringify({
    type: 'user',
    uuid: 'msg-1',
    message: { role: 'user', content: 'hello' },
  })
  let attempts = 0
  const handle = async () => {
    attempts++
    if (attempts === 1) throw new Error('attachment unavailable')
  }
  handleIngressMessage(message, posted, received, handle)
  await Promise.resolve()
  await Promise.resolve()
  expect(received.has('msg-1')).toBe(false)
  handleIngressMessage(message, posted, received, handle)
  expect(attempts).toBe(2)
  expect(received.has('msg-1')).toBe(true)
})

test('removing and re-adding a uuid does not corrupt the bounded ring', () => {
  const ids = new BoundedUUIDSet(2)
  ids.add('a')
  ids.delete('a')
  ids.add('a')
  ids.add('b')
  expect(ids.has('a')).toBe(true)
  ids.add('c')
  expect(ids.has('a')).toBe(false)
  expect(ids.has('b')).toBe(true)
  expect(ids.has('c')).toBe(true)
})
