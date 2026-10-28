import Foundation
import Observation
import SwiftUI

@MainActor @Observable
final class ChatTimelineRowModel: Identifiable {
    let id: V2TimelineItemID
    private(set) var value: V2TimelineItem
    private(set) var structure: TimelineRowStructure
    private(set) var text: String
    private(set) var layoutGeneration = 0
    @ObservationIgnored private var hasFlushed = false
    @ObservationIgnored private var entryCache: (cwd: String?, entry: TimelineEntryPresentation)?
    /// Captured by the Equatable row wrapper. The reference itself cannot be
    /// compared for content changes because both sides see the same mutations.
    @ObservationIgnored private(set) var contentRevision = 0

    init(_ value: V2TimelineItem) {
        id = value.id; self.value = value
        structure = TimelineRowStructure(value)
        text = value.displayText
    }

    func flush(_ next: V2TimelineItem) {
        // Unchanged rows neither recompute their structure nor rescan their text.
        if hasFlushed && value == next { return }
        hasFlushed = true
        var changed = false
        let received = next.displayText
        // Snapshots/corrections are authoritative replacements, not token deltas.
        if !received.hasPrefix(text) { layoutGeneration += 1; changed = true }
        if received != text {
            text = received
            changed = true
        }
        if value != next { value = next; entryCache = nil; changed = true }
        let nextStructure = TimelineRowStructure(next)
        if structure != nextStructure { structure = nextStructure; changed = true }
        if changed { contentRevision &+= 1 }
    }

    /// Tool/marker rows parse their payload once per item version instead of
    /// on every body evaluation of the row and its expanded details.
    func entry(cwd: String?) -> TimelineEntryPresentation {
        if let entryCache, entryCache.cwd == cwd { return entryCache.entry }
        let entry = TimelineEntryPresentation(item: value, cwd: cwd)
        entryCache = (cwd, entry)
        return entry
    }

}

/// Receives full repository projections without exposing each transport frame to
/// SwiftUI. Only flush() publishes rows. Transport updates are coalesced in
/// `pending`, and the visible row advances to the latest text on the next tick.
@MainActor @Observable
final class SessionTimelinePresentation {
    private(set) var rows: [ChatTimelineRowModel] = []
    private(set) var pendingMessages: [V2PendingMessage] = []
    private(set) var hasPresentedSnapshot = false
    private(set) var presentationRevision = 0
    /// The live subagent status line for the current turn, if the server has
    /// reported one. It is presentation-only; the server does not replay the
    /// event into a durable snapshot.
    private(set) var subagentActivity: SubagentActivityPresentation?
    /// Bumped only when an older page is prepended. The timeline disables
    /// insertion animation for this value and restores the reader's offset.
    private(set) var historyInsertionRevision = 0
    @ObservationIgnored private var rowsByID: [V2TimelineItemID: ChatTimelineRowModel] = [:]
    @ObservationIgnored private var pending: [V2TimelineItem]?
    @ObservationIgnored private var lastConnection: V2SessionConnectionState = .inactive
    @ObservationIgnored private var lastActivityEventID: String?
    @ObservationIgnored private var activityPendingBoundary: String?
    @ObservationIgnored private weak var repository: V2SessionRepository?
    @ObservationIgnored private var activityResyncTask: Task<Void, Never>?
    @ObservationIgnored private var wake: AsyncStream<Void>.Continuation?

    func presentOpening(_ items: [V2TimelineItem], pendingMessages: [V2PendingMessage]) {
        stage(items)
        flush()
        synchronizePending(pendingMessages)
    }

    func receive(_ observation: V2SessionObservation) {
        defer { lastConnection = observation.connection }
        if let event = observation.data?.lastExtensionEvent { receiveActivity(event) }
        guard let data = observation.data else { return }
        stage(data.items)
        // `lastExtensionEvent` can be a running snapshot from before the view
        // reopened, and the server replays neither it nor the final empty
        // update on a reconnect. Ask for the authoritative set on every fresh
        // connection so a finished subagent cannot stay "running" forever.
        if observation.connection == .connected, lastConnection != .connected {
            resyncActivity(sessionID: observation.sessionId)
        }
    }

    /// `runtime.activity` is an extension event: the projection keeps the most
    /// recent one in `lastExtensionEvent` and it is re-read on every emission.
    /// Event identity, not observation ticks, decides when to rebuild the block.
    private func receiveActivity(_ event: V2SessionEvent) {
        guard event.type == "runtime.activity", event.eventId != lastActivityEventID else { return }
        lastActivityEventID = event.eventId
        guard let encoded = try? JSONEncoder().encode(event.payload),
              let payload = try? JSONDecoder().decode(V2AgentActivity.self, from: encoded) else { return }
        applyActivity(payload.agents, at: event.receivedAt)
    }

    private func resyncActivity(sessionID: V2SessionID) {
        guard activityResyncTask == nil, let repository else { return }
        activityResyncTask = Task { [weak self, weak repository] in
            defer { self?.activityResyncTask = nil }
            guard let repository,
                  let runs = try? await repository.sessionActivity(sessionId: sessionID) else { return }
            // A result that lands after another drop describes the old socket.
            guard !Task.isCancelled, let self, self.lastConnection == .connected else { return }
            self.applyActivity(runs, at: Date())
        }
    }

    private func applyActivity(_ runs: [V2AgentRun], at date: Date) {
        guard !runs.isEmpty || subagentActivity != nil else { return }
        var activity = subagentActivity ?? SubagentActivityPresentation()
        // An empty authoritative set finishes every agent that is still shown.
        activity.receive(runs, at: date)
        guard activity.isRunning else {
            // The status line reports work that is happening now. Once the last
            // agent ends, a finished-only block ("已结束 N 个子代理") is residue
            // from the previous turn and stays under every new reply, so it is
            // retired instead of kept.
            if subagentActivity != nil { subagentActivity = nil }
            return
        }
        if subagentActivity != activity { subagentActivity = activity }
    }

    func stage(_ items: [V2TimelineItem]) {
        pending = items.filter(\.isVisibleInChat)
        wake?.yield(())
    }

    func flush() {
        if let pending {
            let previousFirst = rows.first?.value.orderSeq
            var updated: [ChatTimelineRowModel] = []
            updated.reserveCapacity(pending.count)
            var orderChanged = pending.count != rows.count
            var contentChanged = orderChanged
            for (index, value) in pending.enumerated() {
                let existing = rowsByID[value.id]
                // A live batch republishes the whole projection with one
                // row changed. A row that is already published at this position
                // and still carries the same version fields has nothing to do:
                // skip it before `flush` deep-compares its JSON payload again.
                // New rows are absent from `rows`, so they never take this path.
                if let existing, index < rows.count, rows[index] === existing,
                   existing.value.matchesPresentationVersion(of: value) {
                    updated.append(existing)
                    continue
                }
                let row: ChatTimelineRowModel
                if let existing { row = existing } else {
                    row = ChatTimelineRowModel(value)
                    rowsByID[value.id] = row
                }
                let previousContentRevision = row.contentRevision
                row.flush(value)
                if row.contentRevision != previousContentRevision { contentChanged = true }
                if !orderChanged, rows[index] !== row { orderChanged = true }
                updated.append(row)
            }
            let firstPresentation = !hasPresentedSnapshot
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
                    // New rows fade in. A reconnect can deliver a whole batch
                    // of messages in one flush, and they used to pop in at
                    // once. Opacity changes no layout: every row's final
                    // height is applied in this same pass, so the follow and
                    // history logic is not chasing an animation.
                    let publish = {
                        rows = updated
                        rowsByID = Dictionary(uniqueKeysWithValues: updated.map { ($0.id, $0) })
                    }
                    if firstPresentation {
                        // Opening uses the mask fade, not a transcript fade.
                        publish()
                    } else {
                        withAnimation(.easeOut(duration: 0.22)) { publish() }
                    }
                }
            }
            if firstPresentation { hasPresentedSnapshot = true }
            if contentChanged || orderChanged || firstPresentation { presentationRevision &+= 1 }
            self.pending = nil
        }
    }

    func run(sessionID: V2SessionID, repository: V2SessionRepository) async {
        let session = repository.session(id: sessionID)
        self.repository = repository
        let signal = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
        wake = signal.continuation
        defer {
            signal.continuation.finish(); wake = nil
            activityResyncTask?.cancel(); activityResyncTask = nil
            self.repository = nil
        }
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
                    // The server publishes a streaming item every 120 ms. Each
                    // flush re-groups the whole transcript and re-runs the
                    // lazy stack's diff, so coalescing bursts here keeps the
                    // main thread free for the scroll gesture it is serving.
                    do { try await Task.sleep(for: .milliseconds(80)) } catch { return }
                    guard !Task.isCancelled else { return }
                    repeat {
                        // Transport frames are coalesced, then presented in one
                        // main-actor update without an artificial reveal delay.
                        self.flush()
                        self.synchronizePending(session.pendingMessages)
                    } while !Task.isCancelled && self.pending != nil
                }
            }
            await group.waitForAll()
        }
    }

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
        if pendingMessages.map(\.id) != visible.map(\.id) {
            pendingMessages = visible
            presentationRevision &+= 1
        }
    }
}

extension V2TimelineItem {
    /// Whether `other` is the same published version of this item. An unchanged
    /// row can be left alone without walking its retained JSON tree. The full
    /// `==` still compares `raw`, so callers that need the whole value keep it.
    ///
    /// `displayText` is part of the comparison on purpose. A streamed token can
    /// arrive with an unchanged revision and sequence (the server reuses one
    /// revision for several deltas), and a version that does not refresh
    /// `contentHash` would otherwise look unchanged: the row would be skipped
    /// and the token would never reach the screen. Comparing the decoded text
    /// is a cheap string check when it did not change and keeps the reveal
    /// pipeline correct when it did.
    nonisolated func matchesPresentationVersion(of other: Self) -> Bool {
        id == other.id && revision == other.revision && updatedSeq == other.updatedSeq
            && contentHash == other.contentHash && status == other.status
            && type == other.type && role == other.role && orderSeq == other.orderSeq
            && displayText == other.displayText
    }

    var isAssistantText: Bool {
        isReasoning || type == .message && role == .assistant
    }
    var isStreamingText: Bool {
        (status == .pending || status == .running)
            && isAssistantText
    }
}
