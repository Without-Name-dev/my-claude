import Foundation

/// Reduces server projections without network I/O or UI state. Live projections may
/// change A -> B -> A at the same durable sequence and must not use event-ID dedup.
struct V2SessionProjection {
    private(set) var data: V2SessionData
    private let maximumItems: Int
    private var eventIDs: Set<String> = []
    /// Fixed-size ring of recent durable event IDs; replacing the oldest slot
    /// avoids shifting the whole window for every event once it is full.
    private var eventOrder: [String] = []
    private var eventOrderNext = 0
    private static let dedupeWindow = 2048
    private let decoder = JSONDecoder()

    init(snapshot: V2SessionSnapshot, maximumItems: Int) {
        data = V2SessionData(snapshot: snapshot)
        self.maximumItems = max(1, maximumItems)
        replaceTimeline(snapshot.timeline.items, hasMore: snapshot.timeline.hasMore)
    }

    init(archive: V2SessionSnapshot, hasNewerItems: Bool, maximumItems: Int) {
        self.init(snapshot: archive, maximumItems: maximumItems)
        data.hasNewerItems = hasNewerItems
    }

    static func placeholder(_ session: V2SessionMeta, maximumItems: Int) -> Self {
        let empty = V2RuntimeCapabilitySnapshot(revision: 0, capabilities: [])
        return Self(snapshot: .init(session: session, state: nil, timeline: .init(items: [], nextSeq: 0, hasMore: false),
            approvals: [], notices: [], effectiveCapabilities: empty, runtimeCapabilities: empty, catalogs: [:],
            eventCursor: "seq:0", serverTime: ""), maximumItems: maximumItems)
    }

    var sequence: Int { Self.sequence(data.cursor) }

    static func sequence(_ cursor: String) -> Int {
        Int(cursor.dropFirst(4)) ?? 0
    }

    mutating func advanceCursor(_ cursor: String) {
        if Self.sequence(cursor) > sequence { data.cursor = cursor }
    }

    mutating func markStale() { data.liveStateIsFresh = false }

    mutating func applyLive(_ live: V2SessionLiveState) {
        guard live.state.sessionId == data.session.id,
              (live.state.runtimeId ?? live.state.runtime) == data.session.effectiveRuntimeId else { return }
        // The read can race a newer frame that was already in flight: never
        // replace a newer state or capability set with an older one.
        let sameRuntime = data.state.map {
            ($0.runtimeId ?? $0.runtime) == (live.state.runtimeId ?? live.state.runtime)
        } ?? false
        if !sameRuntime || live.state.updatedSeq >= (data.state?.updatedSeq ?? 0) {
            data.state = live.state
        }
        if live.capabilities.revision >= data.capabilities.revision {
            data.capabilities = live.capabilities
        }
        data.notices = live.notices
        data.liveStateIsFresh = data.session.connectorStatus == .online
    }

    /// Returns false when the metadata was stale or identical, so callers can
    /// skip republishing the whole projection.
    @discardableResult mutating func applyMeta(_ session: V2SessionMeta) -> Bool {
        guard session.id == data.session.id, session.updatedSeq >= data.session.updatedSeq,
              session != data.session else { return false }
        let changedRuntime = session.effectiveRuntimeId != data.session.effectiveRuntimeId
        data.session = session
        if changedRuntime || session.connectorStatus != .online { markStale() }
        return true
    }

    mutating func applyState(_ state: V2RuntimeState) {
        guard state.sessionId == data.session.id,
              (state.runtimeId ?? state.runtime) == data.session.effectiveRuntimeId else { return }
        // State is monotonic per runtime. An HTTP response that awaited while a
        // newer socket frame landed used to flip the page back to "running"
        // (or re-open a resolved approval); an older frame must not win.
        if let current = data.state,
           (current.runtimeId ?? current.runtime) == (state.runtimeId ?? state.runtime),
           state.updatedSeq < current.updatedSeq {
            return
        }
        data.state = state
    }

    mutating func applyCapabilities(_ snapshot: V2RuntimeCapabilitySnapshot) {
        guard snapshot.revision >= data.capabilities.revision else { return }
        data.capabilities = snapshot
    }

    mutating func apply(_ event: V2SessionEvent) throws {
        guard event.sessionId == data.session.id else { return }
        // Deduplicate first. The cursor comparison is not a safe gate on its
        // own: the server publishes after releasing the session lock, so N+1
        // can arrive before N. Dropping N because the cursor already passed it
        // lost the row for good — the recovery query starts at the cursor — and
        // left an echoed message stuck on "accepted".
        if !event.isLiveProjection, eventIDs.contains(event.eventId) { return }
        if event.sequence < sequence {
            // The cursor is ahead of this frame, so dropping it would lose it:
            // recovery starts at the cursor. Every applicator below is
            // idempotent and revision-guarded, so an older copy can only fill
            // a gap. The cursor is deliberately not advanced.
            switch event.type {
            case "timeline.item_created", "timeline.item_updated":
                merge([try item("item", in: event)], history: false)
            case "session.meta.updated":
                applyMeta(try payload("session", in: event))
            case "runtime.state.updated":
                // A late state frame used to be dropped for good, leaving the
                // page on "running" or showing a resolved approval after the
                // turn ended.
                applyState(try payload("state", in: event))
            case "runtime.capability.updated":
                applyCapabilities(try payload("capabilitySet", in: event))
            case "timeline.snapshot":
                // merge keeps the newest revision of every item.
                mergeSnapshot(try items("items", in: event), hasMore: event.payload["hasMore"]?.boolValue ?? false)
            case "runtime.notice.snapshot":
                mergeNotices(try payload("notices", in: event))
            case "runtime.notice.updated":
                mergeNotices([try payload("notice", in: event)])
            default:
                break
            }
            remember(event)
            return
        }
        switch event.type {
        case "session.meta.updated":
            applyMeta(try payload("session", in: event))
        case "runtime.state.updated":
            applyState(try payload("state", in: event))
        case "runtime.capability.updated":
            applyCapabilities(try payload("capabilitySet", in: event))
        case "timeline.item_created", "timeline.item_updated":
            merge([try item("item", in: event)], history: false)
        case "timeline.snapshot":
            mergeSnapshot(try items("items", in: event), hasMore: event.payload["hasMore"]?.boolValue ?? false)
        case "runtime.notice.snapshot":
            data.notices = try payload("notices", in: event)
        case "runtime.notice.updated":
            mergeNotices([try payload("notice", in: event)])
        case "runtime.catalog.updated":
            break // Catalog reads are independently cached and invalidated by the repository.
        case "session.subscribed", "session.refetch_required":
            return // These are recovery signals, never cursor acknowledgements.
        default:
            data.lastExtensionEvent = event
        }
        advanceCursor(event.cursor)
        remember(event)
    }

    private mutating func remember(_ event: V2SessionEvent) {
        guard !event.isLiveProjection else { return }
        eventIDs.insert(event.eventId)
        if eventOrder.count < Self.dedupeWindow {
            eventOrder.append(event.eventId)
        } else {
            eventIDs.remove(eventOrder[eventOrderNext])
            eventOrder[eventOrderNext] = event.eventId
            eventOrderNext = (eventOrderNext + 1) % Self.dedupeWindow
        }
    }

    mutating func applyHistory(_ page: V2SessionTimelinePage) {
        guard page.sessionId == data.session.id else { return }
        merge(page.items, history: true)
        data.hasOlderItems = page.hasMore
        // nextSeq is the server high watermark, not proof that intervening events were read.
    }

    /// Opening starts from one page even if a previous visit loaded more. Keep
    /// the event cursor and newer-window flag; this only narrows durable history.
    @discardableResult mutating func limitToLatest(_ count: Int) -> Bool {
        let count = max(1, min(count, maximumItems))
        guard data.items.count > count else { return false }
        data.items = Array(data.items.suffix(count))
        data.hasOlderItems = true
        return true
    }

    mutating func applyLatest(_ page: V2SessionTimelinePage) {
        guard page.sessionId == data.session.id else { return }
        // A GET can finish after newer socket frames. Its high watermark bounds
        // the window it may replace, but never acknowledges the event cursor.
        let newer = data.items.filter { $0.updatedSeq > page.nextSeq }
        // A refresh must not discard the pages the person already scrolled
        // into. Replacing the whole window with the newest 100 collapsed the
        // transcript by thousands of points mid-read — the visible jump — and
        // forced a reload of history that was already on screen.
        let boundary = page.items.first?.orderSeq
        let older = boundary.map { first in data.items.filter { $0.orderSeq < first } } ?? []
        replaceTimeline(older + page.items, hasMore: page.hasMore || !older.isEmpty)
        merge(newer, history: false)
    }

    private mutating func replaceTimeline(_ items: [V2TimelineItem], hasMore: Bool) {
        data.items = []
        data.hasNewerItems = false
        data.hasOlderItems = hasMore
        merge(items, history: false)
    }

    private mutating func mergeSnapshot(_ items: [V2TimelineItem], hasMore: Bool) {
        merge(items, history: false, preservingCurrentWindow: true)
        data.hasOlderItems = data.hasOlderItems || hasMore
    }

    /// Merge notices by revision without removing any. Used when the frame may
    /// be older than what is already applied; an authoritative snapshot still
    /// replaces the list.
    private mutating func mergeNotices(_ incoming: [V2RuntimeNotice]) {
        for notice in incoming where notice.sessionId == data.session.id {
            if let index = data.notices.firstIndex(where: { $0.id == notice.id }) {
                if notice.revision >= data.notices[index].revision { data.notices[index] = notice }
            } else {
                data.notices.append(notice)
            }
        }
    }

    private mutating func merge(_ incoming: [V2TimelineItem], history: Bool, preservingCurrentWindow: Bool = false) {
        if !history, !preservingCurrentWindow, incoming.count == 1 { mergeOne(incoming[0]); return }
        // A snapshot or history page can contain hundreds of items. Inserting
        // each into the array and shifting every later index made recovery
        // quadratic on the main actor. Resolve versions by ID, then sort once.
        var itemsByID: [V2TimelineItemID: V2TimelineItem] = [:]
        itemsByID.reserveCapacity(data.items.count + incoming.count)
        for item in data.items { itemsByID[item.id] = item }
        let start = data.items.first?.orderSeq ?? 0
        let end = data.items.last?.orderSeq ?? 0

        for item in incoming where item.sessionId == data.session.id {
            if let old = itemsByID[item.id] {
                guard item.revision > old.revision || (item.revision == old.revision && item.updatedSeq >= old.updatedSeq) else { continue }
                itemsByID[item.id] = item
            } else if !history, data.hasOlderItems, item.orderSeq < start {
                continue
            } else if !history, data.hasNewerItems, item.orderSeq > end {
                continue
            } else {
                itemsByID[item.id] = item
            }
        }
        data.items = itemsByID.values.sorted { ($0.orderSeq, $0.id) < ($1.orderSeq, $1.id) }

        // Apply window limits
        if preservingCurrentWindow {
            // Keep all
        } else if history {
            if data.items.count > maximumItems {
                data.items = Array(data.items.prefix(maximumItems))
                data.hasNewerItems = true
            }
        } else {
            if data.items.count > maximumItems {
                data.items = Array(data.items.suffix(maximumItems))
                data.hasOlderItems = true
            }
        }
    }

    /// A streamed token is one `item_updated` for an item that is almost always
    /// already present. Replace it in place instead of rebuilding a dictionary
    /// and re-sorting the whole window for every socket frame.
    private mutating func mergeOne(_ item: V2TimelineItem) {
        guard item.sessionId == data.session.id else { return }
        let key = (item.orderSeq, item.id)
        let position = insertionIndex(of: key)
        if position < data.items.count, data.items[position].id == item.id {
            let old = data.items[position]
            guard item.revision > old.revision || (item.revision == old.revision && item.updatedSeq >= old.updatedSeq) else { return }
            data.items[position] = item
            return
        }
        // An item whose orderSeq moved sorts elsewhere; the server keeps orderSeq
        // stable, so this scan only runs for genuinely new or relocated items.
        if let existing = data.items.firstIndex(where: { $0.id == item.id }) {
            let old = data.items[existing]
            guard item.revision > old.revision || (item.revision == old.revision && item.updatedSeq >= old.updatedSeq) else { return }
            data.items.remove(at: existing)
            data.items.insert(item, at: insertionIndex(of: key))
            return
        }
        let start = data.items.first?.orderSeq ?? 0
        let end = data.items.last?.orderSeq ?? 0
        if data.hasOlderItems, item.orderSeq < start { return }
        if data.hasNewerItems, item.orderSeq > end { return }
        data.items.insert(item, at: position)
        if data.items.count > maximumItems {
            data.items.removeFirst(data.items.count - maximumItems)
            data.hasOlderItems = true
        }
    }

    private func insertionIndex(of key: (Int, String)) -> Int {
        var low = 0, high = data.items.count
        while low < high {
            let mid = (low + high) / 2
            let candidate = (data.items[mid].orderSeq, data.items[mid].id)
            if candidate < key { low = mid + 1 } else { high = mid }
        }
        return low
    }

    private func item(_ key: String, in event: V2SessionEvent) throws -> V2TimelineItem {
        guard let raw = event.payload[key] else {
            throw HTTPError.decoding(message: String(localized: "Missing '\(key)' in \(event.type)."))
        }
        do { return try V2TimelineItem(json: raw) }
        catch let error as DecodingError {
            throw HTTPError.decoding(message: "\(event.type) · \(key): \(error.v2Description)")
        }
    }

    private func items(_ key: String, in event: V2SessionEvent) throws -> [V2TimelineItem] {
        guard let raw = event.payload[key]?.arrayValue else {
            throw HTTPError.decoding(message: String(localized: "Missing '\(key)' in \(event.type)."))
        }
        do { return try raw.map { try V2TimelineItem(json: $0) } }
        catch let error as DecodingError {
            throw HTTPError.decoding(message: "\(event.type) · \(key): \(error.v2Description)")
        }
    }

    private func payload<Value: Decodable>(_ key: String, in event: V2SessionEvent) throws -> Value {
        guard let raw = event.payload[key] else {
            throw HTTPError.decoding(message: String(localized: "Missing '\(key)' in \(event.type)."))
        }
        do { return try decoder.decode(Value.self, from: JSONEncoder().encode(raw)) }
        catch let error as DecodingError {
            throw HTTPError.decoding(message: "\(event.type) · \(key): \(error.v2Description)")
        }
    }
}
