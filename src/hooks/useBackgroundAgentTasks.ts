import { useEffect, useMemo, useState } from 'react'
import { useAppState } from '../state/AppState.js'
import {
  isLocalAgentTask,
  type LocalAgentTaskState,
} from '../tasks/LocalAgentTask/LocalAgentTask.js'
import type { AppState } from '../state/AppState.js'

/** The visible list is shared by the selector and its keyboard focus handler. */
export function getBackgroundAgentTasks(
  tasks: AppState['tasks'],
  now = Date.now(),
): LocalAgentTaskState[] {
  return Object.values(tasks)
    .filter(isLocalAgentTask)
    .filter(t => t.agentType !== 'main-session')
    .filter(t => t.evictAfter === undefined || t.evictAfter > now)
    .sort((a, b) => a.startTime - b.startTime)
}

export function useBackgroundAgentTasks(): LocalAgentTaskState[] {
  const tasks = useAppState(s => s.tasks)
  const [expiryTick, setExpiryTick] = useState(0)
  // Completion grace periods expire even if no task changes afterwards.
  // Otherwise a stale row can remain visible but cannot receive focus.
  useEffect(() => {
    const now = Date.now()
    const deadlines = Object.values(tasks)
      .filter(isLocalAgentTask)
      .map(task => task.evictAfter)
      .filter(
        (deadline): deadline is number =>
          typeof deadline === 'number' && deadline > now,
      )
    if (deadlines.length === 0) return
    const delay = Math.min(
      2_147_483_647,
      Math.max(1, Math.min(...deadlines) - now + 1),
    )
    const timer = setTimeout(() => setExpiryTick(value => value + 1), delay)
    timer.unref?.()
    return () => clearTimeout(timer)
  }, [tasks, expiryTick])
  return useMemo(() => {
    return getBackgroundAgentTasks(tasks)
  }, [tasks, expiryTick])
}
