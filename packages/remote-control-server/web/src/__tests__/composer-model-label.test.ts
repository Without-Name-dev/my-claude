import { expect, test } from 'bun:test'
import { composerModelLabel } from '../lib/composer-model-label'

test('shows only the selected model, including encoded names', () => {
  expect(composerModelLabel('rcs-provider:id:glm-5.3', 'old')).toBe('glm-5.3')
  expect(composerModelLabel('rcs-provider:id:vendor%2Fmodel%3Av2')).toBe(
    'vendor/model:v2',
  )
  expect(composerModelLabel('local-model')).toBe('local-model')
  expect(composerModelLabel('', 'active-model')).toBe('active-model')
  expect(composerModelLabel('rcs-provider:id:%invalid', 'active-model')).toBe(
    'active-model',
  )
  expect(composerModelLabel('', undefined)).toBe('设置')
})
