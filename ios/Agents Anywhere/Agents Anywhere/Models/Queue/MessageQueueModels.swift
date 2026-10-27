import Foundation

// MARK: - Queue Message Models

/// Represents a queued message in the message queue system
public struct QueuedMessage: Identifiable, Codable, Equatable {
    public let id: String
    public let sessionId: String
    public let content: String
    public let attachments: [String]?
    public let status: QueueStatus
    public let source: String
    public let errorMessage: String?
    public let createdAt: Date
    public let startedAt: Date?
    public let completedAt: Date?
    public let seq: Int

    enum CodingKeys: String, CodingKey {
        case id, content, attachments, status, source
        case sessionId = "sessionId"
        case errorMessage = "errorMessage"
        case createdAt = "createdAt"
        case startedAt = "startedAt"
        case completedAt = "completedAt"
        case seq
    }
}

/// Queue message status
public enum QueueStatus: String, Codable, Equatable {
    case queued
    case running
    case completed
    case error
    case cancelled

    public var displayName: String {
        switch self {
        case .queued: return "排队中"
        case .running: return "执行中"
        case .completed: return "已完成"
        case .error: return "失败"
        case .cancelled: return "已取消"
        }
    }

    public var color: String {
        switch self {
        case .queued: return "yellow"
        case .running: return "blue"
        case .completed: return "green"
        case .error: return "red"
        case .cancelled: return "gray"
        }
    }
}

// MARK: - Session Configuration Models

/// Session configuration for effort, model, and modes
public struct SessionConfig: Codable, Equatable {
    public let sessionId: String
    public var effortLevel: EffortLevel
    public var modelName: String
    public var fastMode: Bool
    public var autoMode: Bool
    public let updatedAt: Date

    enum CodingKeys: String, CodingKey {
        case sessionId = "sessionId"
        case effortLevel = "effortLevel"
        case modelName = "modelName"
        case fastMode = "fastMode"
        case autoMode = "autoMode"
        case updatedAt = "updatedAt"
    }
}

/// Effort level for reasoning
public enum EffortLevel: String, Codable, Equatable, CaseIterable {
    case low
    case medium
    case high
    case xhigh
    case max
    case auto

    public var displayName: String {
        switch self {
        case .low: return "低"
        case .medium: return "中"
        case .high: return "高"
        case .xhigh: return "极高"
        case .max: return "最高"
        case .auto: return "自动"
        }
    }
}

// MARK: - Agent and Workflow Models

/// Agent execution for workflow visualization
public struct AgentExecution: Identifiable, Codable, Equatable {
    public let id: String
    public let sessionId: String
    public let parentId: String?
    public let label: String
    public let phase: String?
    public let status: AgentStatus
    public let startedAt: Date?
    public let finishedAt: Date?
    public let tokenCount: Int
    public let errorMessage: String?

    enum CodingKeys: String, CodingKey {
        case id, label, phase, status
        case sessionId = "sessionId"
        case parentId = "parentId"
        case startedAt = "startedAt"
        case finishedAt = "finishedAt"
        case tokenCount = "tokenCount"
        case errorMessage = "errorMessage"
    }
}

/// Agent execution status
public enum AgentStatus: String, Codable, Equatable {
    case pending
    case running
    case completed
    case error
    case cancelled

    public var displayName: String {
        switch self {
        case .pending: return "等待中"
        case .running: return "运行中"
        case .completed: return "已完成"
        case .error: return "失败"
        case .cancelled: return "已取消"
        }
    }
}

/// Workflow run for dashboard
public struct WorkflowRun: Identifiable, Codable, Equatable {
    public let id: String
    public let sessionId: String
    public let name: String
    public let status: WorkflowStatus
    public let currentPhase: String?
    public let phaseCount: Int
    public let agentCount: Int
    public let totalTokens: Int
    public let startedAt: Date
    public let finishedAt: Date?
    public let errorMessage: String?

    enum CodingKeys: String, CodingKey {
        case id, name, status, phaseCount, agentCount, totalTokens
        case sessionId = "sessionId"
        case currentPhase = "currentPhase"
        case startedAt = "startedAt"
        case finishedAt = "finishedAt"
        case errorMessage = "errorMessage"
    }
}

/// Workflow status
public enum WorkflowStatus: String, Codable, Equatable {
    case running
    case completed
    case error
    case cancelled

    public var displayName: String {
        switch self {
        case .running: return "运行中"
        case .completed: return "已完成"
        case .error: return "失败"
        case .cancelled: return "已取消"
        }
    }
}
