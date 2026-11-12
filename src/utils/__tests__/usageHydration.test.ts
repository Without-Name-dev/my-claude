import { describe, expect, test } from 'bun:test'
import { sumUsageFromTranscriptText } from '../usageHydration.js'

const line = (uuid: string, model: string, usage: Record<string, number>) =>
  JSON.stringify({
    type: 'assistant',
    uuid,
    message: { role: 'assistant', model, usage },
  })

describe('sumUsageFromTranscriptText', () => {
  test('sums billed usage per model', () => {
    const text = [
      line('a', 'deepseek/deepseek-flash[1m]', {
        input_tokens: 10,
        output_tokens: 5,
        cache_read_input_tokens: 100,
        cache_creation_input_tokens: 50,
      }),
      line('b', 'deepseek/deepseek-flash[1m]', {
        input_tokens: 2,
        output_tokens: 1,
        cache_read_input_tokens: 200,
        cache_creation_input_tokens: 0,
      }),
      line('c', 'glm-5.3[1m]', { input_tokens: 7, output_tokens: 3 }),
    ].join('\n')
    const totals = sumUsageFromTranscriptText(text)
    const ds = totals.get('deepseek/deepseek-flash[1m]')
    expect(ds?.inputTokens).toBe(12)
    expect(ds?.outputTokens).toBe(6)
    expect(ds?.cacheReadInputTokens).toBe(300)
    expect(ds?.cacheCreationInputTokens).toBe(50)
    expect(totals.get('glm-5.3[1m]')?.inputTokens).toBe(7)
  })

  test('counts a duplicated entry once', () => {
    const usage = {
      input_tokens: 10,
      output_tokens: 5,
      cache_read_input_tokens: 0,
    }
    const text = [line('same', 'm', usage), line('same', 'm', usage)].join('\n')
    expect(sumUsageFromTranscriptText(text).get('m')?.inputTokens).toBe(10)
  })

  test('ignores synthetic models, non-assistant rows and broken lines', () => {
    const text = [
      line('a', '<synthetic>', { input_tokens: 99 }),
      JSON.stringify({
        type: 'user',
        uuid: 'u',
        message: { role: 'user', content: 'hi' },
      }),
      '{not json',
      line('b', 'm', { input_tokens: 1 }),
    ].join('\n')
    const totals = sumUsageFromTranscriptText(text)
    expect([...totals.keys()]).toEqual(['m'])
    expect(totals.get('m')?.inputTokens).toBe(1)
  })
})
