// Model provider management contract (server: go/internal/server/model_provider_handlers.go).
// The credential never crosses the wire in either direction except when the
// user explicitly submits or clears one; only credentialConfigured is visible.

export const MODEL_PROVIDER_PROTOCOLS = [
  "anthropic_messages",
  "openai_chat_completions",
] as const

export type ModelProviderProtocol = (typeof MODEL_PROVIDER_PROTOCOLS)[number]

export function isModelProviderProtocol(value: string): value is ModelProviderProtocol {
  return (MODEL_PROVIDER_PROTOCOLS as readonly string[]).includes(value)
}

export const MODEL_PROVIDER_AUTH_TYPES = ["api_key", "bearer"] as const

export type ModelProviderAuthType = (typeof MODEL_PROVIDER_AUTH_TYPES)[number]

export function isModelProviderAuthType(value: string): value is ModelProviderAuthType {
  return (MODEL_PROVIDER_AUTH_TYPES as readonly string[]).includes(value)
}

export type ModelProviderView = {
  id: string
  name: string
  protocol: string
  baseUrl: string
  authType: string
  modelId: string
  modelDisplayName: string
  credentialConfigured: boolean
  enabled: boolean
  revision: number
  capabilities: Record<string, unknown>
  createdAt: string
  updatedAt: string
}

export type ModelProviderListResponse = {
  providers: ModelProviderView[]
  serverTime: string
}

export type ModelProviderResponse = {
  provider: ModelProviderView
  serverTime: string
}

export type ModelProviderCreateRequest = {
  name: string
  protocol: ModelProviderProtocol
  baseUrl: string
  authType?: ModelProviderAuthType
  modelId: string
  modelDisplayName?: string
  credential?: string
  enabled?: boolean
  capabilities?: Record<string, unknown>
}

/** PATCH body. An absent credential keeps the stored value; clearCredential
 * removes it. expectedRevision is required for optimistic concurrency. */
export type ModelProviderUpdateRequest = {
  name?: string
  protocol?: ModelProviderProtocol
  baseUrl?: string
  authType?: ModelProviderAuthType
  modelId?: string
  modelDisplayName?: string
  enabled?: boolean
  capabilities?: Record<string, unknown>
  credential?: string
  clearCredential?: boolean
  expectedRevision: number
}

export type ModelProviderDeleteResponse = {
  ok: boolean
  connectorDefaults: number
  explicitRoutes: number
  serverTime: string
}

/** Reference counts carried by the 409 provider_referenced detail payload. */
export type ProviderReferences = {
  connectorDefaults: number
  explicitRoutes: number
}

export type ModelProviderCheckMode = "connect" | "request"

export type ModelProviderCheckResult = {
  mode: string
  reachable: boolean
  authOk: boolean
  /** Upstream status; absent for a connect probe, which sends no request. */
  status?: number
  detail?: string | null
  /** Unix milliseconds, matching capabilities.check. */
  at?: number
}

export type ModelProviderCheckResponse = {
  providerId: string
  check: ModelProviderCheckResult
  serverTime: string
}

// ── Connector model route + gateway ────────────────────────────

export type RouteProviderRef = {
  providerId: string
  providerName: string
  modelId: string
  modelDisplayName: string
  providerRevision: number
  enabled: boolean
}

export type GatewayCredentialStatus = "none" | "active" | "revoked"

export type ConnectorGatewayView = {
  credentialStatus: GatewayCredentialStatus
  connected: boolean
  credentialPrefix?: string
  createdAt?: string
  lastUsedAt?: string
}

export type ConnectorModelRouteView = {
  connectorId: string
  default: RouteProviderRef | null
  gateway: ConnectorGatewayView
}

export type ConnectorModelRouteResponse = {
  route: ConnectorModelRouteView
  serverTime: string
}

export type RouteApplyScope = "none" | "inherit_only" | "all"

export type ConnectorModelRouteSetRequest = {
  /** null clears the device default. */
  providerId: string | null
  applyToSessions?: RouteApplyScope
}

export type ConnectorModelRouteApplyResult = {
  pinned: number
  failed: number
  failedSessions: string[]
}

export type ConnectorModelRouteSetResponse = {
  route: ConnectorModelRouteView
  applied?: ConnectorModelRouteApplyResult
  serverTime: string
}

export type ConnectorGatewayEnableResponse = {
  ok: boolean
  /** false means the desktop was offline and the push must be retried. */
  pushed: boolean
  baseUrl: string
  gateway: ConnectorGatewayView
  serverTime: string
}

export type ConnectorGatewayDisableResponse = {
  ok: boolean
  pushed: boolean
  gateway: ConnectorGatewayView
  serverTime: string
}
