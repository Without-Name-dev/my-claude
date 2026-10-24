# Runtime slash commands

Enter `/` in a connected session to load that runtime's command catalog. Commands
run through the session command API. Their input is never sent as a normal model
message when lookup, validation or execution fails.

Selecting a command that accepts arguments inserts an editable draft. Submit the
draft to execute it. The request preserves its original text, including whitespace
and newlines; attachments remain in the draft and are not sent with commands.

## Results and refresh

`result.executionState` distinguishes these outcomes:

- `accepted`: the native runtime accepted asynchronous work. It may still be running.
- `completed`: a synchronous command finished, or the runtime explicitly rejected it.
- `unknown`: a timeout, disconnection or malformed acknowledgement prevents a
  trustworthy conclusion after dispatch. `retryable: false` prevents automatic
  retry; inspect refreshed session state before choosing a further action.

An HTTP 200 response with `ok: false` is a failure. Web shows the native message or
text and keeps the draft after failure or uncertainty. A late response cannot
erase newer input or update a different session. Commands refresh when the menu
reopens, the connection recovers, runtime availability/status changes or the
catalog revision changes.

The shared UI contract is documented in
[connector-to-runtime](../../docs/runtime-protocol/connector-to-runtime.md#commands)
and [Web behavior](../../docs/runtime-protocol/web-behavior.md#commands).

## Headless verification

From `connector/`:

```bash
uv run pytest -q tests/test_runtime_rpc_params.py
```

From `server/`:

```bash
uv run pytest -q tests/test_session_commands_contract.py
```

From `web-next/`:

```bash
corepack yarn test
corepack yarn typecheck
```

These checks do not start an interactive development server or prove an installed
desktop client's behavior. Manual smoke testing requires a connected session and
the matching connector build.
