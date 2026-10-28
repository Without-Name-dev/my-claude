"use client"

import * as React from "react"
import { ArrowDown, ChevronDown, CircleAlert, Loader2, WifiOff } from "lucide-react"
import { toast } from "sonner"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Marker, MarkerContent, MarkerIcon } from "@/components/ui/marker"
import { ScrollArea } from "@/components/ui/scroll-area"
import { createClientId } from "@/lib/id"
import { isApiError, isAuthFailure } from "@/lib/api/errors"
import { cn } from "@/lib/utils"
import { dashboardApi } from "@/features/dashboard/api"
import { parseProtocolEventData } from "@/features/dashboard/stream"
import type {
  AgentRun,
  Notice,
  ProtocolCapabilitySet,
  ProtocolEventEnvelope,
  ProtocolModelCatalog,
  ProtocolPermissionCatalog,
  SessionLocalTimelineState,
  SessionSnapshotResponse,
  SessionRuntimeState,
  SessionView,
  TimelineItem,
} from "@/features/dashboard/types"
import { useTranslations } from "next-intl"
import { InteractionCard, NotificationCard } from "@/components/session/session-approval-card"
import { SessionSkeleton, SessionSkeletonInline } from "@/components/session/session-skeleton"
import { TimelineEntry } from "@/components/session/session-timeline-entry"
import { SubagentActivityBlock } from "@/components/session/session-subagent-activity"
import { TurnActions, type TurnAction } from "@/components/session/turn-actions"
import {
  JsonBlock,
  timelineItemStatusIsActive,
  ToolMarkerRowContent,
} from "@/components/session/session-tool-cards"
import { timelineRunCounts } from "@/components/session/timeline-summary"
import { needsOlderTimelinePage } from "@/components/session/timeline-autofill"
import { createTimelineScrollFollow } from "@/components/session/timeline-scroll-follow"
import { createSessionEventBuffer } from "@/components/session/session-event-buffer"
import { CAPABILITY, capabilityIsUsable, findCapability } from "@/components/session/capabilities"
import { SessionComposer, type AttachedFile } from "@/components/session/session-composer"
import { commandResult, commandTransportFailure, type CommandOutcome } from "@/components/session/runtime-command-model"
import { createRecoveredSubscriptionTracker, useRuntimeCommands } from "@/components/session/use-runtime-commands"
import {
  acceptSessionEventId,
  bufferedEventsAfterLiveCapabilityRead,
  drainSessionEventBuffer,
  mergeEffectiveCapabilities,
  SessionEventSequenceCursor,
  settleSessionEventRecovery,
  sessionEventUsesDurableEventIdDedup,
  stableStringify,
} from "@/components/session/session-event-state"
import {
  buildOptimisticUserMessage,
  isOptimisticTimelineItem,
  markOptimisticItemFailed,
  mergeTimelineItems,
  preserveOptimisticItems,
  timelineClientMessageId,
} from "@/components/session/optimistic-timeline"
import { nextTimelineResetVersion } from "@/components/session/session-review-history"
import { createTurnReviewDisplaySelector, type SessionReviewTarget } from "@/components/session/session-review-model"
import { SessionReviewCard } from "@/components/session/session-review-card"
import { SessionReviewTag } from "@/components/session/session-review-tag"
import {
  effectiveRuntimeStatus,
  isVisibleTimelineItem,
  messageText,
  runtimeLabel,
  textOf,
} from "@/components/session/session-utils"
import { stripInjectedAttachmentMentions } from "@/features/dashboard/attachments"
import { sessionRuntimeId, sessionRuntimeType } from "@/features/dashboard/runtime-instances"
import { useWorkspace } from "@/components/workspace-context"
import { useAuth } from "@/components/auth/auth-context"

type SessionDetailProps = {
  token: string
  sessionId: string
  fallbackSession: SessionView | null
  connectorDeviceOs?: string | null
  onOpenReview?: (target?: SessionReviewTarget) => void
  onSessionUpdated?: (session: SessionView) => void
  onMemorySnapshotUpdated?: (snapshot: SessionMemorySnapshot | null) => void
  onStreamProgress?: (sessionId: string, nextSeq: number | null) => void
}

export type SessionMemorySnapshot = {
  session: SessionView
  state?: SessionRuntimeState | null
  items: TimelineItem[]
  notices: Notice[]
  nextSeq: number
  hasMore: boolean
  timelineResetVersion: number
  serverTime: string
  pendingInteractionCount: number
}

type SessionRemoteState = {
  session: SessionView
  state?: SessionRuntimeState | null
  items: TimelineItem[]
  notices: Notice[]
  /** Live subagents; an empty array means none is running. */
  agents: AgentRun[]
  /** Whether `agents` came from a successful read or a live frame. A failed
   * refetch must not wipe a set that is still valid. */
  agentsLoaded: boolean
  nextSeq: number
  hasMore: boolean
  timelineResetVersion: number
  serverTime: string
  eventCursor: string
  effectiveCapabilities: ProtocolCapabilitySet | null
  catalogs: {
    model?: ProtocolModelCatalog
    permission?: ProtocolPermissionCatalog
    [key: string]: unknown
  }
}

function remoteStateFromOptimisticState(optimisticState: SessionLocalTimelineState): SessionRemoteState {
  return {
    ...optimisticState,
    notices: optimisticState.notices ?? [],
    agents: [],
    agentsLoaded: false,
    timelineResetVersion: 0,
    eventCursor: `seq:${optimisticState.nextSeq}`,
    effectiveCapabilities: null,
    catalogs: {},
  }
}

const INITIAL_TIMELINE_LIMIT = 100
// Live events append to the timeline without the scroll-to-bottom prune. Keep
// the in-memory window bounded so a long session does not rebuild the whole
// conversation on every frame; older items remain loadable from the server.
const MAX_TIMELINE_ITEMS = 500
const INITIAL_LOAD_MAX_ATTEMPTS = 3
const TIMELINE_PAGE_LIMIT = 100
const LOAD_OLDER_SCROLL_THRESHOLD = 96
const SCROLL_TO_BOTTOM_PRUNE_DISTANCE = 180
const INITIAL_SCROLL_LAYOUT_QUIET_MS = 120
const INITIAL_SCROLL_LAYOUT_FALLBACK_MS = 900
const SCROLL_TO_BOTTOM_PRUNE_CHECK_MS = 120
const COMPOSER_DRAFT_STORAGE_PREFIX = "agents-anywhere.sessionComposerDraft.v1."
type ComposerDraftState = {
  sessionId: string
  value: string
}
type SessionSendBatch = {
  pending: number
  accepted: number
  previousRuntimeState: SessionRuntimeState | null
}
type SessionSourceErrorCode =
  | "session_archived"
  | "session_unavailable"
  | "session_deleted"
  | "session_missing"

async function loadInitialSessionState(
  token: string,
  sessionId: string,
  options: { reason?: string } = {},
): Promise<SessionRemoteState> {
  const reason = options.reason ?? "session-detail.initial-load"
  const [snapshot, noticesResponse, activityResponse] = await Promise.all([
    dashboardApi.getSessionSnapshot(token, sessionId, INITIAL_TIMELINE_LIMIT, {
      reason,
    }),
    dashboardApi.getSessionNotices(token, sessionId).catch(() => null),
    // Activity is live-only: a client that connects after the agents started
    // learns them here, not from a replayed event.
    dashboardApi.getSessionActivity(token, sessionId).catch(() => null),
  ])
  const state = sessionStateFromSnapshot(snapshot)
  if (noticesResponse) {
    state.notices = mergeNotices(
      state.notices,
      serverNotices(noticesResponse.notices, noticesResponse.serverTime),
    )
  }
  if (activityResponse) {
    const runs = readAgentRuns(activityResponse.agents)
    if (runs !== null) {
      state.agents = runs
      state.agentsLoaded = true
    }
  }
  return state
}

function sessionStateFromSnapshot(snapshot: SessionSnapshotResponse): SessionRemoteState {
  return {
    session: snapshot.session,
    state: snapshot.state ?? null,
    items: mergeTimelineItems([], snapshot.timeline.items),
    notices: serverNotices(snapshot.notices, snapshot.serverTime),
    agents: [],
    agentsLoaded: false,
    nextSeq: snapshot.timeline.nextSeq,
    hasMore: snapshot.timeline.hasMore,
    timelineResetVersion: 0,
    serverTime: snapshot.serverTime,
    eventCursor: snapshot.eventCursor,
    effectiveCapabilities: snapshot.effectiveCapabilities,
    catalogs: snapshot.catalogs ?? {},
  }
}

/** Reads the activity endpoint/frame's agent list, dropping malformed rows. */
function readAgentRuns(value: unknown): AgentRun[] | null {
  if (!Array.isArray(value)) return null
  const runs: AgentRun[] = []
  for (const raw of value) {
    if (!raw || typeof raw !== "object") continue
    const record = raw as Record<string, unknown>
    if (typeof record.agentId !== "string") continue
    runs.push({
      agentId: record.agentId,
      label: typeof record.label === "string" ? record.label : "",
      status: typeof record.status === "string" ? record.status : "",
      startedAt: typeof record.startedAt === "number" ? record.startedAt : 0,
      updatedAt: typeof record.updatedAt === "number" ? record.updatedAt : 0,
      ...(typeof record.tool === "string" && record.tool ? { tool: record.tool } : {}),
      ...(typeof record.detail === "string" && record.detail ? { detail: record.detail } : {}),
      ...(typeof record.text === "string" && record.text ? { text: record.text } : {}),
    })
  }
  return runs
}

function agentRunsEqual(left: AgentRun[], right: AgentRun[]): boolean {
  if (left.length !== right.length) return false
  return stableStringify(left) === stableStringify(right)
}

function mergeSessionSnapshot(
  current: SessionRemoteState,
  incoming: SessionRemoteState,
): SessionRemoteState {
  const items = mergeTimelineItems(current.items, incoming.items)
  const currentCapabilities = current.effectiveCapabilities
  const incomingCapabilities = incoming.effectiveCapabilities
  const effectiveCapabilities = currentCapabilities && incomingCapabilities
    && currentCapabilities.revision > incomingCapabilities.revision
    ? currentCapabilities
    : incomingCapabilities ?? currentCapabilities
  const catalogs = mergeVersionedCatalogs(current.catalogs, incoming.catalogs)
  return {
    ...incoming,
    session: current.session.updatedSeq > incoming.session.updatedSeq ? current.session : incoming.session,
    state: mergeRuntimeState(current.state, incoming.state, current.session.id),
    items,
    hasMore: current.hasMore || incoming.hasMore,
    notices: mergeSnapshotNotices(current.notices, incoming.notices, incoming.nextSeq),
    // A refetch whose activity read failed leaves `agents` empty without
    // meaning "none"; keep the live set in that case.
    agents: incoming.agentsLoaded ? incoming.agents : current.agents,
    agentsLoaded: incoming.agentsLoaded || current.agentsLoaded,
    effectiveCapabilities,
    catalogs,
    nextSeq: Math.max(current.nextSeq, incoming.nextSeq),
    timelineResetVersion: current.timelineResetVersion,
    eventCursor: incoming.nextSeq >= current.nextSeq
      ? incoming.eventCursor
      : current.eventCursor,
  }
}

// Trim the oldest items once the live timeline outgrows the window. Only do
// this while the viewport follows the bottom: trimming above a reader's
// scroll position would move the text under them.
function boundTimelineItems(
  state: SessionRemoteState,
  following: boolean,
): SessionRemoteState {
  if (!following || state.items.length <= MAX_TIMELINE_ITEMS) return state
  return {
    ...state,
    items: state.items.slice(-MAX_TIMELINE_ITEMS),
    hasMore: true,
  }
}

function nextOptimisticRuntimeState(
  state: SessionRuntimeState | null | undefined,
  session: SessionView,
  status: SessionRuntimeState["status"],
): SessionRuntimeState {
  const now = new Date().toISOString()
  return {
    sessionId: session.id,
    runtime: session.runtime,
    runtimeId: sessionRuntimeId(session),
    runtimeType: sessionRuntimeType(session),
    externalSessionId: session.externalSessionId,
    status,
    selections: state?.selections ?? {},
    statusReason: state?.statusReason ?? null,
    error: state?.error ?? null,
    metadata: state?.metadata ?? {},
    updatedSeq: state?.updatedSeq ?? session.updatedSeq,
    createdAt: state?.createdAt ?? now,
    updatedAt: now,
  }
}

function selectionPatchFromComposerSelections(
  current: Record<string, string | null>,
  selections: { model?: string; permission?: string },
): Record<string, string | null> {
  const patch: Record<string, string | null> = {}
  if (selections.model && selections.model !== current.model) {
    patch.model = selections.model
  }
  if (selections.permission && selections.permission !== current.permission) {
    patch.permission = selections.permission
  }
  return patch
}

function runtimeStateWithSelections(
  state: SessionRuntimeState | null | undefined,
  session: SessionView,
  selections: Record<string, string | null>,
): SessionRuntimeState {
  const nextState = nextOptimisticRuntimeState(state, session, state?.status ?? "idle")
  return {
    ...nextState,
    selections: {
      ...nextState.selections,
      ...selections,
    },
  }
}

function runtimeStateWithSelectionResult(
  state: SessionRuntimeState | null | undefined,
  session: SessionView,
  selectionPatch: Record<string, string | null>,
  resultState: SessionRuntimeState | null | undefined,
): SessionRuntimeState {
  const nextState = state ?? resultState
  return runtimeStateWithSelections(nextState, session, {
    ...selectionPatch,
    ...(resultState?.selections ?? {}),
  })
}

function runtimeStateWithSelectionRollback(
  state: SessionRuntimeState | null | undefined,
  session: SessionView,
  previousSelections: Record<string, string | null>,
  selectionPatch: Record<string, string | null>,
): SessionRuntimeState {
  const nextState = nextOptimisticRuntimeState(state, session, state?.status ?? "idle")
  const selections = { ...nextState.selections }
  for (const key of Object.keys(selectionPatch)) {
    if (Object.hasOwn(previousSelections, key)) selections[key] = previousSelections[key] ?? null
    else delete selections[key]
  }
  return { ...nextState, selections }
}

function composerDraftStorageKey(sessionId: string): string {
  return `${COMPOSER_DRAFT_STORAGE_PREFIX}${sessionId}`
}

function readComposerDraft(sessionId: string): string {
  try {
    return window.localStorage.getItem(composerDraftStorageKey(sessionId)) ?? ""
  } catch {
    return ""
  }
}

function writeComposerDraft(sessionId: string, value: string) {
  try {
    const key = composerDraftStorageKey(sessionId)
    if (value) window.localStorage.setItem(key, value)
    else window.localStorage.removeItem(key)
  } catch {
    // Draft persistence is best-effort; private contexts can still use the composer.
  }
}

function SessionDetailView({
  token,
  sessionId,
  fallbackSession,
  connectorDeviceOs,
  onOpenReview,
  onSessionUpdated,
  onMemorySnapshotUpdated,
  onStreamProgress,
}: SessionDetailProps) {
  const tSession = useTranslations("dashboard.session")
  const tNew = useTranslations("dashboard.new")
  const tCommon = useTranslations("common")
  const {
    addOptimisticMessage,
    clearResolvedOptimisticMessages,
    composerInsertion,
    consumeComposerInsertion,
    getOptimisticItems,
    getOptimisticSessionState,
    isOptimisticSession,
    markOptimisticMessageFailed,
    replaceHome,
    subscribeSessionStream,
    whenRealtimeStreamResolved,
  } = useWorkspace()
  const { signOut } = useAuth()
  const signOutRef = React.useRef(signOut)
  signOutRef.current = signOut
  const subscribeSessionStreamRef = React.useRef(subscribeSessionStream)
  subscribeSessionStreamRef.current = subscribeSessionStream
  const whenRealtimeStreamResolvedRef = React.useRef(whenRealtimeStreamResolved)
  whenRealtimeStreamResolvedRef.current = whenRealtimeStreamResolved
  const initialOptimisticState = isOptimisticSession(sessionId)
    ? getOptimisticSessionState(sessionId)
    : null
  const [state, setState] = React.useState<SessionRemoteState | null>(() =>
    initialOptimisticState ? remoteStateFromOptimisticState(initialOptimisticState) : null,
  )
  const stateRef = React.useRef(state)
  stateRef.current = state
  const memorySnapshotRef = React.useRef<{
    callback: SessionDetailProps["onMemorySnapshotUpdated"]
    snapshot: SessionMemorySnapshot | null
  }>({ callback: undefined, snapshot: null })
  const [loading, setLoading] = React.useState(() => !initialOptimisticState)
  const [error, setError] = React.useState<string | null>(null)
  const [sending, setSending] = React.useState(false)
  const activeSessionIdRef = React.useRef(sessionId)
  activeSessionIdRef.current = sessionId
  const sendBatchesRef = React.useRef(new Map<string, SessionSendBatch>())
  const [interrupting, setInterrupting] = React.useState(false)
  const [resolvingNoticeId, setResolvingNoticeId] = React.useState<string | null>(null)
  const [resolvingActionId, setResolvingActionId] = React.useState<string | null>(null)
  const [showScrollBottom, setShowScrollBottom] = React.useState(false)
  const [loadingOlder, setLoadingOlder] = React.useState(false)
  const [sourceErrorCode, setSourceErrorCode] = React.useState<SessionSourceErrorCode | null>(null)
  const [commandQuery, setCommandQuery] = React.useState<string | null>(null)
  const [catalogRecoveryGeneration, setCatalogRecoveryGeneration] = React.useState(0)
  const [blockingInteractionStackHeight, setBlockingInteractionStackHeight] = React.useState(0)
  const [composerHeight, setComposerHeight] = React.useState(144)
  const [timelineGroupOpenByKey, setTimelineGroupOpenByKey] = React.useState<Record<string, boolean>>({})
  const [timelineItemOpenById, setTimelineItemOpenById] = React.useState<Record<string, boolean>>({})
  const [composerDraftState, setComposerDraftState] = React.useState<ComposerDraftState>(() => ({
    sessionId,
    value: readComposerDraft(sessionId),
  }))
  const timelineRef = React.useRef<HTMLDivElement | null>(null)
  const timelineContentRef = React.useRef<HTMLDivElement | null>(null)
  const composerContainerRef = React.useRef<HTMLDivElement | null>(null)
  const eventSequenceCursorRef = React.useRef<SessionEventSequenceCursor | null>(null)
  if (eventSequenceCursorRef.current === null) {
    eventSequenceCursorRef.current = new SessionEventSequenceCursor(
      sessionId,
      initialOptimisticState?.nextSeq ?? 0,
    )
  }
  const eventSequenceCursor = eventSequenceCursorRef.current
  const timelineFollowRef = React.useRef<ReturnType<typeof createTimelineScrollFollow> | null>(null)
  const initialScrollDoneRef = React.useRef(false)
  const loadingOlderRef = React.useRef(false)
  const pendingPrependScrollRestoreRef = React.useRef<{ scrollHeight: number; scrollTop: number } | null>(null)
  const initialScrollQuietTimerRef = React.useRef<number | null>(null)
  const initialScrollFallbackTimerRef = React.useRef<number | null>(null)
  const pruneAfterScrollTimerRef = React.useRef<number | null>(null)
  const pruneAfterScrollCleanupRef = React.useRef<(() => void) | null>(null)
  const streamConnectedRef = React.useRef(false)
  const catalogFetchKeyRef = React.useRef<string | null>(null)
  const selectionUpdateSeqRef = React.useRef(0)
  const selectionWritesRef = React.useRef(new Map<string, Promise<unknown>>())
  const commandVisitRef = React.useRef({ sessionId, sequence: 0 })
  if (commandVisitRef.current.sessionId !== sessionId) {
    commandVisitRef.current = { sessionId, sequence: commandVisitRef.current.sequence + 1 }
  }
  const commandRequestSeqRef = React.useRef(0)
  const activeCommandRequestRef = React.useRef<number | null>(null)

  const session = state?.session ?? fallbackSession
  const runtimeState = state?.state ?? null
  const runtimeStatus = effectiveRuntimeStatus(runtimeState, session)
  const turnInProgress = runtimeStatus === "waiting"
    || runtimeStatus === "pending"
    || runtimeStatus === "running"
    || runtimeStatus === "stopping"
    || runtimeStatus === "waiting_approval"
  const sessionRuntime = session ? sessionRuntimeId(session) : null
  const sessionRuntimeScope = session
    ? { runtimeId: sessionRuntimeId(session), runtimeType: sessionRuntimeType(session) }
    : undefined
  const effectiveCapabilities = state?.effectiveCapabilities ?? null
  const canUseModelCatalog = Boolean(
    sessionRuntime &&
      effectiveCapabilities &&
      capabilityIsUsable(effectiveCapabilities, CAPABILITY.modelCatalog, sessionRuntimeScope),
  )
  const canUsePermissionCatalog = Boolean(
    sessionRuntime &&
      effectiveCapabilities &&
      capabilityIsUsable(effectiveCapabilities, CAPABILITY.permissionCatalog, sessionRuntimeScope),
  )
  const commandSessionId = session?.id === sessionId ? sessionId : null
  const commandCapability = session ? findCapability(effectiveCapabilities, CAPABILITY.commands, sessionRuntimeScope) : null
  const catalogMetadata = commandCapability?.metadata as Record<string, unknown> | undefined
  const catalogRevision = typeof catalogMetadata?.catalogRevision === "string" ? catalogMetadata.catalogRevision : ""
  const commandAvailable = Boolean(commandCapability?.supported && commandCapability.available && commandCapability.allowed && session?.connectorStatus === "online")
  const { commands: runtimeCommands, loading: commandsLoading, error: commandsError } = useRuntimeCommands({
    token, sessionId: commandSessionId, open: commandQuery !== null,
    available: commandAvailable, catalogRevision, runtimeStatus, recoveryGeneration: catalogRecoveryGeneration,
  })

  const composerDraft = composerDraftState.sessionId === sessionId ? composerDraftState.value : ""
  const isLocalOptimisticSession = isOptimisticSession(sessionId)
  const hasInitialSessionState = state !== null

  React.useEffect(() => {
    setSourceErrorCode(null)
    activeCommandRequestRef.current = null
    return () => { activeCommandRequestRef.current = null }
  }, [sessionId])

  React.useEffect(() => {
    if (session?.id === sessionId && session.sourceAvailability === "archived") {
      setSourceErrorCode("session_archived")
    }
  }, [session?.id, session?.sourceAvailability, sessionId])

  const handleCommandQueryChange = React.useCallback((query: string | null) => {
    setCommandQuery(query)
  }, [])
  const handleTimelineGroupOpenChange = React.useCallback((key: string, open: boolean) => {
    setTimelineGroupOpenByKey((current) => {
      if (current[key] === open) return current
      return { ...current, [key]: open }
    })
  }, [])
  const handleTimelineItemOpenChange = React.useCallback((itemId: string, open: boolean) => {
    setTimelineItemOpenById((current) => {
      if (current[itemId] === open) return current
      return { ...current, [itemId]: open }
    })
  }, [])

  const handleSelectionChange = async (
    selections: { model?: string; permission?: string },
  ): Promise<boolean> => {
    if (!session) return false
    const selectionSessionId = session.id
    const selectionPatch = selectionPatchFromComposerSelections(state?.state?.selections ?? {}, selections)
    if (Object.keys(selectionPatch).length === 0) return true

    const previousRuntimeState = state?.state ?? null
    const selectionUpdateSeq = selectionUpdateSeqRef.current + 1
    selectionUpdateSeqRef.current = selectionUpdateSeq
    setState((current) =>
      current && current.session.id === selectionSessionId
        ? {
            ...current,
            state: runtimeStateWithSelections(current.state, current.session, selectionPatch),
          }
        : current,
    )
    try {
      const previousWrite = selectionWritesRef.current.get(selectionSessionId) ?? Promise.resolve()
      const write = previousWrite.catch(() => undefined).then(() => dashboardApi.updateSessionSelections(token, selectionSessionId, selectionPatch))
      selectionWritesRef.current.set(selectionSessionId, write)
      const result = await write.finally(() => {
        if (selectionWritesRef.current.get(selectionSessionId) === write) selectionWritesRef.current.delete(selectionSessionId)
      })
      if (selectionUpdateSeqRef.current !== selectionUpdateSeq) return true
      setState((current) =>
        current && current.session.id === selectionSessionId
          ? {
              ...current,
              state: runtimeStateWithSelectionResult(
                current.state,
                current.session,
                selectionPatch,
                result.state,
              ),
            }
          : current,
      )
      return true
    } catch (err) {
      // An official operation can partially succeed. Roll back the optimistic selection.
      if (selectionUpdateSeqRef.current === selectionUpdateSeq) {
        setState((current) =>
          current && current.session.id === selectionSessionId
            ? {
                ...current,
                state: runtimeStateWithSelectionRollback(
                  current.state,
                  current.session,
                  previousRuntimeState?.selections ?? {},
                  selectionPatch,
                ),
              }
            : current,
        )
      }
      if (selectionUpdateSeqRef.current === selectionUpdateSeq) {
        toast.error(err instanceof Error ? err.message : tSession("updateSelectionsFailed"))
      }
      return selectionUpdateSeqRef.current !== selectionUpdateSeq
    }
  }

  const applyOptimisticItems = React.useCallback((next: SessionRemoteState): SessionRemoteState => ({
    ...next,
    items: preserveOptimisticItems(next.items, getOptimisticItems(sessionId)),
  }), [getOptimisticItems, sessionId])
  const applyOptimisticItemsRef = React.useRef(applyOptimisticItems)
  const clearResolvedOptimisticMessagesRef = React.useRef(clearResolvedOptimisticMessages)
  const getOptimisticSessionStateRef = React.useRef(getOptimisticSessionState)
  const onSessionUpdatedRef = React.useRef(onSessionUpdated)
  const tSessionRef = React.useRef(tSession)

  React.useEffect(() => {
    applyOptimisticItemsRef.current = applyOptimisticItems
    clearResolvedOptimisticMessagesRef.current = clearResolvedOptimisticMessages
    getOptimisticSessionStateRef.current = getOptimisticSessionState
    onSessionUpdatedRef.current = onSessionUpdated
    tSessionRef.current = tSession
  }, [applyOptimisticItems, clearResolvedOptimisticMessages, getOptimisticSessionState, onSessionUpdated, tSession])

  React.useEffect(() => {
    setTimelineGroupOpenByKey({})
    setTimelineItemOpenById({})
    catalogFetchKeyRef.current = null
  }, [sessionId])

  React.useEffect(() => {
    const runtime = sessionRuntime
    if (!runtime) return
    const needsModelCatalog = canUseModelCatalog && !state?.catalogs.model
    const needsPermissionCatalog = canUsePermissionCatalog && !state?.catalogs.permission
    if (!needsModelCatalog && !needsPermissionCatalog) return
    const catalogFetchKey = [
      sessionId,
      runtime,
      needsModelCatalog ? "model" : "no-model",
      needsPermissionCatalog ? "permission" : "no-permission",
    ].join(":")
    if (catalogFetchKeyRef.current === catalogFetchKey) return
    catalogFetchKeyRef.current = catalogFetchKey

    let cancelled = false
    void Promise.all([
      needsModelCatalog
        ? dashboardApi.getSessionModelCatalog(token, sessionId)
        : Promise.resolve(null),
      needsPermissionCatalog
        ? dashboardApi.getSessionPermissionCatalog(token, sessionId)
        : Promise.resolve(null),
    ])
      .then(([modelCatalogResponse, permissionCatalogResponse]) => {
        if (cancelled) return
        setState((current) => {
          if (!current || current.session.id !== sessionId) return current
          return {
            ...current,
            catalogs: {
              ...current.catalogs,
              ...(modelCatalogResponse ? { model: modelCatalogResponse.catalog } : {}),
              ...(permissionCatalogResponse
                ? { permission: permissionCatalogResponse.catalog }
                : {}),
            },
          }
        })
      })
      .catch(() => {
        if (catalogFetchKeyRef.current === catalogFetchKey) {
          catalogFetchKeyRef.current = null
        }
        if (cancelled || process.env.NODE_ENV === "production") return
        console.debug("[AgentsAnywhere] session catalog refresh failed", {
          sessionId,
          runtime,
        })
      })

    return () => {
      cancelled = true
    }
  }, [
    canUseModelCatalog,
    canUsePermissionCatalog,
    sessionId,
    sessionRuntime,
    state?.catalogs.model,
    state?.catalogs.permission,
    token,
  ])

  React.useEffect(() => {
    const optimisticState = getOptimisticSessionState(sessionId)
    if (isLocalOptimisticSession) {
      if (optimisticState) {
        setState(remoteStateFromOptimisticState(optimisticState))
      }
      return
    }
    const optimisticItems = getOptimisticItems(sessionId)
    setState((current) => {
      if (!current) return current
      const serverItems = current.items.filter((item) => !isOptimisticTimelineItem(item))
      return { ...current, items: preserveOptimisticItems(serverItems, optimisticItems) }
    })
  }, [getOptimisticItems, getOptimisticSessionState, isLocalOptimisticSession, sessionId])

  React.useEffect(() => {
    setComposerDraftState({ sessionId, value: readComposerDraft(sessionId) })
  }, [sessionId])

  React.useEffect(() => {
    writeComposerDraft(composerDraftState.sessionId, composerDraftState.value)
  }, [composerDraftState])

  const setComposerDraft = React.useCallback((value: string) => {
    setComposerDraftState({ sessionId, value })
  }, [sessionId])

  React.useEffect(() => {
    if (!composerInsertion || composerInsertion.sessionId !== sessionId) return
    setComposerDraftState((current) => {
      const currentValue = current.sessionId === sessionId ? current.value : readComposerDraft(sessionId)
      const separator = currentValue.trim().length > 0 && !/\s$/.test(currentValue) ? " " : ""
      return {
        sessionId,
        value: `${currentValue}${separator}${composerInsertion.text}`,
      }
    })
    consumeComposerInsertion(composerInsertion.id)
  }, [composerInsertion, consumeComposerInsertion, sessionId])

  React.useEffect(() => {
    const previous = memorySnapshotRef.current
    const callbackChanged = previous.callback !== onMemorySnapshotUpdated
    if (!state) {
      if (callbackChanged || previous.snapshot !== null) {
        memorySnapshotRef.current = { callback: onMemorySnapshotUpdated, snapshot: null }
        onMemorySnapshotUpdated?.(null)
      }
      return
    }
    const pendingInteractionCount = blockingInteractions(state.notices, state.session.id).length
    const snapshot = previous.snapshot
    if (
      !callbackChanged &&
      snapshot &&
      snapshot.session === state.session &&
      snapshot.state === (state.state ?? null) &&
      snapshot.items === state.items &&
      snapshot.notices === state.notices &&
      snapshot.nextSeq === state.nextSeq &&
      snapshot.hasMore === state.hasMore &&
      snapshot.timelineResetVersion === state.timelineResetVersion &&
      snapshot.pendingInteractionCount === pendingInteractionCount
    ) return
    const next: SessionMemorySnapshot = {
      session: state.session,
      state: state.state ?? null,
      items: state.items,
      notices: state.notices,
      nextSeq: state.nextSeq,
      hasMore: state.hasMore,
      timelineResetVersion: state.timelineResetVersion,
      serverTime: state.serverTime,
      pendingInteractionCount,
    }
    memorySnapshotRef.current = { callback: onMemorySnapshotUpdated, snapshot: next }
    onMemorySnapshotUpdated?.(next)
  }, [onMemorySnapshotUpdated, state])

  React.useEffect(() => {
    onStreamProgress?.(sessionId, state?.nextSeq ?? 0)
  }, [onStreamProgress, sessionId, state?.nextSeq])

  React.useEffect(() => {
    return () => onStreamProgress?.(sessionId, null)
  }, [onStreamProgress, sessionId])

  const distanceFromBottom = React.useCallback(() => {
    const viewport = timelineRef.current
    if (!viewport) return 0
    return viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight
  }, [])

  const updateScrollBottomState = React.useCallback(() => {
    setShowScrollBottom(distanceFromBottom() > 96)
  }, [distanceFromBottom])

  React.useLayoutEffect(() => {
    const viewport = timelineRef.current
    const content = timelineContentRef.current
    if (!viewport || !content) return
    const follow = createTimelineScrollFollow(viewport, content, updateScrollBottomState, () => {
      pruneAfterScrollCleanupRef.current?.()
    })
    timelineFollowRef.current = follow
    return () => {
      follow.dispose()
      timelineFollowRef.current = null
      pruneAfterScrollCleanupRef.current?.()
    }
  }, [session?.id, sessionId, updateScrollBottomState])

  React.useEffect(() => {
    return () => {
      if (initialScrollQuietTimerRef.current !== null) {
        window.clearTimeout(initialScrollQuietTimerRef.current)
      }
      if (initialScrollFallbackTimerRef.current !== null) {
        window.clearTimeout(initialScrollFallbackTimerRef.current)
      }
      if (pruneAfterScrollTimerRef.current !== null) {
        window.clearTimeout(pruneAfterScrollTimerRef.current)
      }
    }
  }, [])

  const loadOlderTimeline = React.useCallback(async () => {
    if (loadingOlderRef.current || loadingOlder || !state?.hasMore) return
    const oldestItem = state.items[0]
    if (!oldestItem) return

    const viewport = timelineRef.current
    const previousScrollHeight = viewport?.scrollHeight ?? 0
    const previousScrollTop = viewport?.scrollTop ?? 0

    loadingOlderRef.current = true
    setLoadingOlder(true)
    try {
      const older = await dashboardApi.getSessionTimelineBefore(
        token,
        sessionId,
        oldestItem.orderSeq,
        TIMELINE_PAGE_LIMIT,
      )
      setState((current) => {
        if (!current || current.session.id !== sessionId) return current
        if (older.items.length === 0) return { ...current, hasMore: older.hasMore, serverTime: older.serverTime }
        const items = mergeTimelineItems(older.items, current.items)
        pendingPrependScrollRestoreRef.current = {
          scrollHeight: previousScrollHeight,
          scrollTop: previousScrollTop,
        }
        return {
          ...current,
          items,
          hasMore: older.hasMore,
          nextSeq: Math.max(current.nextSeq, older.nextSeq),
          serverTime: older.serverTime,
        }
      })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : tSession("loadFailed"))
    } finally {
      loadingOlderRef.current = false
      setLoadingOlder(false)
    }
  }, [loadingOlder, sessionId, state?.hasMore, state?.items, tSession, token])

  const handleTimelineScroll = React.useCallback(() => {
    const viewport = timelineRef.current
    updateScrollBottomState()
    if (!viewport || viewport.scrollTop > LOAD_OLDER_SCROLL_THRESHOLD) return
    void loadOlderTimeline()
  }, [loadOlderTimeline, updateScrollBottomState])

  // A first page that does not fill the viewport cannot be scrolled, so the
  // scroll handler above never fires. Pull older pages until it can scroll.
  React.useEffect(() => {
    if (loading || loadingOlder || !state?.hasMore) return
    if (!needsOlderTimelinePage(timelineRef.current, state.hasMore)) return
    void loadOlderTimeline()
  }, [loadOlderTimeline, loading, loadingOlder, state])

  React.useEffect(() => {
    initialScrollDoneRef.current = false
    if (initialScrollQuietTimerRef.current !== null) {
      window.clearTimeout(initialScrollQuietTimerRef.current)
      initialScrollQuietTimerRef.current = null
    }
    if (initialScrollFallbackTimerRef.current !== null) {
      window.clearTimeout(initialScrollFallbackTimerRef.current)
      initialScrollFallbackTimerRef.current = null
    }
    setSending((sendBatchesRef.current.get(sessionId)?.pending ?? 0) > 0)
    setInterrupting(false)
    setError(null)
    // Only a not-yet-bound local session is hydrated from the optimistic store.
    // After binding, its id belongs to a real session: reusing the placeholder
    // state would show the old optimistic status/selections under the new id
    // instead of the server snapshot.
    const optimisticState = isLocalOptimisticSession
      ? getOptimisticSessionStateRef.current(sessionId)
      : null
    eventSequenceCursor.switchTo(sessionId, optimisticState?.nextSeq ?? 0)
    if (optimisticState) {
      setState(remoteStateFromOptimisticState(optimisticState))
      eventSequenceCursor.advance(sessionId, optimisticState.nextSeq)
      setLoading(false)
    } else {
      setLoading(true)
      setState(null)
    }
  }, [
    isLocalOptimisticSession,
    onSessionUpdated,
    sessionId,
    token,
  ])

  React.useEffect(() => {
    if (isLocalOptimisticSession) return
    let cancelled = false
    let socket: WebSocket | null = null
    let reconnectTimer: number | null = null
    let refetchPromise: Promise<void> | null = null
    let recoveryPromise: Promise<void> | null = null
    let recoveryStarting = false
    let snapshotReady = false
    let snapshotHydrated = false
    let initialRetryTimer: number | null = null
    let initialLoadAttempt = 0
    let socketSubscribed = false
    let connectionSequence = 0
    const recoveredSubscriptions = createRecoveredSubscriptionTracker(() => {
      if (!cancelled) setCatalogRecoveryGeneration((generation) => generation + 1)
    })
    let bufferedEvents: ProtocolEventEnvelope[] = []
    let processedEventIds = new Set<string>()
    const commitEvents = (
      events: ProtocolEventEnvelope[],
      options: { allowStaleSequence?: boolean } = {},
    ) => {
      if (cancelled) return
      setState((current) => {
        const next = events.reduce((stateAfterEvent, event) =>
          !options.allowStaleSequence
            && stateAfterEvent
            && event.sequence < stateAfterEvent.nextSeq
            ? stateAfterEvent
            : mergeSessionEvent(stateAfterEvent, event), current)
        return next
          ? boundTimelineItems(next, timelineFollowRef.current?.isFollowing() ?? false)
          : next
      })
      const items = events.flatMap((event) => {
        const item = readPayloadValue<TimelineItem>(event.payload.item)
        if (item) return [item]
        return Array.isArray(event.payload.items) ? event.payload.items.filter(isTimelineItem) : []
      })
      if (items.length > 0) clearResolvedOptimisticMessagesRef.current(sessionId, items)
    }
    const renderBuffer = createSessionEventBuffer(commitEvents)
    const refetch = (reason: string) => {
      if (refetchPromise) return refetchPromise
      refetchPromise = loadInitialSessionState(token, sessionId, { reason })
        .then((next) => {
          if (cancelled) return
          snapshotHydrated = true
          setError(null)
          const merged = applyOptimisticItemsRef.current(next)
          // Events already received have advanced the cursor, even if their
          // 30 fps render batch has not committed yet. Commit them before the
          // snapshot so neither the buffer nor a newer live projection is lost.
          renderBuffer.flush()
          clearResolvedOptimisticMessagesRef.current(sessionId, merged.items)
          eventSequenceCursor.advance(
            sessionId,
            cursorSequence(next.eventCursor) || next.nextSeq,
          )
          processedEventIds = new Set()
          setState((current) => current?.session.id === sessionId
            ? mergeSessionSnapshot(current, merged)
            : merged)
          onSessionUpdatedRef.current?.(next.session)
        })
        .catch((error) => {
          if (!cancelled && isAuthFailure(error)) signOutRef.current?.()
        })
        .finally(() => {
          refetchPromise = null
        })
      return refetchPromise
    }

    const applyEvent = (
      event: ProtocolEventEnvelope,
      options: { detectGap?: boolean; recovered?: boolean } = {},
    ) => {
      if (cancelled || event.sessionId !== sessionId) return
      if (event.type === "keepalive") return
      const previousSequence = eventSequenceCursor.current(sessionId)
      const usesDurableEventIdDedup = sessionEventUsesDurableEventIdDedup(event)
      if (!eventSequenceCursor.accepts(sessionId, event.sequence)) return
      if (!acceptSessionEventId(event, processedEventIds)) return
      if (usesDurableEventIdDedup) {
        if (processedEventIds.size > 1000) {
          processedEventIds = new Set(Array.from(processedEventIds).slice(-500))
        }
      }
      if (event.type === "session.refetch_required") {
        void recoverEvents(
          eventSequenceCursor.current(sessionId),
          "session.refetch_required",
        )
        return
      }
      // Revisions are allocated per write, so a frame can legitimately skip
      // numbers. A jump of more than one still means some revision's frames
      // were lost or arrived out of order; advancing past them would drop them
      // for good. Recover from the cursor before this frame. A timeline
      // snapshot already replaces everything, so it needs no recovery.
      const sequenceGap = options.detectGap !== false
        && event.type !== "timeline.snapshot"
        && event.sequence > previousSequence + 1
      if (!sessionEventCanUpdateState(event)) {
        eventSequenceCursor.advance(sessionId, event.sequence)
        if (sequenceGap) {
          void recoverEvents(previousSequence, "sequence-gap", { allowCursorBehind: true })
        }
        return
      }
      if (options.recovered) {
        // A recovered frame can sit below state.nextSeq: that watermark already
        // moved past the lost revision when a later live frame arrived first.
        // The sequence guard exists for late duplicate frames, so bypass it
        // here and let the per-item revision checks decide.
        commitEvents([event], { allowStaleSequence: true })
      } else {
        renderBuffer.push(event)
      }
      eventSequenceCursor.advance(sessionId, event.sequence)
      if (sequenceGap) {
        void recoverEvents(previousSequence, "sequence-gap", { allowCursorBehind: true })
      }
    }

    const recoverEvents = async (
      afterSeq: number,
      reason: string,
      options: { allowCursorBehind?: boolean } = {},
    ) => {
      if (recoveryPromise || recoveryStarting) return recoveryPromise
      recoveryStarting = true
      // Events received before this recovery request causally precede its
      // response. Apply them first so the recovered live projection wins at
      // the same sequence; events received during the request remain queued.
      // recoveryStarting also makes a buffered refetch_required join this
      // recovery instead of recursively starting another one.
      drainBufferedEvents({ allowWhileRecoveryStarting: true })
      const recoveryAfterSeq = options.allowCursorBehind
        ? afterSeq
        : Math.max(afterSeq, eventSequenceCursor.current(sessionId))
      try {
        const recoveryRequest = (async () => {
            let recoveryCursor = recoveryAfterSeq
            let recoveryEvents: ProtocolEventEnvelope[] = []
            while (true) {
              const page = await dashboardApi.getSessionEvents(token, sessionId, `seq:${recoveryCursor}`)
              const nextCursor = cursorSequence(page.nextCursor)
              if (page.snapshotRequired && nextCursor <= recoveryCursor) {
                await refetch(`${reason}:snapshot-required`)
                return
              }
              recoveryEvents = recoveryEvents.concat(page.events)
              recoveryCursor = nextCursor
              if (!page.snapshotRequired) break
            }
            if (cancelled) return
            const recoveredSession = recoveryEvents.reduce<SessionView | null>(
              (latest, event) => {
                if (event.type !== "session.meta.updated") return latest
                return readPayloadValue<SessionView>(event.payload.session) ?? latest
              },
              null,
            )
            // /events only replays timeline items, so it rarely carries the
            // session meta this reads. Fall back to the snapshot-hydrated
            // session; otherwise the live capability read below never runs.
            const liveReadSession = recoveredSession ?? stateRef.current?.session ?? null
            const shouldReadLiveCapabilities = liveReadSession?.connectorStatus === "online"
            // Rewind to the recovery cursor so events between it and the last
            // live frame are accepted instead of rejected as stale. Live
            // events are buffered while recovery runs, so nothing else
            // advances the cursor in this window.
            eventSequenceCursor.replaceFromSnapshot(sessionId, recoveryAfterSeq)
            for (const event of recoveryEvents) {
              if (event.type === "runtime.capability.updated" && shouldReadLiveCapabilities) {
                continue
              }
              applyEvent(event, { detectGap: false, recovered: true })
            }
            eventSequenceCursor.advance(sessionId, recoveryCursor)
            // Runtime state and pending notices are not recovered by /events.
            // Read them even when offline so the Server can return its last
            // cached/persisted fact. Keep events received during these reads
            // queued until all settle.
            const bufferedBeforeLiveRead = bufferedEvents
            bufferedEvents = []
            const liveReadConnection = connectionSequence
            const [stateResult, capabilitiesResult, noticesResult, activityResult] = await Promise.allSettled([
              dashboardApi.getSessionRuntimeState(token, sessionId),
              shouldReadLiveCapabilities
                ? dashboardApi.getSessionRuntimeCapabilities(token, sessionId)
                : Promise.resolve(null),
              dashboardApi.getSessionNotices(token, sessionId),
              // Activity frames are live-only, so a recovery gap can lose them;
              // the registry has the current set.
              shouldReadLiveCapabilities
                ? dashboardApi.getSessionActivity(token, sessionId)
                : Promise.resolve(null),
            ])
            if (cancelled) return
            const liveReadIsCurrent = liveReadConnection === connectionSequence
            const liveState = liveReadIsCurrent && stateResult.status === "fulfilled"
              ? stateResult.value
              : null
            const liveCapabilities = liveReadIsCurrent && capabilitiesResult.status === "fulfilled"
              ? capabilitiesResult.value
              : null
            const liveNotices = liveReadIsCurrent && noticesResult.status === "fulfilled"
              ? {
                  notices: serverNotices(noticesResult.value.notices, noticesResult.value.serverTime),
                  serverTime: noticesResult.value.serverTime,
                }
              : null
            const liveAgents = liveReadIsCurrent && activityResult.status === "fulfilled" && activityResult.value
              ? readAgentRuns(activityResult.value.agents)
              : null
            bufferedEvents = bufferedEventsAfterLiveCapabilityRead(
              liveState
                ? bufferedBeforeLiveRead.filter((event) => event.type !== "runtime.state.updated")
                : bufferedBeforeLiveRead,
              bufferedEvents,
              liveCapabilities !== null,
            )
            renderBuffer.flush()
            setState((current) => {
              if (!current || current.session.id !== sessionId) return current
              // Include facts received before the read in its version check.
              // Other buffered domains (including refetch signals) keep their
              // existing ordering and are drained after recovery is released.
              const stateBeforeRead = bufferedBeforeLiveRead.reduce(
                (latest, event) => event.type === "runtime.state.updated" && event.sessionId === sessionId
                  ? mergeRuntimeState(
                      latest,
                      readPayloadValue<SessionRuntimeState>(event.payload.state),
                      sessionId,
                    )
                  : latest,
                current.state,
              )
              const nextRuntimeState = mergeRuntimeState(stateBeforeRead, liveState?.state, sessionId)
              const nextCapabilities = mergeEffectiveCapabilities(
                current.effectiveCapabilities,
                liveCapabilities?.capabilitySet ?? null,
              )
              const nextNotices = liveNotices
                ? replaceNotices(current.notices, liveNotices.notices, resolvingNoticeId)
                : current.notices
              const nextAgents = liveAgents !== null && !agentRunsEqual(current.agents, liveAgents)
                ? liveAgents
                : current.agents
              const nextAgentsLoaded = liveAgents !== null ? true : current.agentsLoaded
              return nextRuntimeState === current.state &&
                nextCapabilities === current.effectiveCapabilities &&
                nextNotices === current.notices &&
                nextAgents === current.agents &&
                nextAgentsLoaded === current.agentsLoaded
                ? current
                : {
                    ...current,
                    state: nextRuntimeState,
                    effectiveCapabilities: nextCapabilities,
                    notices: nextNotices,
                    agents: nextAgents,
                    agentsLoaded: nextAgentsLoaded,
                    serverTime: liveState?.serverTime
                      ?? liveCapabilities?.serverTime
                      ?? liveNotices?.serverTime
                      ?? current.serverTime,
                  }
            })
          })()
          .catch(async (error) => {
            if (isAuthFailure(error)) {
              signOutRef.current?.()
              return
            }
            await refetch(`${reason}:recovery-failed`)
          })
        recoveryPromise = settleSessionEventRecovery(
          recoveryRequest,
          () => {
            recoveryPromise = null
            recoveryStarting = false
          },
          drainBufferedEvents,
        )
        return recoveryPromise
      } catch {
        return undefined
      } finally {
        // Synchronous setup failures happen before settleSessionEventRecovery
        // owns cleanup.
        if (!recoveryPromise) {
          recoveryStarting = false
          drainBufferedEvents()
        }
      }
    }

    function drainBufferedEvents(
      options: { allowWhileRecoveryStarting?: boolean } = {},
    ) {
      const pending = bufferedEvents
      bufferedEvents = []
      const remaining = drainSessionEventBuffer(
        pending,
        applyEvent,
        () => Boolean(
          recoveryPromise || (
            recoveryStarting && !options.allowWhileRecoveryStarting
          ),
        ),
      )
      if (remaining.length > 0) {
        bufferedEvents = [...remaining, ...bufferedEvents]
      }
    }

    const recoverAfterSubscription = async (reason: string, connection: number) => {
      const pendingRecovery = recoveryPromise
      if (pendingRecovery) await pendingRecovery
      if (cancelled || !snapshotReady || !socketSubscribed) return
      if (!snapshotHydrated) {
        // The first snapshot failed: a subscription is the earliest safe point
        // to retry hydration, otherwise recovered events have no state to merge.
        await refetch(`${reason}:hydrate`)
        if (cancelled || !socketSubscribed || connection !== connectionSequence || !snapshotHydrated) return
      }
      await recoverEvents(eventSequenceCursor.current(sessionId), reason)
      if (!cancelled && socketSubscribed && connection === connectionSequence) recoveredSubscriptions.recovered(connection)
    }

    const handleEvent = (event: ProtocolEventEnvelope, connection: number) => {
      if (event.type === "session.subscribed") {
        // The Server registers the broker subscription before emitting this
        // event. Recover only after that point so a same-sequence live
        // projection cannot fall between recovery and socket registration.
        socketSubscribed = true
        if (snapshotReady) {
          void recoverAfterSubscription("websocket.subscribed", connection)
        }
        return
      }
      if (!snapshotReady || recoveryPromise || recoveryStarting) {
        bufferedEvents.push(event)
        return
      }
      applyEvent(event)
    }

    let unsubscribeShared: (() => void) | null = null

    const connectSharedStream = () => {
      unsubscribeShared?.()
      unsubscribeShared = null
      unsubscribeShared = subscribeSessionStreamRef.current(sessionId, {
        onEvent: (event) => {
          if (cancelled) return
          handleEvent(event, connectionSequence)
        },
        onReady: () => {
          if (cancelled) return
          // The shared stream has no per-session subscribe ack; becoming ready
          // (including after a reconnect) is the safe point to recover events.
          const connection = ++connectionSequence
          recoveredSubscriptions.observed(connection)
          socketSubscribed = true
          streamConnectedRef.current = true
          void recoverAfterSubscription("websocket.shared-stream", connection)
        },
        onUnavailable: () => {
          // The shared stream was abandoned (it never opened, or this attempt
          // replaced a previous "unified" verdict). Continue on the legacy
          // per-session socket so live events keep flowing.
          if (cancelled) return
          unsubscribeShared?.()
          unsubscribeShared = null
          socketSubscribed = false
          streamConnectedRef.current = false
          void connectSessionSocket()
        },
      })
    }

    const connectSessionSocket = async () => {
      const connection = ++connectionSequence
      recoveredSubscriptions.observed(connection)
      try {
        const ticket = await dashboardApi.createWsTicket(token, createClientId("web"), sessionId)
        if (cancelled) return
        socketSubscribed = false
        socket = new WebSocket(dashboardApi.sessionWebSocketUrl(sessionId, ticket.ticket))
        socket.onopen = () => {
          if (!cancelled) streamConnectedRef.current = true
        }
        socket.onmessage = (message) => {
          if (cancelled || typeof message.data !== "string") return
          const event = parseProtocolEventData(message.data)
          if (!event) return
          handleEvent(event, connection)
        }
        socket.onclose = () => {
          if (cancelled) return
          socketSubscribed = false
          streamConnectedRef.current = false
          reconnectTimer = window.setTimeout(() => {
            reconnectTimer = null
            void connect()
          }, 1200)
        }
      } catch (error) {
        if (cancelled) return
        if (isAuthFailure(error)) {
          signOutRef.current?.()
          return
        }
        reconnectTimer = window.setTimeout(() => {
          reconnectTimer = null
          void connect()
        }, 2000)
      }
    }

    const connect = async () => {
      // The shared stream is usable for this page lifetime or not; the answer
      // never changes after the first resolution.
      const mode = await whenRealtimeStreamResolvedRef.current()
      if (cancelled) return
      if (mode === "unified") {
        connectSharedStream()
        return
      }
      void connectSessionSocket()
    }

    void connect()

    const loadInitial = (reason: string) => {
      loadInitialSessionState(token, sessionId, { reason })
        .then((next) => {
          if (cancelled) return
          snapshotHydrated = true
          setError(null)
          const merged = applyOptimisticItemsRef.current(next)
          clearResolvedOptimisticMessagesRef.current(sessionId, merged.items)
          setState((current) => current ? mergeSessionSnapshot(current, merged) : merged)
          eventSequenceCursor.advance(
            sessionId,
            cursorSequence(next.eventCursor) || next.nextSeq,
          )
          onSessionUpdatedRef.current?.(next.session)
          snapshotReady = true
          if (socketSubscribed) {
            void recoverAfterSubscription("websocket.initial-subscription", connectionSequence)
          } else {
            drainBufferedEvents()
          }
        })
        .catch((err) => {
          if (cancelled) return
          if (isAuthFailure(err)) {
            signOutRef.current?.()
            return
          }
          initialLoadAttempt += 1
          if (initialLoadAttempt < INITIAL_LOAD_MAX_ATTEMPTS) {
            const delay = Math.min(1000 * 2 ** (initialLoadAttempt - 1), 8000)
            initialRetryTimer = window.setTimeout(() => {
              initialRetryTimer = null
              loadInitial(`${reason}:retry`)
            }, delay)
            return
          }
          snapshotReady = true
          setError(err instanceof Error ? err.message : tSessionRef.current("loadFailed"))
          setLoading(false)
          if (socketSubscribed) {
            void recoverAfterSubscription("websocket.initial-subscription", connectionSequence)
          } else {
            drainBufferedEvents()
          }
        })
    }

    loadInitial("session-detail.initial-load")

    return () => {
      cancelled = true
      renderBuffer.dispose()
      bufferedEvents = []
      streamConnectedRef.current = false
      unsubscribeShared?.()
      if (reconnectTimer !== null) window.clearTimeout(reconnectTimer)
      if (initialRetryTimer !== null) window.clearTimeout(initialRetryTimer)
      socket?.close()
    }
  }, [
    isLocalOptimisticSession,
    sessionId,
    token,
  ])

  // Offered both from the header and from a send that found the conversation
  // not live on the desktop.
  const openSessionOnComputer = React.useCallback(async (targetSessionId: string) => {
    if (!token || !targetSessionId) return
    try {
      const response = await dashboardApi.openSessionRuntime(token, targetSessionId)
      if (response.result?.sent) toast.success(tSession("openOnComputerSent"))
      else toast.error(tSession("openOnComputerFailed"))
    } catch (err) {
      toast.error(err instanceof Error ? err.message : tSession("openOnComputerFailed"))
    }
  }, [tSession, token])

  const handleSend = async (
    content: string,
    attachments: AttachedFile[],
    selections: { model?: string; permission?: string },
    route: "message" | "steer",
  ): Promise<boolean> => {
    if (!session || (!content.trim() && attachments.length === 0)) return false
    const sendSessionId = session.id
    const uploadedAttachments = attachments.flatMap((attachment) =>
      attachment.uploaded ? [attachment.uploaded] : [],
    )
    if (uploadedAttachments.length !== attachments.length) return false
    const clientMessageId = createClientId("msg")
    const messageText = content.trim() || tNew("attachmentOnlyPrompt")
    let batch = sendBatchesRef.current.get(sendSessionId)
    if (!batch) {
      batch = {
        pending: 0,
        accepted: 0,
        previousRuntimeState: state?.state ?? null,
      }
      sendBatchesRef.current.set(sendSessionId, batch)
    }
    batch.pending += 1
    if (activeSessionIdRef.current === sendSessionId) setSending(true)
    timelineFollowRef.current?.resume()
    const optimisticMessage = buildOptimisticUserMessage({
      sessionId: sendSessionId,
      clientMessageId,
      text: messageText,
      attachments,
      items: state?.items ?? [],
      nextSeq: state?.nextSeq ?? eventSequenceCursor.current(sendSessionId),
    })
    addOptimisticMessage({
      clientMessageId,
      sessionId: sendSessionId,
      item: optimisticMessage,
    })
    setState((current) => {
      if (!current || current.session.id !== sendSessionId) return current
      return {
        ...current,
        state: nextOptimisticRuntimeState(
          current.state,
          current.session,
          effectiveRuntimeStatus(current.state, current.session) === "running" ? "running" : "waiting",
        ),
        items: mergeTimelineItems(current.items, [optimisticMessage]),
      }
    })
    try {
      const selectionPatch = selectionPatchFromComposerSelections(runtimeState?.selections ?? {}, selections)
      if (Object.keys(selectionPatch).length > 0) {
        const selectionResult = await dashboardApi.updateSessionSelections(token, sendSessionId, selectionPatch)
        setState((current) =>
          current && current.session.id === sendSessionId
            ? {
                ...current,
                state: runtimeStateWithSelectionResult(
                  current.state,
                  current.session,
                  selectionPatch,
                  selectionResult.state,
                ),
              }
            : current,
        )
      }
      const messageOptions = {
        attachments: uploadedAttachments.map((attachment) => ({ fileId: attachment.fileId })),
        clientMessageId,
      }
      if (route === "steer") {
        await dashboardApi.steerSessionMessage(token, sendSessionId, messageText, messageOptions)
      } else {
        await dashboardApi.sendSessionMessage(token, sendSessionId, messageText, messageOptions)
      }
      batch.accepted += 1
      return true
    } catch (err) {
      const nextSourceErrorCode = sessionSourceErrorCode(err)
      const message = nextSourceErrorCode
        ? sourceErrorMessage(nextSourceErrorCode, tSession, runtimeLabel(sessionRuntimeType(session)))
        : err instanceof Error
          ? err.message
          : tSession("sendFailed")
      markOptimisticMessageFailed(clientMessageId, message)
      setState((current) => {
        if (!current || current.session.id !== sendSessionId) return current
        return {
          ...current,
          items: current.items.map((item) =>
            timelineClientMessageId(item) === clientMessageId && isOptimisticTimelineItem(item)
              ? markOptimisticItemFailed(item, message)
              : item,
          ),
        }
      })
      if (nextSourceErrorCode) {
        const sourceAvailability = sourceAvailabilityFromError(nextSourceErrorCode)
        const nextSession: SessionView = {
          ...session,
          sourceAvailability,
          sourceAvailabilityReason: message,
          sourceAvailabilityUpdatedAt: new Date().toISOString(),
          sourceObservationOrigin: "operation",
        }
        setState((current) =>
          current && current.session.id === sendSessionId ? { ...current, session: nextSession } : current,
        )
        onSessionUpdated?.(nextSession)
        setSourceErrorCode(nextSourceErrorCode)
      } else if (isSessionNotLiveError(err)) {
        toast.error(err instanceof Error ? err.message : tSession("sendFailed"), {
          action: {
            label: tSession("openOnComputer"),
            onClick: () => { void openSessionOnComputer(sendSessionId) },
          },
        })
      } else {
        toast.error(err instanceof Error ? err.message : tSession("sendFailed"))
      }
      return false
    } finally {
      batch.pending -= 1
      if (batch.pending === 0) {
        if (batch.accepted === 0) {
          setState((current) => {
            if (!current || current.session.id !== sendSessionId || current.state?.status !== "waiting") return current
            return { ...current, state: batch.previousRuntimeState }
          })
        }
        if (sendBatchesRef.current.get(sendSessionId) === batch) sendBatchesRef.current.delete(sendSessionId)
      }
      if (activeSessionIdRef.current === sendSessionId) setSending(batch.pending > 0)
    }
  }

  const handleInterrupt = async () => {
    if (!session || interrupting) return
    setInterrupting(true)
    try {
      await dashboardApi.interruptSession(token, session.id)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : tSession("interruptFailed"))
    } finally {
      setInterrupting(false)
    }
  }

  const handleSessionCommand = async (
    command: string,
    options: { args: string[]; raw: string },
  ): Promise<CommandOutcome> => {
    const visit = commandVisitRef.current
    if (!session || session.id !== visit.sessionId) {
      return { ok: false, state: "completed", code: "session_unavailable", message: tSession("commandUnavailable"), result: null }
    }
    const request = ++commandRequestSeqRef.current
    activeCommandRequestRef.current = request
    const isCurrentRequest = () => commandVisitRef.current === visit && activeCommandRequestRef.current === request
    try {
      const response = await dashboardApi.sendSessionCommand(token, visit.sessionId, command, options)
      if (isCurrentRequest() && response.session?.id === visit.sessionId) {
        setState((current) => current?.session.id === visit.sessionId ? { ...current, session: response.session! } : current)
        onSessionUpdated?.(response.session)
      }
      return commandResult(response)
    } catch (err) {
      return commandTransportFailure(err, tSession("commandFailed"))
    } finally {
      if (isCurrentRequest()) activeCommandRequestRef.current = null
    }
  }

  const removeNoticeFromState = React.useCallback((noticeId: string) => {
    setState((current) => {
      if (!current || !current.notices.some((notice) => notice.noticeId === noticeId)) {
        return current
      }
      return {
        ...current,
        notices: current.notices.filter((notice) => notice.noticeId !== noticeId),
      }
    })
  }, [])

  const handleRespondInteraction = React.useCallback(async (
    noticeId: string,
    actionId: string,
    input?: Record<string, unknown>,
  ) => {
    if (!session || resolvingNoticeId) return
    setResolvingNoticeId(noticeId)
    setResolvingActionId(actionId)
    try {
      const response = await dashboardApi.respondInteraction(token, session.id, noticeId, actionId, input)
      if (response.ok) {
        removeNoticeFromState(noticeId)
        return
      }
      if (rpcErrorRemovesNotice(response.error?.code)) {
        removeNoticeFromState(noticeId)
      }
      toast.error(response.error?.message || tSession("resolveInteractionFailed"))
    } catch (err) {
      toast.error(err instanceof Error ? err.message : tSession("resolveInteractionFailed"))
    } finally {
      setResolvingNoticeId(null)
      setResolvingActionId(null)
    }
  }, [removeNoticeFromState, resolvingNoticeId, session, tSession, token])

  React.useLayoutEffect(() => {
    const pendingPrependScrollRestore = pendingPrependScrollRestoreRef.current
    if (pendingPrependScrollRestore) {
      pendingPrependScrollRestoreRef.current = null
      const viewport = timelineRef.current
      if (viewport) {
        viewport.scrollTop =
          viewport.scrollHeight - pendingPrependScrollRestore.scrollHeight + pendingPrependScrollRestore.scrollTop
      }
      updateScrollBottomState()
      return
    }
    timelineFollowRef.current?.schedule()
    updateScrollBottomState()
  }, [runtimeStatus, state?.items.length, state?.notices.length, updateScrollBottomState])

  React.useLayoutEffect(() => {
    if (initialScrollDoneRef.current || !hasInitialSessionState) return

    let cancelled = false
    let resizeObserver: ResizeObserver | null = null

    const clearInitialScrollTimers = () => {
      if (initialScrollQuietTimerRef.current !== null) {
        window.clearTimeout(initialScrollQuietTimerRef.current)
        initialScrollQuietTimerRef.current = null
      }
      if (initialScrollFallbackTimerRef.current !== null) {
        window.clearTimeout(initialScrollFallbackTimerRef.current)
        initialScrollFallbackTimerRef.current = null
      }
    }

    // Finish the first-screen loading phase without overriding a user's scroll.
    const completeInitialLayout = () => {
      if (cancelled || initialScrollDoneRef.current) return
      const viewport = timelineRef.current
      if (!viewport) return

      initialScrollDoneRef.current = true
      clearInitialScrollTimers()
      resizeObserver?.disconnect()
      resizeObserver = null
      setLoading(false)
      timelineFollowRef.current?.scrollNow()
      updateScrollBottomState()
    }

    const scheduleCompleteAfterQuietLayout = () => {
      if (cancelled || initialScrollDoneRef.current) return
      if (initialScrollQuietTimerRef.current !== null) {
        window.clearTimeout(initialScrollQuietTimerRef.current)
      }
      initialScrollQuietTimerRef.current = window.setTimeout(() => {
        initialScrollQuietTimerRef.current = null
        completeInitialLayout()
      }, INITIAL_SCROLL_LAYOUT_QUIET_MS)
    }

    const viewport = timelineRef.current
    const content = timelineContentRef.current
    if (viewport) {
      timelineFollowRef.current?.scrollNow()
      updateScrollBottomState()
    }
    if (content) {
      resizeObserver = new ResizeObserver(() => {
        timelineFollowRef.current?.schedule()
        scheduleCompleteAfterQuietLayout()
      })
      resizeObserver.observe(content)
    }
    scheduleCompleteAfterQuietLayout()
    initialScrollFallbackTimerRef.current = window.setTimeout(
      completeInitialLayout,
      INITIAL_SCROLL_LAYOUT_FALLBACK_MS,
    )

    return () => {
      cancelled = true
      resizeObserver?.disconnect()
      clearInitialScrollTimers()
    }
  }, [hasInitialSessionState, sessionId, updateScrollBottomState])

  const scrollToBottom = React.useCallback(() => {
    pruneAfterScrollCleanupRef.current?.()
    const viewport = timelineRef.current
    const shouldPrune = (state?.items.length ?? 0) > INITIAL_TIMELINE_LIMIT
    if (!viewport) {
      if (shouldPrune) {
        setState((current) =>
          current && current.items.length > INITIAL_TIMELINE_LIMIT
            ? { ...current, items: current.items.slice(-INITIAL_TIMELINE_LIMIT), hasMore: true }
            : current,
        )
      }
      return
    }

    if (pruneAfterScrollTimerRef.current !== null) {
      window.clearTimeout(pruneAfterScrollTimerRef.current)
      pruneAfterScrollTimerRef.current = null
    }

    let settled = false
    const pruneIfAtBottom = () => {
      if (!timelineFollowRef.current?.isFollowing() || distanceFromBottom() > SCROLL_TO_BOTTOM_PRUNE_DISTANCE) return false
      setState((current) =>
        current && current.items.length > INITIAL_TIMELINE_LIMIT
          ? { ...current, items: current.items.slice(-INITIAL_TIMELINE_LIMIT), hasMore: true }
          : current,
      )
      return true
    }
    const cleanup = () => {
      viewport.removeEventListener("scrollend", handleScrollEnd)
      if (pruneAfterScrollTimerRef.current !== null) {
        window.clearTimeout(pruneAfterScrollTimerRef.current)
        pruneAfterScrollTimerRef.current = null
      }
      pruneAfterScrollCleanupRef.current = null
    }
    const finish = () => {
      if (settled) return
      if (shouldPrune && !pruneIfAtBottom()) return
      settled = true
      cleanup()
      if (!shouldPrune) updateScrollBottomState()
    }
    const handleScrollEnd = () => {
      if (settled) return
      settled = true
      cleanup()
      if (shouldPrune && !pruneIfAtBottom()) {
        updateScrollBottomState()
      }
    }
    const scheduleCheck = () => {
      if (settled) return
      pruneAfterScrollTimerRef.current = window.setTimeout(() => {
        pruneAfterScrollTimerRef.current = null
        finish()
        if (!settled) scheduleCheck()
      }, SCROLL_TO_BOTTOM_PRUNE_CHECK_MS)
    }

    if (shouldPrune) {
      pruneAfterScrollCleanupRef.current = () => {
        settled = true
        cleanup()
      }
      viewport.addEventListener("scrollend", handleScrollEnd, { once: true })
      scheduleCheck()
    }
    timelineFollowRef.current?.resume("smooth")
  }, [distanceFromBottom, state?.items.length, updateScrollBottomState])

  const interactions = React.useMemo(
    () => openInteractions(state?.notices ?? [], session?.id ?? sessionId),
    [session?.id, sessionId, state?.notices],
  )
  const blockingInteractionList = React.useMemo(
    () => blockingInteractions(state?.notices ?? [], session?.id ?? sessionId),
    [session?.id, sessionId, state?.notices],
  )
  const timelineInteractions = React.useMemo(
    () => interactions.filter((notice) => !isSessionBlockingInteraction(notice, session?.id ?? sessionId)),
    [interactions, session?.id, sessionId],
  )
  const interactionByTarget = React.useMemo(
    () => new Map(timelineInteractions.map((notice) => [noticeTimelineTargetId(notice), notice])),
    [timelineInteractions],
  )
  const detachedInteractions = timelineInteractions.filter((notice) => !noticeTimelineTargetId(notice))
  const detachedNotifications = React.useMemo(
    () => openNotifications(state?.notices ?? []),
    [state?.notices],
  )
  const blockingInteractionCount = blockingInteractionList.length
  const timelineBottomPadding = `calc(${composerHeight}px + ${blockingInteractionStackHeight}px + 2rem)`
  const scrollBottomButtonOffset = `calc(${composerHeight}px + ${blockingInteractionStackHeight}px + 0.5rem)`
  const interactionTargetIds = React.useMemo(
    () => new Set(timelineInteractions.map(noticeTimelineTargetId).filter((id): id is string => Boolean(id))),
    [timelineInteractions],
  )

  React.useEffect(() => {
    if (blockingInteractionCount === 0 && blockingInteractionStackHeight !== 0) {
      setBlockingInteractionStackHeight(0)
    }
  }, [blockingInteractionCount, blockingInteractionStackHeight])

  React.useLayoutEffect(() => {
    const node = composerContainerRef.current
    if (!node) return
    const publishHeight = () => setComposerHeight(Math.ceil(node.getBoundingClientRect().height))
    publishHeight()
    const resizeObserver = new ResizeObserver(publishHeight)
    resizeObserver.observe(node)
    return () => resizeObserver.disconnect()
  }, [session?.id])
  const timelineGroups = React.useMemo(
    () => groupTimelineItems((state?.items ?? []).filter(isVisibleTimelineItem), interactionTargetIds),
    [interactionTargetIds, state?.items],
  )
  const selectTurnReviewDisplay = React.useMemo(() => createTurnReviewDisplaySelector(), [sessionId])
  const turnReviewDisplay = React.useMemo(() => {
    return selectTurnReviewDisplay(state?.session.id === sessionId ? state.items.filter(isVisibleTimelineItem) : [], {
      root: state?.session.cwd,
      caseInsensitivePaths: connectorDeviceOs === "windows",
      turnInProgress,
    })
  }, [connectorDeviceOs, selectTurnReviewDisplay, sessionId, state?.items, state?.session.id, state?.session.cwd, turnInProgress])
  const completedTurnReviewsByEndItemId = React.useMemo(
    () => new Map(turnReviewDisplay.completedTurns.map((turn) => [turn.endItemId, turn])),
    [turnReviewDisplay.completedTurns],
  )
  const turnActionsByGroupKey = React.useMemo(
    () => buildTurnActionsByGroupKey(
      timelineGroups,
      turnInProgress,
    ),
    [timelineGroups, turnInProgress],
  )

  if (loading && !session) return <SessionSkeleton />

  if (error && !session) {
    return (
      <div className="mx-auto flex h-full max-w-3xl items-center justify-center px-6">
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>{tSession("unavailable")}</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      </div>
    )
  }

  if (!session) return null

  const sourceErrorDialog = sourceErrorCode
    ? sourceErrorDialogContent(sourceErrorCode, tSession, runtimeLabel(sessionRuntimeType(session)))
    : null
  const dismissSourceErrorDialog = () => {
    const shouldLeaveSession = sourceErrorCode === "session_archived"
    setSourceErrorCode(null)
    if (shouldLeaveSession) replaceHome()
  }
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden overscroll-none">
      {error ? (
        <Alert variant="destructive" className="mx-auto mt-4 w-[calc(100%-2rem)] max-w-3xl">
          <CircleAlert />
          <AlertTitle>{tSession("refreshFailed")}</AlertTitle>
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}

      <div className="relative min-h-0 flex-1 overflow-hidden">
        <ScrollArea
          viewportRef={timelineRef}
          className="h-full"
          viewportProps={{ onScroll: handleTimelineScroll }}
        >
          <div
            ref={timelineContentRef}
            aria-busy={runtimeStatus === "waiting" || runtimeStatus === "pending" || runtimeStatus === "running"}
            className={cn(
              "mx-auto flex w-full min-w-0 max-w-[calc(48rem+2rem)] flex-col gap-3 overflow-hidden px-4 pb-44 pt-20",
            )}
            style={{ paddingBottom: timelineBottomPadding }}
          >
            {loadingOlder ? (
              <div className="flex justify-center py-2 text-muted-foreground">
                <Loader2 className="size-4 animate-spin" />
              </div>
            ) : null}
            {loading && !state ? <SessionSkeletonInline /> : null}
            {state &&
            timelineGroups.length === 0 &&
            detachedInteractions.length === 0 &&
            detachedNotifications.length === 0 &&
            blockingInteractionList.length === 0 ? (
              <p className="py-12 text-center text-sm text-muted-foreground">{tSession("noActivity")}</p>
            ) : null}
            {timelineGroups.map((group) => {
              const groupKey = timelineGroupKey(group)
              const turnAction = turnActionsByGroupKey.get(groupKey)
              const completedTurnReview = timelineGroupItems(group)
                .map((item) => completedTurnReviewsByEndItemId.get(item.id))
                .find((turn) => turn !== undefined)
              return (
                <React.Fragment key={groupKey}>
                  <LiveTimelineGroupEntry
                    group={group}
                    token={token}
                    session={session}
                    interactionByTarget={interactionByTarget}
                    resolvingNoticeId={resolvingNoticeId}
                    resolvingActionId={resolvingActionId}
                    groupOpen={group.kind === "single" ? false : timelineGroupOpenByKey[group.key] ?? false}
                    itemOpenById={timelineItemOpenById}
                    onGroupOpenChange={handleTimelineGroupOpenChange}
                    onItemOpenChange={handleTimelineItemOpenChange}
                    onRespondInteraction={handleRespondInteraction}
                  />
                  {completedTurnReview && onOpenReview ? (
                    <SessionReviewCard
                      key={completedTurnReview.review.key}
                      review={completedTurnReview.review}
                      onReview={() => onOpenReview({
                        orderSeq: completedTurnReview.endOrderSeq,
                        resetVersion: state?.timelineResetVersion ?? 0,
                      })}
                    />
                  ) : null}
                  {turnAction ? (
                    <TurnActions
                      token={token}
                      sessionId={session.id}
                      action={turnAction}
                    />
                  ) : null}
                </React.Fragment>
              )
            })}
            {state && state.agents.length > 0 ? (
              <SubagentActivityBlock agents={state.agents} />
            ) : null}
            {detachedInteractions.map((notice) => (
              <InteractionCard
                key={notice.noticeId}
                notice={notice}
                resolvingNoticeId={resolvingNoticeId}
                resolvingActionId={resolvingActionId}
                onRespondInteraction={handleRespondInteraction}
              />
            ))}
            {detachedNotifications.map((notice) => (
              <NotificationCard key={notice.noticeId} notice={notice} />
            ))}
            {runtimeStatus === "waiting" || runtimeStatus === "pending" || runtimeStatus === "running" ? (
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 className="size-4 animate-spin" />
                <span>
                  {runtimeStatus === "waiting" || runtimeStatus === "pending"
                    ? tSession("runtimePending", { runtime: session.runtimeName?.trim() || runtimeLabel(sessionRuntimeType(session)) })
                    : tSession("runtimeWorking", { runtime: session.runtimeName?.trim() || runtimeLabel(sessionRuntimeType(session)) })}
                </span>
              </div>
            ) : null}
          </div>
        </ScrollArea>
        {showScrollBottom ? (
          <Button
            type="button"
            size="sm"
            variant="secondary"
            className="absolute left-1/2 z-30 h-8 -translate-x-1/2 gap-1.5 rounded-full border bg-background/95 px-3 shadow-lg backdrop-blur"
            style={{ bottom: scrollBottomButtonOffset }}
            onClick={scrollToBottom}
          >
            <ArrowDown data-icon="inline-start" />
            {tSession("bottom")}
          </Button>
        ) : null}
      </div>

      <div className="pointer-events-none absolute inset-x-0 bottom-0 z-10">
        <BlockingInteractionStack
          notices={blockingInteractionList}
          resolvingNoticeId={resolvingNoticeId}
          resolvingActionId={resolvingActionId}
          onHeightChange={setBlockingInteractionStackHeight}
          onRespondInteraction={handleRespondInteraction}
        />
        <div ref={composerContainerRef} className="pointer-events-auto relative">
          {onOpenReview && turnReviewDisplay.activeReview ? (
            <SessionReviewTag files={turnReviewDisplay.activeReview.files} onReview={() => onOpenReview()} />
          ) : null}
          <SessionComposer
            token={token}
            session={session}
            runtimeState={runtimeState}
            pendingInteractionCount={blockingInteractionCount}
            creatingSession={isLocalOptimisticSession}
            sending={sending}
            interrupting={interrupting}
            value={composerDraft}
            effectiveCapabilities={state?.effectiveCapabilities ?? null}
            modelCatalog={state?.catalogs.model ?? null}
            permissionCatalog={state?.catalogs.permission ?? null}
            runtimeCommands={runtimeCommands}
            commandsLoading={commandsLoading}
            commandsError={commandsError}
            onCommandQueryChange={handleCommandQueryChange}
            onValueChange={setComposerDraft}
            onSelectionChange={handleSelectionChange}
            onSend={handleSend}
            onInterrupt={handleInterrupt}
            onCommand={handleSessionCommand}
          />
        </div>
      </div>
      <Dialog
        open={sourceErrorDialog !== null}
        onOpenChange={(open: boolean) => {
          if (!open) dismissSourceErrorDialog()
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{sourceErrorDialog?.title}</DialogTitle>
            <DialogDescription>{sourceErrorDialog?.description}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button onClick={dismissSourceErrorDialog}>{tCommon("gotIt")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

export const SessionDetail = React.memo(SessionDetailView)

// The server distinguishes "the conversation has nowhere to run" from a
// transport failure so the person is offered a terminal instead of a retry.
function isSessionNotLiveError(error: unknown): boolean {
  if (!isApiError(error)) return false
  if (error.code === "session_not_live") return true
  const detail = error.detail.toLowerCase()
  return detail.includes("no live claude session") || detail.includes("not live on the desktop")
}

function sessionSourceErrorCode(error: unknown): SessionSourceErrorCode | null {
  if (!isApiError(error)) return null
  if (
    error.code === "session_archived" ||
    error.code === "session_unavailable" ||
    error.code === "session_deleted" ||
    error.code === "session_missing"
  ) {
    return error.code
  }
  return null
}

function sourceAvailabilityFromError(
  code: SessionSourceErrorCode,
): SessionView["sourceAvailability"] {
  if (code === "session_archived") return "archived"
  if (code === "session_deleted") return "deleted"
  if (code === "session_missing") return "missing"
  return "unavailable"
}

function sourceErrorDialogContent(
  code: SessionSourceErrorCode,
  tSession: ReturnType<typeof useTranslations>,
  client: string,
): { title: string; description: string } {
  if (code === "session_archived") {
    return {
      title: tSession("sourceArchivedTitle", { client }),
      description: tSession("sourceArchivedDescription", { client }),
    }
  }
  if (code === "session_deleted") {
    return {
      title: tSession("sourceDeletedTitle", { client }),
      description: tSession("sourceDeletedDescription", { client }),
    }
  }
  if (code === "session_missing") {
    return {
      title: tSession("sourceMissingTitle", { client }),
      description: tSession("sourceMissingDescription", { client }),
    }
  }
  return {
    title: tSession("sourceUnavailableTitle", { client }),
    description: tSession("sourceUnavailableDescription", { client }),
  }
}

function sourceErrorMessage(
  code: SessionSourceErrorCode,
  tSession: ReturnType<typeof useTranslations>,
  client: string,
): string {
  return sourceErrorDialogContent(code, tSession, client).description
}

function BlockingInteractionStack({
  notices,
  resolvingNoticeId,
  resolvingActionId,
  onHeightChange,
  onRespondInteraction,
}: {
  notices: Notice[]
  resolvingNoticeId: string | null
  resolvingActionId: string | null
  onHeightChange: (height: number) => void
  onRespondInteraction: (noticeId: string, actionId: string, input?: Record<string, unknown>) => void
}) {
  const stackRef = React.useRef<HTMLDivElement | null>(null)

  React.useLayoutEffect(() => {
    const node = stackRef.current
    if (!node) return

    const publishHeight = () => {
      onHeightChange(Math.ceil(node.getBoundingClientRect().height))
    }

    publishHeight()
    const resizeObserver = new ResizeObserver(publishHeight)
    resizeObserver.observe(node)
    return () => {
      resizeObserver.disconnect()
    }
  }, [onHeightChange, notices.length])

  if (notices.length === 0) return null

  const activeNotice = notices[0]!
  const backingNotices = notices.slice(1, 4).reverse()

  return (
    <div ref={stackRef} className="pointer-events-auto mx-auto w-full max-w-[calc(48rem+2rem)] px-4 pb-1">
      <div className={cn("relative", backingNotices.length > 0 && "pt-4")}>
        {backingNotices.map((notice, index) => {
          const depth = backingNotices.length - index
          return (
            <div
              key={notice.noticeId}
              aria-hidden="true"
              className={cn(
                "pointer-events-none absolute inset-x-0 top-4 h-16 origin-top rounded-xl border bg-card shadow-sm",
                notice.severity === "error" ? "border-destructive/25" : "border-border/80",
              )}
              style={{
                transform: `translateY(-${depth * 8}px) scale(${1 - depth * 0.014})`,
                opacity: 1 - depth * 0.16,
              }}
            />
          )
        })}
        <div className="relative max-h-[38vh] overflow-y-auto rounded-xl shadow-lg shadow-background/20">
          <InteractionCard
            notice={activeNotice}
            resolvingNoticeId={resolvingNoticeId}
            resolvingActionId={resolvingActionId}
            onRespondInteraction={onRespondInteraction}
          />
        </div>
      </div>
    </div>
  )
}

export type TimelineSingleGroup = {
  kind: "single"
  item: TimelineItem
}

export type TimelineToolRunGroup = {
  kind: "tool-run"
  key: string
  items: TimelineItem[]
}

export type TimelineReconnectGroup = {
  kind: "reconnect"
  key: string
  items: TimelineItem[]
}

export type TimelineAgentCallGroup = {
  kind: "agent-calls"
  key: string
  parentItemId: string
  items: TimelineItem[]
}

export type TimelineGroup = TimelineSingleGroup | TimelineToolRunGroup | TimelineReconnectGroup | TimelineAgentCallGroup

export function timelineGroupKey(group: TimelineGroup): string {
  return group.kind === "single" ? group.item.id : group.key
}

function timelineGroupItems(group: TimelineGroup): TimelineItem[] {
  return group.kind === "single" ? [group.item] : group.items
}

function buildTurnActionsByGroupKey(
  groups: TimelineGroup[],
  suppressLatestTurn: boolean,
): Map<string, TurnAction> {
  const actions = new Map<string, TurnAction>()
  let copyParts: string[] = []
  let itemIds: string[] = []
  let endGroupKey: string | null = null
  let turnOpen = false

  const commitTurn = () => {
    if (endGroupKey && itemIds.length > 0) {
      actions.set(endGroupKey, {
        copyText: copyParts.join("\n\n").trim(),
        itemIds: [...new Set(itemIds)],
      })
    }
    copyParts = []
    itemIds = []
    endGroupKey = null
    turnOpen = false
  }

  for (const group of groups) {
    const items = timelineGroupItems(group)
    const startsTurn = items.some((item) => item.type === "message" && item.role === "user")
    if (startsTurn) {
      commitTurn()
      turnOpen = true
    }
    const replies = items.filter((item) => item.type === "message" && item.role === "assistant")
    if (replies.length > 0 && !turnOpen) turnOpen = true
    if (!turnOpen) continue
    endGroupKey = timelineGroupKey(group)
    for (const item of replies) {
      itemIds.push(item.id)
      const text = stripInjectedAttachmentMentions(messageText(item)).trim()
      if (text) copyParts.push(text)
    }
  }
  if (!suppressLatestTurn) commitTurn()
  return actions
}

export function groupTimelineItems(items: TimelineItem[], interactionTargetIds: Set<string>): TimelineGroup[] {
  const groups: TimelineGroup[] = []
  let pendingTools: TimelineItem[] = []
  let pendingReconnects: TimelineItem[] = []
  let pendingAgentCalls: TimelineItem[] = []
  let pendingAgentParentId: string | null = null

  const flushTools = () => {
    if (pendingTools.length >= 2) {
      groups.push({
        kind: "tool-run",
        key: `tool-run:${pendingTools[0]?.id ?? "unknown"}`,
        items: pendingTools,
      })
    } else {
      for (const item of pendingTools) groups.push({ kind: "single", item })
    }
    pendingTools = []
  }

  const flushReconnects = () => {
    if (pendingReconnects.length >= 2) {
      groups.push({
        kind: "reconnect",
        key: `reconnect:${pendingReconnects[0]?.id ?? "unknown"}`,
        items: pendingReconnects,
      })
    } else {
      for (const item of pendingReconnects) groups.push({ kind: "single", item })
    }
    pendingReconnects = []
  }

  const flushAgentCalls = () => {
    if (pendingAgentCalls.length >= 2 && pendingAgentParentId) {
      groups.push({
        kind: "agent-calls",
        key: `agent-calls:${pendingAgentParentId}:${pendingAgentCalls[0]?.id ?? "unknown"}`,
        parentItemId: pendingAgentParentId,
        items: pendingAgentCalls,
      })
    } else {
      for (const item of pendingAgentCalls) groups.push({ kind: "single", item })
    }
    pendingAgentCalls = []
    pendingAgentParentId = null
  }

  for (const item of items) {
    const agentParentId = nestedAgentParentId(item)
    if (agentParentId && !interactionTargetIds.has(item.id)) {
      flushReconnects()
      flushTools()
      if (pendingAgentParentId && pendingAgentParentId !== agentParentId) {
        flushAgentCalls()
      }
      pendingAgentParentId = agentParentId
      pendingAgentCalls.push(item)
      continue
    }
    flushAgentCalls()
    if (isReconnectErrorItem(item) && !interactionTargetIds.has(item.id)) {
      flushTools()
      pendingReconnects.push(item)
      continue
    }
    flushReconnects()
    if (isToolRunBarItem(item) && !interactionTargetIds.has(item.id)) {
      pendingTools.push(item)
      continue
    }
    flushTools()
    groups.push({ kind: "single", item })
  }
  flushAgentCalls()
  flushReconnects()
  flushTools()
  return groups
}

function nestedAgentParentId(item: TimelineItem): string | null {
  if (item.type !== "tool" || textOf(item.content.kind) !== "agent_call") return null
  return textOf(item.content.parentItemId)
}

function isReconnectErrorItem(item: TimelineItem): boolean {
  return item.type === "system" && item.status === "failed" && reconnectMessage(item) !== null
}

function reconnectMessage(item: TimelineItem): string | null {
  const details = recordOf(item.content.details)
  const error = recordOf(details?.error)
  const message =
    textOf(error?.message) ||
    textOf(details?.message) ||
    textOf(item.content.message) ||
    textOf(item.content.text)
  if (!message || !/^Reconnecting\.\.\./.test(message)) return null
  return message
}

function reconnectAttempt(message: string): string | null {
  return message.match(/(\d+\s*\/\s*\d+)/)?.[1]?.replace(/\s+/g, "") ?? null
}

function recordOf(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : null
}

function isToolRunBarItem(item: TimelineItem): boolean {
  if (item.type === "system" && textOf(item.content.kind) === "reasoning") return true
  if (item.type === "tool") return true
  if (item.type !== "artifact") return false
  return (item.content.kind ?? "artifact") !== "diff"
}

type LiveTimelineGroupProps = Omit<React.ComponentProps<typeof TimelineGroupEntry>, "onGroupOpenChange"> & {
  onGroupOpenChange: (key: string, open: boolean) => void
}

// Group objects are rebuilt when any item changes. Compare their immutable item
// references so a streamed tail (or composer keystroke) does not render every
// historical context menu, attachment list and message again.
const LiveTimelineGroupEntry = React.memo(function LiveTimelineGroupEntry({
  group, onGroupOpenChange, ...props
}: LiveTimelineGroupProps) {
  return <TimelineGroupEntry {...props} group={group} onGroupOpenChange={group.kind === "single"
    ? undefined : (open) => onGroupOpenChange(group.key, open)} />
}, (previous, next) => {
  const left = previous.group
  const right = next.group
  if (left.kind !== right.kind) return false
  if (left.kind === "single") {
    if (right.kind !== "single" || left.item !== right.item) return false
  } else {
    if (right.kind === "single" || left.key !== right.key
      || left.items.length !== right.items.length
      || left.items.some((item, index) => item !== right.items[index])) return false
    if (left.kind === "agent-calls"
      && (right.kind !== "agent-calls" || left.parentItemId !== right.parentItemId)) return false
  }
  const keys = Object.keys(previous) as Array<keyof LiveTimelineGroupProps>
  return keys.length === Object.keys(next).length
    && keys.every((key) => key === "group" || previous[key] === next[key])
})

export function TimelineGroupEntry({
  group,
  token,
  session,
  interactionByTarget,
  resolvingNoticeId,
  resolvingActionId,
  groupOpen,
  itemOpenById,
  readOnly = false,
  attachmentUrl,
  onGroupOpenChange,
  onItemOpenChange,
  onRespondInteraction,
}: {
  group: TimelineGroup
  token: string
  session: SessionView
  interactionByTarget: ReadonlyMap<string | null, Notice>
  resolvingNoticeId: string | null
  resolvingActionId: string | null
  groupOpen: boolean
  itemOpenById: Record<string, boolean>
  readOnly?: boolean
  attachmentUrl?: (fileId: string) => string
  onGroupOpenChange?: (open: boolean) => void
  onItemOpenChange: (itemId: string, open: boolean) => void
  onRespondInteraction: (noticeId: string, actionId: string, input?: Record<string, unknown>) => void
}) {
  if (group.kind === "reconnect") {
    return (
      <ReconnectGroup
        group={group}
        open={groupOpen}
        onOpenChange={onGroupOpenChange}
      />
    )
  }
  if (group.kind === "tool-run") {
    return (
      <ToolRunGroup
        group={group}
        token={token}
        session={session}
        interactionByTarget={interactionByTarget}
        resolvingNoticeId={resolvingNoticeId}
        resolvingActionId={resolvingActionId}
        open={groupOpen}
        itemOpenById={itemOpenById}
        readOnly={readOnly}
        attachmentUrl={attachmentUrl}
        onOpenChange={onGroupOpenChange}
        onItemOpenChange={onItemOpenChange}
        onRespondInteraction={onRespondInteraction}
      />
    )
  }
  if (group.kind === "agent-calls") {
    return (
      <AgentCallGroup
        group={group}
        token={token}
        session={session}
        interactionByTarget={interactionByTarget}
        resolvingNoticeId={resolvingNoticeId}
        resolvingActionId={resolvingActionId}
        open={groupOpen}
        itemOpenById={itemOpenById}
        readOnly={readOnly}
        attachmentUrl={attachmentUrl}
        onOpenChange={onGroupOpenChange}
        onItemOpenChange={onItemOpenChange}
        onRespondInteraction={onRespondInteraction}
      />
    )
  }
  return (
    <TimelineEntry
      token={token}
      session={session}
      item={group.item}
      interaction={interactionByTarget.get(group.item.id)}
      resolvingNoticeId={resolvingNoticeId}
      resolvingActionId={resolvingActionId}
      toolOpen={itemOpenById[group.item.id] ?? false}
      readOnly={readOnly}
      attachmentUrl={attachmentUrl}
      onToolOpenChange={(open) => onItemOpenChange(group.item.id, open)}
      onRespondInteraction={onRespondInteraction}
    />
  )
}

function ReconnectGroup({
  group,
  open,
  onOpenChange,
}: {
  group: TimelineReconnectGroup
  open: boolean
  onOpenChange?: (open: boolean) => void
}) {
  const tSession = useTranslations("dashboard.session")
  const attempts = group.items
    .map((item) => reconnectMessage(item))
    .filter((message): message is string => Boolean(message))
    .map(reconnectAttempt)
    .filter((attempt): attempt is string => Boolean(attempt))
  const firstAttempt = attempts[0]
  const lastAttempt = attempts[attempts.length - 1]
  const attemptRange = firstAttempt && lastAttempt && firstAttempt !== lastAttempt
    ? `${firstAttempt}–${lastAttempt}`
    : lastAttempt ?? String(group.items.length)
  const title = tSession("reconnectSummary", { count: group.items.length, attempts: attemptRange })

  return (
    <Collapsible open={open} onOpenChange={onOpenChange} className="min-w-0 max-w-full overflow-hidden">
      <div className="flex min-w-0 max-w-full flex-col gap-2 overflow-hidden">
        <CollapsibleTrigger asChild>
          <Marker asChild className="w-full">
            <button type="button" className="text-left">
              <ChevronDown className="shrink-0 -rotate-90 transition-transform group-data-[state=open]/marker:rotate-0" />
              <MarkerIcon>
                <WifiOff />
              </MarkerIcon>
              <MarkerContent className="code-mono text-sm">{title}</MarkerContent>
            </button>
          </Marker>
        </CollapsibleTrigger>
        <CollapsibleContent className="min-w-0 max-w-full overflow-hidden">
          <div className="flex flex-col gap-2">
            {group.items.map((item) => (
              <JsonBlock key={item.id} value={item} />
            ))}
          </div>
        </CollapsibleContent>
      </div>
    </Collapsible>
  )
}

function ToolRunGroup({
  group,
  token,
  session,
  interactionByTarget,
  resolvingNoticeId,
  resolvingActionId,
  open,
  itemOpenById,
  readOnly,
  attachmentUrl,
  onOpenChange,
  onItemOpenChange,
  onRespondInteraction,
}: {
  group: TimelineToolRunGroup
  token: string
  session: SessionView
  interactionByTarget: ReadonlyMap<string | null, Notice>
  resolvingNoticeId: string | null
  resolvingActionId: string | null
  open: boolean
  itemOpenById: Record<string, boolean>
  readOnly: boolean
  attachmentUrl?: (fileId: string) => string
  onOpenChange?: (open: boolean) => void
  onItemOpenChange: (itemId: string, open: boolean) => void
  onRespondInteraction: (noticeId: string, actionId: string, input?: Record<string, unknown>) => void
}) {
  const tSession = useTranslations("dashboard.session")
  const summary = toolRunSummary(group.items, tSession)
  const status = toolRunStatus(group.items)

  return (
    <Collapsible open={open} onOpenChange={onOpenChange} className="min-w-0 max-w-full overflow-hidden">
      <div className="flex min-w-0 max-w-full flex-col gap-2 overflow-hidden">
        <CollapsibleTrigger asChild>
          <Marker asChild className="w-full">
            <button type="button" className="text-left">
              <ToolMarkerRowContent
                collapsible
                kind="tool"
                status={status}
                title={summary}
              />
            </button>
          </Marker>
        </CollapsibleTrigger>
        <CollapsibleContent className="min-w-0 max-w-full overflow-hidden">
          <div className="flex flex-col gap-2">
            {group.items.map((item) => (
              <TimelineEntry
                key={item.id}
                token={token}
                session={session}
                item={item}
                interaction={interactionByTarget.get(item.id)}
                resolvingNoticeId={resolvingNoticeId}
                resolvingActionId={resolvingActionId}
                toolOpen={itemOpenById[item.id] ?? false}
                readOnly={readOnly}
                attachmentUrl={attachmentUrl}
                onToolOpenChange={(open) => onItemOpenChange(item.id, open)}
                onRespondInteraction={onRespondInteraction}
              />
            ))}
          </div>
        </CollapsibleContent>
      </div>
    </Collapsible>
  )
}

function AgentCallGroup({
  group,
  token,
  session,
  interactionByTarget,
  resolvingNoticeId,
  resolvingActionId,
  open,
  itemOpenById,
  readOnly,
  attachmentUrl,
  onOpenChange,
  onItemOpenChange,
  onRespondInteraction,
}: {
  group: TimelineAgentCallGroup
  token: string
  session: SessionView
  interactionByTarget: ReadonlyMap<string | null, Notice>
  resolvingNoticeId: string | null
  resolvingActionId: string | null
  open: boolean
  itemOpenById: Record<string, boolean>
  readOnly: boolean
  attachmentUrl?: (fileId: string) => string
  onOpenChange?: (open: boolean) => void
  onItemOpenChange: (itemId: string, open: boolean) => void
  onRespondInteraction: (noticeId: string, actionId: string, input?: Record<string, unknown>) => void
}) {
  const tSession = useTranslations("dashboard.session")
  const status = toolRunStatus(group.items)

  return (
    <Collapsible open={open} onOpenChange={onOpenChange} className="min-w-0 max-w-full overflow-hidden">
      <div className="flex min-w-0 max-w-full flex-col gap-2 overflow-hidden">
        <CollapsibleTrigger asChild>
          <Marker asChild className="w-full">
            <button type="button" className="text-left">
              <ToolMarkerRowContent
                collapsible
                kind="agent_call"
                status={status}
                title={tSession("agentCallGroupSummary", { count: group.items.length })}
              />
            </button>
          </Marker>
        </CollapsibleTrigger>
        <CollapsibleContent className="min-w-0 max-w-full overflow-hidden">
          <div className="flex flex-col gap-2">
            {group.items.map((item) => (
              <TimelineEntry
                key={item.id}
                token={token}
                session={session}
                item={item}
                interaction={interactionByTarget.get(item.id)}
                resolvingNoticeId={resolvingNoticeId}
                resolvingActionId={resolvingActionId}
                nestedAgentCall
                toolOpen={itemOpenById[item.id] ?? false}
                readOnly={readOnly}
                attachmentUrl={attachmentUrl}
                onToolOpenChange={(open) => onItemOpenChange(item.id, open)}
                onRespondInteraction={onRespondInteraction}
              />
            ))}
          </div>
        </CollapsibleContent>
      </div>
    </Collapsible>
  )
}

function toolRunStatus(items: TimelineItem[]): TimelineItem["status"] {
  if (items.some((item) => timelineItemStatusIsActive(item.status))) return "running"
  return "done"
}

function toolRunSummary(
  items: TimelineItem[],
  tSession: (key: string, values?: Record<string, string | number>) => string,
): string {
  const counts = timelineRunCounts(items)

  const parts: string[] = []
  if (counts.reasoning > 0) parts.push(tSession("toolSummaryReasoning", { count: counts.reasoning }))
  if (counts.tools > 0) parts.push(tSession("toolSummaryTools", { count: counts.tools }))
  return parts.length > 0 ? parts.join(", ") : tSession("toolSummaryItems", { count: items.length })
}

/**
 * A timeline snapshot is authoritative for the window it covers: the server
 * publishes it after an edit deleted every later item. Merging it as a union
 * kept the deleted rows on screen forever.
 */
function applyTimelineSnapshot(current: TimelineItem[], snapshot: TimelineItem[]): TimelineItem[] {
  if (snapshot.length === 0) {
    return current.filter(isOptimisticTimelineItem)
  }
  let min = Number.POSITIVE_INFINITY
  let max = Number.NEGATIVE_INFINITY
  const present = new Set<string>()
  for (const item of snapshot) {
    present.add(item.id)
    if (item.orderSeq < min) min = item.orderSeq
    if (item.orderSeq > max) max = item.orderSeq
  }
  const kept = current.filter((item) => {
    if (isOptimisticTimelineItem(item) || present.has(item.id)) return true
    // Absent from the authoritative window means the server deleted it; rows
    // outside the window were never covered by this snapshot.
    return item.orderSeq < min || item.orderSeq > max
  })
  return mergeTimelineItems(kept, snapshot)
}

function mergeSessionEvent(
  current: SessionRemoteState | null,
  event: ProtocolEventEnvelope,
): SessionRemoteState | null {
  if (!current) return current

  const session = event.type === "session.meta.updated"
    ? readPayloadValue<SessionView>(event.payload.session)
    : null
  const runtimeState = event.type === "runtime.state.updated"
    ? readPayloadValue<SessionRuntimeState>(event.payload.state)
    : null
  const item = event.type === "timeline.item_created" || event.type === "timeline.item_updated"
    ? readPayloadValue<TimelineItem>(event.payload.item)
    : null
  const timelineSnapshot = event.type === "timeline.snapshot" && Array.isArray(event.payload.items)
    ? event.payload.items.filter(isTimelineItem)
    : null
  const notice = event.type === "runtime.notice.updated"
    ? serverNotices([event.payload.notice], event.emittedAt)[0] ?? null
    : null
  const noticeSnapshot = event.type === "runtime.notice.snapshot" && Array.isArray(event.payload.notices)
    ? serverNotices(event.payload.notices, event.emittedAt)
    : null
  const capabilitySet = event.type === "runtime.capability.updated"
    ? readPayloadValue<ProtocolCapabilitySet>(event.payload.capabilitySet)
    : null
  const catalogUpdate = event.type === "runtime.catalog.updated"
    ? catalogUpdateFromEvent(event)
    : null
  const agentRuns = event.type === "runtime.activity"
    ? readAgentRuns(event.payload.agents)
    : null

  const nextNotices = noticeSnapshot
    ? noticeSnapshot
    : notice
      ? mergeNotices(current.notices, [notice])
      : current.notices
  const nextItems = timelineSnapshot
    ? applyTimelineSnapshot(current.items, timelineSnapshot)
    : item
      ? mergeTimelineItems(current.items, [item])
      : current.items
  const nextHasMore = timelineSnapshot !== null && typeof event.payload.hasMore === "boolean"
    ? current.hasMore || event.payload.hasMore
    : current.hasMore
  const timelineResetVersion = nextTimelineResetVersion(
    current.timelineResetVersion,
    timelineSnapshot !== null,
  )
  const acceptsSession = Boolean(session && session.updatedSeq >= current.session.updatedSeq)
  const nextSession = acceptsSession && session && !sessionSemanticallyEqual(current.session, session)
    ? session
    : current.session
  const nextRuntimeState = mergeRuntimeState(current.state, runtimeState, current.session.id)
  const nextEffectiveCapabilities = mergeEffectiveCapabilities(
    current.effectiveCapabilities,
    capabilitySet,
  )
  const nextCatalogs = catalogUpdate && !catalogsSemanticallyEqual(current.catalogs[catalogUpdate.catalogType], catalogUpdate.catalog)
    ? {
        ...current.catalogs,
        [catalogUpdate.catalogType]: catalogUpdate.catalog,
      }
    : current.catalogs
  const nextAgents = agentRuns !== null && !agentRunsEqual(current.agents, agentRuns)
    ? agentRuns
    : current.agents
  const nextAgentsLoaded = agentRuns !== null ? true : current.agentsLoaded
  const nextSeq = Math.max(current.nextSeq, event.sequence)
  const nextEventCursor = event.sequence >= current.nextSeq ? event.cursor : current.eventCursor

  if (
    nextSession === current.session &&
    nextRuntimeState === current.state &&
    nextItems === current.items &&
    nextNotices === current.notices &&
    nextAgents === current.agents &&
    nextAgentsLoaded === current.agentsLoaded &&
    nextEffectiveCapabilities === current.effectiveCapabilities &&
    nextCatalogs === current.catalogs &&
    nextSeq === current.nextSeq &&
    timelineResetVersion === current.timelineResetVersion &&
    nextEventCursor === current.eventCursor
  ) {
    return current
  }

  return {
    ...current,
    session: nextSession,
    state: nextRuntimeState,
    items: nextItems,
    notices: nextNotices,
    agents: nextAgents,
    agentsLoaded: nextAgentsLoaded,
    nextSeq,
    hasMore: nextHasMore,
    timelineResetVersion,
    eventCursor: nextEventCursor,
    effectiveCapabilities: nextEffectiveCapabilities,
    catalogs: nextCatalogs,
    serverTime: event.emittedAt ?? current.serverTime,
  }
}

function sessionEventCanUpdateState(event: ProtocolEventEnvelope): boolean {
  return (
    event.type === "session.meta.updated" ||
    event.type === "runtime.state.updated" ||
    event.type === "runtime.capability.updated" ||
    event.type === "runtime.catalog.updated" ||
    event.type === "runtime.activity" ||
    event.type === "runtime.notice.updated" ||
    event.type === "runtime.notice.snapshot" ||
    event.type === "timeline.item_created" ||
    event.type === "timeline.item_updated" ||
    event.type === "timeline.snapshot"
  )
}

function catalogUpdateFromEvent(
  event: ProtocolEventEnvelope,
): { catalogType: "model"; catalog: ProtocolModelCatalog } | { catalogType: "permission"; catalog: ProtocolPermissionCatalog } | null {
  if (event.payload.catalogType === "model") {
    const catalog = readPayloadValue<ProtocolModelCatalog>(event.payload.catalog)
    return catalog ? { catalogType: "model", catalog } : null
  }
  if (event.payload.catalogType === "permission") {
    const catalog = readPayloadValue<ProtocolPermissionCatalog>(event.payload.catalog)
    return catalog ? { catalogType: "permission", catalog } : null
  }
  return null
}

function catalogsSemanticallyEqual(
  left: unknown,
  right: ProtocolModelCatalog | ProtocolPermissionCatalog,
): boolean {
  if (!left) return false
  return stableStringify(left) === stableStringify(right)
}

function sessionSemanticallyEqual(left: SessionView, right: SessionView): boolean {
  return stableStringify({
    id: left.id,
    connectorId: left.connectorId,
    connectorStatus: left.connectorStatus,
    runtime: left.runtime,
    externalSessionId: left.externalSessionId,
    live: left.live,
    title: left.title,
    cwd: left.cwd,
    status: left.status,
    takeover: left.takeover,
    pinned: left.pinned,
    pinnedAt: left.pinnedAt,
    archived: left.archived,
    archivedAt: left.archivedAt,
    unread: left.unread,
    lastReadSeq: left.lastReadSeq,
    lastSyncedAt: left.lastSyncedAt,
    sourceObservedAt: left.sourceObservedAt,
    lastActivityAt: left.lastActivityAt,
    lastItemAt: left.lastItemAt,
    lastItemOrderSeq: left.lastItemOrderSeq,
    sortAt: left.sortAt,
    effectiveRunMode: left.effectiveRunMode,
    runtimeSettings: left.runtimeSettings,
    runtimeSettingsOverride: left.runtimeSettingsOverride,
  }) === stableStringify({
    id: right.id,
    connectorId: right.connectorId,
    connectorStatus: right.connectorStatus,
    runtime: right.runtime,
    externalSessionId: right.externalSessionId,
    live: right.live,
    title: right.title,
    cwd: right.cwd,
    status: right.status,
    takeover: right.takeover,
    pinned: right.pinned,
    pinnedAt: right.pinnedAt,
    archived: right.archived,
    archivedAt: right.archivedAt,
    unread: right.unread,
    lastReadSeq: right.lastReadSeq,
    lastSyncedAt: right.lastSyncedAt,
    sourceObservedAt: right.sourceObservedAt,
    lastActivityAt: right.lastActivityAt,
    lastItemAt: right.lastItemAt,
    lastItemOrderSeq: right.lastItemOrderSeq,
    sortAt: right.sortAt,
    effectiveRunMode: right.effectiveRunMode,
    runtimeSettings: right.runtimeSettings,
    runtimeSettingsOverride: right.runtimeSettingsOverride,
  })
}

function mergeRuntimeState(
  current: SessionRuntimeState | null | undefined,
  incoming: SessionRuntimeState | null | undefined,
  sessionId: string,
): SessionRuntimeState | null | undefined {
  if (!incoming || incoming.sessionId !== sessionId || incoming.updatedSeq < (current?.updatedSeq ?? 0)) {
    return current
  }
  // Even identical facts must advance the version watermark. Otherwise a
  // later, older observation could replace them with different facts.
  return current && incoming.updatedSeq === current.updatedSeq && runtimeStatesSemanticallyEqual(current, incoming)
    ? current
    : incoming
}

function runtimeStatesSemanticallyEqual(
  left: SessionRuntimeState | null,
  right: SessionRuntimeState,
): boolean {
  if (!left) return false
  return stableStringify({
    sessionId: left.sessionId,
    runtime: left.runtime,
    externalSessionId: left.externalSessionId,
    status: left.status,
    selections: left.selections,
    statusReason: left.statusReason,
    error: left.error,
  }) === stableStringify({
    sessionId: right.sessionId,
    runtime: right.runtime,
    externalSessionId: right.externalSessionId,
    status: right.status,
    selections: right.selections,
    statusReason: right.statusReason,
    error: right.error,
  })
}


/**
 * The live notice read returns exactly the still-pending questions: a resolved
 * one is simply absent. Merging that read into the current list kept answered
 * approvals pinned above the composer. Only a notice this client is actively
 * answering survives from the old list.
 */
function replaceNotices(current: Notice[], pending: Notice[], resolving: string | null): Notice[] {
  const inFlight = resolving ? current.filter((notice) => notice.noticeId === resolving) : []
  const next = mergeNotices(inFlight, pending)
  const unchanged = next.length === current.length &&
    next.every((notice, index) => notice === current[index])
  return unchanged ? current : next
}

function mergeNotices(current: Notice[], incoming: Notice[]): Notice[] {
  if (incoming.length === 0) return current
  const byId = new Map(current.map((notice) => [notice.noticeId, notice]))
  for (const notice of incoming) {
    const existing = byId.get(notice.noticeId)
    if (!existing || existing.updatedSeq <= notice.updatedSeq) byId.set(notice.noticeId, notice)
  }
  return Array.from(byId.values()).sort((a, b) => a.updatedSeq - b.updatedSeq || a.noticeId.localeCompare(b.noticeId))
}

function mergeSnapshotNotices(
  current: Notice[],
  incoming: Notice[],
  snapshotSequence: number,
): Notice[] {
  return mergeNotices(
    incoming,
    current.filter((notice) => notice.updatedSeq > snapshotSequence),
  )
}

function mergeVersionedCatalogs(
  current: SessionRemoteState["catalogs"],
  incoming: SessionRemoteState["catalogs"],
): SessionRemoteState["catalogs"] {
  const merged = { ...incoming }
  for (const [key, value] of Object.entries(current)) {
    const next = merged[key]
    if (
      isRevisionedValue(value) &&
      (!isRevisionedValue(next) || value.revision > next.revision)
    ) {
      merged[key] = value
    }
  }
  return merged
}

function isRevisionedValue(value: unknown): value is { revision: number } {
  return Boolean(
    value &&
    typeof value === "object" &&
    "revision" in value &&
    typeof value.revision === "number",
  )
}

function cursorSequence(cursor: string | null | undefined): number {
  if (!cursor) return 0
  const raw = cursor.startsWith("seq:") ? cursor.slice(4) : cursor
  const value = Number(raw)
  return Number.isFinite(value) ? value : 0
}

function readPayloadValue<T>(value: unknown): T | null {
  return value && typeof value === "object" ? value as T : null
}

function isTimelineItem(value: unknown): value is TimelineItem {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false
  const item = value as Partial<TimelineItem>
  return typeof item.id === "string" && typeof item.updatedSeq === "number"
}

/**
 * Normalize notices from snapshots, events and the notices endpoint. Some
 * servers omit updatedSeq/updatedAt; fill them in rather than dropping the
 * notice the session is blocked on.
 */
function serverNotices(value: unknown, serverTime: string): Notice[] {
  if (!Array.isArray(value)) return []
  const notices: Notice[] = []
  for (const item of value) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue
    const notice = item as Partial<Notice>
    if (typeof notice.noticeId !== "string" || typeof notice.status !== "string") continue
    notices.push({
      ...(notice as Notice),
      createdAt: typeof notice.createdAt === "string" ? notice.createdAt : serverTime,
      updatedSeq: typeof notice.updatedSeq === "number" ? notice.updatedSeq : 0,
      updatedAt: typeof notice.updatedAt === "string"
        ? notice.updatedAt
        : typeof notice.createdAt === "string" ? notice.createdAt : serverTime,
    })
  }
  return notices
}

function openInteractions(notices: Notice[], _sessionId?: string): Notice[] {
  return notices.filter((notice) =>
    notice.type === "interaction" && (
      notice.status === "open" ||
      notice.status === "responding" ||
      notice.status === "response_accepted" ||
      notice.status === "resolving" ||
      notice.status === "failed"
    ),
  )
}

function openNotifications(notices: Notice[]): Notice[] {
  return notices.filter((notice) => notice.type === "notification" && notice.status === "open")
}

function blockingInteractions(notices: Notice[], sessionId: string): Notice[] {
  return openInteractions(notices).filter((notice) => isSessionBlockingInteraction(notice, sessionId))
}

function isSessionBlockingInteraction(notice: Notice, sessionId: string): boolean {
  return notice.blocking?.scope === "session" && notice.blocking.targetId === sessionId
}

function rpcErrorRemovesNotice(code: string | undefined): boolean {
  return (
    code === "not_found" ||
    code === "notice_not_found" ||
    code === "interaction_not_found" ||
    code === "request_not_found" ||
    code === "approval_not_found"
  )
}

function noticeTimelineTargetId(notice: Notice): string | null {
  const timelineItemId = notice.source.timelineItemId
  if (typeof timelineItemId === "string" && timelineItemId) return timelineItemId
  const contextTimelineItemId = notice.context.timelineItemId
  if (typeof contextTimelineItemId === "string" && contextTimelineItemId) return contextTimelineItemId
  return null
}
