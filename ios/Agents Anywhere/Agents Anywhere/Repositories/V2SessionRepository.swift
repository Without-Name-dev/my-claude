import Foundation

nonisolated struct V2SessionCachePolicy {
    var maximumSessions = 8
    var maximumTimelineItems = 1000
    var catalogLifetime: TimeInterval = 30
}

/// One repository per authenticated server/account. Views observe values and invoke
/// operations; this layer owns request coalescing, bounded caches and live recovery.
@MainActor
final class V2SessionRepository {
    let scope: V2ClientScope
    private let detail: V2SessionDetailService
    private let interactions: V2RuntimeInteractionService
    private let localStore: V2LocalStore?
    private var persistenceTask: Task<Void, Never>?
    private var prefetchTask: Task<Void, Never>?
    private let policy: V2SessionCachePolicy
    private let now: () -> Date
    private let sleep: (Duration) async throws -> Void
    private var entries: [V2SessionID: Entry] = [:]
    /// Sessions handed to a view but not yet observed by it.
    ///
    /// The page takes its model in `body` and starts observing in `.task`; a
    /// prefetch that hydrates eight other sessions can run in between. Evicting
    /// the entry then disposes the model the view is already holding, and the
    /// page can never finish opening. A short lease keeps it alive until the
    /// observer attaches (or the person leaves).
    private var leasedEntries: [V2SessionID: Date] = [:]
    private let leaseDuration: TimeInterval = 15
    private var accessCounter = 0
    private var suspended = false
    private(set) var network = V2NetworkStatus()

    init(
        scope: V2ClientScope,
        detail: V2SessionDetailService,
        interactions: V2RuntimeInteractionService,
        policy: V2SessionCachePolicy = V2SessionCachePolicy(),
        localStore: V2LocalStore? = nil,
        now: @escaping () -> Date = Date.init,
        sleep: @escaping (Duration) async throws -> Void = { try await Task.sleep(for: $0) }
    ) {
        self.scope = scope
        self.detail = detail
        self.interactions = interactions
        self.policy = policy
        self.localStore = localStore
        self.now = now
        self.sleep = sleep
    }

    var cachedSessionIDs: Set<V2SessionID> { Set(entries.keys) }

    func session(id: V2SessionID) -> V2SessionModel {
        let entry = entry(for: id)
        leasedEntries[id] = now()
        evict(protecting: entry)
        return entry.model
    }

    func cached(sessionId: V2SessionID) -> V2SessionData? {
        guard let entry = entries[sessionId] else { return nil }
        touch(entry)
        return entry.projection?.data
    }

    /// Leaves a scroll trace on the server for later inspection. Never throws:
    /// a failed upload must not disturb the conversation.
    func uploadDiagnostics(sessionId: V2SessionID, trace: String) async {
        let info = Bundle.main.infoDictionary
        let version = info?["CFBundleShortVersionString"] as? String ?? "?"
        let build = info?["CFBundleVersion"] as? String ?? "?"
        let device = "\(ProcessInfo.processInfo.hostName) \(ProcessInfo.processInfo.operatingSystemVersionString)"
        try? await detail.uploadDiagnostics(sessionId: sessionId, trace: trace,
            appVersion: version, build: build, device: device)
    }

    /// Returns cached durable content immediately. Observe to keep live facts fresh.
    func load(sessionId: V2SessionID) async throws -> V2SessionData {
        let entry = entry(for: sessionId)
        await restoreCachedSession(id: sessionId)
        try requireCurrent(entry)
        if let data = entry.projection?.data { return data }
        try requireNetwork()
        return try await hydrate(entry)
    }

    /// A page visit starts with the latest 100 records, including cached visits.
    /// Explicit history loads may then accumulate additional 100-record pages.
    func open(sessionId: V2SessionID) async throws -> V2SessionData {
        let entry = entry(for: sessionId)
        _ = try await load(sessionId: sessionId)
        try Task.checkCancellation()
        try requireCurrent(entry)
        entry.readVersion += 1
        entry.historyTask?.cancel()
        entry.historyTask = nil
        // Trim before a possible latest-page request so an offline fallback
        // also stays bounded and cannot remount the entire cached history.
        if entry.projection?.limitToLatest(100) == true {
            TimelineTrace.shared.record("WINDOW.trim", "kept=100 rows=\(entry.projection?.data.items.count ?? 0)")
            emit(entry)
        }
        if entry.projection?.data.hasNewerItems == true {
            return try await loadLatest(sessionId: sessionId, limit: 100)
        }
        return entry.projection!.data
    }

    func stageCreation(_ submission: NewSessionSubmission) {
        let entry = entry(for: submission.session.id)
        entry.projection = V2SessionProjection.placeholder(submission.session, maximumItems: policy.maximumTimelineItems)
        entry.model.stage(submission.pending)
        emit(entry)
    }

    func bindCreation(_ submission: NewSessionSubmission, response: V2SessionCreateResponse) {
        let entry = entry(for: response.session.id)
        if entry.projection == nil {
            entry.projection = V2SessionProjection.placeholder(response.session, maximumItems: policy.maximumTimelineItems)
            entry.needsSnapshot = true
        }
        for (attachment, uploaded) in zip(submission.pending.attachments, response.attachments ?? []) {
            submission.pending.bindUpload(uploaded.reference(sessionID: response.session.id), localID: attachment.id)
        }
        submission.pending.update(.accepted)
        entry.model.stage(submission.pending)
        emit(entry)
        remove(sessionIds: [submission.session.id])
    }

    func restoreCachedSession(id: String) async {
        guard let localStore else { return }
        let entry = entry(for: id)
        guard entry.projection == nil, !entry.hasReadLocal else { return }
        if entry.localReadTask == nil {
            entry.localReadTask = Task { await localStore.restoreSession(id) }
        }
        let saved = await entry.localReadTask?.value
        guard isCurrent(entry), entry.projection == nil, !entry.hasReadLocal else { return }
        entry.hasReadLocal = true; entry.localReadTask = nil
        guard let saved, saved.archive.session.id == id else { return }
        entry.projection = saved.archive.projection(items: saved.items, maximumItems: policy.maximumTimelineItems)
        entry.model.restoreLocal(saved.archive)
        emit(entry, persist: false)
    }

    /// Writes only sessions whose durable state changed since their last save.
    /// Passing a session ID flushes just that session, e.g. before a send.
    func flushCache(sessionID: V2SessionID? = nil) async {
        if sessionID == nil { persistenceTask?.cancel(); persistenceTask = nil }
        guard let localStore else { return }
        let dirty = entries.values.filter { $0.isDirty && (sessionID == nil || $0.id == sessionID) }
            .sorted { $0.lastAccess < $1.lastAccess }
        for entry in dirty {
            guard let projection = entry.projection else { continue }
            // Clearing before the write avoids losing a change made while the
            // actor encodes; restore the flag if the write was refused or the
            // disk failed so a later flush retries it.
            entry.isDirty = false
            let saved = await localStore.saveSession(V2SessionArchive(data: projection.data, model: entry.model))
            if !saved, isCurrent(entry) {
                entry.isDirty = true
                // Without re-arming, a quiet session whose flush came from the
                // scheduled task never persisted: nothing else scheduled a new
                // one, so the cached state existed only in memory.
                schedulePersistence()
            }
        }
    }

    /// Streaming emits several times per second; a 2.5 s window coalesces them
    /// into one archive write per session instead of one per batch.
    private func schedulePersistence() {
        guard localStore != nil, persistenceTask == nil else { return }
        persistenceTask = Task { [weak self] in
            do { try await Task.sleep(for: .milliseconds(2500)) } catch { return }
            await self?.flushCache()
        }
    }

    /// Multiple observers share one socket; removing the last observer closes it.
    func observe(sessionId: V2SessionID) -> AsyncStream<V2SessionObservation> {
        let entry = entry(for: sessionId)
        let observerID = UUID()
        return AsyncStream(bufferingPolicy: .bufferingNewest(1)) { continuation in
            entry.observers[observerID] = continuation
            continuation.yield(observation(entry))
            continuation.onTermination = { [weak self, weak entry] _ in
                Task { @MainActor [weak self, weak entry] in
                    guard let self, let entry else { return }
                    entry.observers.removeValue(forKey: observerID)
                    if entry.observers.isEmpty {
                        self.stop(entry)
                        self.evict()
                    }
                }
            }
            start(entry)
        }
    }

    /// Warms the bounded cache from a dashboard snapshot so opening a recent
    /// session paints from memory. Capped at the newest active sessions, at most
    /// `concurrency` at a time, and superseded by the next prefetch or reset.
    func prefetchSessions(_ sessions: [V2SessionMeta], limit: Int = 10, concurrency: Int = 3) {
        guard network.availability != .offline, !suspended else { return }
        // Never aim to warm more entries than the cache can hold. Loading in
        // ascending activity order makes the most recently active sessions the
        // newest entries, so the LRU eviction that fires as the cache fills
        // drops the ones the user is least likely to open.
        let newest = sessions
            .filter { !$0.archived && !$0.userArchived && !$0.id.hasPrefix("local:") }
            .sorted {
                let left = V2Timestamp.seconds($0.sortAt ?? $0.lastActivityAt) ?? 0
                let right = V2Timestamp.seconds($1.sortAt ?? $1.lastActivityAt) ?? 0
                return left > right
            }
            .prefix(min(max(0, limit), policy.maximumSessions))
            .map(\.id)
            .filter { cached(sessionId: $0) == nil }
        let candidates = Array(newest.reversed())
        guard !candidates.isEmpty else { return }
        prefetchTask?.cancel()
        let width = max(1, concurrency)
        prefetchTask = Task { [weak self] in
            guard let self else { return }
            var start = 0
            while start < candidates.count {
                if Task.isCancelled { return }
                let batch = Array(candidates[start..<min(start + width, candidates.count)])
                start += width
                await withTaskGroup(of: Void.self) { group in
                    for id in batch {
                        group.addTask { @MainActor [weak self] in
                            guard let self else { return }
                            _ = try? await self.load(sessionId: id)
                        }
                    }
                    await group.waitForAll()
                }
            }
        }
    }

    /// Explicit refresh replaces the aggregate and then re-establishes live recovery.
    func refresh(sessionId: V2SessionID) async throws -> V2SessionData {
        try requireNetwork()
        let entry = entry(for: sessionId)
        stop(entry)
        entry.loadTask?.cancel()
        entry.loadTask = nil
        entry.readVersion += 1
        entry.historyTask?.cancel()
        entry.historyTask = nil
        defer { if isCurrent(entry) { start(entry) } }
        return try await hydrate(entry)
    }

    func loadOlder(sessionId: V2SessionID, limit: Int = 100) async throws -> V2SessionData {
        try requireNetwork()
        _ = try await load(sessionId: sessionId)
        let entry = entry(for: sessionId)
        if let task = entry.historyTask { return try await task.value }
        guard let data = entry.projection?.data,
              data.hasOlderItems, let before = data.items.first?.orderSeq else {
            return entry.projection!.data
        }
        let version = entry.readVersion
        let task = Task { [self] in
            defer { if entry.readVersion == version { entry.historyTask = nil } }
            let page = try await detail.loadOlderItems(sessionId: sessionId, beforeOrderSeq: before, limit: limit)
            try requireCurrent(entry, version: version)
            entry.projection?.applyHistory(page)
            TimelineTrace.shared.record("WINDOW.older", "page=\(page.items.count) rows=\(entry.projection?.data.items.count ?? 0)")
            emit(entry)
            return entry.projection!.data
        }
        entry.historyTask = task
        return try await task.value
    }

    func loadLatest(sessionId: V2SessionID, limit: Int = 100) async throws -> V2SessionData {
        try requireNetwork()
        _ = try await load(sessionId: sessionId)
        let entry = entry(for: sessionId)
        entry.readVersion += 1
        entry.historyTask?.cancel()
        entry.historyTask = nil
        let version = entry.readVersion
        let page = try await detail.latestItems(sessionId: sessionId, limit: limit)
        try requireCurrent(entry, version: version)
        let before = entry.projection?.data.items.count ?? 0
        entry.projection?.applyLatest(page)
        TimelineTrace.shared.record("WINDOW.latest", "page=\(page.items.count) before=\(before) after=\(entry.projection?.data.items.count ?? 0)")
        emit(entry)
        return entry.projection!.data
    }

    func catalogs(sessionId: V2SessionID, force: Bool = false, capabilities: V2RuntimeCapabilitySnapshot? = nil) async throws -> V2SessionCatalogs {
        let entry = entry(for: sessionId)
        let scopes = capabilities.map { value in Set(["model", "permission"].filter { value.allows("catalog.\($0)") }) }
        if entry.catalogScopes != scopes {
            invalidateCatalogs(entry)
            entry.catalogScopes = scopes
        }
        if !force, let cached = entry.catalogs, let readAt = entry.catalogReadAt,
           now().timeIntervalSince(readAt) < policy.catalogLifetime { return cached }
        try requireNetwork()
        if let task = entry.catalogTask { return try await task.value }
        let version = entry.catalogVersion
        let task = Task { [self] in
            defer {
                if entry.catalogVersion == version { entry.catalogTask = nil }
                evict()
            }
            let catalogs = try await detail.catalogs(sessionId: sessionId, scopes: scopes)
            try requireCurrent(entry)
            guard version == entry.catalogVersion else { throw CacheError.invalidated }
            entry.catalogs = catalogs
            entry.catalogReadAt = now()
            evict()
            return catalogs
        }
        entry.catalogTask = task
        return try await task.value
    }

    /// The command catalog is read once per live connection; it is dropped with
    /// the option catalogs when the connection stops or selections change.
    func commands(sessionId: V2SessionID, force: Bool = false) async throws -> [V2RuntimeCommand] {
        let entry = entry(for: sessionId)
        if !force, let cached = entry.commands { return cached }
        try requireNetwork()
        let version = entry.catalogVersion
        let commands = try await interactions.commands(sessionId: sessionId)
        try requireCurrent(entry)
        guard version == entry.catalogVersion else { throw CacheError.invalidated }
        entry.commands = commands
        return commands
    }

    func executeCommand(sessionId: V2SessionID, request: RuntimeCommandRequest) async throws -> V2RuntimeCommandExecuteResponse {
        try requireNetwork()
        let entry = entry(for: sessionId)
        let response = try await interactions.executeCommand(sessionId: sessionId, command: request.command, arguments: request.args, raw: request.raw)
        try requireCurrent(entry)
        if let session = response.session { entry.projection?.applyMeta(session); emit(entry) }
        return response
    }

    func draftDidChange(sessionID: V2SessionID) {
        entries[sessionID]?.isDirty = true
        schedulePersistence()
    }

    func localWorkDidChange(sessionID: V2SessionID) {
        if let entry = entries[sessionID] { emit(entry) }
    }

    func send(sessionId: V2SessionID, content: String, attachmentIDs: [V2AttachmentID] = [], clientMessageID: String) async throws -> V2RuntimeActionResponse {
        try requireNetwork()
        let entry = entry(for: sessionId)
        let response = try await detail.sendMessage(sessionId: sessionId, content: content, attachmentIds: attachmentIDs, clientMessageId: clientMessageID)
        try requireCurrent(entry)
        // Timeline echoes reconcile by clientMessageId; sending never refetches the snapshot.
        return response
    }

    func interrupt(sessionId: V2SessionID) async throws {
        try requireNetwork()
        let entry = entry(for: sessionId)
        _ = try await detail.interrupt(sessionId: sessionId)
        try requireCurrent(entry)
    }

    /// Opens the conversation in a terminal on its connector's computer. This
    /// does not change the session projection; the connector republishes
    /// presence when the new plugin starts.
    func openOnComputer(sessionId: V2SessionID) async throws -> V2RuntimeOpenResult {
        try requireNetwork()
        let entry = entry(for: sessionId)
        let result = try await detail.openOnComputer(sessionId: sessionId)
        try requireCurrent(entry)
        return result
    }

    /// Stops the terminal running this conversation on the desktop.
    @discardableResult
    func closeOnComputer(sessionId: V2SessionID) async throws -> V2RuntimeOpenResult {
        try requireNetwork()
        let entry = entry(for: sessionId)
        let result = try await detail.closeOnComputer(sessionId: sessionId)
        try requireCurrent(entry)
        return result
    }

    func setTakeover(sessionId: V2SessionID, enabled: Bool) async throws {
        try requireNetwork()
        let entry = entry(for: sessionId)
        let session = try await detail.setTakeover(sessionId: sessionId, enabled: enabled)
        try requireCurrent(entry)
        entry.projection?.applyMeta(session)
        entry.projection?.markStale()
        emit(entry)
        // Takeover was confirmed by the write response. A failed following read
        // must not invite a duplicate toggle or claim the write was rejected.
        do { try await reconcile(entry) }
        catch { if isCurrent(entry) { entry.error = V2ClientFailure(error); emit(entry) } }
    }

    func setSelection(sessionId: V2SessionID, scope: V2RuntimeSelectionScope, selectionId: V2SelectionID?) async throws {
        try requireNetwork()
        let entry = entry(for: sessionId)
        let state = try await detail.updateSelection(sessionId: sessionId, scope: scope, selectionId: selectionId)
        try requireCurrent(entry)
        if let state { entry.projection?.applyState(state) }
        invalidateCatalogs(entry)
        emit(entry)
    }

    func respond(sessionId: V2SessionID, noticeId: V2NoticeID, actionId: String, input: JSONValue? = nil) async throws {
        try requireNetwork()
        let entry = entry(for: sessionId)
        _ = try await interactions.respond(sessionId: sessionId, noticeId: noticeId, actionId: actionId, input: input)
        try requireCurrent(entry)
        // Response acceptance is not notice resolution; wait for authoritative live facts.
        entry.projection?.markStale()
        emit(entry)
        do { try await reconcile(entry) }
        catch {
            // The action already succeeded. A failed read must not turn an
            // accepted approval into a retryable write failure in the UI.
            if isCurrent(entry) { entry.error = V2ClientFailure(error); emit(entry) }
        }
    }

    func sync(sessionId: V2SessionID) async throws {
        try requireNetwork()
        let entry = entry(for: sessionId)
        _ = try await detail.sync(sessionId: sessionId)
        try requireCurrent(entry)
        try await reconcile(entry)
    }

    /// Current subagent run set for a session whose socket just reconnected.
    /// `runtime.activity` is live-only, so the presentation refetches instead
    /// of trusting a running block from before the drop.
    func sessionActivity(sessionId: V2SessionID) async throws -> [V2AgentRun] {
        try requireNetwork()
        guard let entry = entries[sessionId] else { throw CacheError.invalidated }
        let runs = try await detail.activity(sessionId: sessionId)
        try requireCurrent(entry)
        return runs
    }

    func applyMetadata(_ sessions: [V2SessionMeta]) {
        for session in sessions {
            guard let entry = entries[session.id] else { continue }
            let previous = entry.projection?.data.session
            // Dashboard pushes repeat every session on each tick; unchanged
            // metadata must not re-project and re-persist every open session.
            let changed = entry.projection?.applyMeta(session) ?? false
            // Everything below must reason about the metadata the projection
            // actually accepted. A stale snapshot is rejected by applyMeta, and
            // acting on it anyway invalidated catalogs and tore the socket down
            // and back up on every repeated push of the old value.
            let accepted = entry.projection?.data.session
            let invalidates = accepted.map { $0.connectorStatus != .online }
                ?? (session.connectorStatus != .online)
            let bindingChanged = previous?.effectiveRuntimeId != accepted?.effectiveRuntimeId
            if changed || invalidates { invalidateCatalogs(entry) }
            if changed || invalidates { emit(entry) }
            // A projection still loading has no previous binding to compare, so
            // it must not stop and restart its own socket on every tick.
            if changed, let previous, let accepted, accepted.connectorStatus == .online,
               previous.connectorStatus != .online || bindingChanged {
                // A dashboard projection can arrive before the session socket frame.
                // Restart observed sessions so their runtime facts match the new binding.
                stop(entry)
                start(entry)
            }
        }
    }

    func suspend() {
        suspended = true
        prefetchTask?.cancel(); prefetchTask = nil
        for entry in entries.values { stop(entry); emit(entry, persist: false) }
    }

    func resume() {
        suspended = false
        for entry in entries.values { start(entry) }
    }

    func updateConnectivity(_ status: V2NetworkStatus) {
        let wasOffline = network.availability == .offline
        network = status
        for entry in entries.values {
            if status.availability == .offline {
                stop(entry)
                entry.readVersion += 1
                entry.loadTask?.cancel()
                entry.loadTask = nil
                entry.historyTask?.cancel()
                entry.historyTask = nil
                entry.connection = .offline
            } else if wasOffline {
                start(entry)
            }
            emit(entry, persist: false)
        }
    }

    private func requireNetwork() throws {
        if network.availability == .offline {
            throw V2ClientFailure(kind: .offline, message: String(localized: "You are offline. Cached content is still available."))
        }
    }

    func remove(sessionIds: [V2SessionID]) {
        for id in sessionIds {
            if let localStore { Task { await localStore.removeSession(id) } }
            guard let entry = entries.removeValue(forKey: id) else { continue }
            dispose(entry)
        }
    }

    /// Cancellation plus identity checks prevent late responses from repopulating a signed-out cache.
    func reset() {
        persistenceTask?.cancel(); persistenceTask = nil
        prefetchTask?.cancel(); prefetchTask = nil
        let old = Array(entries.values)
        entries.removeAll()
        for entry in old { dispose(entry) }
    }

    private func hydrate(_ entry: Entry) async throws -> V2SessionData {
        if let task = entry.loadTask { return try await task.value }
        entry.readVersion += 1
        entry.historyTask?.cancel()
        entry.historyTask = nil
        let version = entry.readVersion
        let task = Task { [self] in
            defer {
                if entry.readVersion == version { entry.loadTask = nil }
                evict()
            }
            let snapshot = try await detail.load(sessionId: entry.id)
            try requireCurrent(entry, version: version)
            guard snapshot.session.id == entry.id else { throw CacheError.invalidated }
            entry.projection = V2SessionProjection(snapshot: snapshot, maximumItems: policy.maximumTimelineItems)
            invalidateCatalogs(entry)
            entry.error = nil
            emit(entry)
            evict()
            return entry.projection!.data
        }
        entry.loadTask = task
        return try await task.value
    }

    private func start(_ entry: Entry) {
        guard isCurrent(entry), !entry.model.isLocalCreation, !suspended, network.availability != .offline,
              !entry.observers.isEmpty, entry.connectionTask == nil else { return }
        let connectionID = UUID()
        entry.connectionID = connectionID
        entry.connectionTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if entry.connectionID == connectionID { entry.connectionTask = nil }
            }
            var attempt = 0
            while self.isCurrent(entry), entry.connectionID == connectionID, !Task.isCancelled {
                do {
                    entry.connection = attempt == 0 ? .connecting : .reconnecting
                    self.emit(entry, persist: false)
                    _ = try await self.load(sessionId: entry.id)
                    if entry.needsSnapshot {
                        _ = try await self.hydrate(entry)
                        entry.needsSnapshot = false
                    }
                    let events = try await self.detail.updates(sessionId: entry.id, clientId: "ios-session-\(connectionID)")
                    for try await event in events {
                        try self.requireCurrent(entry)
                        guard entry.connectionID == connectionID else { return }
                        if event.type == "session.subscribed" {
                            // The socket is registered before recovery, closing the snapshot/subscribe race.
                            try await self.reconcile(entry)
                            try self.requireCurrent(entry)
                            guard entry.connectionID == connectionID else { return }
                            entry.connection = .connected
                            attempt = 0
                            TimelineTrace.shared.record("SESSION.connected", "attempt=\(attempt)")
                            self.emit(entry, persist: false)
                        } else {
                            do {
                                try await self.receive(event, entry: entry)
                            } catch {
                                // Recover promptly from a snapshot instead of
                                // leaving an undecodable frame unresolved until
                                // some unrelated future reconnect.
                                guard isUndecodableEvent(error) else { throw error }
                                // Drop the frame and keep listening. Rethrowing
                                // ended the consumer loop, and the coordinator
                                // replays its per-session buffer to the next
                                // subscriber - the same undecodable frame came
                                // back and the session looped on
                                // load-hydrate-resubscribe-fail for ever.
                                entry.needsSnapshot = true
                                entry.projection?.markStale()
                                TimelineTrace.shared.record("EVENT.decode", "type=\(event.type)")
                            }
                        }
                    }
                    throw CacheError.connectionClosed
                } catch {
                    guard self.isCurrent(entry), entry.connectionID == connectionID, !Task.isCancelled else { return }
                    entry.projection?.markStale()
                    self.invalidateCatalogs(entry)
                    let failure = V2ClientFailure(error)
                    entry.error = failure
                    // A decode failure is usually a frame the server has since
                    // moved past; leaving the session permanently failed made
                    // the chat unable to sync or send until the app restarted.
                    // Retry it like a transport failure (with backoff).
                    guard failure.permitsAutomaticReconnect || failure.kind == .invalidResponse else {
                        TimelineTrace.shared.record("SESSION.failed", "kind=\(failure.kind)")
                        entry.connection = .failed(failure.message)
                        self.emit(entry, persist: false)
                        return
                    }
                    TimelineTrace.shared.record("SESSION.reconnecting",
                        "kind=\(failure.kind) attempt=\(attempt)")
                    entry.connection = .reconnecting
                    self.emit(entry, persist: false)
                    attempt += 1
                    do { try await self.sleep(.seconds(min(1 << min(attempt - 1, 4), 15))) }
                    catch { return }
                }
            }
        }
    }

    private func receive(_ event: V2SessionEvent, entry: Entry) async throws {
        guard event.sessionId == entry.id else { return }
        if let pending = entry.loadTask { _ = try await pending.value }
        if let pending = entry.recoveryTask { try await pending.value }
        if event.type == "session.refetch_required" || event.sequence > (entry.projection?.sequence ?? 0) + 1 {
            do {
                try await reconcile(entry)
            } catch is CancellationError {
                throw CancellationError()
            } catch let error as CacheError {
                throw error
            } catch {
                // Do not apply the newer event after a failed recovery: doing
                // so advances the cursor past messages that were never read.
                entry.projection?.markStale()
                TimelineTrace.shared.record("EVENT.recoverFailed", "type=\(event.type)")
                throw error
            }
        }
        try requireCurrent(entry)
        let readTypes = ["runtime.state.updated", "runtime.capability.updated", "runtime.notice.snapshot", "runtime.notice.updated"]
        if readTypes.contains(event.type), event.receivedAt < entry.projectionBarrier { return }
        let wasOnline = entry.projection?.data.session.connectorStatus == .online
        let previousRuntime = entry.projection?.data.session.effectiveRuntimeId
        if event.type == "timeline.snapshot", event.sequence >= (entry.projection?.sequence ?? 0) {
            entry.readVersion += 1
            entry.historyTask?.cancel()
            entry.historyTask = nil
        }
        try entry.projection?.apply(event)
        if event.type == "runtime.catalog.updated" || entry.projection?.data.session.connectorStatus != .online {
            invalidateCatalogs(entry)
        }
        emitAfterBacklog(entry)
        if entry.projection?.data.session.connectorStatus == .online,
           !wasOnline || previousRuntime != entry.projection?.data.session.effectiveRuntimeId {
            try await reconcile(entry)
        }
    }

    private func reconcile(_ entry: Entry) async throws {
        if let task = entry.recoveryTask { return try await task.value }
        let version = entry.connectionID
        let task = Task { [self] in
            defer { if entry.connectionID == version { entry.recoveryTask = nil } }
            if entry.projection == nil { _ = try await hydrate(entry) }
            var recoveryCursor = entry.projection!.data.cursor
            var needsSnapshot = false
            // Whatever this pass applied must be persisted even if a later
            // page fails: replaying a multi-megabyte backlog from the same
            // cursor never converges on a slow link.
            defer {
                entry.isDirty = true
                schedulePersistence()
            }
            while true {
                let recovery = try await detail.recover(sessionId: entry.id, after: recoveryCursor)
                try requireCurrent(entry)
                guard entry.connectionID == version else { throw CacheError.invalidated }
                let nextSequence = V2SessionProjection.sequence(recovery.nextCursor)
                let currentSequence = V2SessionProjection.sequence(recoveryCursor)
                if recovery.snapshotRequired && nextSequence <= currentSequence {
                    needsSnapshot = true
                    break
                }
                // Apply and advance per page, so a timeout on a later page
                // resumes where it stopped instead of starting over.
                var skippedUndecodable = false
                for event in recovery.events {
                    do {
                        try entry.projection?.apply(event)
                    } catch {
                        // Projection errors are wrapped as HTTPError.decoding,
                        // so `catch is DecodingError` alone was dead code.
                        // One malformed event used to wedge recovery at the
                        // same cursor for ever (the retry re-fetched it and
                        // failed again). Skip it, keep the rest of the page,
                        // and fall back to a snapshot so nothing stays
                        // silently missing.
                        guard isUndecodableEvent(error) else { throw error }
                        skippedUndecodable = true
                        TimelineTrace.shared.record("EVENT.decodeSkipped", "type=\(event.type)")
                    }
                }
                entry.projection?.advanceCursor(recovery.nextCursor)
                recoveryCursor = recovery.nextCursor
                if skippedUndecodable { needsSnapshot = true }
                if !recovery.snapshotRequired { break }
            }
            if needsSnapshot {
                _ = try await hydrate(entry)
            }
            entry.projection?.markStale()
            invalidateCatalogs(entry)
            if entry.projection?.data.session.connectorStatus == .online {
                // Frames received before this read must not overwrite its newer live projection.
                let barrier = now()
                do {
                    let live = try await detail.liveState(sessionId: entry.id)
                    try requireCurrent(entry)
                    guard entry.connectionID == version else { throw CacheError.invalidated }
                    entry.projection?.applyLive(live)
                    entry.projectionBarrier = barrier
                    entry.error = nil
                } catch {
                    try requireCurrent(entry)
                    guard entry.connectionID == version else { throw CacheError.invalidated }
                    entry.error = V2ClientFailure(error)
                    throw error
                }
            }
            emit(entry)
        }
        entry.recoveryTask = task
        return try await task.value
    }

    private func entry(for id: V2SessionID) -> Entry {
        // A view may look up its stable model while a containing sidebar moves.
        // Re-projecting here walks every historical item and also registers all
        // of those observable values as dependencies of the caller's view body.
        if let entry = entries[id] { touch(entry); return entry }
        let entry = Entry(id: id, model: V2SessionModel(id: id, scope: scope, repository: self))
        entries[id] = entry
        touch(entry)
        if network.availability == .offline { entry.connection = .offline }
        entry.model.update(observation(entry), network: network)
        return entry
    }

    private func touch(_ entry: Entry) { accessCounter += 1; entry.lastAccess = accessCounter }
    private func isCurrent(_ entry: Entry) -> Bool { entries[entry.id] === entry }

    private func requireCurrent(_ entry: Entry, version: Int? = nil) throws {
        try Task.checkCancellation()
        guard isCurrent(entry), version == nil || version == entry.readVersion else { throw CacheError.invalidated }
    }

    private func observation(_ entry: Entry) -> V2SessionObservation {
        V2SessionObservation(sessionId: entry.id, data: entry.projection?.data, connection: entry.connection, error: entry.error)
    }

    /// Frames buffered during a reconnect or a long reconcile are delivered
    /// back-to-back without suspending. Publishing once the main queue turns
    /// again lets the whole backlog apply before the model and rows re-project,
    /// so catch-up cost is frames × log n instead of frames × n.
    private func emitAfterBacklog(_ entry: Entry) {
        guard !entry.emitScheduled else { return }
        entry.emitScheduled = true
        DispatchQueue.main.async { [weak self, weak entry] in
            guard let self, let entry else { return }
            entry.emitScheduled = false
            self.emit(entry)
        }
    }

    /// `persist` marks durable changes (timeline, metadata, cursor, local work).
    /// Connection and connectivity transitions republish without an archive write.
    private func emit(_ entry: Entry, persist: Bool = true) {
        guard isCurrent(entry) else { return }
        entry.model.update(observation(entry), network: network)
        for observer in entry.observers.values { observer.yield(observation(entry)) }
        if persist, entry.projection != nil {
            entry.isDirty = true
            schedulePersistence()
        }
    }

    private func invalidateCatalogs(_ entry: Entry) {
        entry.catalogVersion += 1
        entry.catalogs = nil
        entry.catalogReadAt = nil
        entry.commands = nil
        entry.catalogTask?.cancel()
        entry.catalogTask = nil
    }

    private func stop(_ entry: Entry) {
        entry.connectionID = UUID()
        entry.connectionTask?.cancel()
        entry.connectionTask = nil
        entry.recoveryTask?.cancel()
        entry.recoveryTask = nil
        entry.connection = .inactive
        entry.projection?.markStale()
        invalidateCatalogs(entry)
        emit(entry, persist: false)
    }

    private func dispose(_ entry: Entry) {
        stop(entry)
        entry.loadTask?.cancel()
        entry.historyTask?.cancel()
        for observer in entry.observers.values { observer.finish() }
        entry.observers.removeAll()
        entry.model.invalidate()
    }

    private func evict(protecting protected: Entry? = nil) {
        let cutoff = now().addingTimeInterval(-leaseDuration)
        leasedEntries = leasedEntries.filter { $0.value > cutoff }
        let candidates = entries.values.filter {
            $0 !== protected && $0.observers.isEmpty && $0.loadTask == nil && $0.catalogTask == nil
                && $0.historyTask == nil && !$0.model.hasLocalWork
                && leasedEntries[$0.id] == nil
        }
            .sorted { $0.lastAccess < $1.lastAccess }
        for entry in candidates where entries.count > max(1, policy.maximumSessions) {
            entries.removeValue(forKey: entry.id)
            dispose(entry)
        }
    }
}

@MainActor
private final class Entry {
    let id: V2SessionID
    let model: V2SessionModel
    var needsSnapshot = false
    var hasReadLocal = false
    var isDirty = false
    var emitScheduled = false
    var localReadTask: Task<V2SessionRestore?, Never>?
    var projection: V2SessionProjection?
    var connection = V2SessionConnectionState.inactive
    var error: V2ClientFailure?
    var lastAccess = 0
    var readVersion = 0
    var catalogVersion = 0
    var projectionBarrier = Date.distantPast
    var connectionID = UUID()
    var observers: [UUID: AsyncStream<V2SessionObservation>.Continuation] = [:]
    var loadTask: Task<V2SessionData, Error>?
    var historyTask: Task<V2SessionData, Error>?
    var catalogTask: Task<V2SessionCatalogs, Error>?
    var recoveryTask: Task<Void, Error>?
    var connectionTask: Task<Void, Never>?
    var catalogs: V2SessionCatalogs?
    var catalogScopes: Set<String>?
    var catalogReadAt: Date?
    var commands: [V2RuntimeCommand]?

    init(id: V2SessionID, model: V2SessionModel) { self.id = id; self.model = model }
}

/// True for the two shapes a malformed durable frame can take: Swift's own
/// decoding error, and the projection's HTTPError.decoding wrapper.
private func isUndecodableEvent(_ error: Error) -> Bool {
    if error is DecodingError { return true }
    if let http = error as? HTTPError, case .decoding = http { return true }
    return false
}

private enum CacheError: LocalizedError {
    case invalidated
    case connectionClosed

    var errorDescription: String? {
        switch self {
        case .invalidated: String(localized: "The session request no longer belongs to the active cache.")
        case .connectionClosed: String(localized: "The session connection closed.")
        }
    }
}
