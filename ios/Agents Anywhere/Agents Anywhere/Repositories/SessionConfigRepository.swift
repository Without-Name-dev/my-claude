import Foundation

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

        let fetchedConfig: SessionConfig = try await get(path: "/api/v2/sessions/\(sessionID)/config")
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

        let request = UpdateConfigRequest(
            effortLevel: effortLevel,
            modelName: modelName,
            fastMode: fastMode,
            autoMode: autoMode
        )

        let updatedConfig: SessionConfig = try await patch(path: "/api/v2/sessions/\(sessionID)/config", body: request)
        config = updatedConfig
    }

    /// Execute a command
    public func executeCommand(_ command: String) async throws -> CommandResult {
        isLoading = true
        defer { isLoading = false }

        let request = ExecuteCommandRequest(command: command)
        return try await post(path: "/api/v2/sessions/\(sessionID)/commands/execute", body: request)
    }

    /// Fetch agent executions
    public func fetchAgents() async throws -> [AgentExecution] {
        isLoading = true
        defer { isLoading = false }

        let response: AgentsResponse = try await get(path: "/api/v2/sessions/\(sessionID)/agents")
        return response.agents
    }

    /// Fetch workflow runs
    public func fetchWorkflows() async throws -> [WorkflowRun] {
        isLoading = true
        defer { isLoading = false }

        let response: WorkflowsResponse = try await get(path: "/api/v2/sessions/\(sessionID)/workflows")
        return response.workflows
    }

    // MARK: - HTTP Helpers

    private func get<Response: Decodable>(path: String) async throws -> Response {
        try await transport.request(method: .get, path: path, body: EmptyBody?.none)
    }

    private func post<Body: Encodable, Response: Decodable>(path: String, body: Body) async throws -> Response {
        try await transport.request(method: .post, path: path, body: body)
    }

    private func patch<Body: Encodable, Response: Decodable>(path: String, body: Body) async throws -> Response {
        try await transport.request(method: .patch, path: path, body: body)
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

private struct EmptyBody: Encodable {}
