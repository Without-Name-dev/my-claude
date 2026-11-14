import { expect, test } from 'bun:test'
import { processImageFiles } from '../../components/chat/ChatInput'

test('converts a small HEIC to an API-supported image before sending', async () => {
  const readerDescriptor = Object.getOwnPropertyDescriptor(
    globalThis,
    'FileReader',
  )
  const bitmapDescriptor = Object.getOwnPropertyDescriptor(
    globalThis,
    'createImageBitmap',
  )
  class TestReader {
    result: string | null = null
    onloadend: (() => void) | null = null
    onerror: (() => void) | null = null
    readAsDataURL() {
      this.result = 'data:image/jpeg;base64,aGk='
      this.onloadend?.()
    }
  }
  Object.defineProperty(globalThis, 'FileReader', {
    configurable: true,
    value: TestReader,
  })
  Object.defineProperty(globalThis, 'createImageBitmap', {
    configurable: true,
    value: async () => ({ width: 400, height: 400, close() {} }),
  })
  let calls = 0
  try {
    const input = new File(['heic'], 'photo.heic', { type: 'image/heic' })
    const compress = (async () => {
      calls += 1
      return new File(['jpeg'], 'photo.jpg', { type: 'image/jpeg' })
    }) as unknown as Parameters<typeof processImageFiles>[1]
    const result = await processImageFiles([input], compress)
    expect(calls).toBe(1)
    expect(result).toEqual({
      failed: 0,
      images: [{ mimeType: 'image/jpeg', data: 'aGk=' }],
    })
  } finally {
    if (readerDescriptor)
      Object.defineProperty(globalThis, 'FileReader', readerDescriptor)
    else Reflect.deleteProperty(globalThis, 'FileReader')
    if (bitmapDescriptor)
      Object.defineProperty(globalThis, 'createImageBitmap', bitmapDescriptor)
    else Reflect.deleteProperty(globalThis, 'createImageBitmap')
  }
})
