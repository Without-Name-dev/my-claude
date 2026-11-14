import { describe, expect, test } from 'bun:test'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { PlanPanelView } from '../components/PermissionViews'

describe('plan approval rendering', () => {
  test('escapes untrusted HTML while keeping plan formatting', () => {
    const html = renderToStaticMarkup(
      createElement(PlanPanelView, {
        requestId: 'plan',
        planContent:
          '**步骤** <img src=x onerror="alert(1)"> `x < y`\n```ts\n<script>alert(1)</script>\n```',
        description: '',
        onSubmit: () => {},
      }),
    )

    expect(html).toContain('<strong>步骤</strong>')
    expect(html).toContain('&lt;img src=x onerror=&quot;alert(1)&quot;&gt;')
    expect(html).toContain('x &lt; y')
    expect(html).toContain('&lt;script&gt;alert(1)&lt;/script&gt;')
    expect(html).not.toContain('<img')
    expect(html).not.toContain('<script>')
  })
})
