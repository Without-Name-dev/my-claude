# Anywhere CLI

> v2 Connector. Use the source from the same release line as your Server. The
> Python package version is independent of the 2.0.0 product version.

Local runtime connector for Agents Anywhere. It runs on the machine that owns
the workspace and agent runtimes, connects to the server over HTTP/WebSocket,
executes connector RPC locally, and uploads normalized runtime/session state
back to the backend.

## Layout

```text
connector/
  runtime_protocol/  AgentRuntime, RuntimeProvider, RuntimeHostClient contracts
  runtimes/          Claude RuntimeProvider/AgentRuntime packages
  server/            Backend auth, ingest, RPC channel, request dispatch, host mapping
  core/              Connector config, runtime owner, runtime config storage
  local/             Local filesystem, shell, and terminal backends
  _reference/        Old adapter implementations retained only as migration references
  cli.py             anywhere-cli CLI
  control.py         Connector controller shared by the CLI and the local control service
tests/          Connector tests
pyproject.toml  Connector dependencies and console script
run.sh          Local helper for saved-config startup
```

## Run

Run from this repository's `connector/` directory. This avoids depending on an
unverified public package version when connecting to a v2 Server. Install dependencies:

```bash
uv sync
```

Start with explicit credentials from the web pairing flow:

```bash
uv run anywhere-cli start \
  --server-url http://127.0.0.1:8000 \
  --connector-id conn_xxx \
  --connector-token cxt_xxx
```

Or save the config locally and start without arguments:

```bash
uv run anywhere-cli configure \
  --server-url http://127.0.0.1:8000 \
  --connector-id conn_xxx \
  --connector-token cxt_xxx

uv run anywhere-cli start
```

The default config path is `~/.agents-anywhere/connector.json`. Override it with
`--config` or `AGENT_CONNECTOR_CONFIG`.

Connector configuration, runtime ownership, sync state, and attachments all
live under `~/.agents-anywhere` by default. Runtime sync cursors are stored as
atomic JSON in `<connectorId>/<runtimeId>/sync-state.json`; runtime message
bindings use the adjacent `kv.json`. Connector prepares these stores before
constructing a runtime and retains all opened stores for periodic/shutdown flush,
including stopped instances. Existing sync and KV keys and read/write interfaces
are unchanged, including runtime source-key isolation. Connector does not use SQLite. On first
use, the v2 connector performs a one-time local data migration from the old
`~/.agent-server` directory into `~/.agents-anywhere` and discards obsolete
SQLite sync state.

When an instance directory does not exist, startup copies the entire legacy
`connector-state.json` and `connector-kv.json` into a temporary directory and
publishes it after both files validate. Legacy files remain untouched apart from
flushing pending committed sync state before copying. Existing instance directories
are never recopied or merged; reads and deletes do not fall back to legacy data.
Copied files retain their old namespaces, so foreign instance records are not
selected by that runtime's normal keys. A failed migration blocks that runtime's
startup and can be retried. Agent-native histories and the machine ownership record
are not relocated.

Each projected item is compared with its last successfully ingested fingerprint in `sync-state.json`.
Unchanged items are omitted, including after reconnect/restart; new and modified
items are sent as a delta. First sync sends all items. Item removals or incompatible
checkpoint versions use a session replacement snapshot. Replacement is deferred
while a session is active, without committing its checkpoint. Failed ingestion
never advances the prepared fingerprint state. Live notifications do not advance
this scanner checkpoint, so the latest live items can be safely resent once by
the next successful scan. Fingerprints scale with the number of timeline items.
Recovery progress is tracked per session, so one failing session does not force
all successful sessions to reread their history on every poll.

## Local startup ownership

All CLI and Desktop launches use Python's per-user startup check at
`<OS user home>/.agents-anywhere/connector-runtime.json`, independent of private
configuration or data paths. Python records the actual Connector PID, its startup
source and process start time. A record blocks startup only while its PID still
identifies that Connector process. A live unrelated process or a reused PID does
not block startup; an inspection permission failure is reported instead of bypassed.

Each accepted configured start appends its Connector ID once to the ordered history,
including CLI launches and existing bindings. Python removes only its own runtime
record at normal shutdown. A crash leaves a record that the next start checks against
the actual process. Stopping the backend connection through `connector.stop` retains
ownership while the RPC process is alive.

Local control callers receive the MeshCall application error `connector_already_running` on a
conflict. `connector.acquireOwnership` supports preflight before credentials exist;
a rejected request keeps the RPC channel alive for `connector.getState` and retry.
Direct CLI startup reports the conflict and exits with code `2`.

Desktop remains the only installation-metadata writer. It does not append shared
IDs or check startup PIDs. See the
[local machine v2 contract](../contracts/local-machine/2.0/README.md) for fields,
atomic file transactions and legacy migration.

## Runtime Discovery

The default provider is Claude. The connector reports attached runtime
capabilities to the server. Claude Code is discovered through the `claude` CLI,
or through an explicit path configured with `CLAUDE_BIN`. If Claude Code is not on
`PATH`, set:

```bash
CLAUDE_BIN=/path/to/claude
```

Legacy ACP adapters are not part of the default provider registry.

Connected sessions expose native slash commands through a live runtime catalog.
See [runtime slash commands](docs/runtime-commands.md) for commands, result
states and headless verification.

The connector uses local runtime credentials and local filesystem permissions.
Agents Anywhere does not proxy Claude account credentials.

## API Namespace

Connector configuration stores the server origin, for example
`http://127.0.0.1:8000`; do not include `/api/v2` in `--server-url`.

The connector adds the v2 namespace internally and talks to `/api/v2/connector/*`
and `/api/v2/health`. See `../docs/api/namespace.md` for the namespace rules.

## Local Operations

The server can ask an online connector to perform local work:

- read/list/write files inside workspace-safe roots
- upload/download file content through the server
- run one-shot shell commands
- start and wait for shell tasks
- create, write, resize, stream, list, and close interactive terminals
- start, interrupt, sync, and approve runtime turns

## Environment

| Variable | Purpose |
| --- | --- |
| `AGENT_CONNECTOR_CONFIG` | Connector config path. |
| `AGENT_CONNECTOR_DATA_DIR` | Connector data directory. Defaults to `~/.agents-anywhere`. |
| `AGENT_SERVER_URL` | Server URL used when `--server-url` is omitted. |
| `AGENT_CONNECTOR_ID` | Connector id used when `--connector-id` is omitted. |
| `AGENT_CONNECTOR_TOKEN` | Connector token used when `--connector-token` is omitted. |
| `AGENT_CONNECTOR_STATE_FILE` | Legacy sync state source; its parent is the root for new `<connectorId>/<runtimeId>/` directories. Defaults to `~/.agents-anywhere/connector-state.json`. Config `statePath` takes precedence. |
| `AGENT_CONNECTOR_KV_FILE` | Legacy KV copy source. Defaults to `~/.agents-anywhere/connector-kv.json`; new runtime writes use the instance's `kv.json`. |
| `AGENT_CONNECTOR_ATTACHMENTS_ROOT` | Runtime attachment download directory. Defaults to `~/.agents-anywhere/attachments`. |
| `CLAUDE_BIN` | Explicit Claude Code CLI path. |

## Verify

```bash
uv run ruff check connector tests
uv run pytest -q
```