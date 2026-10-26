# My Claude (Go)

Two binaries replace the Python server and connector:

```
cmd/aa-server     the API and the web client's origin
cmd/aa-connector  the desktop agent that drives Claude Code
```

## Server

```bash
go build -o bin/aa-server ./cmd/aa-server
AA_DATA_DIR=/var/lib/agents-anywhere \
AA_LISTEN=0.0.0.0:8080 \
AA_PUBLIC_URL=http://your-host \
AGENT_SERVER_SECRET=$(openssl rand -hex 32) \
bin/aa-server
```

| Variable | Default | Meaning |
|---|---|---|
| `AA_LISTEN` | `:8080` | address the API binds |
| `AA_DATA_DIR` | `data` | root for the database and uploads |
| `AA_DB_PATH` | `$AA_DATA_DIR/server.db` | SQLite file |
| `AA_UPLOAD_DIR` | `$AA_DATA_DIR/uploads` | attachment storage |
| `AA_PUBLIC_URL` | `http://127.0.0.1:8080` | base URL handed to a pairing connector |
| `AA_WEB_DIR` | unset | pre-built web client to serve from the same origin |
| `AGENT_SERVER_SECRET` | dev value | HMAC key for every token; **set this** |
| `AA_SETUP_TOKEN` | generated | fixes the first-run bootstrap token instead of printing one |

The database is created and migrated on start. `-migrate` applies migrations and exits.

### Behind nginx

The client calls the API at a relative `/api/v2`, so both must share an origin.
The deployment on `8.166.138.207` uses:

```nginx
upstream agents_anywhere_api { server 127.0.0.1:8080; keepalive 32; }

map $http_upgrade $connection_upgrade { default upgrade; "" close; }

server {
    listen 80 default_server;
    root /opt/agents-anywhere/web;
    client_max_body_size 256m;

    location /api/ {
        proxy_pass http://agents_anywhere_api;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
        proxy_buffering off;
    }

    location / { try_files $uri $uri/index.html $uri.html /index.html; }
}
```

## Connector

Pair once, then run it as a service.

```bash
# 1. Ask the server for a code (no credentials needed):
curl -X POST http://your-host/api/v2/pairing/start -d '{}'

# 2. In the app, add a device and type the six-digit code.
#    The connector polls and receives connector.json:
curl -X POST http://your-host/api/v2/pairing/poll -d '{"pairingId":"pair_..."}'

# 3. Run it:
AA_CONNECTOR_DATA_DIR=~/.agents-anywhere \
AA_BRIDGE_DIR=~/.claude/phone-bridge \
AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE=1 \
bin/aa-connector --config ~/.agents-anywhere/connector.json
```

| Variable | Default | Meaning |
|---|---|---|
| `AA_SERVER_URL` | `http://127.0.0.1:8080` | where the server lives |
| `AA_CONNECTOR_TOKEN` | — | overrides the pairing file |
| `AA_CONNECTOR_DATA_DIR` | `~/.agents-anywhere` | where `connector.json` is read from |
| `AA_BRIDGE_DIR` | `~/.claude/phone-bridge` | the phone-pane plugin's directory |
| `AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE` | unset | must be set to drive Claude through the plugin |
| `AA_TRANSCRIPT_ROOT` | `~/.claude/projects` | where sessions are discovered |
| `AA_ALLOW_SPAWN` | unset | set to `0` to refuse opening sessions in a terminal on this machine |

### What the connector does

- **Discovers sessions.** Every transcript under `~/.claude/projects` becomes a
  session on the server, grouped into projects by working directory. Scans are
  incremental: only changed transcripts are re-sent.
- **Drives the phone-pane plugin.** A queued message is written to the bridge;
  the plugin submits it inside the running Claude session, and the answer is
  streamed back token by token. Messages sent during a turn stay in FIFO order;
  `turn.begin` and `turn.end` include the submit `requestId`, so the Server can
  publish each user's message as queued, processing, and complete independently.
- **Opens a session on the desktop.** When the server asks (the "open on
  computer" action) or a message is addressed to a conversation with no live
  plugin, the connector starts a terminal with `claude --resume <id>` for an
  existing transcript or `claude --session-id <id>` for a new one. Windows uses
  Windows Terminal, falling back to `cmd /c start`; Linux tries `$TERMINAL` and
  the common emulators; macOS opens Terminal.app. Spawns are throttled to one
  per runtime session per minute, the server asks at most once a minute per
  session, and `AA_ALLOW_SPAWN=0` disables them entirely. The connector
  advertises `sessionOpen` in its hello, so a server never reports a terminal
  as opened on a build that cannot start one.
- **Answers approvals.** The plugin's permission hook calls a loopback endpoint
  the connector owns; the connector forwards the question to the phone and
  blocks until someone decides.
- **Reads files.** The web client's file browser is an RPC the connector serves.

## Layout

```
internal/auth        tokens and password hashing
internal/bridge      the phone-pane file protocol
internal/config      environment configuration
internal/connector   discovery, turn watching, approvals, RPC
internal/protocol    frames shared by both binaries
internal/server      HTTP handlers, websockets, projections
internal/store       SQLite schema, migrations and queries
```
