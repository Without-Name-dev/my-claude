import { expect, test } from 'bun:test'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { ChatInput } from '../../components/chat/ChatInput'

test('running turn without a draft renders one stop action', () => {
  const html = renderToStaticMarkup(
    createElement(ChatInput, {
      onSubmit: () => {},
      isLoading: true,
      onInterrupt: () => {},
    }),
  )
  expect(html.match(/aria-label="停止生成"/g)).toHaveLength(1)
  expect(html).not.toContain('aria-label="发送消息"')
})

test('running turn with a draft renders send rather than stop', () => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: () => 'queued follow-up' },
  })
  try {
    const html = renderToStaticMarkup(
      createElement(ChatInput, {
        onSubmit: () => {},
        isLoading: true,
        onInterrupt: () => {},
        draftKey: 'test',
      }),
    )
    expect(html.match(/aria-label="发送消息"/g)).toHaveLength(1)
    expect(html).not.toContain('aria-label="停止生成"')
  } finally {
    if (previous) Object.defineProperty(globalThis, 'localStorage', previous)
    else Reflect.deleteProperty(globalThis, 'localStorage')
  }
})
