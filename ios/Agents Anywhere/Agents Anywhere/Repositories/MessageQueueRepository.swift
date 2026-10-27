import Foundation

/// Repository for message queue operations
@MainActor
public final class MessageQueueRepository: ObservableObject {
    private let transport: any HTTPTransport
    private let sessionID: String

    @Published public private(set) var messages: [QueuedMessage] = []
    @Published public private(set) var isLoading = false
    @Published public private(set) var error: Error?

    private var lastSeq: Int = 0

    public init(transport: any HTTPTransport, sessionID: String) {
        self.transport = transport
        self.sessionID = sessionID
    }

    /// Enqueue a new message
    public func enqueue(content: String, attachments: [String]? = nil, source: String = "ios") async throws -> QueuedMessage {
        isLoading = true
        defer { isLoading = false }

        let request = EnqueueRequest(content: content, attachments: attachments, source: source)
        let message: QueuedMessage = try await post(path: "/api/v2/sessions/\(sessionID)/queue", body: request)

        messages.append(message)
        lastSeq = max(lastSeq, message.seq)

        return message
    }

    /// Fetch queue messages (supports offline recovery with ?since=seq)
    public func fetchMessages(since: Int? = nil) async throws {
        isLoading = true
        defer { isLoading = false }

        let sinceParam = since ?? lastSeq
        let response: QueueResponse = try await get(path: "/api/v2/sessions/\(sessionID)/queue?since=\(sinceParam)")

        // Merge with existing messages
        for newMsg in response.messages {
            if let index = messages.firstIndex(where: { $0.id == newMsg.id }) {
                messages[index] = newMsg
            } else {
                messages.append(newMsg)
            }
            lastSeq = max(lastSeq, newMsg.seq)
        }

        messages.sort { $0.seq < $1.seq }
    }

    /// Cancel a queued message
    public func cancel(messageID: String) async throws {
        isLoading = true
        defer { isLoading = false }

        try await delete(path: "/api/v2/sessions/\(sessionID)/queue/\(messageID)")

        messages.removeAll { $0.id == messageID }
    }

    /// Update message status (typically called by system)
    public func updateStatus(messageID: String, status: QueueStatus, errorMessage: String? = nil) async throws {
        let request = UpdateStatusRequest(status: status.rawValue, errorMessage: errorMessage)
        try await patch(path: "/api/v2/sessions/\(sessionID)/queue/\(messageID)", body: request)

        if let index = messages.firstIndex(where: { $0.id == messageID }) {
            messages[index].status = status
            messages[index].errorMessage = errorMessage
        }
    }

    // MARK: - HTTP Helpers

    private func get<Response: Decodable>(path: String) async throws -> Response {
        try await transport.request(method: .get, path: path, body: EmptyBody?.none)
    }

    private func post<Body: Encodable, Response: Decodable>(path: String, body: Body) async throws -> Response {
        try await transport.request(method: .post, path: path, body: body)
    }

    private func patch<Body: Encodable>(path: String, body: Body) async throws {
        let _: EmptyResponse = try await transport.request(method: .patch, path: path, body: body)
    }

    private func delete(path: String) async throws {
        let _: EmptyResponse = try await transport.request(method: .delete, path: path, body: EmptyBody?.none)
    }
}

// MARK: - Request/Response Models

private struct EnqueueRequest: Encodable {
    let content: String
    let attachments: [String]?
    let source: String
}

private struct QueueResponse: Decodable {
    let messages: [QueuedMessage]
}

private struct UpdateStatusRequest: Encodable {
    let status: String
    let errorMessage: String?
}

private struct EmptyResponse: Decodable {}
private struct EmptyBody: Encodable {}
