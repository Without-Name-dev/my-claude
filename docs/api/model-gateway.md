# Model Gateway & Provider Management API

Status: **implemented in the Go Server** (this release). Clients adopt it per the versioning rules; the connector requires the `gateway.config` RPC (an older connector answers `unknown method`, which surfaces as a `502` when enabling the gateway).

The model gateway lets desktop Claude sessions send their model requests through the Go Server. The server routes each request to one of the signed-in user's configured third-party providers. Providers, credentials, routing, and gateway access are strictly per-user; no endpoint accepts a client-stated user id as authority.

Related design document: `MyClaude-模型代理与权限切换完整技术方案.md` (product source of truth).

## Data model

One **model provider** = one provider endpoint + one credential + one model id. Selecting a provider in a session selects its model; there is no second model picker. Credentials are stored server-side in the clear by explicit product decision and are **never** returned by any API, event, or log — only `credentialConfigured: true/false`.

A **device default** (`connector_model_defaults`) names one provider for a desktop. A **session route** (`session_model_routes`) either inherits that default or pins the session to one provider explicitly. A deleted provider leaves explicit routes pointing at nothing; those routes report `failed` / "provider deleted" and are never silently rerouted.

A **gateway credential** is a revocable token issued per connector. Only its hash is stored; the plaintext is delivered to the connector exactly once, over its authenticated websocket, at issuance time, and the server waits for the connector's acknowledgement. Issuing requires the connector to be online (`409` otherwise); if the device does not acknowledge, the fresh credential is revoked again and the call answers `502`. Re-issuing rotates the token.

## Provider management

Base: `/api/v2/model-providers`. All endpoints require a user bearer token.

### `GET /model-providers`

```json
{ "providers": [ {
  "id": "prv_x", "name": "My provider", "protocol": "anthropic_messages",
  "baseUrl": "https://provider.example", "authType": "api_key",
  "modelId": "provider-model-id", "modelDisplayName": "My model",
  "credentialConfigured": true, "enabled": true, "revision": 3,
  "capabilities": { "check": { "reachable": true, "authOk": true, "status": 200, "at": 1760000000000 } },
  "createdAt": "…", "updatedAt": "…"
} ], "serverTime": "…" }
```

- `protocol`: `anthropic_messages` (transparent forwarding) or `openai_chat_completions` (server-side bidirectional conversion). The Responses API is a different protocol and is not accepted.
- `authType`: `api_key` (sent as `x-api-key` to Anthropic-protocol upstreams) or `bearer` (`Authorization: Bearer`). OpenAI-protocol upstreams always authenticate with a bearer.
- `capabilities` is a free-form object. `capabilities.check` appears after a user-initiated check.

### `POST /model-providers`

Body: `{ name, protocol, baseUrl, authType?, modelId, modelDisplayName?, credential?, enabled?, capabilities? }`. The four required fields must be present. `credential` is the secret to store (write-only). Defaults: `authType: "api_key"`, `enabled: true`.

Validation: name 1–100 chars; `baseUrl` absolute http(s) URL (max 2048); `modelId` 1–200 chars. Base URL normalization: trailing `/` is stripped; for `anthropic_messages`, a base ending in `/v1` gets `/messages` appended, a base ending in `/v1/messages` is used as-is, anything else gets `/v1/messages`; OpenAI protocol analogously with `/chat/completions`.

### `GET /model-providers/{id}` — one provider, same shape. 404 for another user's id.

### `PATCH /model-providers/{id}`

Body: any editable field, plus:

- `expectedRevision` (int) — optimistic concurrency. A mismatch answers **409** with "provider was modified by another client; refresh and edit again". Omitting it skips the check (not recommended from UIs).
- `credential` (string) — **replace** the stored secret.
- `clearCredential` (true) — **remove** the stored secret. Mutually exclusive with `credential`.

An absent credential field always means "keep the stored value" — an empty form can never delete a key. A PATCH that would change nothing leaves `revision` untouched (so it cannot invalidate another client's `expectedRevision`); a real change bumps `revision` and invalidates the server's in-process route cache for the whole account. Requests already in flight finish on their original upstream.

### `DELETE /model-providers/{id}[?force=true]`

If connector defaults or explicit session routes reference the provider, the first call answers **409**:

```json
{ "detail": { "code": "provider_referenced",
  "message": "provider is referenced by routes; pass force=true after reviewing them",
  "connectorDefaults": 1, "explicitRoutes": 4 } }
```

`force=true` deletes: connector defaults are removed (FK cascade), explicit routes keep `mode: "explicit"` with a missing target and turn `failed` / "provider deleted" — visible in the session route view, never silently inherited.

### `POST /model-providers/{id}/check`

Body: `{ "mode": "connect" | "request" }` (default `connect`). `connect` probes reachability only and sends **no credential and no request**, so `authOk` is false and `status` is absent; pass on `reachable`. `request` sends a minimal real model request (1 token, non-streaming) — it can cost money and is never run implicitly; pass on `reachable && authOk`, where `authOk` is true only when that request actually succeeded (2xx). Response:

```json
{ "providerId": "prv_x", "check": {
  "mode": "request", "reachable": true, "authOk": true, "status": 200,
  "detail": "model request succeeded", "at": 1760000000000 },
  "serverTime": "…" }
```

The result is also persisted into `capabilities.check`, including `mode`. `at` is Unix milliseconds, matching the stored shape.

## Routing

### `GET /sessions/{session_id}/model-route`

```json
{ "route": {
  "sessionId": "ses_x", "mode": "explicit", "status": "waiting_for_turn_end",
  "statusDetail": null,
  "target": { "providerId": "prv_b", "providerName": "…", "modelId": "…",
              "modelDisplayName": "…", "providerRevision": 3, "enabled": true },
  "deviceDefault": { …same shape… },
  "applied": { …provider the gateway last served under… },
  "lastRequest": { "providerId": "prv_a", "modelId": "…", "status": "completed",
                   "upstreamStatus": 200, "at": "…" },
  "gateway": { "credentialStatus": "active", "connected": true, "credentialPrefix": "gwc_…", "lastUsedAt": "…" }
}, "serverTime": "…" }
```

- `mode`: `explicit` | `inherit_device_default`.
- `status`: `not_connected`, `unknown`, `waiting_for_turn_end`, `applying`, `applied`, `failed`. `applied` means the gateway will use the target from the next request on — it never claims an upstream request succeeded. `lastRequest` reports the real outcome separately.
- `gateway.connected` is true only when desktop Claude processes actually used the credential recently; issuing a credential is not the same as being wired.

### `PUT /sessions/{session_id}/model-route`

Body: `{ "providerId": "prv_b" }` (explicit) or `{ "inherit": true }` / `{ "providerId": null }` (inherit). Optional `idempotencyKey` (string): a retried request with the same key returns the original operation instead of creating a second one.

The route lands synchronously. If the session has a turn in flight, `status` is `waiting_for_turn_end`: the open stream finishes on its current provider, and requests already associated with that prompt keep it too (via the prompt-id pin) while the next turn uses the target. Only a request actually served by the target flips the status to `applied` and completes the operation; serving the previous provider leaves the switch waiting. Without the `x-claude-code-prompt-id` header the pin cannot form, so a same-turn follow-up may already use the new target (see the verification caveat below). Response: `{ route, operation, serverTime }`.

Sessions watchers also receive `runtime.state.updated` (new selections) and `runtime.catalog.updated` (`{ catalogType: "model", catalog }`) under a bumped sequence.

### `GET /connectors/{connector_id}/model-route`

`{ route: { connectorId, default: {…}|null, gateway: {…} }, serverTime }`.

### `PUT /connectors/{connector_id}/model-route`

Body: `{ "providerId": "prv_b" | null, "applyToSessions": "none" | "inherit_only" | "all" }`.

- `providerId: null` clears the default (explicit session routes untouched). Selecting a provider that is disabled or has no credential answers `409`: routing never accepts a target that cannot serve a request.
- `applyToSessions: "none"` (default) / `"inherit_only"`: inheriting sessions follow automatically.
- `"all"`: additionally pins **every** session on this desktop to the provider as an explicit route. Each session is independent; the response counts them:
  `"applied": { "pinned": 12, "failed": 1, "failedSessions": ["ses_y"] }`.

### `GET /model-route-operations/{operation_id}` / `POST …/cancel`

Operations are the durable record of a switch (`pending`, `waiting_for_turn_end`, `applied`, `failed`, `cancelled`), queryable after a disconnect. In this version a switch is recorded synchronously, so an operation is never left `pending`: it is `applied` immediately or `waiting_for_turn_end` behind a running turn. Cancel therefore refuses both with `409` — the honest way to change a queued switch is to choose another provider, which cancels the superseded operation. Cancel never rolls back an upstream request already sent.

## Gateway credential (device page)

### `POST /connectors/{connector_id}/model-route/gateway`

Issues (or rotates) the connector's gateway credential and pushes `{ baseUrl, token }` to the connector over its websocket (`gateway.config` RPC), **waiting for the connector's acknowledgement**. The connector must be **online** — issuance answers `409` otherwise — and if the device does not acknowledge (offline mid-request, or a connector that predates the RPC) the credential is revoked again and the call answers `502` with the reason. A `200` therefore means the device really stored the config; `"pushed": true` is not merely "frame queued". The gateway base URL is the configured public URL (`<AA_PUBLIC_URL>/api/v2/gateway`, never derived from request headers). The token is never in the response.

### `DELETE /connectors/{connector_id}/model-route/gateway`

Revokes the active credential and pushes a clear, waiting for acknowledgement. `pushed: false` means the device was offline (or did not acknowledge) and still holds the revoked token in its local config until it reconnects; the server also re-sends the clear when the device reconnects. The token cannot authenticate in the meantime. In-flight requests already carrying it may continue; no new request authenticates.

## The gateway itself (data plane)

Endpoint: `<server>/api/v2/gateway` — set on desktops via `ANTHROPIC_BASE_URL`; the credential rides `ANTHROPIC_AUTH_TOKEN`.

- `POST /v1/messages` (+ query parameters such as `?beta=…`, preserved for Anthropic-protocol upstreams) — streaming and non-streaming.
- `POST /v1/messages/count_tokens` — forwarded for Anthropic-protocol upstreams; OpenAI-protocol upstreams answer `404 not_found_error` (the gateway never invents token counts).
- `GET /v1/models` — the user's enabled providers as model entries (optional for Claude Code).

Authentication accepts `Authorization: Bearer gwc_…` or `x-api-key: gwc_…`. Failures answer in the Anthropic error shape (`{"type":"error","error":{…}}`).

Session identification reads `x-claude-code-session-id`. **Verified behavior caveat:** whether a given Claude Code build sends this header (and `x-claude-code-prompt-id` for turn association) must be confirmed on-site; the gateway is built to degrade honestly:

- Header present, session known → session route (explicit, else device default). A header naming a retired pre-`/clear` id resolves through the rebind map.
- Header absent or session unknown → the device default route, recorded as **unbound** (session attribution stays null). Never attributed to another session.
- No route resolvable → explicit error ("no model provider is configured for this device…"). No silent fallback to a different provider.

A provider that answers a streaming request with a complete JSON body is replayed to the client as the equivalent SSE sequence, so the client's event parser always sees a stream; a provider that returns an `error` object is reported as a failed request, never as an empty message.

Only the request's top-level `model` field is rewritten (to the routed provider's model id). Every other top-level member (`system`, `messages`, `tools`, …) is carried through verbatim as raw JSON; the body is re-encoded as a JSON object, so top-level key order may differ while the member values do not. Streaming responses are flushed chunk-by-chunk; a client disconnect cancels the upstream request. A truncated stream is reported as ended — the gateway never replays a request that an upstream may already have accepted.

Redirects from an upstream are refused (a redirect would carry the provider credential to a different host), and connections to link-local or unspecified addresses are blocked; loopback and private addresses stay reachable for self-hosted providers. Body ceiling: `AA_GATEWAY_MAX_BODY_BYTES` (default 64 MiB). Upstream header wait: `AA_GATEWAY_UPSTREAM_HEADER_TIMEOUT` (default 5m). Reverse proxies must disable response buffering for the gateway path (see `go/README.md`).

## Runtime catalogs (existing endpoints, now populated)

- `GET /sessions/{id}/runtime/catalogs/model` and the connector-level equivalent now return one model entry per provider: `id` = `selectionId` = provider id, `metadata: { providerId, modelId, protocol, revision, routeState }`, `default: true` on the device default's provider, `enabled` mirroring the provider. `metadata.routeState` is `applied`, `waiting_for_turn_end`, or `last_request_failed` (empty otherwise) and the same state is appended to `description` so existing pickers show it. A `PATCH …/runtime/selections` with `model: <providerId>` is a route switch — the same write as `PUT /model-route` — and publishes `runtime.state.updated` plus `runtime.catalog.updated`. A `model` value that is neither empty nor one of the account's providers answers `400` with "choose another". A provider that is disabled or has no credential answers `409`, exactly like `PUT /model-route`; in both cases no column is written and no switch is claimed.
- `GET …/catalogs/permission` publishes exactly four modes (`default`, `acceptEdits`, `plan`, `auto`) with i18n label keys, all `enabled: false` for now: no verified plugin API exists to read or set a running session's live mode, so remote switching stays a stored **target**, honestly disabled, until such an API is confirmed. `bypassPermissions` and `dontAsk` are deliberately absent.

## Privacy & safety rules

- Provider credentials: stored plaintext (explicit product decision), never echoed, never in URLs/logs/errors, `gateway.config` delivery is the only plaintext transit (server → connector over its authenticated socket).
- Request logs hold session/provider/model/status/bytes/timestamps only — no message bodies, no prompts, no credentials.
- The route cache lives in the Go process (bounded, LRU, invalidated by every management write). It assumes the single-instance deployment this server documents; a second server process would keep serving stale routes until restart.
- For OpenAI-protocol streams, usage is requested via `stream_options.include_usage`; providers that ignore it report zero output tokens, and `message_start` carries `input_tokens: 0` because OpenAI sends usage only at the end.
- Metrics are counters per provider (requests, errors, streams, bytes) exposed via the admin service endpoint.
- Outbound provider connections refuse redirects and block link-local addresses (including cloud metadata endpoints) and the AWS IPv6 metadata address, after DNS resolution. If the server is configured with `HTTP(S)_PROXY`, the dial guard sees the proxy rather than the target, so an operator who needs strict egress control should also restrict the proxy or run the server without one.
- A provider `baseUrl` is fetched from the server, so an authenticated user can ask the server to reach any host it can reach (including a LAN address for a self-hosted provider). Operators who do not want that should deploy on a network where the server cannot reach sensitive internal services; the server does not currently apply an IP allow/deny list because legitimate self-hosted providers often live on a private address.
