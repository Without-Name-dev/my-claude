"use client"

import * as React from "react"
import { Braces, ChevronDown, CircleAlert, Clock, Copy, FilePenLine, Sparkles } from "lucide-react"
import dynamic from "next/dynamic"
import { useTranslations } from "next-intl"
import { toast } from "sonner"

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import { Marker, MarkerContent, MarkerIcon } from "@/components/ui/marker"
import { JsonBlock, TimelineStatusBadge, ToolCard } from "@/components/session/session-tool-cards"
import { useSessionFilePreviewOpener } from "@/components/session/session-file-preview-context"
import { openSessionFilePreview } from "@/components/markdown-text"
import { cn } from "@/lib/utils"
import { copyText } from "@/lib/clipboard"
import type { Notice, SessionView, TimelineItem } from "@/features/dashboard/types"
import { firstTextOf, messageText, recordsOf, textOf } from "@/components/session/session-utils"
import { extractAttachments, stripInjectedAttachmentMentions } from "@/features/dashboard/attachments"
import { MessageAttachments } from "@/components/session/message-attachments"
import { CollapsibleUserMessage } from "@/components/session/collapsible-user-message"
import { splitGraphemes, useTypewriter } from "@/hooks/use-typewriter"
import { useThrottledText } from "@/hooks/use-throttled-text"

const MarkdownText = dynamic(() => import("../markdown-text").then((mod) => ({ default: mod.MarkdownText })), { ssr: false })
const INLINE_REASONING_SUMMARY_MAX_CHARS = 80

export function TimelineEntry({
  token,
  session,
  item,
  interaction,
  resolvingNoticeId,
  resolvingActionId,
  toolOpen,
  nestedAgentCall = false,
  readOnly = false,
  attachmentUrl,
  onToolOpenChange,
  onRespondInteraction,
}: {
  token: string
  session: SessionView
  item: TimelineItem
  interaction?: Notice
  resolvingNoticeId: string | null
  resolvingActionId: string | null
  toolOpen?: boolean
  nestedAgentCall?: boolean
  readOnly?: boolean
  attachmentUrl?: (fileId: string) => string
  onToolOpenChange?: (open: boolean) => void
  onRespondInteraction: (noticeId: string, actionId: string, input?: Record<string, unknown>) => void
}) {
  let entry: React.ReactNode
  if (item.type === "message") {
    entry = <MessageCard token={token} session={session} item={item} readOnly={readOnly} attachmentUrl={attachmentUrl} />
    return <TimelineEntryContextMenu item={item}>{entry}</TimelineEntryContextMenu>
  }
  if (item.type === "tool" || isFileChangeArtifact(item)) {
    entry = (
      <div className={cn(nestedAgentCall && isNestedAgentCall(item) && "ml-5 border-l border-border/60 pl-3")}>
        <ToolCard
          item={item}
          token={token}
          session={session}
          interaction={interaction}
          resolvingNoticeId={resolvingNoticeId}
          resolvingActionId={resolvingActionId}
          open={toolOpen}
          onOpenChange={onToolOpenChange}
          onRespondInteraction={onRespondInteraction}
          readOnly={readOnly}
        />
      </div>
    )
    return <TimelineEntryContextMenu item={item}>{entry}</TimelineEntryContextMenu>
  }
  if (item.type === "marker") entry = <MarkerCard item={item} />
  else if (item.type === "system") entry = <SystemCard token={readOnly ? "" : token} session={session} item={item} />
  else if (item.type === "artifact") entry = <ArtifactCard token={token} session={session} item={item} readOnly={readOnly} />
  else entry = <UnknownTimelineItem item={item} />
  return <TimelineEntryContextMenu item={item}>{entry}</TimelineEntryContextMenu>
}

function isFileChangeArtifact(item: TimelineItem): boolean {
  return item.type === "artifact" && textOf(item.content.kind) === "file_change"
}

function isNestedAgentCall(item: TimelineItem): boolean {
  return textOf(item.content.kind) === "agent_call" && Boolean(textOf(item.content.parentItemId))
}

function TimelineEntryContextMenu({ item, children }: { item: TimelineItem; children: React.ReactNode }) {
  const tSession = useTranslations("dashboard.session")
  const tCommon = useTranslations("common")
  const text = timelineItemCopyText(item)
  const copyTimelineValue = async (value: string) => {
    if (!value) return
    try {
      await copyText(value)
    } catch {
      toast.error(tCommon("copyFailed"))
    }
  }
  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>
        <div className="min-w-0 max-w-full select-text">{children}</div>
      </ContextMenuTrigger>
      <ContextMenuContent className="w-48">
        <ContextMenuItem disabled={!text} onSelect={() => copyTimelineValue(text)}>
          <Copy data-icon="inline-start" />
          {tSession("copyItemText")}
        </ContextMenuItem>
        <ContextMenuItem onSelect={() => copyTimelineValue(item.id)}>
          <Copy data-icon="inline-start" />
          {tSession("copyItemId")}
        </ContextMenuItem>
        <ContextMenuSeparator />
        <ContextMenuItem onSelect={() => copyTimelineValue(JSON.stringify(item, null, 2))}>
          <Braces data-icon="inline-start" />
          {tSession("copyItemJson")}
        </ContextMenuItem>
      </ContextMenuContent>
    </ContextMenu>
  )
}

function timelineItemCopyText(item: TimelineItem): string {
  if (item.type === "message") return stripInjectedAttachmentMentions(messageText(item)).trim()
  return firstTextOf(
    item.content.text,
    item.content.message,
    item.content.rawText,
    item.content.command,
    item.content.label,
    item.content.title,
  )?.trim() ?? ""
}

function MessageCard({
  token,
  session,
  item,
  readOnly,
  attachmentUrl,
}: {
  token: string
  session: SessionView
  item: TimelineItem
  readOnly: boolean
  attachmentUrl?: (fileId: string) => string
}) {
  const tSession = useTranslations("dashboard.session")
  const tNew = useTranslations("dashboard.new")
  const attachments = extractAttachments(item.content)
  const isUser = item.role === "user"
  const isStreamingAssistant = !readOnly && item.role === "assistant"
    && (item.status === "running" || item.status === "pending")
  const hasAttachments = attachments.length > 0
  const message = stripInjectedAttachmentMentions(messageText(item))
  const text = hasAttachments && message.trim() === tNew("attachmentOnlyPrompt") ? "" : message
  const showUserStatus = isUser && item.status === "failed"
  if (!text && !hasAttachments) {
    if (item.role === "assistant") return null
    return (
      <JsonMarker
        item={item}
        title={tSession("timelineUnknownMessage", { role: item.role || "unknown" })}
        icon={<Clock />}
      />
    )
  }
  const content = text ? (
    item.role === "assistant" ? (
      <AssistantMarkdownText
        text={text}
        token={readOnly ? "" : token}
        session={session}
        streaming={isStreamingAssistant}
      />
    ) : (
      <MarkdownText text={text} token={readOnly ? "" : token} session={session} />
    )
  ) : null
  const attachmentList = (
    <MessageAttachments
      token={token}
      session={session}
      attachments={attachments}
      align={isUser ? "right" : "left"}
      attachmentUrl={attachmentUrl}
    />
  )

  return (
    <div className={cn("flex min-w-0 w-full max-w-full overflow-hidden", isUser && "justify-end")}>
      <div className={cn(
        "flex min-w-0 w-full flex-col gap-2 text-sm leading-relaxed",
        isUser ? "max-w-[88%] items-end" : "max-w-full",
      )}>
        {isUser ? attachmentList : null}
        {content ? (
          <div
            className={cn(
              "min-w-0 max-w-full",
              isUser ? "rounded-2xl bg-secondary px-4 py-3 text-secondary-foreground" : "bg-transparent px-0 py-1",
            )}
          >
            {isUser ? <CollapsibleUserMessage>{content}</CollapsibleUserMessage> : content}
          </div>
        ) : null}
        {!isUser ? attachmentList : null}
        {showUserStatus ? <TimelineStatusBadge status={item.status} /> : null}
      </div>
    </div>
  )
}

const MARKDOWN_COMMIT_INTERVAL_MS = 50

function AssistantMarkdownText({
  text,
  token,
  session,
  streaming,
}: {
  text: string
  token: string
  session: SessionView
  streaming: boolean
}) {
  const typedText = useTypewriter(text, streaming)
  // Typewriter updates run on every animation frame. Markdown parsing is much
  // more expensive, so it consumes a value committed at most 20 times/second.
  const renderedText = useThrottledText(typedText, streaming, MARKDOWN_COMMIT_INTERVAL_MS)
  const containerRef = React.useRef<HTMLDivElement>(null)

  return (
    <div ref={containerRef} className="relative min-w-0 max-w-full">
      <MarkdownText text={renderedText} token={token} session={session} />
      {streaming ? <StreamingCaret containerRef={containerRef} revision={renderedText} /> : null}
    </div>
  )
}

function StreamingCaret({
  containerRef,
  revision,
}: {
  containerRef: React.RefObject<HTMLDivElement | null>
  revision: string
}) {
  const caretRef = React.useRef<HTMLSpanElement>(null)
  const updateCaretRef = React.useRef<() => void>(() => undefined)

  React.useLayoutEffect(() => {
    const container = containerRef.current
    const caret = caretRef.current
    if (!container || !caret) return

    const update = () => {
      const range = lastVisibleTextRange(container)
      const rect = range?.getBoundingClientRect()
      if (!rect || (rect.width === 0 && rect.height === 0)) {
        caret.hidden = true
        return
      }
      const containerRect = container.getBoundingClientRect()
      caret.hidden = false
      caret.style.left = `${rect.right - containerRect.left}px`
      caret.style.top = `${rect.top - containerRect.top}px`
      caret.style.height = `${Math.max(rect.height, 14)}px`
    }

    updateCaretRef.current = update
    update()

    const observer = new ResizeObserver(update)
    observer.observe(container)
    window.addEventListener("resize", update)
    let cancelled = false
    void document.fonts?.ready.then(() => {
      if (!cancelled) update()
    })
    return () => {
      cancelled = true
      observer.disconnect()
      window.removeEventListener("resize", update)
    }
  }, [containerRef])

  // The caret is a sibling of the Markdown tree, positioned over its final
  // rendered character. It never becomes part of the Markdown source, so it
  // cannot break emphasis, code fences, tables, or inline formatting.
  React.useLayoutEffect(() => {
    updateCaretRef.current()
  }, [revision])

  return (
    <span
      ref={caretRef}
      aria-hidden="true"
      className="aa-typewriter-caret pointer-events-none absolute z-10 w-0.5 rounded-full bg-foreground/60"
    />
  )
}

function lastVisibleTextRange(container: HTMLElement): Range | null {
  const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT)
  let lastNode: Text | null = null
  let lastEnd = 0
  let node = walker.nextNode()
  while (node) {
    if (node.nodeType === Node.TEXT_NODE) {
      const textNode = node as Text
      const value = textNode.nodeValue ?? ""
      let end = value.length
      while (end > 0 && /\s/.test(value[end - 1] ?? "")) end -= 1
      if (end > 0 && !textNode.parentElement?.closest("[hidden], [aria-hidden='true']")) {
        lastNode = textNode
        lastEnd = end
      }
    }
    node = walker.nextNode()
  }
  if (!lastNode || lastEnd === 0) return null

  const value = lastNode.nodeValue ?? ""
  const grapheme = splitGraphemes(value.slice(0, lastEnd)).at(-1)
  if (!grapheme) return null
  const range = document.createRange()
  range.setStart(lastNode, lastEnd - grapheme.length)
  range.setEnd(lastNode, lastEnd)
  return range
}

function SystemCard({ token, session, item }: { token: string; session: SessionView; item: TimelineItem }) {
  const kind = textOf(item.content.kind) || "system"
  if (kind === "reasoning") return <ReasoningEntry token={token} session={session} item={item} />
  if (kind === "compact") return <CompactMarker item={item} />
  const text = textOf(item.content.text) || textOf(item.content.message) || textOf(item.content.rawText)
  const failed = item.status === "failed" || kind === "error"
  const title = text ? `${kind}: ${text}` : `${kind}: ${item.status}`
  return (
    <JsonMarker
      item={item}
      title={title}
      icon={failed ? <CircleAlert /> : <Clock />}
      destructive={failed}
      detail={systemDetail(item.content)}
    />
  )
}

function MarkerCard({ item }: { item: TimelineItem }) {
  const kind = textOf(item.content.kind) || "system"
  if (kind === "compact") return <CompactMarker item={item} />

  const label = textOf(item.content.label) || textOf(item.content.title) || kind
  const failed = item.status === "failed" || kind === "error"
  return (
    <JsonMarker
      item={item}
      title={label}
      icon={failed ? <CircleAlert /> : <Clock />}
      destructive={failed}
      detail={systemDetail(item.content)}
    />
  )
}

function CompactMarker({ item }: { item: TimelineItem }) {
  const tSession = useTranslations("dashboard.session")
  const state = compactTimelineState(item)
  const label = state === "started" ? "conversationCompacting"
    : state === "failed" ? "conversationCompactionFailed" : "conversationCompacted"
  return (
    <Marker variant="separator" role="status" className="w-full normal-case">
      <MarkerContent className={cn("flex-none text-sm font-normal", state === "started" && "shimmer", state === "failed" && "text-destructive")}>
        {tSession(label)}
      </MarkerContent>
    </Marker>
  )
}

function compactTimelineState(item: TimelineItem): "started" | "completed" | "failed" {
  const contentState = textOf(item.content.state)
  if ([contentState, item.status].some(state => state === "failed" || state === "interrupted" || state === "cancelled")) {
    return "failed"
  }
  if (contentState === "started" || contentState === "running" || contentState === "inProgress") {
    return "started"
  }
  if (item.status === "running" || item.status === "pending") {
    return "started"
  }
  return "completed"
}

function ReasoningEntry({ token, session, item }: { token: string; session: SessionView; item: TimelineItem }) {
  const tSession = useTranslations("dashboard.session")
  const summaries = recordsOf(item.content.summaries)
    .map((summary) => textOf(summary.text))
    .filter((text): text is string => Boolean(text))
  const rawText = textOf(item.content.rawText) || textOf(item.content.text)
  const lines = summaries.length > 0 ? summaries : rawText ? [rawText] : []
  const inlineSummary = lines.length === 1 ? inlineReasoningSummary(lines[0] ?? "") : null
  const title = inlineSummary
    ? tSession("reasoningSingleSummary", { summary: inlineSummary })
    : lines.length > 1
    ? tSession("reasoningSummary", { count: lines.length })
    : lines.length === 1
    ? tSession("reasoningSummary", { count: 1 })
    : tSession("reasoning")
  const marker = (
    <Marker className="w-full">
      <MarkerIcon>
        <Sparkles />
      </MarkerIcon>
      <MarkerContent className="code-mono text-sm">{title}</MarkerContent>
    </Marker>
  )

  if (lines.length === 0 || inlineSummary) return marker
  const markdown = lines.join("\n\n")

  return (
    <Collapsible className="min-w-0 max-w-full overflow-hidden">
      <div className="flex min-w-0 max-w-full flex-col gap-2 overflow-hidden">
        <CollapsibleTrigger asChild>
          <Marker asChild className="w-full">
            <button type="button" className="text-left">
              <ChevronDown className="shrink-0 -rotate-90 transition-transform group-data-[state=open]/marker:rotate-0" />
              <MarkerIcon>
                <Sparkles />
              </MarkerIcon>
              <MarkerContent className="code-mono text-sm">{title}</MarkerContent>
            </button>
          </Marker>
        </CollapsibleTrigger>
        <CollapsibleContent className="min-w-0 max-w-full overflow-hidden">
          <div className="flex flex-col gap-2 pl-7 text-sm leading-relaxed text-muted-foreground">
            <MarkdownText text={markdown} token={token} session={session} />
          </div>
        </CollapsibleContent>
      </div>
    </Collapsible>
  )
}

function inlineReasoningSummary(text: string): string | null {
  if (text.includes("\n") || text.includes("\r")) return null
  const plain = stripMarkdownForInlineSummary(text)
  if (!plain || plain.length > INLINE_REASONING_SUMMARY_MAX_CHARS) return null
  return plain
}

function stripMarkdownForInlineSummary(text: string): string {
  return text
    .replace(/!\[([^\]]*)\]\([^)]+\)/g, "$1")
    .replace(/\[([^\]]+)\]\([^)]+\)/g, "$1")
    .replace(/`([^`]+)`/g, "$1")
    .replace(/[*_~#>]+/g, "")
    .replace(/\\([\\`*_{}\[\]()#+\-.!|])/g, "$1")
    .replace(/\s+/g, " ")
    .trim()
}

function ArtifactCard({
  token,
  session,
  item,
  readOnly,
}: {
  token: string
  session: SessionView
  item: TimelineItem
  readOnly: boolean
}) {
  const openFilePreview = useSessionFilePreviewOpener()
  const kind = textOf(item.content.kind) || "artifact"
  if (kind === "diff") return null
  const path = firstTextOf(item.content.path, item.content.filePath, item.content.file, item.content.uri)
  const title = path ?? kind
  const markerContent = (
    <>
      <MarkerIcon>
        <FilePenLine />
      </MarkerIcon>
      <span
        className={cn(
          "code-mono min-w-0 flex-1 truncate text-sm",
          path && !readOnly && "underline-offset-2 group-hover/marker:underline",
        )}
        onClick={(event) => {
          if (!path || readOnly) return
          event.preventDefault()
          event.stopPropagation()
          openSessionFilePreview(token, session, path, openFilePreview)
        }}
      >
        {title}
      </span>
      <TimelineStatusBadge status={item.status} />
    </>
  )

  if (!hasExpandableDetail(item.content)) {
    return <Marker className="w-full">{markerContent}</Marker>
  }

  return (
    <Collapsible className="min-w-0 max-w-full overflow-hidden">
      <div className="flex min-w-0 max-w-full flex-col gap-2 overflow-hidden">
        <CollapsibleTrigger asChild>
          <Marker asChild>
            <button type="button" className="w-full text-left">
              <ChevronDown className="shrink-0 -rotate-90 transition-transform group-data-[state=open]/marker:rotate-0" />
              {markerContent}
            </button>
          </Marker>
        </CollapsibleTrigger>
        <CollapsibleContent className="min-w-0 max-w-full overflow-hidden">
          <JsonBlock value={item.content} />
        </CollapsibleContent>
      </div>
    </Collapsible>
  )
}

export function JsonMarker({
  item,
  title,
  icon,
  destructive,
  defaultOpen = false,
  detail,
}: {
  item: TimelineItem
  title: string
  icon?: React.ReactNode
  destructive?: boolean
  defaultOpen?: boolean
  detail?: unknown
}) {
  const detailValue = detail === undefined ? item : detail
  const hasDetail = hasExpandableDetail(detailValue)
  const marker = (
    <Marker className={cn("w-full", destructive && "text-destructive hover:text-destructive")}>
      {icon ? <MarkerIcon>{icon}</MarkerIcon> : null}
      <MarkerContent className="code-mono text-sm">{title}</MarkerContent>
      <TimelineStatusBadge status={item.status} />
    </Marker>
  )

  if (!hasDetail) return marker

  return (
    <Collapsible defaultOpen={defaultOpen} className="min-w-0 max-w-full overflow-hidden">
      <div className="flex min-w-0 max-w-full flex-col gap-2 overflow-hidden">
        <CollapsibleTrigger asChild>
          <Marker asChild className={cn("w-full", destructive && "text-destructive hover:text-destructive")}>
            <button type="button" className="text-left">
              <ChevronDown className="shrink-0 -rotate-90 transition-transform group-data-[state=open]/marker:rotate-0" />
              {icon ? <MarkerIcon>{icon}</MarkerIcon> : null}
              <MarkerContent className="code-mono text-sm">{title}</MarkerContent>
              <TimelineStatusBadge status={item.status} />
            </button>
          </Marker>
        </CollapsibleTrigger>
        <CollapsibleContent className="min-w-0 max-w-full overflow-hidden">
          <JsonBlock value={detailValue} />
        </CollapsibleContent>
      </div>
    </Collapsible>
  )
}

function UnknownTimelineItem({ item }: { item: TimelineItem }) {
  const tSession = useTranslations("dashboard.session")
  const title = tSession("timelineUnknownItem", { type: item.type || "unknown" })
  return <JsonMarker item={item} title={title} icon={<Clock />} />
}

function hasExpandableDetail(value: unknown): boolean {
  if (value == null) return false
  if (typeof value === "string") return value.trim().length > 0
  if (Array.isArray(value)) return value.length > 0
  if (typeof value === "object") return Object.keys(value).length > 0
  return true
}

function systemDetail(content: TimelineItem["content"]): Record<string, unknown> {
  const detail = { ...content }
  delete detail.kind
  delete detail.text
  delete detail.message
  delete detail.rawText
  return detail
}
