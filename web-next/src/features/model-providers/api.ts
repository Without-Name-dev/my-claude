import { ApiClient, ApiError, apiClient } from "@/lib/api"
import type {
  ConnectorGatewayDisableResponse,
  ConnectorGatewayEnableResponse,
  ConnectorModelRouteResponse,
  ConnectorModelRouteSetRequest,
  ConnectorModelRouteSetResponse,
  ModelProviderCheckMode,
  ModelProviderCheckResponse,
  ModelProviderCreateRequest,
  ModelProviderDeleteResponse,
  ModelProviderListResponse,
  ModelProviderResponse,
  ModelProviderUpdateRequest,
  ProviderReferences,
} from "./types"

export class ModelProvidersApi {
  constructor(private readonly client: ApiClient = apiClient) {}

  list(token: string): Promise<ModelProviderListResponse> {
    return this.client.get<ModelProviderListResponse>("/model-providers", { token })
  }

  create(token: string, body: ModelProviderCreateRequest): Promise<ModelProviderResponse> {
    return this.client.post<ModelProviderResponse>("/model-providers", body, { token })
  }

  get(token: string, providerId: string): Promise<ModelProviderResponse> {
    return this.client.get<ModelProviderResponse>(
      `/model-providers/${encodeURIComponent(providerId)}`,
      { token },
    )
  }

  update(
    token: string,
    providerId: string,
    body: ModelProviderUpdateRequest,
  ): Promise<ModelProviderResponse> {
    return this.client.patch<ModelProviderResponse>(
      `/model-providers/${encodeURIComponent(providerId)}`,
      body,
      { token },
    )
  }

  delete(
    token: string,
    providerId: string,
    options: { force?: boolean } = {},
  ): Promise<ModelProviderDeleteResponse> {
    return this.client.delete<ModelProviderDeleteResponse>(
      `/model-providers/${encodeURIComponent(providerId)}`,
      { token, query: options.force ? { force: "true" } : undefined },
    )
  }

  check(
    token: string,
    providerId: string,
    mode: ModelProviderCheckMode,
  ): Promise<ModelProviderCheckResponse> {
    return this.client.post<ModelProviderCheckResponse>(
      `/model-providers/${encodeURIComponent(providerId)}/check`,
      { mode },
      { token },
    )
  }

  getConnectorRoute(token: string, connectorId: string): Promise<ConnectorModelRouteResponse> {
    return this.client.get<ConnectorModelRouteResponse>(
      `/connectors/${encodeURIComponent(connectorId)}/model-route`,
      { token },
    )
  }

  setConnectorRoute(
    token: string,
    connectorId: string,
    body: ConnectorModelRouteSetRequest,
  ): Promise<ConnectorModelRouteSetResponse> {
    return this.client.put<ConnectorModelRouteSetResponse>(
      `/connectors/${encodeURIComponent(connectorId)}/model-route`,
      body,
      { token },
    )
  }

  enableConnectorGateway(token: string, connectorId: string): Promise<ConnectorGatewayEnableResponse> {
    return this.client.post<ConnectorGatewayEnableResponse>(
      `/connectors/${encodeURIComponent(connectorId)}/model-route/gateway`,
      {},
      { token },
    )
  }

  disableConnectorGateway(token: string, connectorId: string): Promise<ConnectorGatewayDisableResponse> {
    return this.client.delete<ConnectorGatewayDisableResponse>(
      `/connectors/${encodeURIComponent(connectorId)}/model-route/gateway`,
      { token },
    )
  }
}

export const modelProvidersApi = new ModelProvidersApi()

/** Reads the reference counts out of the 409 `provider_referenced` response so
 * the UI can show them before retrying with force=true. */
export function providerReferencesFromError(error: unknown): ProviderReferences | null {
  if (!(error instanceof ApiError) || error.status !== 409) return null
  const payload = error.payload
  if (!payload || typeof payload !== "object") return null
  const detail = (payload as { detail?: unknown }).detail
  if (!detail || typeof detail !== "object") return null
  const record = detail as Record<string, unknown>
  if (record.code !== "provider_referenced") return null
  return {
    connectorDefaults: typeof record.connectorDefaults === "number" ? record.connectorDefaults : 0,
    explicitRoutes: typeof record.explicitRoutes === "number" ? record.explicitRoutes : 0,
  }
}
