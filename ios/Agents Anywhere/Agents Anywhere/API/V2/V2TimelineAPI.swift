import Foundation

protocol V2TimelineAPIProtocol {
    func editTimelineItem(sessionId: V2SessionID, itemId: String, content: String) async throws -> V2TimelineEditResponse
    func retryMessage(sessionId: V2SessionID, itemId: String) async throws -> V2TimelineRetryResponse
}

struct V2TimelineAPI: V2TimelineAPIProtocol {
    let transport: any HTTPTransport
    
    func editTimelineItem(sessionId: V2SessionID, itemId: String, content: String) async throws -> V2TimelineEditResponse {
        let request = HTTPRequest<V2TimelineEditRequest, V2TimelineEditResponse>(
            method: .patch,
            path: "/sessions/\(sessionId)/timeline/\(itemId)",
            body: V2TimelineEditRequest(content: content)
        )
        return try await transport.send(request)
    }
    
    func retryMessage(sessionId: V2SessionID, itemId: String) async throws -> V2TimelineRetryResponse {
        let request = HTTPRequest<EmptyRequestBody, V2TimelineRetryResponse>(
            method: .post,
            path: "/sessions/\(sessionId)/timeline/\(itemId)/retry"
        )
        return try await transport.send(request)
    }
}

struct V2TimelineEditRequest: Codable {
    let content: String
}

struct V2TimelineEditResponse: Codable {
    let ok: Bool
}

struct V2TimelineRetryResponse: Codable {
    let ok: Bool
    let result: V2TimelineRetryResult?
}

struct V2TimelineRetryResult: Codable {
    let messageId: String
}
