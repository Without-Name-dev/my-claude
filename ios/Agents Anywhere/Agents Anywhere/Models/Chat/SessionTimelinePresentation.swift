import Foundation
import Observation
import SwiftUI

@MainActor @Observable
final class ChatTimelineRowModel: Identifiable {
    let id: V2TimelineItemID
    private(set) var value: V2TimelineItem
    private(set) var structure: TimelineRowStructure
    private(set) var text: String
    private(set) var isRevealing = false
    private(set) var layoutGeneration = 0
    @ObservationIgnored private(set) var settlesAt: TimeInterval = 0
    @ObservationIgnored private var hasFlushed = false
    @ObservationIgnored private var entryCache: (cwd: String?, entry: TimelineEntryPresentation)?
    /// False while a streaming batch withholds the final glyph of the received text.
    @ObservationIgnored private var displaysFullText = false
    /// Captured by the Equatable row wrapper. The reference itself cannot be
    /// compared for content changes because both sides see the same mutations.
    @ObservationIgnored private(set) var contentRevision = 0

    init(_ value: V2TimelineItem, animate: Bool = false) {
        id = value.id; self.value = value
        structure = TimelineRowStructure(value)
        text = animate ? "" : value.displayText
    }

    /// Returns true when newly displayed text starts a reveal batch.
    @discardableResult
    func flush(_ next: V2TimelineItem, animate: Bool, now: TimeInterval) -> Bool {
        // Unchanged rows neither recompute their structure nor rescan their text.
        if hasFlushed && value == next && (animate || displaysFullText) { settle(now: now); return false }
        hasFlushed = true
        var changed = false
        let received = next.displayText
        let appending = received.hasPrefix(text)
        // Snapshots/corrections are authoritative replacements, not token deltas.
        if !appending { layoutGeneration += 1; changed = true }
        let safeText = animate && appending && next.isStreamingText && !received.isEmpty
            ? String(received.dropLast()) : received
        let displayed = safeText.hasPrefix(text) || !appending ? safeText : received
        displaysFullText = displayed == received
        var started = false
        if displayed != text {
            isRevealing = animate && appending
            started = isRevealing
            if isRevealing { settlesAt = now + ReplyPresentation.revealSeconds + ReplyPresentation.drawSlack }
            text = displayed
            changed = true
        }
        if value != next { value = next; entryCache = nil; changed = true }
        let nextStructure = TimelineRowStructure(next)
        if structure != nextStructure { structure = nextStructure; changed = true }
        let wasRevealing = isRevealing
        if !animate || now >= settlesAt { isRevealing = false }
        if wasRevealing != isRevealing { changed = true }
        if changed { contentRevision &+= 1 }
        return started
    }

    /// Tool/marker rows parse their payload once per item version instead of
    /// on every body evaluation of the row and its expanded details.
    func entry(cwd: String?) -> TimelineEntryPresentation {
        if let entryCache, entryCache.cwd == cwd { return entryCache.entry }
        let entry = TimelineEntryPresentation(item: value, cwd: cwd)
        entryCache = (cwd, entry)
        return entry
    }

    /// The drawing clock follows the last revealed batch, not the item's
    /// running status. A tool wait or thinking pause stops redrawing; the next
    /// appended text starts a new batch.
    func settle(now: TimeInterval) {
        if isRevealing && now >= settlesAt {
            isRevealing = false
            contentRevision &+= 1
        }
    }
}

/// Receives full repository projections without exposing each transport frame to
/// SwiftUI. Only flush() publishes rows. Received text waits in `pending` while
/// the previous batch reveals; the next batch goes on screen when it ends.
@MainActor @Observable
final class SessionTimelinePresentation {
    private(set) var rows: [ChatTimelineRowModel] = []
    private(set) var pendingMessages: [V2PendingMessage] = []
    private(set) var hasPresentedSnapshot = false
    /// The live subagent status line for the current turn, if the server has
    /// reported one. It is presentation-only; the server does not replay the
    /// event into a durable snapshot.
    private(set) var subagentActivity: SubagentActivityPresentation?
    /// Bumped only when an older page is prepended. The timeline disables
    /// insertion animation for this value and restores the reader's offset.
    private(set) var historyInsertionRevision = 0
    @ObservationIgnored private var rowsByID: [V2TimelineItemID: ChatTimelineRowModel] = [:]
    @ObservationIgnored private var pending: [V2TimelineItem]?
    @ObservationIgnored private var animatePending = false
    @ObservationIgnored private var initialized = false
    @ObservationIgnored private var lastConnection: V2SessionConnectionState = .inactive
    @ObservationIgnored private var lastActivityEventID: String?
    @ObservationIgnored private var activityPendingBoundary: String?
    @ObservationIgnored private var wake: AsyncStream<Void>.Continuation?
    /// When the newest reveal batch ends and the buffer may publish again.
    @ObservationIgnored private(set) var nextBatchAt: TimeInterval = 0

    func presentOpening(_ items: [V2TimelineItem], pendingMessages: [V2PendingMessage]) {
        stage(items, animate: false)
        flush()
        synchronizePending(pendingMessages)
    }

    func receive(_ observation: V2SessionObservation) {
        defer { lastConnection = observation.connection }
        if let event = observation.data?.lastExtensionEvent { receiveActivity(event) }
        guard let data = observation.data else { return }
        stage(data.items, animate: initialized && lastConnection == .connected && observation.connection == .connected)
        initialized = true
    }

    /// `runtime.activity` is an extension event: the projection keeps the most
    /// recent one in `lastExtensionEvent` and it is re-read on every emission.
    /// Event identity, not observation ticks, decides when to rebuild the block.
    private func receiveActivity(_ event: V2SessionEvent) {
        guard event.type == "runtime.activity", event.eventId != lastActivityEventID else { return }
        lastActivityEventID = event.eventId
        guard let encoded = try? JSONEncoder().encode(event.payload),
              let payload = try? JSONDecoder().decode(V2AgentActivity.self, from: encoded) else { return }
        guard !payload.agents.isEmpty || subagentActivity != nil else { return }
        var activity = subagentActivity ?? SubagentActivityPresentation()
        activity.receive(payload.agents, at: event.receivedAt)
        if subagentActivity != activity { subagentActivity = activity }
    }

    func stage(_ items: [V2TimelineItem], animate: Bool) {
        pending = items.filter(\.isVisibleInChat)
        // Once a recovery snapshot is staged, preserve its snap semantics until
        // that tick even if a live event arrives immediately afterwards.
        animatePending = pendingWasStaged ? animatePending && animate : animate
        pendingWasStaged = true
        wake?.yield(())
    }
    @ObservationIgnored private var pendingWasStaged = false

    func flush(now: TimeInterval = ProcessInfo.processInfo.systemUptime) {
        if let pending {
            let previousTail = rows.last?.value.orderSeq ?? Int.min
            let previousFirst = rows.first?.value.orderSeq
            var started = false
            var updated: [ChatTimelineRowModel] = []
            updated.reserveCapacity(pending.count)
            var orderChanged = pending.count != rows.count
            for (index, value) in pending.enumerated() {
                let existing = rowsByID[value.id]
                let animate = animatePending && (existing != nil || value.orderSeq > previousTail)
                let row: ChatTimelineRowModel
                if let existing { row = existing } else {
                    row = ChatTimelineRowModel(value, animate: animate && value.isAssistantText)
                    rowsByID[value.id] = row
                }
                if row.flush(value, animate: animate, now: now) { started = true }
                if !orderChanged, rows[index] !== row { orderChanged = true }
                updated.append(row)
            }
            if started { nextBatchAt = now + ReplyPresentation.batchInterval }
            if orderChanged {
                // An older page changes the first published row. Publish it in
                // a transaction with animations disabled; even if a caller has
                // an animation in flight, history insertion stays a jump that
                // the explicit point restore then corrects.
                let insertedOlder: Bool
                if let previousFirst, let first = updated.first?.value.orderSeq, first < previousFirst {
                    insertedOlder = true
                } else {
                    insertedOlder = false
                }
                if insertedOlder {
                    var transaction = Transaction(animation: nil)
                    transaction.disablesAnimations = true
                    withTransaction(transaction) {
                        historyInsertionRevision &+= 1
                        rows = updated
                        rowsByID = Dictionary(uniqueKeysWithValues: updated.map { ($0.id, $0) })
                    }
                } else {
                    rows = updated
                    rowsByID = Dictionary(uniqueKeysWithValues: updated.map { ($0.id, $0) })
                }
            }
            if !hasPresentedSnapshot { hasPresentedSnapshot = true }
            self.pending = nil; pendingWasStaged = false
        }
        for row in rows where row.isRevealing { row.settle(now: now) }
    }

    func run(sessionID: V2SessionID, repository: V2SessionRepository) async {
        let session = repository.session(id: sessionID)
        let signal = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
        wake = signal.continuation
        defer { signal.continuation.finish(); wake = nil }
        await withTaskGroup(of: Void.self) { group in
            group.addTask { @MainActor [weak self] in
                defer { signal.continuation.finish() }
                for await value in repository.observe(sessionId: sessionID) {
                    guard !Task.isCancelled, let self else { return }
                    self.receive(value)
                }
            }
            group.addTask { @MainActor [weak self] in
                for await _ in signal.stream {
                    guard !Task.isCancelled, let self else { return }
                    repeat {
                        // Text received during a reveal waits for it to end, then
                        // goes on screen as one batch. Text after a pause shows at
                        // once. Without new text, poll briefly to notice arrivals
                        // and settle rows whose reveal has ended.
                        let now = ProcessInfo.processInfo.systemUptime
                        // Without new text, sleep until the earliest reveal ends
                        // instead of waking at a fixed cadence.
                        let wait = self.pending != nil ? self.nextBatchAt - now
                            : max(Self.minimumIdleWait, (self.rows.filter(\.isRevealing).map(\.settlesAt).min() ?? now) - now)
                        if wait > 0 {
                            do { try await Task.sleep(for: .seconds(wait)) } catch { return }
                        }
                        let current = ProcessInfo.processInfo.systemUptime
                        guard self.pending == nil || current >= self.nextBatchAt else { continue }
                        self.flush(now: current)
                        self.synchronizePending(session.pendingMessages)
                    } while !Task.isCancelled && (self.pending != nil || self.rows.contains { $0.isRevealing })
                }
            }
            await group.waitForAll()
        }
    }

    private static let minimumIdleWait: TimeInterval = 1.0 / 60

    func synchronizePending(_ messages: [V2PendingMessage]) {
        // Change optimistic membership in the same tick that publishes echoes,
        // avoiding a blank first user row between HTTP/realtime and UI clocks.
        let visible = messages.filter { message in
            !rows.contains { $0.value.role == .user && $0.value.source["clientMessageId"]?.stringValue == message.id }
        }
        // A pending message starts the next turn. A finished status line belongs
        // to the previous turn and must not drift below the new one; a running
        // block stays because the subagents still belong to this reply.
        let boundary = visible.last?.id
        if boundary != activityPendingBoundary {
            activityPendingBoundary = boundary
            if boundary != nil, subagentActivity?.isRunning == false { subagentActivity = nil }
        }
        if pendingMessages.map(\.id) != visible.map(\.id) { pendingMessages = visible }
    }
}

extension V2TimelineItem {
    var isAssistantText: Bool {
        isReasoning || type == .message && role == .assistant
    }
    var isStreamingText: Bool {
        (status == .pending || status == .running)
            && isAssistantText
    }
}
