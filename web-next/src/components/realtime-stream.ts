"use client"

import * as React from "react"
import { dashboardApi } from "@/features/dashboard/api"
import {
  parseDashboardSnapshotMessage,
  parseProtocolEventEnvelope,
} from "@/features/dashboard/stream"
import type { DashboardSnapshotMessage, ProtocolEventEnvelope } from "@/features/dashboard/types"
import { isAuthFailure } from "@/lib/api/errors"

const STREAM_CLIENT_ID = "web-stream"
const STREAM_READY_TIMEOUT_MS = 5_000
const RECONNECT_BASE_MS = 1_000
const RECONNECT_MAX_MS = 30_000
const HEARTBEAT_INTERVAL_MS = 15_000
// The Server sends keepalive every 15 s; three missed frames mean a dead socket.
const HEARTBEAT_STALE_MS = HEARTBEAT_INTERVAL_MS * 3

export type RealtimeStreamMode = "connecting" | "unified" | "fallback"

export type SessionStreamHandlers = {
  onEvent: (event: ProtocolEventEnvelope) => void
  /** The shared stream is usable: on subscribe after ready, and after every reconnect. */
  onReady?: () => void
  /**
   * The shared stream was abandoned for this page lifetime; the subscriber keeps
   * working by opening its legacy per-session socket. Called at most once for a
   * given subscription.
   */
  onUnavailable?: () => void
}

export type RealtimeStream = {
  mode: RealtimeStreamMode
  subscribeSession: (sessionId: string, handlers: SessionStreamHandlers) => () => void
  whenModeResolved: () => Promise<RealtimeStreamMode>
}

type UseRealtimeStreamOptions = {
  token: string | null
  /** Whether this page needs live workspace data at all (false on admin/settings). */
  enabled: boolean
  onDashboardSnapshot: (message: DashboardSnapshotMessage) => void
  onAuthFailure: () => void
}

function reconnectDelay(attempt: number): number {
  const base = Math.min(RECONNECT_BASE_MS * 2 ** Math.min(attempt, 5), RECONNECT_MAX_MS)
  return Math.min(base + Math.round(base * 0.2 * Math.random()), RECONNECT_MAX_MS)
}

/**
 * Owns the one multiplexed `/api/v2/stream/ws` socket for the tab: connection,
 * reconnect with capped backoff, heartbeat detection, ticket refresh on every
 * (re)connect and routing of dashboard and session frames.
 *
 * If the endpoint cannot open (or does not send `stream.ready` in time) the
 * hook settles on "fallback" for the rest of the page lifetime and never
 * retries the shared endpoint. Callers then keep using the legacy sockets.
 */
export function useRealtimeStream({
  token,
  enabled,
  onDashboardSnapshot,
  onAuthFailure,
}: UseRealtimeStreamOptions): RealtimeStream {
  const [mode, setMode] = React.useState<RealtimeStreamMode>(() => token ? "connecting" : "fallback")
  // null until the stream is known to work or to be unavailable.
  const settledModeRef = React.useRef<RealtimeStreamMode | null>(token ? null : "fallback")
  const waitersRef = React.useRef<Array<(mode: RealtimeStreamMode) => void>>([])
  const fellBackRef = React.useRef(false)
  const readyRef = React.useRef(false)
  const handlersRef = React.useRef(new Map<string, Set<SessionStreamHandlers>>())
  const snapshotHandlerRef = React.useRef(onDashboardSnapshot)
  snapshotHandlerRef.current = onDashboardSnapshot
  const authFailureRef = React.useRef(onAuthFailure)
  authFailureRef.current = onAuthFailure

  const settleMode = React.useCallback((next: RealtimeStreamMode) => {
    setMode((current) => current === next ? current : next)
    if (next === "connecting") return
    settledModeRef.current = next
    const waiters = waitersRef.current
    waitersRef.current = []
    for (const resolve of waiters) resolve(next)
  }, [])

  const whenModeResolved = React.useCallback((): Promise<RealtimeStreamMode> => {
    const settled = settledModeRef.current
    if (settled) return Promise.resolve(settled)
    return new Promise((resolve) => { waitersRef.current.push(resolve) })
  }, [])

  const subscribeSession = React.useCallback((sessionId: string, handlers: SessionStreamHandlers) => {
    // A subscriber that arrives after the permanent fallback decision must not
    // sit forever on a stream that will never reconnect. This also closes the
    // race where "unified" resolved just before the fallback and the view
    // subscribes late.
    if (fellBackRef.current) {
      handlers.onUnavailable?.()
      return () => {}
    }
    let subscribers = handlersRef.current.get(sessionId)
    if (!subscribers) {
      subscribers = new Set()
      handlersRef.current.set(sessionId, subscribers)
    }
    subscribers.add(handlers)
    if (readyRef.current) handlers.onReady?.()
    return () => {
      const current = handlersRef.current.get(sessionId)
      if (!current) return
      current.delete(handlers)
      if (current.size === 0) handlersRef.current.delete(sessionId)
    }
  }, [])

  React.useEffect(() => () => {
    // A waiter must not hang forever if the provider unmounts while connecting,
    // and the registry must not outlive the provider.
    const waiters = waitersRef.current
    waitersRef.current = []
    for (const resolve of waiters) resolve("fallback")
    handlersRef.current.clear()
  }, [])

  React.useEffect(() => {
    if (!token) {
      settleMode("fallback")
      return
    }
    if (fellBackRef.current) {
      settleMode("fallback")
      return
    }
    if (!enabled) return

    // A fresh connection attempt is starting. If this effect is re-arming after
    // the socket was torn down (the workspace was hidden, or the token changed)
    // an earlier "unified" verdict no longer describes the wire: consumers must
    // wait for this attempt instead of subscribing to a stream that may fall
    // back before it is ready.
    if (settledModeRef.current !== null) {
      settledModeRef.current = null
      setMode("connecting")
    }

    let cancelled = false
    let socket: WebSocket | null = null
    let reconnectTimer: number | null = null
    let readyTimer: number | null = null
    let heartbeatTimer: number | null = null
    let snapshotFrame: number | null = null
    let pendingSnapshot: DashboardSnapshotMessage | null = null
    let reconnectAttempt = 0
    let established = false
    let connecting = false
    let lastFrameAt = 0

    const scheduleDashboardSnapshot = (message: DashboardSnapshotMessage) => {
      pendingSnapshot = message
      if (snapshotFrame !== null) return
      snapshotFrame = window.requestAnimationFrame(() => {
        snapshotFrame = null
        const snapshot = pendingSnapshot
        pendingSnapshot = null
        if (!cancelled && snapshot) snapshotHandlerRef.current(snapshot)
      })
    }

    const stopHeartbeatWatch = () => {
      if (heartbeatTimer === null) return
      window.clearInterval(heartbeatTimer)
      heartbeatTimer = null
    }

    const startHeartbeatWatch = () => {
      stopHeartbeatWatch()
      heartbeatTimer = window.setInterval(() => {
        if (cancelled || !socket || socket.readyState !== WebSocket.OPEN) return
        if (Date.now() - lastFrameAt > HEARTBEAT_STALE_MS) {
          // The tab was asleep or the connection died silently; onclose reconnects.
          socket.close()
        }
      }, HEARTBEAT_INTERVAL_MS)
    }

    const notifyReady = () => {
      for (const subscribers of handlersRef.current.values()) {
        for (const handler of Array.from(subscribers)) handler.onReady?.()
      }
    }

    const markReady = () => {
      readyRef.current = true
      established = true
      reconnectAttempt = 0
      lastFrameAt = Date.now()
      if (readyTimer !== null) {
        window.clearTimeout(readyTimer)
        readyTimer = null
      }
      startHeartbeatWatch()
      settleMode("unified")
      notifyReady()
    }

    const fallBack = () => {
      if (cancelled) return
      cancelled = true
      fellBackRef.current = true
      readyRef.current = false
      established = false
      stopHeartbeatWatch()
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer)
        reconnectTimer = null
      }
      if (readyTimer !== null) {
        window.clearTimeout(readyTimer)
        readyTimer = null
      }
      if (snapshotFrame !== null) {
        window.cancelAnimationFrame(snapshotFrame)
        snapshotFrame = null
      }
      pendingSnapshot = null
      const current = socket
      socket = null
      current?.close()
      // Hand every live subscriber over to its legacy socket. Without this a
      // session view that subscribed under a "unified" verdict (for example
      // after the workspace socket was re-armed) would go silent forever.
      const subscribers = handlersRef.current
      handlersRef.current = new Map()
      for (const set of subscribers.values()) {
        for (const handler of Array.from(set)) handler.onUnavailable?.()
      }
      settleMode("fallback")
    }

    const scheduleReconnect = () => {
      if (cancelled || reconnectTimer !== null) return
      const delay = reconnectDelay(reconnectAttempt)
      reconnectAttempt += 1
      reconnectTimer = window.setTimeout(() => {
        reconnectTimer = null
        void connect()
      }, delay)
    }

    const connect = async () => {
      if (cancelled || connecting) return
      connecting = true
      try {
        // A fresh ticket on every (re)connect; the Server only accepts it once.
        const ticket = await dashboardApi.createStreamWsTicket(token, STREAM_CLIENT_ID)
        if (cancelled) return
        const nextSocket = new WebSocket(dashboardApi.streamWebSocketUrl(ticket.ticket))
        socket = nextSocket
        let closeHandled = false

        const handleClose = () => {
          if (closeHandled) return
          closeHandled = true
          if (socket === nextSocket) socket = null
          readyRef.current = false
          stopHeartbeatWatch()
          if (readyTimer !== null) {
            window.clearTimeout(readyTimer)
            readyTimer = null
          }
          if (cancelled) return
          // The shared endpoint is only abandoned while it has never worked.
          if (!established) fallBack()
          else scheduleReconnect()
        }

        nextSocket.onmessage = (event) => {
          if (cancelled || typeof event.data !== "string") return
          lastFrameAt = Date.now()
          let parsed: unknown
          try {
            parsed = JSON.parse(event.data)
          } catch {
            return
          }
          if (!parsed || typeof parsed !== "object") return
          const type = (parsed as { type?: unknown }).type
          if (type === "stream.ready") {
            markReady()
            return
          }
          if (type === "keepalive") return
          if (type === "dashboard.snapshot") {
            const snapshot = parseDashboardSnapshotMessage(parsed)
            if (snapshot) scheduleDashboardSnapshot(snapshot)
            return
          }
          const sessionEvent = parseProtocolEventEnvelope(parsed)
          if (!sessionEvent) return
          const subscribers = handlersRef.current.get(sessionEvent.sessionId)
          if (!subscribers) return
          for (const handler of Array.from(subscribers)) handler.onEvent(sessionEvent)
        }
        nextSocket.onerror = () => {
          nextSocket.close()
        }
        nextSocket.onclose = handleClose
        lastFrameAt = Date.now()
        readyTimer = window.setTimeout(() => {
          readyTimer = null
          if (cancelled) return
          nextSocket.close()
          handleClose()
        }, STREAM_READY_TIMEOUT_MS)
      } catch (error) {
        if (cancelled) return
        if (isAuthFailure(error)) {
          // The session is going away, but a consumer waiting on the mode must
          // not hang until the provider unmounts. Fall back first so the waiters
          // and subscribers get an answer; the legacy sockets will surface the
          // same auth failure and sign out.
          if (!established) fallBack()
          authFailureRef.current()
          return
        }
        if (!established) fallBack()
        else scheduleReconnect()
      } finally {
        connecting = false
      }
    }

    const resume = () => {
      if (cancelled) return
      if (document.visibilityState === "hidden") return
      if (socket || connecting) return
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer)
        reconnectTimer = null
      }
      void connect()
    }

    document.addEventListener("visibilitychange", resume)
    window.addEventListener("online", resume)
    void connect()

    return () => {
      cancelled = true
      document.removeEventListener("visibilitychange", resume)
      window.removeEventListener("online", resume)
      if (reconnectTimer !== null) window.clearTimeout(reconnectTimer)
      if (readyTimer !== null) window.clearTimeout(readyTimer)
      stopHeartbeatWatch()
      if (snapshotFrame !== null) window.cancelAnimationFrame(snapshotFrame)
      pendingSnapshot = null
      const current = socket
      socket = null
      current?.close()
      readyRef.current = false
    }
  }, [enabled, settleMode, token])

  return {
    mode,
    subscribeSession,
    whenModeResolved,
  }
}
