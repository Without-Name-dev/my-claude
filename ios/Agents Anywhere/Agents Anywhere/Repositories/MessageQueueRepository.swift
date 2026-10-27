import Foundation
import Combine

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

        let requestBody = EnqueueRequest(content: content, attachments: attachments, source: source)
        let request = HTTPRequest(
            method: .post,
            path: "/api/v2/sessions/\(sessionID)/queue",
            body: requestBody
        )
        let message: QueuedMessage = try await transport.send(request)

        messages.append(message)
        lastSeq = max(lastSeq, message.seq)

        return message
    }

    /// Fetch queue messages (supports offline recovery with ?since=seq)
    public func fetchMessages(since: Int? = nil) async throws {
        isLoading = true
        defer { isLoading = false }

        let sinceParam = since ?? lastSeq
        let request = HTTPRequest<EmptyRequestBody, QueueResponse>(
            method: .get,
            path: "/api/v2/sessions/\(sessionID)/queue",
            queryItems: [URLQueryItem(name: "since", value: String(sinceParam))]
        )
        let response = try await transport.send(request)

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

        let request = HTTPRequest<EmptyRequestBody, EmptyResponse>(
            method: .delete,
            path: "/api/v2/sessions/\(sessionID)/queue/\(messageID)"
        )
        let _: EmptyResponse = try await transport.send(request)

        messages.removeAll { $0.id == messageID }
    }

    /// Update message status (typically called by system)
    public func updateStatus(messageID: String, status: QueueStatus, errorMessage: String? = nil) async throws {
        let requestBody = UpdateStatusRequest(status: status.rawValue, errorMessage: errorMessage)
        let request = HTTPRequest(
            method: .patch,
            path: "/api/v2/sessions/\(sessionID)/queue/\(messageID)",
            body: requestBody
        )
        let _: EmptyResponse = try await transport.send(request)

        if let index = messages.firstIndex(where: { $0.id == messageID }) {
            messages[index].status = status
            messages[index].errorMessage = errorMessage
        }
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
