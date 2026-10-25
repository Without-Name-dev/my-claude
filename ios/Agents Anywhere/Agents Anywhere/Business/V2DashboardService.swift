import Foundation

struct V2DashboardData: Hashable {
    let connectors: [V2Connector]
    let projects: [V2Project]
    let sessions: [V2SessionMeta]
}

struct V2DashboardService {
    let connectorAPI: any V2ConnectorAPIProtocol
    let projectAPI: V2ProjectAPI
    let sessionAPI: any V2SessionAPIProtocol
    let realtimeAPI: any V2RealtimeAPIProtocol
    /// Shared unified stream. Nil for HTTP-only callers and in tests.
    var streamCoordinator: V2StreamCoordinator? = nil

    /// Reads the durable dashboard resources concurrently.
    func load() async throws -> V2DashboardData {
        async let connectors = connectorAPI.listConnectors()
        async let projects = projectAPI.list()
        async let inventory = sessionAPI.sessionInventory()
        let (connectorResponse, projectResponse, sessionResponse) = try await (connectors, projects, inventory)
        return V2DashboardData(
            connectors: connectorResponse.connectors,
            projects: projectResponse.projects, sessions: sessionResponse.sessions
        )
    }

    /// Opens the dashboard push channel. The unified stream, when usable, is
    /// shared with every other consumer; otherwise this opens the legacy
    /// dashboard socket exactly as before.
    func updates(clientId: String) async throws -> AsyncThrowingStream<V2DashboardSnapshot, Error> {
        if let streamCoordinator, !streamCoordinator.isUsingFallback {
            do {
                return try await streamCoordinator.dashboardUpdates()
            } catch is CancellationError {
                throw CancellationError()
            } catch {
                if let urlError = error as? URLError, urlError.code == .cancelled { throw error }
                // The coordinator already recorded the process-wide fallback.
            }
        }
        let ticket = try await realtimeAPI.ticket(clientId: clientId, scope: .dashboard)
        return try realtimeAPI.dashboardSnapshots(ticket: ticket.ticket)
    }

    func markRead(sessionIds: [V2SessionID]) async throws -> [V2SessionMeta] {
        try validateSessionIds(sessionIds)
        return try await sessionAPI.markRead(sessionIds: sessionIds).sessions
    }

    func renameSession(sessionId: V2SessionID, title: String) async throws -> V2SessionMeta {
        let trimmedTitle = title.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedTitle.isEmpty else { throw V2BusinessError.emptySessionTitle }
        let request = V2SessionMetaPatchRequest(
            title: trimmedTitle,
            pinned: nil,
            archived: nil
        )
        return try await sessionAPI.patchSessionMeta(sessionId: sessionId, request: request).session
    }

    func setSessionPinned(sessionId: V2SessionID, pinned: Bool) async throws -> V2SessionMeta {
        let request = V2SessionMetaPatchRequest(
            title: nil,
            pinned: pinned,
            archived: nil
        )
        return try await sessionAPI.patchSessionMeta(sessionId: sessionId, request: request).session
    }

    func archive(sessionIds: [V2SessionID]) async throws -> [V2SessionMeta] {
        try validateSessionIds(sessionIds)
        return try await sessionAPI.archive(sessionIds: sessionIds).sessions
    }

    func unarchive(sessionIds: [V2SessionID]) async throws -> [V2SessionMeta] {
        try validateSessionIds(sessionIds)
        return try await sessionAPI.unarchive(sessionIds: sessionIds).sessions
    }

    private func validateSessionIds(_ sessionIds: [V2SessionID]) throws {
        if sessionIds.isEmpty {
            throw V2BusinessError.emptySessionSelection
        }
        if sessionIds.count > 200 { throw V2BusinessError.tooManySessions }
    }
}
