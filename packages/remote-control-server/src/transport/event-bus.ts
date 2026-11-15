import { randomUUID } from 'node:crypto'
import { debug, error as logError } from '../logger'

export interface SessionEvent {
  id: string
  sessionId: string
  type: string
  payload: unknown
  direction: 'inbound' | 'outbound'
  seqNum: number
  createdAt: number
}

type Subscriber = (event: SessionEvent) => void

const MAX_EVENTS_PER_BUS = 5000

export class EventBus {
  /**
   * Identity of this bus instance. Sequence numbers restart at 1 whenever the
   * process restarts, so a client cursor is only meaningful together with the
   * generation it was issued by. Without it a stale cursor silently swallowed
   * every new-epoch event whose seqNum happened to be below it.
   */
  readonly generation = randomUUID()
  private subscribers = new Set<Subscriber>()
  private events: SessionEvent[] = []
  private seqNum = 0
  private closed = false
  private snapshots = new Map<string, SessionEvent>()

  subscribe(callback: Subscriber): () => void {
    this.subscribers.add(callback)
    return () => this.subscribers.delete(callback)
  }

  subscriberCount(): number {
    return this.subscribers.size
  }

  publish(event: Omit<SessionEvent, 'seqNum' | 'createdAt'>): SessionEvent {
    if (this.closed) throw new Error('EventBus is closed')
    const full: SessionEvent = {
      ...event,
      seqNum: ++this.seqNum,
      createdAt: Date.now(),
    }
    this.events.push(full)
    if (event.direction === 'inbound') {
      const payload = event.payload as Record<string, unknown> | null
      const raw = payload?.raw as Record<string, unknown> | undefined
      const isInit =
        event.type === 'system' && (raw?.subtype ?? payload?.subtype) === 'init'
      if (
        isInit ||
        event.type === 'task_state' ||
        event.type === 'workflow_state' ||
        event.type === 'usage_state' ||
        // Latest terminal queue snapshot: a long turn can evict the event from
        // the rolling history before a browser opens mid-run.
        event.type === 'terminal_queue'
      ) {
        this.snapshots.set(isInit ? 'system/init' : event.type, full)
      }
    }
    // Evict oldest events when exceeding limit
    if (this.events.length > MAX_EVENTS_PER_BUS) {
      this.events = this.events.slice(-Math.floor(MAX_EVENTS_PER_BUS / 2))
    }
    debug(
      `[RC-DEBUG] bus publish: sessionId=${event.sessionId} type=${event.type} dir=${event.direction} seq=${full.seqNum} subscribers=${this.subscribers.size}`,
    )
    for (const cb of this.subscribers) {
      try {
        cb(full)
      } catch (err) {
        logError(`[RC-DEBUG] bus subscriber error:`, err)
      }
    }
    return full
  }

  getLastSeqNum(): number {
    return this.seqNum
  }

  /** True once removeEventBus()/close() retired this bus (session archived). */
  isClosed(): boolean {
    return this.closed
  }

  getOldestSeqNum(): number {
    return this.events.length > 0 ? this.events[0]!.seqNum : 0
  }

  /**
   * True when a client sitting at `seqNum` cannot be replayed in full: the
   * cursor is older than the surviving buffer (events were evicted) or newer
   * than the bus itself (the process restarted and numbering began again).
   * Either way the gap is permanent, so the caller must not pretend it
   * delivered a complete stream.
   */
  hasGapFrom(seqNum: number): boolean {
    if (seqNum <= 0) return false
    if (seqNum > this.seqNum) return true
    const oldest = this.getOldestSeqNum()
    if (oldest === 0) return false
    return oldest > seqNum + 1
  }

  getEventsSince(seqNum: number): SessionEvent[] {
    const idx = this.events.findIndex(e => e.seqNum > seqNum)
    if (idx === -1) return []
    return this.events.slice(idx)
  }

  /** Latest execution state survives rolling history eviction. */
  getStateSnapshots(): SessionEvent[] {
    return [...this.snapshots.values()]
  }

  close() {
    this.closed = true
    this.subscribers.clear()
    this.snapshots.clear()
  }
}

/** Global registry of per-session event buses */
const buses = new Map<string, EventBus>()

export function getEventBus(sessionId: string): EventBus {
  let bus = buses.get(sessionId)
  if (!bus) {
    bus = new EventBus()
    buses.set(sessionId, bus)
  }
  return bus
}

export function removeEventBus(sessionId: string) {
  const bus = buses.get(sessionId)
  if (bus) {
    bus.close()
    buses.delete(sessionId)
  }
}

export function getAllEventBuses(): Map<string, EventBus> {
  return buses
}
