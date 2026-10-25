import Foundation

struct V2SessionDetailService {
    let sessionAPI: any V2SessionAPIProtocol
    let runtimeAPI: any V2RuntimeAPIProtocol
    let realtimeAPI: any V2RealtimeAPIProtocol
    /// Shared unified stream. Nil for HTTP-only callers and in tests.
    var streamCoordinator: V2StreamCoordinator? = nil

    func load(sessionId: V2SessionID, itemLimit: Int = 100) async throws -> V2SessionSnapshot {
        try validatePageSize(itemLimit)
        return try await sessionAPI.snapshot(sessionId: sessionId, limit: itemLimit)
    }

    func loadOlderItems(
        sessionId: V2SessionID,
        beforeOrderSeq: Int,
        limit: Int = 100
    ) async throws -> V2SessionTimelinePage {
        try validatePageSize(limit)
        return try await sessionAPI.timelineHistory(
            sessionId: sessionId,
            beforeOrderSeq: beforeOrderSeq,
            limit: limit
        )
    }

    func latestItems(sessionId: V2SessionID, limit: Int = 100) async throws -> V2SessionTimelinePage {
        try validatePageSize(limit)
        return try await sessionAPI.latestTimeline(sessionId: sessionId, limit: limit)
    }

    func refreshRuntimeState(sessionId: V2SessionID) async throws -> V2RuntimeState {
        try await runtimeAPI.state(sessionId: sessionId).state
    }

    /// The live `runtime.activity` event only reports changes, so a reconnected
    /// view reads the current subagent set from the server's registry.
    func activity(sessionId: V2SessionID) async throws -> [V2AgentRun] {
        try await sessionAPI.activity(sessionId: sessionId).agents
    }

    func sendMessage(
        sessionId: V2SessionID,
        content: String,
        attachmentIds: [V2AttachmentID],
        clientMessageId: String
    ) async throws -> V2RuntimeActionResponse {
        let normalizedContent = content.trimmingCharacters(in: .whitespacesAndNewlines)
        if attachmentIds.count > 10 { throw V2BusinessError.tooManyAttachments(maximum: 10) }
        if normalizedContent.isEmpty, attachmentIds.isEmpty {
            throw V2BusinessError.emptyMessage
        }
        return try await runtimeAPI.sendMessage(
            sessionId: sessionId,
            request: V2RuntimeMessageSendRequest(
                content: normalizedContent,
                attachments: attachmentIds.map { V2AttachmentSendReference(fileId: $0) },
                clientMessageId: clientMessageId
            )
        ).requireSuccess()
    }

    func interrupt(sessionId: V2SessionID) async throws -> V2RuntimeActionResponse {
        try await runtimeAPI.interrupt(sessionId: sessionId).requireSuccess()
    }

    /// Asks the desktop to bring this conversation up in a terminal. The
    /// response is not a runtime action: it says whether the connector was
    /// handed the request, not whether the terminal finished starting.
    func openOnComputer(sessionId: V2SessionID) async throws -> V2RuntimeOpenResult {
        let response = try await sessionAPI.openRuntime(sessionId: sessionId)
        guard response.ok, let result = response.result else {
            throw response.error ?? V2RuntimeError(
                code: nil,
                message: String(localized: "The connector did not accept the open request.")
            )
        }
        return result
    }

    /// Asks the desktop to stop the terminal running this conversation. The
    /// transcript is untouched: the next message opens it again.
    func closeOnComputer(sessionId: V2SessionID) async throws -> V2RuntimeOpenResult {
        let response = try await sessionAPI.closeRuntime(sessionId: sessionId)
        guard response.ok, let result = response.result else {
            throw response.error ?? V2RuntimeError(
                code: nil,
                message: String(localized: "The connector did not accept the close request.")
            )
        }
        return result
    }

    func updateSelection(
        sessionId: V2SessionID,
        scope: V2RuntimeSelectionScope,
        selectionId: V2SelectionID?
    ) async throws -> V2RuntimeState? {
        let response = try await runtimeAPI.updateSelections(
            sessionId: sessionId,
            request: V2RuntimeSelectionUpdateRequest(selections: [scope: selectionId])
        )
        guard response.ok else {
            throw V2RuntimeError(code: response.connectorResult?["error"]?["code"]?.stringValue,
                message: response.connectorResult?["error"]?["message"]?.stringValue ?? String(localized: "The runtime did not accept this selection."))
        }
        return response.state
    }

    /// Opens the session push channel. The unified stream, when usable, serves
    /// every session from one socket; otherwise this opens the legacy
    /// per-session socket exactly as before.
    func updates(
        sessionId: V2SessionID,
        clientId: String
    ) async throws -> AsyncThrowingStream<V2SessionEvent, Error> {
        if let streamCoordinator, !streamCoordinator.isUsingFallback {
            do {
                let events = try await streamCoordinator.sessionUpdates(id: sessionId)
                return Self.withSubscriptionMarker(events, sessionId: sessionId,
                    isReady: streamCoordinator.isReady)
            } catch is CancellationError {
                throw CancellationError()
            } catch {
                if let urlError = error as? URLError, urlError.code == .cancelled { throw error }
                // The coordinator already recorded the process-wide fallback.
            }
        }
        let ticket = try await realtimeAPI.ticket(clientId: clientId, scope: .session(sessionId))
        return try realtimeAPI.sessionEvents(sessionId: sessionId, ticket: ticket.ticket)
    }

    /// Broker envelopes carry no `session.subscribed` frame, but the repository
    /// uses that signal to start recovery and mark the connection live. Re-emit
    /// it ahead of the shared stream without inventing any server payload.
    private static func withSubscriptionMarker(
        _ events: AsyncThrowingStream<V2SessionEvent, Error>,
        sessionId: V2SessionID,
        isReady: Bool
    ) -> AsyncThrowingStream<V2SessionEvent, Error> {
        let (stream, continuation) = AsyncThrowingStream<V2SessionEvent, Error>.makeStream(
            bufferingPolicy: .bufferingNewest(2048)
        )
        let task = Task {
            // A reconnecting socket delivers this marker itself once it is
            // ready again; inventing one now would mark the page live early.
            if isReady {
                continuation.yield(V2SessionEvent.subscribedMarker(sessionId: sessionId))
            }
            do {
                for try await event in events {
                    continuation.yield(event)
                }
                continuation.finish()
            } catch {
                continuation.finish(throwing: error)
            }
        }
        continuation.onTermination = { _ in task.cancel() }
        return stream
    }

    func recover(
        sessionId: V2SessionID,
        after cursor: String
    ) async throws -> V2EventRecoveryResponse {
        try await realtimeAPI.recover(sessionId: sessionId, after: cursor)
    }

    func setTakeover(sessionId: V2SessionID, enabled: Bool) async throws -> V2SessionMeta {
        try await sessionAPI.setTakeover(sessionId: sessionId, enabled: enabled).session
    }

    func sync(sessionId: V2SessionID) async throws -> V2RuntimeActionResponse {
        try await sessionAPI.sync(sessionId: sessionId).requireSuccess()
    }

    /// Recovery replays durable data only. Refresh ephemeral runtime facts separately.
    func liveState(sessionId: V2SessionID) async throws -> V2SessionLiveState {
        async let state = runtimeAPI.state(sessionId: sessionId)
        async let capabilities = runtimeAPI.capabilities(sessionId: sessionId)
        async let notices = runtimeAPI.notices(sessionId: sessionId)
        return try await V2SessionLiveState(
            state: state.state, capabilities: capabilities.capabilitySet, notices: notices.notices
        )
    }

    func catalogs(sessionId: V2SessionID, scopes: Set<String>? = nil) async throws -> V2SessionCatalogs {
        async let model = scopes?.contains("model") != false
            ? runtimeAPI.modelCatalog(sessionId: sessionId).catalog
            : V2ModelCatalog(runtime: "", revision: 0, models: [])
        async let permission = scopes?.contains("permission") != false
            ? runtimeAPI.permissionCatalog(sessionId: sessionId).catalog
            : V2PermissionCatalog(runtime: "", revision: 0, permissions: [])
        return try await V2SessionCatalogs(model: model, permission: permission)
    }

    private func validatePageSize(_ limit: Int) throws {
        if !(1...500).contains(limit) {
            throw V2BusinessError.invalidPageSize
        }
    }
}
