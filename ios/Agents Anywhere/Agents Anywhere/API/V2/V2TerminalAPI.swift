import Foundation

protocol V2TerminalAPIProtocol {
    func listTerminals(connectorId: V2ConnectorID) async throws -> V2TerminalListResponse
}

struct V2TerminalAPI: V2TerminalAPIProtocol {
    let transport: any HTTPTransport

    func listTerminals(connectorId: V2ConnectorID) async throws -> V2TerminalListResponse {
        let request = HTTPRequest<EmptyRequestBody, V2TerminalListResponse>(
            method: .get,
            path: "/connectors/\(connectorId.v2URLPathComponentEncoded)/terminals"
        )
        return try await transport.send(request)
    }
}
