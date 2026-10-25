import type { DashboardSnapshotMessage, ProtocolEventEnvelope } from "@/features/dashboard/types"

/**
 * Parses a dashboard snapshot frame. The shared stream sends the same shape as
 * the dashboard WebSocket, so the legacy and unified paths share this parser.
 */
export function parseDashboardSnapshotMessage(value: unknown): DashboardSnapshotMessage | null {
  if (!value || typeof value !== "object") return null
  const message = value as Partial<DashboardSnapshotMessage>
  if (!(
    message.type === "dashboard.snapshot" &&
    Array.isArray(message.connectors) &&
    Array.isArray(message.projects) &&
    Array.isArray(message.sessions)
  )) return null

  return message as DashboardSnapshotMessage
}

/** Parses an event envelope frame; returns null for keepalive and malformed frames. */
export function parseProtocolEventEnvelope(value: unknown): ProtocolEventEnvelope | null {
  if (!value || typeof value !== "object") return null
  const event = value as Partial<ProtocolEventEnvelope>
  if (typeof event.type !== "string") return null
  if (event.type === "keepalive") return null
  if (
    event.protocolVersion !== "1.0" ||
    typeof event.eventId !== "string" ||
    typeof event.emittedAt !== "string" ||
    typeof event.sessionId !== "string" ||
    typeof event.sequence !== "number" ||
    typeof event.cursor !== "string"
  ) {
    return null
  }
  return {
    protocolVersion: event.protocolVersion,
    eventId: event.eventId,
    sequence: event.sequence,
    cursor: event.cursor,
    type: event.type,
    sessionId: event.sessionId,
    emittedAt: event.emittedAt,
    payload: event.payload ?? {},
  }
}

/** Parses raw socket data as an event envelope. */
export function parseProtocolEventData(data: string): ProtocolEventEnvelope | null {
  try {
    return parseProtocolEventEnvelope(JSON.parse(data))
  } catch {
    return null
  }
}
