import Foundation

/// One user-owned third-party provider. The server never returns the stored
/// credential; only `credentialConfigured` says whether one exists.
nonisolated struct V2ModelProvider: Decodable, Identifiable, Hashable {
    let id: String
    let name: String
    let protocolName: String
    let baseUrl: String
    let authType: String
    let modelId: String
    let modelDisplayName: String
    let credentialConfigured: Bool
    let enabled: Bool
    let revision: Int
    let capabilities: JSONValue
    let createdAt: String
    let updatedAt: String

    enum CodingKeys: String, CodingKey {
        case id, name, baseUrl, authType, modelId, modelDisplayName
        case credentialConfigured, enabled, revision, capabilities, createdAt, updatedAt
        case protocolName = "protocol"
    }

    /// The protocol as the server spells it (`anthropic_messages`,
    /// `openai_chat_completions`).
    static let anthropicProtocol = "anthropic_messages"
    static let openAIProtocol = "openai_chat_completions"
    var isOpenAIProtocol: Bool { protocolName == Self.openAIProtocol }

    /// The most recent user-initiated check, when one was run.
    var lastCheck: V2ModelProviderCheck? {
        guard let entry = capabilities["check"], case .object = entry else { return nil }
        return try? JSONDecoder().decode(V2ModelProviderCheck.self, from: JSONEncoder().encode(entry))
    }
}

nonisolated struct V2ModelProviderCheck: Codable, Hashable {
    /// "connect" (reachability only, no credential sent) or "request" (a real
    /// one-token model request: reachability and authentication).
    let mode: String?
    let reachable: Bool
    let authOk: Bool
    let status: Int?
    let detail: String?
    let at: Int?

    /// Whether the check should be presented as a pass. A connect probe never
    /// proves authentication, so requiring authOk there would always fail.
    var passed: Bool {
        mode == "request" ? reachable && authOk : reachable
    }
}

nonisolated struct V2ModelProviderListResponse: Decodable {
    let providers: [V2ModelProvider]
    let serverTime: String
}

nonisolated struct V2ModelProviderResponse: Decodable {
    let provider: V2ModelProvider
    let serverTime: String
}

nonisolated struct V2ModelProviderCreateRequest: Encodable {
    let name: String
    let protocolName: String
    let baseUrl: String
    let authType: String
    let modelId: String
    let modelDisplayName: String?
    let credential: String?
    let enabled: Bool?

    enum CodingKeys: String, CodingKey {
        case name, baseUrl, authType, modelId, modelDisplayName, credential, enabled
        case protocolName = "protocol"
    }
}

/// PATCH semantics: nil means "leave unchanged". The credential is only
/// touched when `credential` is present (replace) or `clearCredential` is
/// true (remove) — an empty form can never delete the stored key.
nonisolated struct V2ModelProviderPatchRequest: Encodable {
    let name: String?
    let protocolName: String?
    let baseUrl: String?
    let authType: String?
    let modelId: String?
    let modelDisplayName: String?
    let enabled: Bool?
    let credential: String?
    let clearCredential: Bool?
    let expectedRevision: Int?

    enum CodingKeys: String, CodingKey {
        case name, baseUrl, authType, modelId, modelDisplayName, enabled, credential,
             clearCredential, expectedRevision
        case protocolName = "protocol"
    }
}

nonisolated struct V2ModelProviderCheckResponse: Decodable {
    let providerId: String
    let check: V2ModelProviderCheck
    let serverTime: String
}

// MARK: - Routes

/// A provider as a route names it.
nonisolated struct V2ModelProviderRef: Decodable, Hashable {
    let providerId: String
    let providerName: String?
    let modelId: String?
    let modelDisplayName: String?
    let providerRevision: Int?
    let enabled: Bool?
    let missing: Bool?

    var displayName: String { providerName ?? String(localized: "未知供应商") }
}

nonisolated struct V2GatewayStatus: Decodable, Hashable {
    let credentialStatus: String
    let connected: Bool?
    let credentialPrefix: String?
    let createdAt: String?
    let lastUsedAt: String?

    var isActive: Bool { credentialStatus == "active" }
    var isRevoked: Bool { credentialStatus == "revoked" }
}

nonisolated struct V2ConnectorModelRoute: Decodable, Hashable {
    let connectorId: String
    /// The device default. Named `defaultProvider` because `default` is a
    /// keyword; the wire key stays "default".
    let defaultProvider: V2ModelProviderRef?
    let gateway: V2GatewayStatus?

    enum CodingKeys: String, CodingKey {
        case connectorId, gateway
        case defaultProvider = "default"
    }
}

nonisolated struct V2ConnectorModelRouteResponse: Decodable {
    let route: V2ConnectorModelRoute
    let serverTime: String
}

nonisolated struct V2ConnectorModelRouteSetRequest: Encodable {
    let providerId: String?
    let applyToSessions: String?
}

nonisolated struct V2ConnectorModelRouteApplied: Decodable, Hashable {
    let pinned: Int
    let failed: Int
    let failedSessions: [String]?
}

nonisolated struct V2ConnectorModelRouteSetResponse: Decodable {
    let route: V2ConnectorModelRoute
    let applied: V2ConnectorModelRouteApplied?
    let serverTime: String
}

nonisolated struct V2ConnectorGatewayResponse: Decodable {
    let ok: Bool
    let pushed: Bool
    let baseUrl: String?
    let gateway: V2GatewayStatus
    let serverTime: String
}
