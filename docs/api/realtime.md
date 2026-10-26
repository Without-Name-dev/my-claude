# Realtime API

Status: proposal and current-behavior map.

Agents Anywhere uses realtime channels for three different lifecycles:

- session detail updates;
- dashboard connector/session list updates;
- connector RPC/ingest presence;
- terminal streams.

Connector channel endpoint names stay stable. Session and dashboard realtime
semantics should be tightened around the new session model:

```text
SessionMeta and SessionTimeline are durable Server facts.
Runtime state, notices, catalogs, capabilities, commands, and selections are
non-durable RuntimeLive facts.
```

## Session realtime

Primary realtime channel:

```text
WS /api/v2/sessions/{sessionId}/ws?ticket=...
```

Recovery endpoint:

```text
GET /api/v2/sessions/{sessionId}/events?after=seq:123
```

Ticket endpoint:

```text
POST /api/v2/ws-ticket
```

### Intended lifecycle

```text
GET /sessions/{sessionId}/snapshot
  -> reads durable meta/timeline from Server and RuntimeLive facts from runtime RPC
  -> receive durable eventCursor
POST /ws-ticket
  -> session scope ticket
WS /sessions/{sessionId}/ws?ticket=...
  -> receive session.subscribed
  -> receive incremental events
GET /sessions/{sessionId}/events?after=seq:...
  -> post-subscription cursor reconciliation and reconnect recovery
```

`/events` is not snapshot polling. It is a cursor recovery API. If WebSocket is healthy, the client should not call `/events` on a fixed interval.

### Target event types

```text
session.subscribed
session.meta.updated
timeline.item_created
timeline.item_updated
timeline.snapshot
runtime.state.updated
runtime.notice.snapshot
runtime.notice.updated
runtime.capability.updated
runtime.catalog.updated
runtime.refetch_required
session.refetch_required
```

Removed compatibility event types:

- `session.status_changed`
- `notice.created`
- `notice.updated`
- `notice.snapshot`
- embedded `effectiveCapabilities` aliases in session realtime events

Use `runtime.state.updated`, `runtime.capability.updated`,
`runtime.notice.updated`, and `runtime.notice.snapshot` for live Runtime facts.

### Per-session live presence

Every `SessionView` (session list, snapshot, and `session.meta.updated` payload)
carries a boolean `live`. It is true when the phone-pane plugin in that
conversation is still refreshing its presence stamp on the desktop, which means
the conversation can actually accept a prompt right now. It is false for a
session whose plugin stopped stamping, whose connector is offline, or whose
connector predates presence reporting.

The Server republishes `session.meta.updated` for the sessions whose `live`
value changed. A connector going offline clears its whole set.

### Recovery rules

- Event cursor format is `seq:{number}`.
- Timeline items are upsert-only.
- A sequence gap does not automatically require snapshot.
- Server returns `snapshotRequired=true` only when durable meta/timeline
  recovery is explicitly impossible.
- Client pulls snapshot only for initial load or `snapshotRequired=true`.
- Runtime state and catalogs are not recovered from Server DB. After the
  initial session subscription and reconnect, Web and Desktop read
  `GET /sessions/{sessionId}/runtime/state` after successful event recovery.
  This read also runs when the connector is offline, using the endpoint's
  cached/persisted fallback. Older runtime-state revisions are ignored; events
  received before the live read participate in its version check, while events
  received during the read are applied afterwards. Failed live reads retain
  the existing state and buffered events. Catalogs use their runtime live
  endpoints when needed.
- Runtime notices are recovered when Server has a sequence-backed projection.
  Session meta and the latest persisted effective capability projection are
  also returned when recovery starts at the current cursor, because presence
  and capabilities can change without advancing the durable session sequence.
  When the connector is online, Web reads the live session capability endpoint
  after recovery and keeps that Runtime result authoritative.
- `runtime.capability.updated` carries effective capabilities. On a session
  WebSocket, it is session-scoped and controls current session actions. On a
  dashboard/runtime WebSocket, it is runtime-scoped and controls setup,
  create-session, catalog, and feature entry points.
- Web should not derive action availability from `runtime.state.updated` when a
  matching effective capability exists.

### Message dispatch status

An accepted user timeline item starts as `pending` while it waits in the
Connector's FIFO queue. The Connector carries the submit `requestId` on
`turn.begin` and `turn.end`, allowing the Server to update that exact user item
to `running` when processing starts and to `done`, `failed` or `cancelled` when
the request finishes. Clients can keep accepting messages while a turn runs and
show these states on each submitted bubble independently.

### Connector transcript reconciliation

The Connector inventory includes a transcript version derived from file size
and modification time. The Server requests a full import when a conversation
is opened or during a periodic full inventory. Ordinary inventory changes
request only a recent tail. An empty server timeline sets `force: true`, which
bypasses a connector's confirmed-version cache so a server-side history reset
can be repaired. The
Connector waits until a transcript has been quiet for
ten seconds, then reads it on a bounded background queue so a large file
cannot block the connector WebSocket receive loop. Full imports are sent as
ordered `session.history` chunks with stable item IDs; `complete` is false on
intermediate chunks and true on the final chunk. Tool outputs that arrive
after their call was sent are emitted as an update to the same item ID. Each
chunk stays below the connector WebSocket frame limit.

The Server persists intermediate chunks without publishing partial timelines.
After the final chunk it publishes one timeline reset and returns
`session.history.ack` with `complete: true` and the `full` mode echoed from the
request. The Connector tracks full and tail versions separately, so
acknowledging a recent tail cannot suppress a later full import. It retries an
unacknowledged version after reconnect or on the next refresh.

## Dashboard realtime

Primary realtime channel:

```text
WS /api/v2/dashboard/ws?ticket=...
```

Removed legacy SSE:

```text
GET /api/v2/sessions/events/dashboard?token=...
```

Do not add new clients for this route. Dashboard lifecycle updates use
`/dashboard/ws`.

The Web client should prefer dashboard WebSocket and stop fixed-interval polling of:

```text
GET /api/v2/connectors
GET /api/v2/projects
GET /api/v2/sessions/list
```

### Current dashboard behavior

The current dashboard WebSocket sends:

```text
dashboard.snapshot
```

on connect, and sends another full snapshot when a debounced `dashboard.changed` invalidation arrives.

Each snapshot contains the complete owned project and session metadata inventories,
including both active and archived sessions. `sessionPages` reports no more pages.
Web and iOS group, filter and sort these sessions locally. Expanding a project,
switching a device filter or opening archives does not fetch another session list.
An explicit HTTP refresh reads `/projects` and `/sessions/list` once each, alongside
the connector list; it does not issue a request for every project or archive state.

Project mutations refresh the shared project list. A session with an unknown or
missing project binding also triggers a shared project refresh and remains visible
under ungrouped sessions. Clients coalesce concurrent project reads and remember
unresolved bindings, so every message on an unassigned session does not cause a new
request. A failed read can retry after a later update. Newer push or mutation data
takes precedence over an earlier HTTP read.

This is acceptable as a near-term replacement for polling. If full snapshots become too heavy, the next step is delta events:

```text
connector.created
connector.updated
connector.deleted
connector.presence.updated
runtime.updated
session.created
session.meta.updated
session.state.updated
session.archived
```

Do not implement delta dashboard events until the snapshot WebSocket path is stable.

## Connector realtime

Stable connector channel:

```text
WS /api/v2/connector/ws
```

Stable connector HTTP endpoints:

```text
POST /api/v2/connector/auth
POST /api/v2/connector/ingest
```

These endpoint names should not change as part of the runtime protocol refactor.

Connector `hello` and `beat` frames may carry `liveSessions`, an array of runtime
session ids whose phone-pane presence stamp is no older than 30 seconds. The
Server remembers the last set per connector, clears it when the connector goes
offline, and exposes it as `SessionView.live`. The field is additive: an older
connector omits it, and an older Server ignores it. On `beat` the key is always
present (possibly `null` or `[]`), so a Server can distinguish "nothing is
live" from an older connector that cannot report presence at all.

`hello.capabilities` may also carry:

- `bridgeConfigured`: the connector has a phone-pane bridge directory, so a
  queued submit has somewhere to land even if no plugin is running right now.
- `sessionOpen`: the connector can start a terminal for a conversation
  (`session.open`). A Server must not send `session.open` to a connector that
  omits it, and must not report the open as sent; an older connector ignores an
  unknown frame type.

When a message is sent to a conversation with no live plugin, the Server asks
its connector to open a terminal at most once per minute per session; retries
inside that window reuse the terminal that is already starting. If the
connector cannot open sessions, or has no bridge directory to deliver into, the
send is refused with `session_not_live` instead of being acknowledged.

### Connector runtime RPC methods

The runtime protocol refactor evolves payload semantics behind
`WS /api/v2/connector/ws`. Runtime configuration is provider-managed and
read-only once a runtime instance is running:

```text
runtime.discover
runtime.configSchema
runtime.config
runtime.validateConfig
runtime.start
runtime.stop
runtime.modelCatalog
runtime.permissionCatalog
session.discover
session.create
session.sync
session.state
session.selections.update
session.commands
session.command.execute
interaction.respond
turn.start
turn.steer
turn.interrupt
```

`runtime.configSchema` performs a live provider read for UI/CLI configuration
forms. `runtime.config` returns saved raw values when the runtime is stopped,
and the running runtime's read-only effective `RuntimeConfig` projection when
it is running. Config mutation still flows through `runtime.validateConfig` and
`runtime.start`; running `AgentRuntime` instances do not accept direct config
updates.

The `runtime.start` reply and the `runtime.statusChanged` notification carry
the instance lifecycle `status` plus an optional `error` (only meaningful for
`error`) and an optional `phase` (`{code, message, retryable?}`) that names the
step a non-error status is in, for example `syncing` while a runtime instance is
`starting`. A phase is progress, not a fault: the Server stores it on the
runtime instance (`phase` in `RuntimeInstanceStatus`), does not re-issue
`runtime.start` while an instance reports a starting phase (session operations
get a retryable `409 runtime_starting` instead), and clients render it as
progress text next to the `starting` badge.

`runtime.discover` includes runtime-native capability inputs. They are not the
primary frontend contract. The frontend contract is effective capability:

```text
GET /connectors/{connectorId}/runtimes/{runtime}/capabilities
GET /sessions/{sessionId}/runtime/capabilities
runtime.capability.updated
```

The Server may apply authorization, takeover, connector reachability, and
feature policy. It must not infer runtime-owned action availability from
durable session data.

Runtime host live events are sent as connector WebSocket notifications on
`WS /api/v2/connector/ws`. `POST /api/v2/connector/ingest` is reserved for
explicit bulk sync and disconnected WebSocket fallback; it must still compute
changed timeline items and publish session WebSocket events for frontend
convergence.

`POST /api/v2/connector/ingest` isolates notification failures inside a batch.
One malformed or conflicting notification must not produce HTTP 500 or prevent
later notifications in the same request from being applied. The response
includes:

```json
{
  "accepted": 2,
  "rejected": [
    {
      "index": 1,
      "method": "timeline.itemUpsert",
      "code": "notification_failed",
      "message": "...",
      "errorType": "ValidationError"
    }
  ],
  "serverTime": "..."
}
```

`accepted` counts successfully applied notifications. `rejected` reports
per-notification failures using the original zero-based batch index. Explicit
protocol violations that are intentionally unsupported may still return HTTP
400; unexpected processing failures should be logged and reported through
`rejected` instead of surfacing as HTTP 500.

The target semantic connector notification methods are:

```text
session.meta.upsert
timeline.sync
timeline.itemUpsert
runtime.state.updated
runtime.notice.snapshot
runtime.notice.updated
runtime.capability.updated
runtime.catalog.updated
runtime.error
```

Compatibility connector notification names may remain during migration:

```text
session.state.updated
notice.upsert
protocol.capabilitiesUpdated
```

The Server should translate compatibility names into RuntimeLive events without
treating them as durable session truth.

The connector application layer bridges `RuntimeHostClient` calls to these
server-facing notification payloads. Runtime adapters should never call
connector HTTP/WS transports directly.

## Terminal realtime

Current terminal channels include:

```text
WS /api/v2/sessions/{sessionId}/terminals/{terminalId}/stream
WS /api/v2/connectors/{connectorId}/terminals/{terminalId}/stream
WS /api/v2/connectors/{connectorId}/terminals-v2/{terminalId}/stream
WS /api/v2/connector/terminals/{terminalId}/relay
```

Terminal APIs are local capability APIs, not Agent Runtime Protocol session APIs. They can be cleaned up later as a separate local-capabilities interface pass.

Do not block the runtime protocol migration on terminal endpoint consolidation.
