import Foundation

struct V2ModelProviderAPI {
    let transport: any HTTPTransport

    func list() async throws -> V2ModelProviderListResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2ModelProviderListResponse>(
            method: .get, path: "/model-providers"))
    }
    func create(_ body: V2ModelProviderCreateRequest) async throws -> V2ModelProviderResponse {
        try await transport.send(HTTPRequest<V2ModelProviderCreateRequest, V2ModelProviderResponse>(
            method: .post, path: "/model-providers", body: body))
    }
    func provider(_ id: String) async throws -> V2ModelProviderResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2ModelProviderResponse>(
            method: .get, path: path(id)))
    }
    func update(_ id: String, _ body: V2ModelProviderPatchRequest) async throws -> V2ModelProviderResponse {
        try await transport.send(HTTPRequest<V2ModelProviderPatchRequest, V2ModelProviderResponse>(
            method: .patch, path: path(id), body: body))
    }
    func delete(_ id: String, force: Bool) async throws {
        let _: V2ModelProviderDeleteResponse = try await transport.send(
            HTTPRequest<EmptyRequestBody, V2ModelProviderDeleteResponse>(
                method: .delete,
                path: path(id),
                queryItems: force ? [URLQueryItem(name: "force", value: "true")] : []))
    }
    func check(_ id: String, mode: String) async throws -> V2ModelProviderCheckResponse {
        try await transport.send(HTTPRequest<V2ModelProviderCheckRequest, V2ModelProviderCheckResponse>(
            method: .post, path: path(id) + "/check", body: .init(mode: mode)))
    }

    func connectorRoute(_ connectorId: String) async throws -> V2ConnectorModelRouteResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2ConnectorModelRouteResponse>(
            method: .get, path: connectorPath(connectorId)))
    }
    func setConnectorRoute(
        _ connectorId: String, providerId: String?, applyToSessions: String
    ) async throws -> V2ConnectorModelRouteSetResponse {
        try await transport.send(HTTPRequest<V2ConnectorModelRouteSetRequest, V2ConnectorModelRouteSetResponse>(
            method: .put, path: connectorPath(connectorId),
            body: .init(providerId: providerId, applyToSessions: applyToSessions)))
    }
    func enableGateway(_ connectorId: String) async throws -> V2ConnectorGatewayResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2ConnectorGatewayResponse>(
            method: .post, path: connectorPath(connectorId) + "/gateway"))
    }
    func disableGateway(_ connectorId: String) async throws -> V2ConnectorGatewayResponse {
        try await transport.send(HTTPRequest<EmptyRequestBody, V2ConnectorGatewayResponse>(
            method: .delete, path: connectorPath(connectorId) + "/gateway"))
    }

    private func path(_ id: String) -> String { "/model-providers/" + id.v2URLPathComponentEncoded }
    private func connectorPath(_ id: String) -> String {
        "/connectors/" + id.v2URLPathComponentEncoded + "/model-route"
    }
}

nonisolated struct V2ModelProviderDeleteResponse: Decodable {
    let ok: Bool?
    let connectorDefaults: Int?
    let explicitRoutes: Int?
    let serverTime: String?
}

nonisolated struct V2ModelProviderCheckRequest: Encodable { let mode: String }
