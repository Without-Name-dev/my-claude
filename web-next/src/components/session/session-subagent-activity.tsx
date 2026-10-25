"use client"

import * as React from "react"
import { useTranslations } from "next-intl"

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Marker } from "@/components/ui/marker"
import { ToolMarkerRowContent } from "@/components/session/session-tool-cards"
import type { AgentRun, TimelineItem } from "@/features/dashboard/types"

const TERMINAL_STATUSES = new Set([
  "done", "completed", "complete", "succeeded", "success", "finished",
  "failed", "error", "cancelled", "canceled", "interrupted", "stopped",
  "aborted", "closed", "ended", "exited",
])
const FAILED_STATUSES = new Set(["failed", "error"])
const INTERRUPTED_STATUSES = new Set(["cancelled", "canceled", "interrupted", "stopped", "aborted"])

function normalizedStatus(status: string): string {
  return status.trim().toLowerCase()
}

function isRunning(agent: AgentRun): boolean {
  return !TERMINAL_STATUSES.has(normalizedStatus(agent.status))
}

function markerStatus(agent: AgentRun): TimelineItem["status"] {
  if (isRunning(agent)) return "running"
  const status = normalizedStatus(agent.status)
  if (FAILED_STATUSES.has(status)) return "failed"
  if (INTERRUPTED_STATUSES.has(status)) return "interrupted"
  return "done"
}

function durationText(start: number | undefined, end: number): string {
  const from = start && Number.isFinite(start) ? start : end
  const seconds = Math.max(0, Math.floor((end - from) / 1000))
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const remaining = seconds % 60
  if (minutes < 60) return `${minutes}m ${remaining}s`
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`
}

function toolLine(agent: AgentRun): string | null {
  const tool = agent.tool?.trim() ?? ""
  const detail = agent.detail?.trim() ?? ""
  if (!tool) return detail || null
  return detail ? `${tool} · ${detail}` : tool
}

/**
 * The live subagent block. It is fed by `runtime.activity`, the same frames the
 * iOS inline block reads: each agent's current tool, its one-line argument and
 * its latest own text, updating as the connector writes them. Drawn with the
 * transcript's marker rows so it sits in the existing agent UI rather than
 * becoming a second visual language.
 */
export function SubagentActivityBlock({ agents }: { agents: AgentRun[] }) {
  const t = useTranslations("dashboard.session")
  const running = agents.filter(isRunning)
  const isActive = running.length > 0
  const [now, setNow] = React.useState(() => Date.now())

  React.useEffect(() => {
    if (!isActive) return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [isActive])

  if (agents.length === 0) return null

  const runningStarts = running
    .map((agent) => agent.startedAt)
    .filter((value) => Number.isFinite(value) && value > 0)
  const startedAt = runningStarts.length > 0 ? Math.min(...runningStarts) : undefined
  const headerStatus: TimelineItem["status"] = isActive
    ? "running"
    : agents.some((agent) => markerStatus(agent) === "failed")
      ? "failed"
      : agents.some((agent) => markerStatus(agent) === "interrupted")
        ? "interrupted"
        : "done"
  const headerTitle = isActive
    ? running.length === 1
      ? t("subagentActivityRunningOne", {
          label: running[0]?.label?.trim() || running[0]?.agentId || "",
          duration: durationText(startedAt, now),
        })
      : t("subagentActivityRunningMany", {
          count: running.length,
          duration: durationText(startedAt, now),
        })
    : agents.length === 1
      ? t("subagentActivityFinishedOne", {
          label: agents[0]?.label?.trim() || agents[0]?.agentId || "",
          duration: durationText(agents[0]?.startedAt, agents[0]?.updatedAt || now),
        })
      : t("subagentActivityFinishedMany", { count: agents.length })

  return (
    <Collapsible className="min-w-0 max-w-full overflow-hidden">
      <CollapsibleTrigger asChild>
        <Marker asChild className="w-full">
          <button type="button" className="text-left">
            <ToolMarkerRowContent
              collapsible
              kind="agent_call"
              status={headerStatus}
              title={headerTitle}
            />
          </button>
        </Marker>
      </CollapsibleTrigger>
      <CollapsibleContent className="min-w-0 max-w-full overflow-hidden">
        <div className="flex flex-col gap-2 pt-1">
          {agents.map((agent) => {
            const agentRunning = isRunning(agent)
            const failed = FAILED_STATUSES.has(normalizedStatus(agent.status))
            const title = agentRunning
              ? t("subagentActivityRunningOne", {
                  label: agent.label?.trim() || agent.agentId,
                  duration: durationText(agent.startedAt, now),
                })
              : failed
                ? t("subagentActivityFailedOne", {
                    label: agent.label?.trim() || agent.agentId,
                  })
                : t("subagentActivityFinishedOne", {
                    label: agent.label?.trim() || agent.agentId,
                    duration: durationText(agent.startedAt, agent.updatedAt || now),
                  })
            const line = agentRunning ? toolLine(agent) : null
            const text = agent.text?.trim() ?? ""
            return (
              <div className="flex min-w-0 flex-col gap-1" key={agent.agentId}>
                <Marker className="w-full">
                  <ToolMarkerRowContent
                    kind="agent_call"
                    status={markerStatus(agent)}
                    title={title}
                  />
                </Marker>
                {line ? (
                  <div className="code-mono truncate pl-7 text-xs text-muted-foreground">{line}</div>
                ) : null}
                {text ? (
                  <div className="line-clamp-6 whitespace-pre-wrap break-words pl-7 text-xs leading-relaxed text-muted-foreground">
                    {text}
                  </div>
                ) : null}
              </div>
            )
          })}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}
