import Foundation

/// Wire payload of the server's `runtime.activity` event. The server wraps the
/// connector's `agent.activity` message as `{ "agents": [...] }`; a snapshot or
/// recovery event may omit the array, in which case no subagent is running.
nonisolated struct V2AgentActivity: Decodable, Hashable {
    let agents: [V2AgentRun]

    private enum CodingKeys: String, CodingKey {
        case agents
    }

    init(agents: [V2AgentRun] = []) {
        self.agents = agents
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        agents = try container.decodeIfPresent([V2AgentRun].self, forKey: .agents) ?? []
    }
}

/// Mirrors `protocol.AgentRunning`. Times are Unix timestamps from the machine
/// hosting the connector, normally milliseconds; the presentation accepts
/// seconds and microseconds too so a plugin version change cannot turn a
/// duration into a multi-decade number.
nonisolated struct V2AgentRun: Decodable, Hashable, Identifiable {
    let agentId: String
    let label: String
    let status: String
    let startedAt: Int64
    let updatedAt: Int64

    var id: String { agentId }

    private enum CodingKeys: String, CodingKey {
        case agentId, label, status, startedAt, updatedAt
    }

    init(agentId: String, label: String, status: String, startedAt: Int64, updatedAt: Int64) {
        self.agentId = agentId
        self.label = label
        self.status = status
        self.startedAt = startedAt
        self.updatedAt = updatedAt
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        agentId = try container.decodeIfPresent(String.self, forKey: .agentId) ?? ""
        label = try container.decodeIfPresent(String.self, forKey: .label) ?? ""
        status = try container.decodeIfPresent(String.self, forKey: .status) ?? ""
        startedAt = try container.decodeIfPresent(Int64.self, forKey: .startedAt) ?? 0
        updatedAt = try container.decodeIfPresent(Int64.self, forKey: .updatedAt) ?? 0
    }
}
