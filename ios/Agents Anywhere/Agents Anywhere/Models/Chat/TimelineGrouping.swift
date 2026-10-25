import Foundation
import Observation

struct ChatTimelineGroup: Identifiable, Equatable {
    enum Kind: Equatable { case single, tools, reconnect, agents(String) }
    let kind: Kind
    let rows: [ChatTimelineRowModel]
    /// Captured when the group value is built. Row references mutate in place,
    /// so identity comparison alone cannot tell an Equatable wrapper that the
    /// content changed.
    var revision = 0

    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.kind == rhs.kind && lhs.revision == rhs.revision && lhs.rows.count == rhs.rows.count
            && lhs.rows.elementsEqual(rhs.rows, by: { left, right in left.id == right.id })
    }
    // The first row owns the group, even as the second tool arrives. A group
    // growing during a stream does not replace the scroll target or its state.
    var id: String { rows[0].id }
    var status: V2TimelineItemStatus {
        if rows.contains(where: { $0.structure.status.isActive }) { return .running }
        return rows.first(where: { $0.structure.status.isFailure })?.structure.status ?? .done
    }
    var title: String {
        switch kind {
        case .single: return ""
        case .agents: return String(localized: "\(rows.count) 次子 Agent 调用")
        case .reconnect:
            let attempts = rows.compactMap { $0.structure.reconnectAttempt }
            // The full retry messages remain available in the expanded rows.
            return String(localized: "连接重试 · \(rows.count) 次") + (attempts.last.map { "（\($0)）" } ?? "")
        case .tools:
            let reasoning = rows.filter { $0.structure.isReasoning }.count
            let tools = rows.count - reasoning
            return [reasoning > 0 ? String(localized: "\(reasoning) 段思考") : nil, tools > 0 ? String(localized: "\(tools) 次工具调用") : nil].compactMap { $0 }.joined(separator: " · ")
        }
    }
}

enum TimelineGrouping {
    static func groups(_ rows: [ChatTimelineRowModel], interactionTargets: Set<String>) -> [ChatTimelineGroup] {
        var groups: [ChatTimelineGroup] = []
        var pending: [ChatTimelineRowModel] = []
        var pendingKind = ChatTimelineGroup.Kind.single
        func make(_ kind: ChatTimelineGroup.Kind, _ rows: [ChatTimelineRowModel]) -> ChatTimelineGroup {
            ChatTimelineGroup(kind: kind, rows: rows,
                revision: rows.reduce(0) { $0 &+ $1.contentRevision })
        }
        func flush() {
            guard !pending.isEmpty else { return }
            groups.append(make(pending.count > 1 ? pendingKind : .single, pending))
            pending = []
        }
        for row in rows {
            let kind: ChatTimelineGroup.Kind = interactionTargets.contains(row.id) ? .single : row.structure.groupKind
            if kind == .single { flush(); groups.append(make(.single, [row])); continue }
            if pendingKind != kind { flush() }
            pendingKind = kind; pending.append(row)
        }
        flush()
        return groups
    }

    static func reconnectMessage(_ item: V2TimelineItem) -> String? {
        guard item.type == .system, item.status == .failed else { return nil }
        let raw = item.raw["content"]
        let message = TimelineText.first(raw?["details"]?["error"]?["message"], raw?["details"]?["message"], raw?["message"], raw?["text"])
        return message?.hasPrefix("Reconnecting...") == true ? message : nil
    }
}

@MainActor @Observable final class TimelineDisclosureState {
    @Observable fileprivate final class Entry { var expanded = false }
    @ObservationIgnored private var entries: [String: Entry] = [:]
    private func entry(_ id: String) -> Entry {
        if let existing = entries[id] { return existing }
        let value = Entry(); entries[id] = value; return value
    }
    // Each fold observes its own bit. Toggling one tool must not reconstruct
    // every other expanded tool's diff and output subtree.
    func isExpanded(_ id: String) -> Bool { entry(id).expanded }
    func toggle(_ id: String) { entry(id).expanded.toggle() }
}
