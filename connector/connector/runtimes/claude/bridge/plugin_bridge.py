"""Connector side of the in-process plugin bridge.

A live Claude Code session can load a small plugin that runs inside its own
process (see the ``phone-pane`` mod).  That plugin has what no external process
can have: ``$.prompt.submit`` to raise a real turn and ``turn.step`` to read the
answer token by token.  It talks to this connector through two files:

``<bridge>/queue/<n>.json``
    One message to submit, written here, consumed by the plugin.
``<bridge>/outbox.json``
    A snapshot of the current turn, rewritten by the plugin as text arrives.

The bridge is off unless ``AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE`` is set, so a
machine without the plugin keeps its existing delivery path.
"""

from __future__ import annotations

import asyncio
import json
import os
import secrets
import time
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from connector.logging import logger

# One token per connector process, written beside the queue. The plugin only
# accepts a message that carries it, so a stray file another tool drops in the
# queue is refused rather than submitted as the person's own words. A process
# running as this user can still read the token; it is a guard against accidents
# and a tamper mark, not a boundary against a hostile local process.
_TOKEN: str | None = None

BRIDGE_ENV = "AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE"
BRIDGE_DIR_ENV = "AGENT_CONNECTOR_CLAUDE_BRIDGE_DIR"
_DISABLED_VALUES = {"0", "false", "no", "off"}
# How stale a heartbeat may be before the bridge counts as absent.
HEARTBEAT_TTL_SECONDS = 30.0

_SEQ_LOCK = asyncio.Lock()
_LAST_SEQ = 0


def enabled() -> bool:
    """Whether delivery through the plugin queue is switched on for this machine."""
    value = os.environ.get(BRIDGE_ENV, "").strip().lower()
    if value in _DISABLED_VALUES:
        return False
    return value != ""


def bridge_dir() -> Path:
    explicit = os.environ.get(BRIDGE_DIR_ENV, "").strip()
    if explicit:
        return Path(explicit)
    return Path.home() / ".claude" / "phone-bridge"


def queue_dir() -> Path:
    return bridge_dir() / "queue"


def outbox_path() -> Path:
    return bridge_dir() / "outbox.json"


def _heartbeat_path() -> Path:
    return bridge_dir() / "heartbeat.txt"


def abort_path() -> Path:
    return bridge_dir() / "abort.txt"


def tools_path() -> Path:
    return bridge_dir() / "tools.json"


def token_path() -> Path:
    return bridge_dir() / "auth.json"


def approval_path() -> Path:
    """Where the plugin reads the approval endpoint's port and token."""
    return bridge_dir() / "approval.json"


def queue_token() -> str:
    """The token a queued message must carry, created once per process."""
    global _TOKEN
    if _TOKEN is None:
        _TOKEN = secrets.token_hex(24)
        path = token_path()
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            temp = path.with_name(".auth.tmp")
            temp.write_text(json.dumps({"token": _TOKEN}), encoding="utf-8")
            os.replace(temp, path)
        except OSError:
            logger.warning("plugin bridge: could not write the queue token")
    return _TOKEN


def read_tools() -> list[dict[str, object]]:
    """The tool calls the plugin has seen in the running turn, oldest first."""
    try:
        raw = tools_path().read_text(encoding="utf-8")
    except OSError:
        return []
    try:
        data = json.loads(raw)
    except ValueError:
        return []
    entries = data.get("entries") if isinstance(data, dict) else None
    if not isinstance(entries, list):
        return []
    return [one for one in entries if isinstance(one, dict)]


async def request_abort() -> bool:
    """Ask the plugin's session to cancel the turn it is running."""
    try:
        path = abort_path()
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(str(int(time.time() * 1000)), encoding="utf-8")
    except OSError:
        logger.warning("plugin bridge: could not write the abort request")
        return False
    return True


def heartbeat_age() -> float | None:
    """Seconds since the plugin last said it was alive, or None if it never did."""
    try:
        raw = _heartbeat_path().read_text(encoding="utf-8").strip()
    except OSError:
        return None
    try:
        stamp = float(raw)
    except ValueError:
        return None
    return max(0.0, time.time() - stamp / 1000.0)


def available() -> bool:
    """Whether a live plugin is answering on this machine right now."""
    age = heartbeat_age()
    return age is not None and age <= HEARTBEAT_TTL_SECONDS


def _next_seq() -> int:
    best = 0
    try:
        for entry in queue_dir().iterdir():
            if entry.suffix == ".json" and entry.stem.isdigit():
                best = max(best, int(entry.stem))
    except OSError:
        return 0
    return best


@dataclass(slots=True)
class BridgeSubmission:
    """What the caller needs to follow the turn it just queued."""

    queued_at: float
    # The turn the outbox already showed when this one was queued. The plugin
    # rewrites the same file, so a snapshot carrying this id is stale.
    previous_turn_id: str


async def _write_queue(
    extra: Mapping[str, Any],
    *,
    action: str,
    message_id: str | None,
    previous_turn_id: str,
) -> BridgeSubmission | None:
    """Write one queue entry in a single rename so the plugin never reads half."""
    global _LAST_SEQ
    async with _SEQ_LOCK:
        seq = max(_LAST_SEQ, _next_seq()) + 1
        _LAST_SEQ = seq
        path = queue_dir() / f"{seq}.json"
        payload: dict[str, Any] = {
            "id": message_id or f"bridge-{seq}",
            "action": action,
            **extra,
            "queuedAt": int(time.time() * 1000),
            "token": queue_token(),
        }
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            # The plugin lists the directory continuously; a half-written file
            # would be read as invalid JSON, so land it in one rename.
            temp = path.with_name(f".{seq}.tmp")
            temp.write_text(
                json.dumps(payload, ensure_ascii=False), encoding="utf-8"
            )
            os.replace(temp, path)
        except OSError:
            logger.warning("plugin bridge: could not queue message seq={}", seq)
            return None
    return BridgeSubmission(
        queued_at=float(payload["queuedAt"]),
        previous_turn_id=previous_turn_id,
    )


async def submit(
    text: str,
    *,
    message_id: str | None = None,
    action: str = "prompt",
) -> BridgeSubmission | None:
    """Queue one message for the plugin.

    ``action`` lets the queue carry more than a prompt (a slash command), so the
    same token-guarded channel can also drive ``$.command.run``.  The stamp
    tells the caller which outbox snapshots are newer than its own message, and
    the previous turn id tells it which ones are leftovers.
    """
    if not text:
        return None
    previous = BridgeOutbox.read()
    return await _write_queue(
        {"text": text},
        action=action,
        message_id=message_id,
        previous_turn_id=previous.turn_id if previous is not None else "",
    )


async def submit_command(
    command: str,
    *,
    args: str | None = None,
    message_id: str | None = None,
) -> bool:
    """Queue a slash command for the plugin to run inside the session.

    The command name is written without its leading slash; ``args`` is the tail
    a person would type after the name.  The plugin resolves aliases itself.
    """
    name = command.strip().lstrip("/").strip()
    if not name:
        return False
    extra: dict[str, Any] = {"command": name}
    if args:
        extra["args"] = args
    previous = BridgeOutbox.read()
    submission = await _write_queue(
        extra,
        action="command",
        message_id=message_id or f"bridge-command-{name}",
        previous_turn_id=previous.turn_id if previous is not None else "",
    )
    return submission is not None


# The plugin adds these fields as facts arrive; they are forwarded to the
# server through the session state metadata, which already accepts free-form
# values (no schema change).
# These are the names the plugin writes into its outbox snapshot; "branch" is
# the git branch, not a second copy of "git".
_OUTBOX_STATE_KEYS = (
    "model",
    "effort",
    "cwd",
    "branch",
    "usage",
    "context",
    "costUsd",
)


@dataclass(slots=True)
class BridgeOutbox:
    """The plugin's snapshot of the turn it is currently running."""

    turn_id: str
    status: str
    chunks: int
    text: str
    ts: float
    # model / effort / cwd / git / usage, only when the plugin reported them.
    state: Mapping[str, Any] = field(default_factory=dict)

    @property
    def running(self) -> bool:
        return self.status == "streaming"

    @property
    def finished(self) -> bool:
        return self.status == "done"

    def state_metadata(self) -> dict[str, Any]:
        """The outbox facts worth reporting, without empty placeholders."""
        return dict(self.state)

    @classmethod
    def read(cls) -> "BridgeOutbox | None":
        try:
            raw = outbox_path().read_text(encoding="utf-8")
        except OSError:
            return None
        try:
            data = json.loads(raw)
        except ValueError:
            return None
        if not isinstance(data, dict):
            return None
        turn_id = data.get("turnId")
        status = data.get("status")
        text = data.get("text")
        state = {
            key: data[key]
            for key in _OUTBOX_STATE_KEYS
            if data.get(key) is not None
        }
        return cls(
            turn_id=turn_id if isinstance(turn_id, str) else "",
            status=status if isinstance(status, str) else "",
            chunks=int(data.get("chunks") or 0)
            if isinstance(data.get("chunks"), (int, float))
            else 0,
            text=text if isinstance(text, str) else "",
            ts=float(data.get("ts") or 0.0),
            state=state,
        )


