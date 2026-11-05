"use client"

import * as React from "react"
import { Loader2 } from "lucide-react"
import { List, useDynamicRowHeight, type RowComponentProps } from "react-window"

import type { Notice, SessionView, TimelineItem } from "@/features/dashboard/types"
import { TimelineEntry } from "@/components/session/session-timeline-entry"

export type TimelineGroup = {
  kind: "single" | "multi"
  key: string
  items: TimelineItem[]
}

type SessionTimelineProps = {
  token: string
  session: SessionView
  groups: TimelineGroup[]
  interactionByTarget: Map<string, Notice>
  resolvingNoticeId: string | null
  resolvingActionId: string | null
  groupOpen: Record<string, boolean>
  itemOpenById: Record<string, boolean>
  loadingOlder: boolean
  hasMore: boolean
  onGroupOpenChange: (key: string, open: boolean) => void
  onItemOpenChange: (id: string, open: boolean) => void
  onRespondInteraction: (noticeId: string, actionId: string, input?: Record<string, unknown>) => void
  onLoadMore: () => void
  onScroll?: (scrollTop: number) => void
  attachmentUrl?: (fileId: string) => string
  readOnly?: boolean
}

type TimelineRowData = Pick<SessionTimelineProps, "token" | "session" | "interactionByTarget" |
  "resolvingNoticeId" | "resolvingActionId" | "itemOpenById" | "onItemOpenChange" |
  "onRespondInteraction" | "attachmentUrl" | "readOnly"> & {
  rows: Array<{ type: "loader" } | { type: "group"; group: TimelineGroup }>
}

function TimelineRow({ index, style, ariaAttributes, rows, ...props }: RowComponentProps<TimelineRowData>) {
  const row = rows[index]
  if (!row) return null
  return (
    <div style={style} {...ariaAttributes} className="px-4">
      {row.type === "loader" ? (
        <div className="flex justify-center py-2 text-muted-foreground">
          <Loader2 className="size-4 animate-spin" />
        </div>
      ) : row.group.items.map((item) => (
        <TimelineEntry
          key={item.id}
          token={props.token}
          session={props.session}
          item={item}
          interaction={props.interactionByTarget.get(item.id)}
          resolvingNoticeId={props.resolvingNoticeId}
          resolvingActionId={props.resolvingActionId}
          toolOpen={props.itemOpenById[item.id] ?? false}
          readOnly={props.readOnly}
          attachmentUrl={props.attachmentUrl}
          onToolOpenChange={(open) => props.onItemOpenChange(item.id, open)}
          onRespondInteraction={props.onRespondInteraction}
        />
      ))}
    </div>
  )
}

export function SessionTimeline(props: SessionTimelineProps) {
  const rows = React.useMemo<TimelineRowData["rows"]>(() => [
    ...(props.loadingOlder && props.hasMore ? [{ type: "loader" as const }] : []),
    ...props.groups.map((group) => ({ type: "group" as const, group })),
  ], [props.groups, props.loadingOlder, props.hasMore])
  // Measurements are indexed; discard them when a prepend or loader changes
  // which row occupies an index. ResizeObserver handles changes within a row.
  const measurementKey = JSON.stringify(rows.map((row) => row.type === "loader" ? null : row.group.key))
  const rowHeight = useDynamicRowHeight({ defaultRowHeight: 120, key: measurementKey })
  const rowProps = React.useMemo<TimelineRowData>(() => ({
    rows, token: props.token, session: props.session,
    interactionByTarget: props.interactionByTarget, resolvingNoticeId: props.resolvingNoticeId,
    resolvingActionId: props.resolvingActionId, itemOpenById: props.itemOpenById,
    onItemOpenChange: props.onItemOpenChange, onRespondInteraction: props.onRespondInteraction,
    attachmentUrl: props.attachmentUrl, readOnly: props.readOnly,
  }), [rows, props.token, props.session, props.interactionByTarget, props.resolvingNoticeId,
    props.resolvingActionId, props.itemOpenById, props.onItemOpenChange,
    props.onRespondInteraction, props.attachmentUrl, props.readOnly])
  const rowKey = React.useCallback((index: number, data: TimelineRowData) => {
    const row = data.rows[index]
    return row?.type === "group" ? row.group.key : "timeline-loader"
  }, [])

  return (
    <List
      className="timeline-virtual-scroll"
      style={{ height: "100%", width: "100%" }}
      rowComponent={TimelineRow}
      rowCount={rows.length}
      rowHeight={rowHeight}
      rowKey={rowKey}
      rowProps={rowProps}
      overscanCount={3}
      onScroll={(event) => {
        const scrollTop = event.currentTarget.scrollTop
        props.onScroll?.(scrollTop)
        if (props.hasMore && !props.loadingOlder && scrollTop < 500) props.onLoadMore()
      }}
    />
  )
}
