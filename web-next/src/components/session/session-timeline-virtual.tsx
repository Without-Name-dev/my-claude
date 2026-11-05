"use client"

import * as React from "react"
import { Loader2 } from "lucide-react"

import type { Notice, SessionView, TimelineItem } from "@/features/dashboard/types"
import { TimelineEntry } from "@/components/session/session-timeline-entry"
import { cn } from "@/lib/utils"

export type TimelineGroup = {
  kind: "single" | "multi"
  key: string
  items: TimelineItem[]
}

type VirtualTimelineProps = {
  token: string
  session: SessionView
  items: TimelineItem[]
  interactionByTarget: Map<string, Notice>
  resolvingNoticeId: string | null
  resolvingActionId: string | null
  itemOpenById: Record<string, boolean>
  loadingOlder: boolean
  hasMore: boolean
  onItemOpenChange: (id: string, open: boolean) => void
  onRespondInteraction: (noticeId: string, actionId: string, input?: Record<string, unknown>) => void
  onLoadMore: () => void
  onScroll?: (scrollTop: number) => void
  attachmentUrl?: (fileId: string) => string
  readOnly?: boolean
  className?: string
}

const LOAD_MORE_THRESHOLD = 500
const OBSERVER_ROOT_MARGIN = "500px"

export function VirtualTimeline({
  token,
  session,
  items,
  interactionByTarget,
  resolvingNoticeId,
  resolvingActionId,
  itemOpenById,
  loadingOlder,
  hasMore,
  onItemOpenChange,
  onRespondInteraction,
  onLoadMore,
  onScroll,
  attachmentUrl,
  readOnly = false,
  className,
}: VirtualTimelineProps) {
  const containerRef = React.useRef<HTMLDivElement>(null)
  const sentinelRef = React.useRef<HTMLDivElement>(null)
  const lastScrollTop = React.useRef(0)

  const handleScroll = React.useCallback(() => {
    if (!containerRef.current) return

    const scrollTop = containerRef.current.scrollTop
    lastScrollTop.current = scrollTop

    onScroll?.(scrollTop)

    if (hasMore && !loadingOlder && scrollTop < LOAD_MORE_THRESHOLD) {
      onLoadMore()
    }
  }, [hasMore, loadingOlder, onLoadMore, onScroll])

  React.useEffect(() => {
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting && hasMore && !loadingOlder) {
          onLoadMore()
        }
      },
      {
        root: containerRef.current,
        rootMargin: OBSERVER_ROOT_MARGIN,
        threshold: 0,
      }
    )

    if (sentinelRef.current) {
      observer.observe(sentinelRef.current)
    }

    return () => observer.disconnect()
  }, [hasMore, loadingOlder, onLoadMore])

  return (
    <div
      ref={containerRef}
      className={cn("h-full overflow-y-auto", className)}
      onScroll={handleScroll}
    >
      <div className="mx-auto flex w-full min-w-0 max-w-[calc(48rem+2rem)] flex-col gap-3 px-4 pb-44 pt-20">
        {loadingOlder && hasMore && (
          <div ref={sentinelRef} className="flex justify-center py-2 text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
          </div>
        )}

        {items.length === 0 && !loadingOlder && (
          <p className="py-12 text-center text-sm text-muted-foreground">
            No activity yet
          </p>
        )}

        {items.map((item) => (
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
    </div>
  )
}
