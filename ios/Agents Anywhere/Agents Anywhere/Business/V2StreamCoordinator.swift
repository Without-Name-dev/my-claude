import Foundation

/// Failures the unified stream reports before it can serve any consumer. After
/// the first failed connection the coordinator stops trying for this process and
/// the services keep using the legacy per-session and dashboard sockets.
nonisolated enum V2StreamError: Error, Hashable, LocalizedError {
    case unavailable
    case readyTimeout
    case invalidHandshake
    case disconnected

    var errorDescription: String? {
        switch self {
        case .unavailable: String(localized: "The unified realtime stream is unavailable.")
        case .readyTimeout: String(localized: "The realtime stream did not become ready in time.")
        case .invalidHandshake: String(localized: "The realtime stream sent an unexpected first frame.")
        case .disconnected: String(localized: "The realtime stream disconnected.")
        }
    }
}

/// Owns the single unified websocket for an authenticated account. Every
/// dashboard and session consumer shares it: dashboard snapshots fan out to one
/// set of observers, session envelopes fan out per session ID, and a short ring
/// buffer lets a session view opened later replay the newest events.
@MainActor
final class V2StreamCoordinator {
    private enum State: Equatable {
        case idle
        case connecting
        case connected
        case fallback
    }

    private static let maximumBufferedEvents = 50
    private static let maximumBufferedSessions = 24
    /// One failed first connection disables the unified endpoint for the whole
    /// process. Consumers fall back to the legacy sockets and never retry.
    private static var processFallbackEngaged = false

    private let realtimeAPI: any V2RealtimeAPIProtocol
    private let streamAPI: V2StreamAPI
    private let clientId: String
    private let readyTimeout: Duration

    private var state = State.idle
    private var connectionTask: Task<Void, Never>?
    private var dashboardObservers: [UUID: AsyncThrowingStream<V2DashboardSnapshot, Error>.Continuation] = [:]
    private var sessionObservers: [V2SessionID: [UUID: AsyncThrowingStream<V2SessionEvent, Error>.Continuation]] = [:]
    private var sessionBuffers: [V2SessionID: [V2SessionEvent]] = [:]
    private var bufferOrder: [V2SessionID] = []
    private var lastDashboardSnapshot: V2DashboardSnapshot?
    private var didRequestPrefetch = false
    private var hasEverConnected = false
    private var isInvalidated = false

    /// Called once, when the first dashboard snapshot arrives.
    var prefetchSessions: (([V2SessionMeta]) -> Void)?

    init(
        realtimeAPI: any V2RealtimeAPIProtocol,
        streamAPI: V2StreamAPI,
        clientId: String = "ios-stream-\(UUID().uuidString)",
        readyTimeout: Duration = .seconds(5)
    ) {
        self.realtimeAPI = realtimeAPI
        self.streamAPI = streamAPI
        self.clientId = clientId
        self.readyTimeout = readyTimeout
    }

    /// True when the unified endpoint is known to be unusable. Services check
    /// this synchronously so a fallback never races a new connection attempt.
    var isUsingFallback: Bool {
        isInvalidated || Self.processFallbackEngaged || state == .fallback
    }

    /// A dashboard snapshot stream. The first use waits for `stream.ready` and
    /// throws if the endpoint is unusable, so the caller can fall back to
    /// `/api/v2/dashboard/ws` exactly as before. Once the socket has been
    /// established, consumers join the existing fan-out even while it reconnects.
    func dashboardUpdates() async throws -> AsyncThrowingStream<V2DashboardSnapshot, Error> {
        try await awaitReady()
        let observerID = UUID()
        let (stream, continuation) = AsyncThrowingStream<V2DashboardSnapshot, Error>.makeStream(
            bufferingPolicy: .bufferingNewest(1)
        )
        continuation.onTermination = { [weak self] _ in
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.dashboardObservers.removeValue(forKey: observerID)
            }
        }
        dashboardObservers[observerID] = continuation
        if let snapshot = lastDashboardSnapshot {
            continuation.yield(snapshot)
        }
        return stream
    }

    /// A session's event stream. Replays the buffered tail first so a view that
    /// opens after the events arrived does not have to refetch them.
    func sessionUpdates(id: V2SessionID) async throws -> AsyncThrowingStream<V2SessionEvent, Error> {
        try await awaitReady()
        let observerID = UUID()
        let (stream, continuation) = AsyncThrowingStream<V2SessionEvent, Error>.makeStream(
            bufferingPolicy: .bufferingNewest(256)
        )
        continuation.onTermination = { [weak self] _ in
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.removeSessionObserver(id: id, observerID: observerID)
            }
        }
        sessionObservers[id, default: [:]][observerID] = continuation
        if let buffered = sessionBuffers[id] {
            for event in buffered { continuation.yield(event) }
        }
        return stream
    }

    /// Closes the socket and every observer. Called when the owning services
    /// lifetime ends, e.g. on sign-out.
    func invalidate() {
        isInvalidated = true
        connectionTask?.cancel()
        connectionTask = nil
        state = .idle
        for continuation in dashboardObservers.values { continuation.finish() }
        dashboardObservers.removeAll()
        for observers in sessionObservers.values {
            for continuation in observers.values { continuation.finish() }
        }
        sessionObservers.removeAll()
        sessionBuffers.removeAll()
        bufferOrder.removeAll()
        lastDashboardSnapshot = nil
        prefetchSessions = nil
    }

    // MARK: - Readiness

    /// Waits for the ready frame without a continuation table: the connection
    /// task owns state and this poll only observes it. The loop is short-lived
    /// (a few seconds at app start) and keeps the failure path obvious.
    private func awaitReady() async throws {
        if isInvalidated || Self.processFallbackEngaged || state == .fallback {
            throw V2StreamError.unavailable
        }
        // Once the unified socket has been established, its own reconnect loop
        // owns recovery. New consumers attach to the existing fan-out instead of
        // opening a second legacy socket while it reconnects; doing otherwise
        // would run two sockets for one stream and could never return to the
        // unified path. The next `.ready` re-signals every session subscriber.
        if hasEverConnected {
            if state == .idle { start() }
            return
        }
        switch state {
        case .connected: return
        case .fallback: throw V2StreamError.unavailable
        case .idle: start()
        case .connecting: break
        }
        let deadline = ContinuousClock.now.advanced(by: readyTimeout)
        while ContinuousClock.now < deadline {
            if isInvalidated { throw V2StreamError.unavailable }
            switch state {
            case .connected: return
            case .fallback: throw V2StreamError.unavailable
            case .idle, .connecting:
                do { try await Task.sleep(for: .milliseconds(50)) } catch { throw CancellationError() }
            }
        }
        // If the socket came up during this wait, a later drop is a normal
        // reconnect, not a reason to disable the endpoint for the process.
        if hasEverConnected {
            if state == .idle { start() }
            return
        }
        // Sign-out can cancel the connection while this poll is waiting. Do not
        // turn that teardown into a process-wide fallback.
        if isInvalidated { throw V2StreamError.unavailable }
        engageFallback()
        throw V2StreamError.readyTimeout
    }

    private func start() {
        guard state == .idle, connectionTask == nil else { return }
        state = .connecting
        connectionTask = Task { [weak self] in
            guard let self else { return }
            await self.runConnectionLoop()
        }
    }

    // MARK: - Connection

    private func runConnectionLoop() async {
        defer {
            connectionTask = nil
            if state != .fallback { state = .idle }
        }
        var attempt = 0
        var connectedOnce = false
        while !Task.isCancelled {
            if Self.processFallbackEngaged {
                // Another coordinator already settled the process on the legacy
                // sockets. Finish this one's observers too; otherwise their
                // streams never end and they wait for frames that cannot come.
                engageFallback()
                return
            }
            do {
                let ticket = try await realtimeAPI.ticket(clientId: clientId, scope: .dashboard)
                let frames = try streamAPI.stream(ticket: ticket.ticket)
                var sawReady = false
                state = .connecting
                for try await frame in frames {
                    switch frame {
                    case .keepalive:
                        break
                    case .ready:
                        if !sawReady {
                            let isReconnect = hasEverConnected
                            sawReady = true
                            connectedOnce = true
                            hasEverConnected = true
                            attempt = 0
                            state = .connected
                            // A reconnect gets a brand new socket and the server
                            // does not replay what the old one missed, so tell
                            // every open session to recover and mark it live.
                            if isReconnect { signalSessionObservers() }
                        }
                    case let .dashboard(snapshot):
                        // Ordering is the contract: anything before ready means
                        // this is not the unified endpoint.
                        guard sawReady || connectedOnce else { throw V2StreamError.invalidHandshake }
                        receiveDashboard(snapshot)
                    case let .session(event):
                        guard sawReady || connectedOnce else { throw V2StreamError.invalidHandshake }
                        receiveSession(event)
                    }
                }
                throw V2StreamError.disconnected
            } catch {
                if Task.isCancelled {
                    if state != .fallback { state = .idle }
                    return
                }
                guard connectedOnce else {
                    engageFallback()
                    return
                }
                state = .connecting
                attempt += 1
                do { try await Task.sleep(for: reconnectDelay(attempt: attempt)) }
                catch {
                    if state != .fallback { state = .idle }
                    return
                }
            }
        }
    }

    private func reconnectDelay(attempt: Int) -> Duration {
        let capped = min(pow(2.0, Double(min(attempt, 5))), 30.0)
        let jitter = Double.random(in: 0...(capped * 0.4))
        return .seconds(capped + jitter)
    }

    private func engageFallback() {
        Self.processFallbackEngaged = true
        state = .fallback
        connectionTask?.cancel()
        connectionTask = nil
        for continuation in dashboardObservers.values { continuation.finish() }
        dashboardObservers.removeAll()
        for observers in sessionObservers.values {
            for continuation in observers.values { continuation.finish() }
        }
        sessionObservers.removeAll()
    }

    // MARK: - Fan-out

    private func receiveDashboard(_ snapshot: V2DashboardSnapshot) {
        guard snapshot.type == "dashboard.snapshot" else { return }
        lastDashboardSnapshot = snapshot
        for continuation in dashboardObservers.values {
            continuation.yield(snapshot)
        }
        guard !didRequestPrefetch else { return }
        didRequestPrefetch = true
        prefetchSessions?(snapshot.sessions)
    }

    private func receiveSession(_ event: V2SessionEvent) {
        buffer(event)
        guard let observers = sessionObservers[event.sessionId] else { return }
        for continuation in observers.values {
            continuation.yield(event)
        }
    }

    /// Every re-established socket needs the same recovery kick the first
    /// subscribe gets: the repository reconciles from its cursor and returns to
    /// `.connected`. The broker itself carries no `session.subscribed` frame.
    private func signalSessionObservers() {
        for (id, observers) in sessionObservers {
            let marker = V2SessionEvent.subscribedMarker(sessionId: id)
            for continuation in observers.values {
                continuation.yield(marker)
            }
        }
    }

    private func buffer(_ event: V2SessionEvent) {
        let id = event.sessionId
        var events = sessionBuffers[id] ?? []
        if events.last?.eventId != event.eventId {
            events.append(event)
            if events.count > Self.maximumBufferedEvents {
                events.removeFirst(events.count - Self.maximumBufferedEvents)
            }
        }
        sessionBuffers[id] = events
        bufferOrder.removeAll { $0 == id }
        bufferOrder.append(id)
        while bufferOrder.count > Self.maximumBufferedSessions {
            let evicted = bufferOrder.removeFirst()
            sessionBuffers.removeValue(forKey: evicted)
        }
    }

    private func removeSessionObserver(id: V2SessionID, observerID: UUID) {
        sessionObservers[id]?.removeValue(forKey: observerID)
        if sessionObservers[id]?.isEmpty == true {
            sessionObservers.removeValue(forKey: id)
        }
    }
}
