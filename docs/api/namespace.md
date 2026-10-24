# API v2 Namespace Migration

Agents Anywhere v2 serves product APIs under `/api/v2`.

The web UI may still be served from `/`, locale routes such as `/en`, and static assets such as `/_next/*`. Only API, SSE, and WebSocket endpoints move.

## Route Mapping

| Old root path | New v2 path |
| --- | --- |
| `/health` | `/api/v2/health` |
| `/auth/*` | `/api/v2/auth/*` |
| `/oauth/*` | `/api/v2/oauth/*` |
| `/.well-known/oauth-authorization-server` | `/api/v2/.well-known/oauth-authorization-server` |
| `/admin/*` | `/api/v2/admin/*` |
| `/agents/*` | `/api/v2/agents/*` |
| `/connectors/*` | `/api/v2/connectors/*` |
| `/pairing/*` | `/api/v2/pairing/*` |
| `/sessions/*` | `/api/v2/sessions/*` |
| `/connector/*` | `/api/v2/connector/*` |

## Web Migration

Keep `AGENTS_ANYWHERE_API` and `NEXT_PUBLIC_AGENTS_ANYWHERE_API` as the server origin, not the API namespace:

```bash
AGENTS_ANYWHERE_API=http://127.0.0.1:8000 yarn dev
```

Do not set it to `http://127.0.0.1:8000/api/v2`.

`web-next/src/lib/api/client.ts` owns the namespace through `apiPath()`. Normal API calls should continue to pass product paths such as `/auth/login` or `/sessions`. The client turns them into `/api/v2/auth/login` and `/api/v2/sessions`.

Any Web code that builds a URL without `ApiClient` must call `apiPath()` explicitly. This applies to:

- SSE endpoints such as session events and dashboard events.
- WebSocket endpoints such as connector terminal streams.
- Direct browser links such as attachment open/download URLs.

The Next.js dev proxy rewrites `/api/v2/*` to the backend. New root-level API rewrites should not be added.

## Connector

Connector config stores the server origin only; do not store `/api/v2` in `serverUrl`.

The Go connector (`go/cmd/aa-connector`) pairs once through the pairing API, then keeps a WebSocket open:

1. `POST /api/v2/pairing/start` creates a pairing session and returns a `pairingId` and six-digit code; no credentials are needed.
2. A signed-in client enters the code and calls `POST /api/v2/pairing/claim`, which binds the code to the user and creates the connector.
3. The connector calls `POST /api/v2/pairing/poll` with the `pairingId`. Once claimed, the response hands out the connector token once; the connector writes `connector.json` under `AA_CONNECTOR_DATA_DIR`.
4. The connector connects to `WS /api/v2/connector/ws` with `Authorization: Bearer <connector token>`.

The Go server registers only `GET /api/v2/connector/ws` for connectors; every other `/api/v2/connector/*` path returns a JSON 404. Session messages, file access, terminals and approvals travel over the connector WebSocket as protocol frames. The Python reference connector additionally used `/api/v2/connector/auth` and `/api/v2/connector/ingest`, which the Go server does not serve.

## Compatibility Rule

Do not add new root-level API routes in v2. If a route is product API, SSE, or WebSocket, mount it under `/api/v2`.
