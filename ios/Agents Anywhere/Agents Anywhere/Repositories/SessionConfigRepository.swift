import Foundation
import Combine

/// Repository for session configuration management
@MainActor
public final class SessionConfigRepository: ObservableObject {
    private let transport: any HTTPTransport
    private let sessionID: String

    @Published public private(set) var config: SessionConfig?
    @Published public private(set) var isLoading = false
    @Published public private(set) var error: Error?

    public init(transport: any HTTPTransport, sessionID: String) {
        self.transport = transport
        self.sessionID = sessionID
    }

    /// Fetch current session configuration
    public func fetchConfig() async throws {
        isLoading = true
        defer { isLoading = false }

        let request = HTTPRequest<EmptyRequestBody, SessionConfig>(
            method: .get,
            path: "/api/v2/sessions/\(sessionID)/config"
        )
        let fetchedConfig = try await transport.send(request)
        config = fetchedConfig
    }

    /// Update session configuration
    public func updateConfig(
        effortLevel: String? = nil,
        modelName: String? = nil,
        fastMode: Bool? = nil,
        autoMode: Bool? = nil
    ) async throws {
        isLoading = true
        defer { isLoading = false }

        let requestBody = UpdateConfigRequest(
            effortLevel: effortLevel,
            modelName: modelName,
            fastMode: fastMode,
            autoMode: autoMode
        )
        let request = HTTPRequest(
            method: .patch,
            path: "/api/v2/sessions/\(sessionID)/config",
            body: requestBody
        )
        let updatedConfig: SessionConfig = try await transport.send(request)
        config = updatedConfig
    }

    /// Execute a command
    public func executeCommand(_ command: String) async throws -> CommandResult {
        isLoading = true
        defer { isLoading = false }

        let requestBody = ExecuteCommandRequest(command: command)
        let request = HTTPRequest(
            method: .post,
            path: "/api/v2/sessions/\(sessionID)/commands/execute",
            body: requestBody
        )
        return try await transport.send(request)
    }

    /// Fetch agent executions
    public func fetchAgents() async throws -> [AgentExecution] {
        isLoading = true
        defer { isLoading = false }

        let request = HTTPRequest<EmptyRequestBody, AgentsResponse>(
            method: .get,
            path: "/api/v2/sessions/\(sessionID)/agents"
        )
        let response = try await transport.send(request)
        return response.agents
    }

    /// Fetch workflow runs
    public func fetchWorkflows() async throws -> [WorkflowRun] {
        isLoading = true
        defer { isLoading = false }

        let request = HTTPRequest<EmptyRequestBody, WorkflowsResponse>(
            method: .get,
            path: "/api/v2/sessions/\(sessionID)/workflows"
        )
        let response = try await transport.send(request)
        return response.workflows
    }
}

// MARK: - Request/Response Models

private struct UpdateConfigRequest: Encodable {
    let effortLevel: String?
    let modelName: String?
    let fastMode: Bool?
    let autoMode: Bool?
}

private struct ExecuteCommandRequest: Encodable {
    let command: String
}

public struct CommandResult: Decodable {
    public let command: String
    public let result: [String: String]?
}

private struct AgentsResponse: Decodable {
    let agents: [AgentExecution]
}

private struct WorkflowsResponse: Decodable {
    let workflows: [WorkflowRun]
}
