import Foundation

nonisolated enum V2RealtimeScope: Hashable {
    case dashboard
    case session(V2SessionID)
}

nonisolated struct V2WebSocketTicketRequest: Encodable, Hashable {
    let clientId: String
    let scope: V2WebSocketTicketScope
}

nonisolated struct V2WebSocketTicketScope: Encodable, Hashable {
    let sessionId: V2SessionID?
    let dashboard: Bool

    init(scope: V2RealtimeScope) {
        switch scope {
        case .dashboard:
            sessionId = nil
            dashboard = true
        case let .session(sessionId):
            self.sessionId = sessionId
            dashboard = false
        }
    }
}

nonisolated struct V2WebSocketTicket: Decodable, Hashable {
    let ticket: String
    let expiresAt: String
    let serverTime: String
}

nonisolated struct V2SessionEvent: Decodable, Identifiable, Hashable {
    let protocolVersion: String
    let eventId: String
    let sequence: Int
    let cursor: String
    let type: String
    let sessionId: V2SessionID
    let emittedAt: String
    let payload: JSONValue
    /// Client receipt time, excluded from the wire contract. Used to order refresh barriers.
    var receivedAt: Date = Date()
    /// Size of the decoded wire frame. Bounds the shared stream's replay
    /// buffer; client bookkeeping, never encoded or part of the contract.
    var wireBytes: Int = 0

    enum CodingKeys: String, CodingKey {
        case protocolVersion, eventId, sequence, cursor, type, sessionId, emittedAt, payload
    }

    var id: String { eventId }

    var isLiveProjection: Bool {
        ["session.meta.updated", "runtime.state.updated", "runtime.capability.updated", "runtime.catalog.updated"].contains(type)
    }

    /// A client-side recovery signal for transports that do not carry the
    /// server's `session.subscribed` frame (the unified stream). It uses the
    /// wire protocol version the server emits and is never persisted or sent.
    static func subscribedMarker(sessionId: V2SessionID, at date: Date = Date()) -> V2SessionEvent {
        V2SessionEvent(
            protocolVersion: "1.0",
            eventId: "client-subscribed-\(sessionId)",
            sequence: 0,
            cursor: "seq:0",
            type: "session.subscribed",
            sessionId: sessionId,
            emittedAt: date.ISO8601Format(),
            payload: .null,
            receivedAt: date
        )
    }
}

nonisolated struct V2EventRecoveryResponse: Decodable, Hashable {
    let events: [V2SessionEvent]
    let nextCursor: String
    let snapshotRequired: Bool
    let serverTime: String
}

nonisolated struct V2DashboardSnapshot: Decodable, Hashable {
    let type: String
    let connectors: [V2Connector]
    let projects: [V2Project]
    let sessionPages: V2DashboardSessionPages
    let sessions: [V2SessionMeta]
    let serverTime: String
}
