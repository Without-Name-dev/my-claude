import { describe, expect, test } from 'bun:test'
import type { DeviceSessionSummary } from '../api/devices'
import { mergeSessionCatalog, rankRecentSessions } from '../lib/session-catalog'

function session(id: string): DeviceSessionSummary {
  return {
    localSessionId: id,
    projectId: 'project',
    cwd: 'project',
    title: id,
    summary: '',
    updatedAt: 1,
  }
}

describe('refreshing the session catalog', () => {
  test('removes sessions absent from a complete refreshed catalog', () => {
    expect(
      mergeSessionCatalog(
        [session('one'), session('deleted')],
        [session('one')],
        false,
      ),
    ).toEqual([session('one')])
  })

  test('keeps previously loaded older sessions when the refreshed page is partial', () => {
    expect(
      mergeSessionCatalog(
        [session('one'), session('older')],
        [session('new'), session('one')],
        true,
      ),
    ).toEqual([session('new'), session('one'), session('older')])
  })
})

test('recent sessions are ordered across devices by activity without mutating input', () => {
  const older = {
    ...session('older'),
    deviceId: 'device-a',
    deviceName: '电脑 A',
    updatedAt: 100,
  }
  const newer = {
    ...session('newer'),
    deviceId: 'device-b',
    deviceName: '电脑 B',
    updatedAt: 200,
  }
  const source = [older, newer]
  expect(rankRecentSessions(source).map(item => item.localSessionId)).toEqual([
    'newer',
    'older',
  ])
  expect(source).toEqual([older, newer])
})
