import Foundation

/// Presentation for the `runtime.activity` status line. The server sends the
/// set of subagents that are running *now*; this type remembers the agents it
/// has already seen so a row that disappears from the payload becomes a
/// finished row rather than vanishing from the expanded block.
nonisolated struct SubagentActivityPresentation: Identifiable, Equatable {
    nonisolated struct Agent: Identifiable, Equatable {
        let id: String
        let label: String
        private(set) var status: String
        let startedAt: Date?
        private(set) var endedAt: Date?
        /// Live activity from the runtime. Empty means the API did not expose
        /// the fact; it is never filled with a guess.
        private(set) var tool: String
        private(set) var detail: String
        private(set) var text: String

        init(
            id: String,
            label: String,
            status: String,
            startedAt: Date?,
            endedAt: Date?,
            tool: String = "",
            detail: String = "",
            text: String = ""
        ) {
            self.id = id
            self.label = label
            self.status = status
            self.startedAt = startedAt
            self.endedAt = endedAt
            self.tool = tool
            self.detail = detail
            self.text = text
        }

        /// "Bash · go test ./...", the line the terminal draws under the agent.
        var toolLine: String? {
            let tool = tool.trimmingCharacters(in: .whitespacesAndNewlines)
            let detail = detail.trimmingCharacters(in: .whitespacesAndNewlines)
            if tool.isEmpty { return detail.isEmpty ? nil : detail }
            return detail.isEmpty ? tool : "\(tool) · \(detail)"
        }

        /// The agent's latest text, shown only when there is some.
        var latestText: String? {
            let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
            return trimmed.isEmpty ? nil : trimmed
        }

        var isRunning: Bool {
            endedAt == nil && !SubagentActivityPresentation.isTerminal(status)
        }

        var displayLabel: String {
            let trimmed = label.trimmingCharacters(in: .whitespacesAndNewlines)
            if !trimmed.isEmpty { return trimmed }
            let identifier = id.trimmingCharacters(in: .whitespacesAndNewlines)
            return identifier.isEmpty ? String(localized: "子代理") : identifier
        }

        var markerStatus: V2TimelineItemStatus {
            guard !isRunning else { return .running }
            switch SubagentActivityPresentation.normalized(status) {
            case "failed", "error": return .failed
            case "cancelled", "canceled", "interrupted", "stopped", "aborted": return .interrupted
            default: return .done
            }
        }

        var statusText: String {
            guard !isRunning else { return String(localized: "运行中") }
            switch SubagentActivityPresentation.normalized(status) {
            case "failed", "error": return String(localized: "失败")
            case "cancelled", "canceled", "interrupted", "stopped", "aborted": return String(localized: "已停止")
            default: return String(localized: "已完成")
            }
        }

        mutating func finish(at date: Date) {
            endedAt = date
            if !SubagentActivityPresentation.isTerminal(status) { status = "done" }
        }
    }

    /// A new epoch is a new block identity (a new turn's subagents). Keeping it
    /// stable while agents start and finish preserves expansion state and the
    /// lazy row's identity.
    private(set) var epoch = 0
    private(set) var agents: [Agent] = []

    init() {}

    var id: String { "runtime.activity:\(epoch)" }
    var isRunning: Bool { agents.contains(where: \.isRunning) }

    var startedAt: Date? {
        agents.filter(\.isRunning).compactMap(\.startedAt).min()
            ?? agents.compactMap(\.startedAt).min()
    }

    var markerStatus: V2TimelineItemStatus {
        if isRunning { return .running }
        if agents.contains(where: { $0.markerStatus == .failed }) { return .failed }
        if agents.contains(where: { $0.markerStatus == .interrupted }) { return .interrupted }
        return .done
    }

    /// Applies one full activity snapshot. Agents absent from `runs` have
    /// stopped; agents still present may be either running or terminal.
    mutating func receive(_ runs: [V2AgentRun], at date: Date) {
        let incomingIsRunning = runs.contains { !Self.isTerminal($0.status) }
        // A new subagent after every known agent finished belongs to a new
        // block. Do not let yesterday's finished count keep growing.
        if !agents.isEmpty, !isRunning, incomingIsRunning {
            agents = []
            epoch &+= 1
        }
        // Agent ids are row identities. Uniquing keeps a duplicate (from a
        // future caller or a malformed snapshot) from trapping here; the loop
        // below still applies the newest values.
        var byID = Dictionary(agents.map { ($0.id, $0) }, uniquingKeysWith: { _, newest in newest })
        let present = Set(runs.map(\.agentId))
        for run in runs {
            let existing = byID[run.agentId]
            let start = Self.date(from: run.startedAt) ?? existing?.startedAt ?? date
            let status = run.status.isEmpty ? "running" : run.status
            let ended: Date? = Self.isTerminal(status)
                ? Self.later(start, Self.date(from: run.updatedAt) ?? existing?.endedAt ?? date)
                : nil
            // Each frame carries the agent's live fields in full. An empty
            // value is the runtime clearing it (a new model response clears the
            // text, a new tool clears the previous detail), so the payload is
            // authoritative. Keeping the previous value on an empty field
            // would leave the last step's text on screen for the rest of the
            // run. An older connector simply sends none and leaves these empty.
            let tool = run.tool
            let detail = run.detail
            let text = run.text
            byID[run.agentId] = Agent(id: run.agentId, label: run.label, status: status,
                startedAt: start, endedAt: ended, tool: tool, detail: detail, text: text)
        }
        let missing = byID.values
            .filter { !present.contains($0.id) && $0.endedAt == nil }
            .map(\.id)
        for identifier in missing {
            guard var agent = byID[identifier] else { continue }
            agent.finish(at: date)
            byID[identifier] = agent
        }
        agents = byID.values.sorted {
            let left = $0.startedAt ?? date
            let right = $1.startedAt ?? date
            return left == right ? $0.id < $1.id : left < right
        }
    }

    /// The collapsed header line. Duration is computed by the view each second
    /// while the block is running; finished blocks only need a stable line.
    func headerTitle(now: Date) -> String {
        guard !agents.isEmpty else { return "" }
        if isRunning {
            let running = agents.filter(\.isRunning)
            let duration = Self.durationText(from: startedAt ?? now, to: now)
            if running.count == 1, let agent = running.first {
                return String(localized: "子代理 \(agent.displayLabel) · 运行中 \(duration)")
            }
            return String(localized: "\(running.count) 个子代理 · 运行中 \(duration)")
        }
        return String(localized: "已结束 \(agents.count) 个子代理")
    }

    func rowTitle(_ agent: Agent, now: Date) -> String {
        let duration = Self.durationText(from: agent.startedAt ?? now, to: agent.endedAt ?? now)
        if agent.isRunning {
            return String(localized: "\(agent.displayLabel) · 运行中 \(duration)")
        }
        return String(localized: "\(agent.displayLabel) · \(agent.statusText) \(duration)")
    }

    static func durationText(from start: Date, to end: Date) -> String {
        let seconds = max(0, Int(end.timeIntervalSince(start)))
        if seconds < 60 { return "\(seconds)s" }
        let minutes = seconds / 60
        let remaining = seconds % 60
        if minutes < 60 { return "\(minutes)m \(remaining)s" }
        return "\(minutes / 60)h \(minutes % 60)m"
    }

    fileprivate static func normalized(_ status: String) -> String {
        status.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
    }

    fileprivate static func isTerminal(_ status: String) -> Bool {
        switch normalized(status) {
        case "done", "completed", "complete", "succeeded", "success", "finished",
             "failed", "error", "cancelled", "canceled", "interrupted", "stopped",
             "aborted", "closed", "ended", "exited":
            return true
        default:
            return false
        }
    }

    private static func date(from timestamp: Int64) -> Date? {
        guard timestamp > 0 else { return nil }
        let seconds: Double
        switch timestamp {
        case ..<100_000_000_000: seconds = Double(timestamp)
        case ..<100_000_000_000_000: seconds = Double(timestamp) / 1_000
        case ..<100_000_000_000_000_000: seconds = Double(timestamp) / 1_000_000
        default: seconds = Double(timestamp) / 1_000_000_000
        }
        return Date(timeIntervalSince1970: seconds)
    }

    private static func later(_ start: Date, _ end: Date) -> Date {
        end < start ? start : end
    }
}
