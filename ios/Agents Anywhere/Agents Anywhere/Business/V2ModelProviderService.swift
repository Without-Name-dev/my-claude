import Foundation

/// Account-scoped model provider management and device gateway control. The
/// service is a thin pass-through to the API: validation lives on the server,
/// and the views are responsible for presenting every outcome honestly.
struct V2ModelProviderService {
    let providerAPI: V2ModelProviderAPI

    func providers() async throws -> [V2ModelProvider] {
        try await providerAPI.list().providers
    }

    func create(_ request: V2ModelProviderCreateRequest) async throws -> V2ModelProvider {
        try await providerAPI.create(request).provider
    }

    func update(_ id: String, _ request: V2ModelProviderPatchRequest) async throws -> V2ModelProvider {
        try await providerAPI.update(id, request).provider
    }

    func delete(_ id: String, force: Bool) async throws {
        try await providerAPI.delete(id, force: force)
    }

    func check(_ id: String, mode: String) async throws -> V2ModelProviderCheck {
        try await providerAPI.check(id, mode: mode).check
    }

    func connectorRoute(_ connectorId: V2ConnectorID) async throws -> V2ConnectorModelRoute {
        try await providerAPI.connectorRoute(connectorId).route
    }

    func setConnectorRoute(
        _ connectorId: V2ConnectorID,
        providerId: String?,
        applyToSessions: String
    ) async throws -> V2ConnectorModelRouteSetResponse {
        try await providerAPI.setConnectorRoute(
            connectorId, providerId: providerId, applyToSessions: applyToSessions)
    }

    func enableGateway(_ connectorId: V2ConnectorID) async throws -> V2ConnectorGatewayResponse {
        try await providerAPI.enableGateway(connectorId)
    }

    func disableGateway(_ connectorId: V2ConnectorID) async throws -> V2ConnectorGatewayResponse {
        try await providerAPI.disableGateway(connectorId)
    }
}
