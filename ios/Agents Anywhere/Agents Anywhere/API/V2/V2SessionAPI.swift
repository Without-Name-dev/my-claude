import Foundation

protocol V2SessionAPIProtocol {
    func setTakeover(sessionId: V2SessionID, enabled: Bool) async throws -> V2SessionTakeoverResponse
    func sync(sessionId: V2SessionID) async throws -> V2RuntimeActionResponse
    /// Asks the desktop connector to open this conversation in a terminal.
    func openRuntime(sessionId: V2SessionID) async throws -> V2RuntimeOpenResponse
    /// Asks the desktop connector to stop the terminal running this
    /// conversation, so the phone can restart it.
    func closeRuntime(sessionId: V2SessionID) async throws -> V2RuntimeOpenResponse
    func listSessions(archived: Bool, cursor: String?) async throws -> V2SessionListResponse
    func sessionInventory() async throws -> V2SessionInventoryResponse
    func sessionMeta(sessionId: V2SessionID) async throws -> V2SessionMetaResponse
    func patchSessionMeta(sessionId: V2SessionID, request: V2SessionMetaPatchRequest) async throws -> V2SessionMetaResponse
    func createSession(request: V2SessionCreateRequest) async throws -> V2SessionCreateResponse
    func createAndStartSession(request: V2SessionCreateAndStartRequest) async throws -> V2SessionCreateResponse
    func markRead(sessionIds: [V2SessionID]) async throws -> V2SessionBulkActionResponse
    func archive(sessionIds: [V2SessionID]) async throws -> V2SessionBulkActionResponse
    func unarchive(sessionIds: [V2SessionID]) async throws -> V2SessionBulkActionResponse
    func pinMessage(sessionId: V2SessionID, messageId: String) async throws -> V2SessionMetaResponse
    func unpinMessage(sessionId: V2SessionID, messageId: String) async throws -> V2SessionMetaResponse
    func snapshot(sessionId: V2SessionID, limit: Int) async throws -> V2SessionSnapshot
    func activity(sessionId: V2SessionID) async throws -> V2AgentActivity
    func latestTimeline(sessionId: V2SessionID, limit: Int) async throws -> V2SessionTimelinePage
    func timelineChanges(sessionId: V2SessionID, afterSeq: Int, limit: Int) async throws -> V2SessionTimelinePage
    func timelineHistory(sessionId: V2SessionID, beforeOrderSeq: Int, limit: Int) async throws -> V2SessionTimelinePage
    /// Leaves a scroll trace on the server for later inspection.
    func uploadDiagnostics(sessionId: V2SessionID, trace: String, capturedAt: Int64,
                           appVersion: String, build: String, device: String) async throws
}

struct V2SessionAPI: V2SessionAPIProtocol {
    let transport: any HTTPTransport

    func sessionInventory() async throws -> V2SessionInventoryResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2SessionInventoryResponse>(method: .get, path: "/sessions/list"))
    }

    func listSessions(archived: Bool = false, cursor: String? = nil) async throws -> V2SessionListResponse {
        var query = [URLQueryItem(name: "archived", value: String(archived)), URLQueryItem(name: "limit", value: "100")]
        if let cursor { query.append(URLQueryItem(name: "cursor", value: cursor)) }
        let request = HTTPRequest<EmptyRequestBody, V2SessionListResponse>(method: .get, path: "/sessions", queryItems: query)
        return try await transport.send(request)
    }

    func sessionMeta(sessionId: V2SessionID) async throws -> V2SessionMetaResponse {
        let request = HTTPRequest<EmptyRequestBody, V2SessionMetaResponse>(
            method: .get,
            path: sessionPath(sessionId, suffix: "meta")
        )
        return try await transport.send(request)
    }

    func patchSessionMeta(
        sessionId: V2SessionID,
        request body: V2SessionMetaPatchRequest
    ) async throws -> V2SessionMetaResponse {
        let request = HTTPRequest<V2SessionMetaPatchRequest, V2SessionMetaResponse>(
            method: .patch,
            path: sessionPath(sessionId, suffix: "meta"),
            body: body
        )
        return try await transport.send(request)
    }

    func createSession(request body: V2SessionCreateRequest) async throws -> V2SessionCreateResponse {
        let request = HTTPRequest<V2SessionCreateRequest, V2SessionCreateResponse>(
            method: .post,
            path: "/sessions",
            body: body
        )
        return try await transport.send(request)
    }

    func createAndStartSession(request body: V2SessionCreateAndStartRequest) async throws -> V2SessionCreateResponse {
        let request = HTTPRequest<V2SessionCreateAndStartRequest, V2SessionCreateResponse>(
            method: .post,
            path: "/sessions/create-and-start",
            body: body
        )
        return try await transport.send(request)
    }

    func markRead(sessionIds: [V2SessionID]) async throws -> V2SessionBulkActionResponse {
        try await bulkAction(path: "/sessions/read", sessionIds: sessionIds)
    }

    func archive(sessionIds: [V2SessionID]) async throws -> V2SessionBulkActionResponse {
        try await bulkAction(path: "/sessions/archive", sessionIds: sessionIds)
    }

    func unarchive(sessionIds: [V2SessionID]) async throws -> V2SessionBulkActionResponse {
        try await bulkAction(path: "/sessions/unarchive", sessionIds: sessionIds)
    }

    func snapshot(sessionId: V2SessionID, limit: Int) async throws -> V2SessionSnapshot {
        let request = HTTPRequest<EmptyRequestBody, V2SessionSnapshot>(
            method: .get,
            path: sessionPath(sessionId, suffix: "snapshot"),
            queryItems: [URLQueryItem(name: "limit", value: String(limit))]
        )
        return try await transport.send(request)
    }

    /// The server's process-memory registry of subagents running now. The live
    /// event carries only changes, so this is the resync after a reconnect.
    func activity(sessionId: V2SessionID) async throws -> V2AgentActivity {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2AgentActivity>(
            method: .get,
            path: sessionPath(sessionId, suffix: "activity")
        ))
    }

    func latestTimeline(sessionId: V2SessionID, limit: Int) async throws -> V2SessionTimelinePage {
        try await timeline(sessionId: sessionId, mode: .latest, limit: limit)
    }

    func timelineChanges(sessionId: V2SessionID, afterSeq: Int, limit: Int) async throws -> V2SessionTimelinePage {
        try await timeline(sessionId: sessionId, mode: .changes, afterSeq: afterSeq, limit: limit)
    }

    func timelineHistory(
        sessionId: V2SessionID,
        beforeOrderSeq: Int,
        limit: Int
    ) async throws -> V2SessionTimelinePage {
        try await timeline(
            sessionId: sessionId,
            mode: .history,
            beforeOrderSeq: beforeOrderSeq,
            limit: limit
        )
    }

    func setTakeover(sessionId: V2SessionID, enabled: Bool) async throws -> V2SessionTakeoverResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2SessionTakeoverResponse>(
            method: enabled ? .post : .delete, path: sessionPath(sessionId, suffix: "takeover")
        ))
    }

    func sync(sessionId: V2SessionID) async throws -> V2RuntimeActionResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2RuntimeActionResponse>(
            method: .post, path: sessionPath(sessionId, suffix: "sync")
        ))
    }

    func openRuntime(sessionId: V2SessionID) async throws -> V2RuntimeOpenResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2RuntimeOpenResponse>(
            method: .post, path: sessionPath(sessionId, suffix: "runtime/open")
        ))
    }

    func closeRuntime(sessionId: V2SessionID) async throws -> V2RuntimeOpenResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2RuntimeOpenResponse>(
            method: .post, path: sessionPath(sessionId, suffix: "runtime/close")
        ))
    }

    func pinMessage(sessionId: V2SessionID, messageId: String) async throws -> V2SessionMetaResponse {
        struct PinRequest: Encodable {
            let messageId: String
        }
        let request = HTTPRequest<PinRequest, V2SessionMetaResponse>(
            method: .post,
            path: sessionPath(sessionId, suffix: "messages/pin"),
            body: PinRequest(messageId: messageId)
        )
        return try await transport.send(request)
    }

    func unpinMessage(sessionId: V2SessionID, messageId: String) async throws -> V2SessionMetaResponse {
        struct UnpinRequest: Encodable {
            let messageId: String
        }
        let request = HTTPRequest<UnpinRequest, V2SessionMetaResponse>(
            method: .post,
            path: sessionPath(sessionId, suffix: "messages/unpin"),
            body: UnpinRequest(messageId: messageId)
        )
        return try await transport.send(request)
    }

    private func bulkAction(path: String, sessionIds: [V2SessionID]) async throws -> V2SessionBulkActionResponse {
        let request = HTTPRequest<[V2SessionID], V2SessionBulkActionResponse>(
            method: .post,
            path: path,
            body: sessionIds
        )
        return try await transport.send(request)
    }

    private func timeline(
        sessionId: V2SessionID,
        mode: V2TimelineMode,
        afterSeq: Int? = nil,
        beforeOrderSeq: Int? = nil,
        limit: Int
    ) async throws -> V2SessionTimelinePage {
        var queryItems = [
            URLQueryItem(name: "mode", value: mode.rawValue),
            URLQueryItem(name: "limit", value: String(limit)),
        ]
        if let afterSeq {
            queryItems.append(URLQueryItem(name: "afterSeq", value: String(afterSeq)))
        }
        if let beforeOrderSeq {
            queryItems.append(URLQueryItem(name: "beforeOrderSeq", value: String(beforeOrderSeq)))
        }
        let request = HTTPRequest<EmptyRequestBody, V2SessionTimelinePage>(
            method: .get,
            path: sessionPath(sessionId, suffix: "timeline"),
            queryItems: queryItems
        )
        return try await transport.send(request)
    }

    func uploadDiagnostics(sessionId: V2SessionID, trace: String, capturedAt: Int64,
                           appVersion: String, build: String, device: String) async throws {
        struct Body: Encodable {
            let trace: String
            let capturedAt: Int64
            let appVersion: String
            let build: String
            let device: String
        }
        struct Response: Decodable {
            let stored: Bool
            let serverTime: String
        }
        let body = Body(trace: trace, capturedAt: capturedAt, appVersion: appVersion, build: build, device: device)
        let request = HTTPRequest<Body, Response>(
            method: .post,
            path: sessionPath(sessionId, suffix: "diagnostics"),
            body: body
        )
        _ = try await transport.send(request)
    }

    private func sessionPath(_ sessionId: V2SessionID, suffix: String) -> String {
        "/sessions/\(sessionId.v2URLPathComponentEncoded)/\(suffix)"
    }
}
