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
    /// A handful of timeline snapshots can dwarf fifty token deltas, so a new
    /// observer's replay is bounded by bytes as well as event count.
    private static let maximumBufferedBytes = 512 * 1024
    /// One failed first connection disables the unified endpoint for the whole
    /// process. Consumers fall back to the legacy sockets and never retry.
    private static var processFallbackEngaged = false

    private let realtimeAPI: any V2RealtimeAPIProtocol
    private let streamAPI: V2StreamAPI
    private let clientId: String
    private let readyTimeout: Duration

    private var state = State.idle
    private var connectionTask: Task<Void, Never>?
    private var connectionGeneration = 0
    private var dashboardObservers: [UUID: AsyncThrowingStream<V2DashboardSnapshot, Error>.Continuation] = [:]
    private var sessionObservers: [V2SessionID: [UUID: AsyncThrowingStream<V2SessionEvent, Error>.Continuation]] = [:]
    private var sessionBuffers: [V2SessionID: [V2SessionEvent]] = [:]
    private var sessionBufferBytes: [V2SessionID: Int] = [:]
    private var bufferOrder: [V2SessionID] = []
    private var lastDashboardSnapshot: V2DashboardSnapshot?
    private var didRequestPrefetch = false
    private var hasEverConnected = false
    private var isInvalidated = false
    private var isSuspended = false
    private var isOffline = false
    /// When the shared socket last delivered any frame. While the socket is
    /// down the retry loop is silent; without this a persistent failure left
    /// every page marked connected with no events behind it, forever.
    private var lastFrameAt = ContinuousClock.now
    private var connectionStartedAt = ContinuousClock.now
    private var stallWatchdog: Task<Void, Never>?

    /// Called once, when the first dashboard snapshot arrives.
    var prefetchSessions: (([V2SessionMeta]) -> Void)?

    /// Whether the shared socket is ready right now. Consumers that synthesise
    /// a "subscribed" signal must ask first: claiming a subscription while the
    /// stream reconnects reports a page as live with nothing behind it.
    var isReady: Bool { state == .connected && !isInvalidated }

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

    /// Backgrounding drops the socket; returning to the foreground reconnects
    /// and re-signals every session subscriber so they recover from their
    /// cursors instead of waiting for the heartbeat watchdog.
    func setActive(_ active: Bool) {
        let suspended = !active
        guard isSuspended != suspended else { return }
        isSuspended = suspended
        if isSuspended { stopConnection() } else { restartIfNeeded() }
    }

    /// Path changes are a scheduling hint. Offline drops the socket, and the
    /// next usable path reconnects without settling the process on the legacy
    /// sockets: a failed socket while offline says nothing about the endpoint.
    func updateConnectivity(_ status: V2NetworkStatus) {
        let offline = status.availability == .offline
        guard isOffline != offline else { return }
        isOffline = offline
        if offline { stopConnection() } else { restartIfNeeded() }
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
        // A resume that raced the registration has no observer to restart for.
        start()
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
        // Register and signal under the same actor turn. During a reconnect,
        // the next ready frame signals this observer instead.
        if isReady { continuation.yield(V2SessionEvent.subscribedMarker(sessionId: id)) }
        if let buffered = sessionBuffers[id] {
            for event in buffered {
                if case .dropped = continuation.yield(event) {
                    continuation.finish(throwing: HTTPError.streamOverflow)
                    break
                }
            }
        }
        start()
        return stream
    }

    /// Closes the socket and every observer. Called when the owning services
    /// lifetime ends, e.g. on sign-out.
    func invalidate() {
        isInvalidated = true
        // A new sign-in deserves a fresh verdict on the unified endpoint; the
        // static below would otherwise keep every later session on the legacy
        // sockets for the life of the process.
        Self.processFallbackEngaged = false
        connectionGeneration &+= 1
        connectionTask?.cancel()
        connectionTask = nil
        stallWatchdog?.cancel()
        stallWatchdog = nil
        state = .idle
        for continuation in dashboardObservers.values { continuation.finish() }
        dashboardObservers.removeAll()
        for observers in sessionObservers.values {
            for continuation in observers.values { continuation.finish() }
        }
        sessionObservers.removeAll()
        sessionBuffers.removeAll()
        sessionBufferBytes.removeAll()
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
            // A suspended app or an unusable path must not spend the ready
            // window and then settle the process on the legacy sockets. The
            // next attempt is free to use the unified endpoint again.
            if isSuspended || isOffline { throw V2StreamError.unavailable }
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
        // A slow first connect is not evidence that the endpoint is missing.
        // Settling the whole process on the legacy sockets here meant one slow
        // network moment (cold radio, a captive portal) disabled the unified
        // stream until the app restarted. Only a rejected handshake engages
        // the fallback (the connection loop decides that); this consumer gets
        // a retryable error instead.
        throw V2StreamError.readyTimeout
    }

    private func start() {
        guard state == .idle, connectionTask == nil, !isSuspended, !isOffline,
              !isInvalidated, !Self.processFallbackEngaged else { return }
        state = .connecting
        connectionStartedAt = .now
        connectionGeneration &+= 1
        let generation = connectionGeneration
        connectionTask = Task { [weak self] in
            guard let self else { return }
            await self.runConnectionLoop(generation: generation)
        }
        startStallWatchdog()
    }

    /// Watches the shared socket, not the individual requests: when it has been
    /// down without a single frame for a minute, the endpoint is not coming
    /// back on its own. Consumers are moved to the legacy sockets instead of
    /// sitting on a fan-out that will never yield again while every page still
    /// claims to be connected.
    private func startStallWatchdog() {
        guard stallWatchdog == nil else { return }
        stallWatchdog = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(5))
                guard !Task.isCancelled, let self else { return }
                self.checkForStall()
            }
        }
    }

    private func checkForStall() {
        guard !isInvalidated, !isSuspended, !isOffline, state != .fallback else { return }
        // A connected socket has its own heartbeat watchdog; this one only
        // measures how long the endpoint has been failing to come up.
        guard state != .connected else { return }
        // A reconnect that started a moment ago (a resume, a path change) gets
        // its full window; only a failure older than the window counts.
        let since = lastFrameAt > connectionStartedAt ? lastFrameAt : connectionStartedAt
        guard ContinuousClock.now - since >= .seconds(60) else { return }
        TimelineTrace.shared.record("STREAM.stall", "no frames for 60s; using legacy sockets")
        engageFallback()
        stallWatchdog?.cancel()
        stallWatchdog = nil
    }

    /// Drops the socket without declaring the endpoint unusable. Observers stay
    /// attached; a resume or a recovered path starts a fresh connection.
    private func stopConnection() {
        connectionGeneration &+= 1
        connectionTask?.cancel()
        connectionTask = nil
        if state != .fallback { state = .idle }
        // Buffered frames belong to the connection that produced them. Replaying
        // them after a reconnect re-applied a state the server had already
        // superseded: status frames carry the session's current seq, so an old
        // "stopping" copy still passed the sequence check and overwrote a fresh
        // "idle". A stale dashboard snapshot did the same to the session list.
        clearConnectionBuffers()
    }

    private func clearConnectionBuffers() {
        sessionBuffers.removeAll()
        sessionBufferBytes.removeAll()
        bufferOrder.removeAll()
        lastDashboardSnapshot = nil
    }

    /// Reconnects only for a live consumer. Without one the next subscribe
    /// starts the socket itself.
    private func restartIfNeeded() {
        guard !isInvalidated, !isSuspended, !isOffline, state == .idle,
              connectionTask == nil, !Self.processFallbackEngaged,
              !dashboardObservers.isEmpty || !sessionObservers.isEmpty else { return }
        start()
    }

    // MARK: - Connection

    private func runConnectionLoop(generation: Int) async {
        defer {
            // A newer connection may already own the task and state.
            if connectionGeneration == generation {
                connectionTask = nil
                if state != .fallback { state = .idle }
            }
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
                guard connectionGeneration == generation, !Task.isCancelled else { return }
                let frames = try streamAPI.stream(ticket: ticket.ticket)
                var sawReady = false
                state = .connecting
                for try await frame in frames {
                    guard connectionGeneration == generation, !Task.isCancelled else { return }
                    lastFrameAt = .now
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
                            TimelineTrace.shared.record("STREAM.ready", "reconnect=\(isReconnect)")
                            // A reconnect gets a brand new socket and the server
                            // does not replay what the old one missed, so tell
                            // every open session to recover and mark it live.
                            signalSessionObservers()
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
                guard connectionGeneration == generation else { return }
                if Task.isCancelled {
                    if state != .fallback { state = .idle }
                    return
                }
                // Retry only failures a new connection can fix. Expired
                // credentials, a missing endpoint or a peer that never spoke
                // the unified protocol are terminal for this process; those
                // consumers fall back to the legacy sockets instead of
                // reconnecting forever.
                //
                // This used to require a successful connection first, but that
                // flag resets with every loop: an app launched offline (or
                // resumed after a suspend) fell back permanently on the first
                // transient failure, and every later login inherited it.
                guard Self.isRetryable(error) else {
                    TimelineTrace.shared.record("STREAM.fallback", "error=\(V2ClientFailure(error).kind)")
                    engageFallback()
                    return
                }
                TimelineTrace.shared.record("STREAM.retry",
                    "attempt=\(attempt) kind=\(V2ClientFailure(error).kind)")
                clearConnectionBuffers()
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

    /// A reconnect can only repair transport failures. Auth/404 and a bad
    /// handshake keep failing until the endpoint or credentials change.
    private static func isRetryable(_ error: Error) -> Bool {
        if let error = error as? V2StreamError {
            return error == .disconnected || error == .readyTimeout
        }
        let failure = V2ClientFailure(error)
        // A cancellation is not evidence that the endpoint is broken: the app
        // backgrounding or a system-initiated socket teardown used to settle
        // the process on the legacy sockets for the rest of the session.
        if failure.kind == .cancelled { return true }
        return failure.permitsAutomaticReconnect
    }

    private func reconnectDelay(attempt: Int) -> Duration {
        let capped = min(pow(2.0, Double(min(attempt, 5))), 30.0)
        let jitter = Double.random(in: 0...(capped * 0.4))
        return .seconds(capped + jitter)
    }

    private func engageFallback() {
        Self.processFallbackEngaged = true
        state = .fallback
        connectionGeneration &+= 1
        connectionTask?.cancel()
        connectionTask = nil
        stallWatchdog?.cancel()
        stallWatchdog = nil
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
            if case .dropped = continuation.yield(event) {
                continuation.finish(throwing: HTTPError.streamOverflow)
            }
        }
    }

    /// Every re-established socket needs the same recovery kick the first
    /// subscribe gets: the repository reconciles from its cursor and returns to
    /// `.connected`. The broker itself carries no `session.subscribed` frame.
    private func signalSessionObservers() {
        for (id, observers) in sessionObservers {
            let marker = V2SessionEvent.subscribedMarker(sessionId: id)
            for continuation in observers.values {
                if case .dropped = continuation.yield(marker) {
                    continuation.finish(throwing: HTTPError.streamOverflow)
                }
            }
        }
    }

    private func buffer(_ event: V2SessionEvent) {
        let id = event.sessionId
        var events = sessionBuffers[id] ?? []
        var bytes = sessionBufferBytes[id] ?? 0
        if events.last?.eventId != event.eventId {
            events.append(event)
            bytes += event.wireBytes
            while !events.isEmpty,
                  events.count > Self.maximumBufferedEvents || bytes > Self.maximumBufferedBytes {
                bytes -= events.removeFirst().wireBytes
            }
        }
        sessionBuffers[id] = events
        sessionBufferBytes[id] = bytes
        bufferOrder.removeAll { $0 == id }
        bufferOrder.append(id)
        while bufferOrder.count > Self.maximumBufferedSessions {
            let evicted = bufferOrder.removeFirst()
            sessionBuffers.removeValue(forKey: evicted)
            sessionBufferBytes.removeValue(forKey: evicted)
        }
    }

    private func removeSessionObserver(id: V2SessionID, observerID: UUID) {
        sessionObservers[id]?.removeValue(forKey: observerID)
        if sessionObservers[id]?.isEmpty == true {
            sessionObservers.removeValue(forKey: id)
        }
    }
}
