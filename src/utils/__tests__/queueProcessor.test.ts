import { afterEach, beforeEach, describe, expect, test } from 'bun:test'

import {
  resetCommandQueue,
  enqueue,
  enqueuePendingNotification,
} from '../messageQueueManager.js'
import { hasQueuedCommands, processQueueIfReady } from '../queueProcessor.js'

beforeEach(() => {
  resetCommandQueue()
})

afterEach(() => {
  resetCommandQueue()
})

describe('processQueueIfReady', () => {
  test('returns processed:false when queue empty', () => {
    const result = processQueueIfReady({
      executeInput: async () => {},
    })
    expect(result.processed).toBe(false)
  })

  test('processes single slash command individually', () => {
    const executed: string[][] = []
    enqueue({ value: '/help', mode: 'prompt' } as any)

    const result = processQueueIfReady({
      executeInput: async cmds => {
        executed.push(cmds.map(c => c.value as string))
      },
    })

    expect(result.processed).toBe(true)
    expect(executed).toHaveLength(1)
    expect(executed[0]).toEqual(['/help'])
  })

  test('processes bash mode command individually', () => {
    const executed: string[][] = []
    enqueue({ value: 'git status', mode: 'bash' } as any)

    const result = processQueueIfReady({
      executeInput: async cmds => {
        executed.push(cmds.map(c => c.value as string))
      },
    })

    expect(result.processed).toBe(true)
    expect(executed).toHaveLength(1)
    expect(executed[0]).toEqual(['git status'])
  })

  test('batches commands with same mode', () => {
    const executed: string[][] = []
    enqueuePendingNotification({
      value: '<task1/>',
      mode: 'task-notification',
    } as any)
    enqueuePendingNotification({
      value: '<task2/>',
      mode: 'task-notification',
    } as any)

    const result = processQueueIfReady({
      executeInput: async cmds => {
        executed.push(cmds.map(c => c.value as string))
      },
    })

    expect(result.processed).toBe(true)
    expect(executed).toHaveLength(1)
    expect(executed[0]).toEqual(['<task1/>', '<task2/>'])
  })

  test('does not mix different modes in same batch', () => {
    const executed: string[][] = []
    enqueue({ value: 'hello', mode: 'prompt' } as any)
    enqueuePendingNotification({
      value: '<task/>',
      mode: 'task-notification',
    } as any)

    const result = processQueueIfReady({
      executeInput: async cmds => {
        executed.push(cmds.map(c => c.value as string))
      },
    })

    expect(result.processed).toBe(true)
    // Only the 'prompt' mode command should be processed (higher priority than task-notification)
    expect(executed).toHaveLength(1)
    expect(executed[0]).toEqual(['hello'])

    // The task-notification is still in queue
    expect(hasQueuedCommands()).toBe(true)
  })

  test('skips commands with agentId set (subagent notifications)', () => {
    // This simulates the v2.1.119 fix: subagent task-notification with agentId
    // should not be processed by the main thread queue processor
    enqueuePendingNotification({
      value: '<task-notification>subagent result</task-notification>',
      mode: 'task-notification',
      agentId: 'agent-123',
    } as any)

    const result = processQueueIfReady({
      executeInput: async () => {},
    })

    // Should not process — it's a subagent notification
    expect(result.processed).toBe(false)
  })

  test('returns processed:false when only subagent commands in queue', () => {
    enqueuePendingNotification({
      value: '<task-notification/>',
      mode: 'task-notification',
      agentId: 'agent-456',
    } as any)
    enqueuePendingNotification({
      value: '<task-notification/>',
      mode: 'task-notification',
      agentId: 'agent-789',
    } as any)

    const result = processQueueIfReady({
      executeInput: async () => {},
    })

    expect(result.processed).toBe(false)
    expect(hasQueuedCommands()).toBe(true)
  })

  test('returns the execution promise so a failed turn is observable', async () => {
    enqueue({ value: 'hello', mode: 'prompt' } as any)
    const result = processQueueIfReady({
      executeInput: async () => {
        throw new Error('boom')
      },
    })
    expect(result.processed).toBe(true)
    await expect(result.execution).rejects.toThrow('boom')
  })

  test('a later prompt never overtakes an intervening slash command', () => {
    const executed: string[][] = []
    enqueue({ value: 'first prompt', mode: 'prompt' } as any)
    enqueue({ value: '/help', mode: 'prompt' } as any)
    enqueue({ value: 'second prompt', mode: 'prompt' } as any)

    const run = () =>
      processQueueIfReady({
        executeInput: async cmds => {
          executed.push(cmds.map(c => c.value as string))
        },
      })

    expect(run().processed).toBe(true)
    // The batch stops at the slash command; it does not reach past it.
    expect(executed[0]).toEqual(['first prompt'])

    expect(run().processed).toBe(true)
    expect(executed[1]).toEqual(['/help'])

    expect(run().processed).toBe(true)
    expect(executed[2]).toEqual(['second prompt'])
  })

  test('a non-string, non-array value does not wedge the drain', () => {
    const executed: string[][] = []
    // A bridge client can deliver a stray object as content. Iterating it
    // used to throw before the dequeue, stalling every later message.
    enqueue({ value: { text: 'oops' }, mode: 'prompt' } as any)
    enqueue({ value: 'after', mode: 'prompt' } as any)

    expect(() =>
      processQueueIfReady({
        executeInput: async cmds => {
          executed.push(
            cmds.map(c =>
              String((c.value as { text?: string }).text ?? c.value),
            ),
          )
        },
      }),
    ).not.toThrow()
    expect(executed[0]).toEqual(['oops', 'after'])
  })

  test('processes main-thread command but skips subagent command', () => {
    const executed: string[][] = []
    enqueuePendingNotification({
      value: '<main-task/>',
      mode: 'task-notification',
    } as any)
    enqueuePendingNotification({
      value: '<sub-task/>',
      mode: 'task-notification',
      agentId: 'agent-123',
    } as any)

    const result = processQueueIfReady({
      executeInput: async cmds => {
        executed.push(cmds.map(c => c.value as string))
      },
    })

    expect(result.processed).toBe(true)
    expect(executed).toHaveLength(1)
    expect(executed[0]).toEqual(['<main-task/>'])

    // Subagent command still in queue
    expect(hasQueuedCommands()).toBe(true)
  })
})

describe('hasQueuedCommands', () => {
  test('returns false when queue empty', () => {
    expect(hasQueuedCommands()).toBe(false)
  })

  test('returns true when commands in queue', () => {
    enqueue({ value: 'hello', mode: 'prompt' } as any)
    expect(hasQueuedCommands()).toBe(true)
  })
})
