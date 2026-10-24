from __future__ import annotations

import asyncio
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from typing import Any

from connector.logging import logger
from connector.runtimes.claude.domain.session import ClaudeSession
from connector.runtimes.claude.sdk.client import (
    SdkLoader,
    build_sdk_options,
    connect_client,
    disconnect_client,
    load_sdk,
    maybe_await,
)
from connector.runtimes.claude.sdk.settings import (
    create_gateway_settings_file,
    remove_gateway_settings_file,
)

# The CLI list only changes when Claude Code is upgraded or the account changes.
CLAUDE_MODEL_DISCOVERY_TTL_SECONDS = 600.0
CLAUDE_MODEL_DISCOVERY_RETRY_SECONDS = 60.0
CLAUDE_MODEL_DISCOVERY_TIMEOUT_SECONDS = 30.0

Clock = Callable[[], float]


@dataclass(slots=True)
class ClaudeModelDiscovery:
    """Reads the model list Claude Code reports in its initialize response.

    This is the same list the CLI shows in `/model`, so the Web picker follows
    CLI upgrades and account entitlements instead of a hand-maintained table.
    Discovery only performs the control handshake; no prompt is sent.
    """

    config_values: Mapping[str, Any]
    sdk_loader: SdkLoader | None = None
    clock: Clock = time.monotonic
    _models: tuple[dict[str, Any], ...] | None = field(default=None, init=False)
    _commands: tuple[dict[str, Any], ...] | None = field(default=None, init=False)
    _expires_at: float = field(default=0.0, init=False)
    _attempted: bool = field(default=False, init=False)
    _inflight: asyncio.Task[None] | None = field(
        default=None,
        init=False,
    )

    @property
    def attempted(self) -> bool:
        return self._attempted

    @property
    def cached_models(self) -> tuple[dict[str, Any], ...]:
        return self._models or ()

    async def models(self) -> tuple[dict[str, Any], ...] | None:
        await self._ensure()
        return self._models

    async def commands(self) -> tuple[dict[str, Any], ...] | None:
        await self._ensure()
        return self._commands

    async def _ensure(self) -> None:
        if self._attempted and self.clock() < self._expires_at:
            return
        if self._inflight is None:
            self._inflight = asyncio.create_task(self._refresh())
        inflight = self._inflight
        try:
            await asyncio.shield(inflight)
        finally:
            if self._inflight is inflight and inflight.done():
                self._inflight = None

    async def _refresh(self) -> None:
        started = self.clock()
        info: Any = None
        try:
            info = await asyncio.wait_for(
                self._read_server_info(),
                timeout=CLAUDE_MODEL_DISCOVERY_TIMEOUT_SECONDS,
            )
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                "Claude catalog discovery failed, using static catalogs error={}",
                exc,
            )
        self._attempted = True
        models = cli_models_from_server_info(info) if info is not None else None
        commands = cli_commands_from_server_info(info) if info is not None else None
        if models:
            self._models = models
        if commands:
            self._commands = commands
            logger.info(
                "Claude command discovery read {} commands elapsed_ms={:.1f}",
                len(commands),
                (self.clock() - started) * 1000,
            )
        if models:
            logger.info(
                "Claude model discovery read {} models elapsed_ms={:.1f} models={}",
                len(models),
                (self.clock() - started) * 1000,
                [model["value"] for model in models],
            )
        if models or commands:
            self._expires_at = self.clock() + CLAUDE_MODEL_DISCOVERY_TTL_SECONDS
        else:
            # Keep the last good lists; retry sooner than a successful read.
            self._expires_at = self.clock() + CLAUDE_MODEL_DISCOVERY_RETRY_SECONDS

    async def _read_server_info(self) -> Any:
        sdk = load_sdk(self.sdk_loader)
        client_cls = getattr(sdk, "ClaudeSDKClient", None)
        if client_cls is None:
            return None
        settings_path = create_gateway_settings_file(self.config_values)
        try:
            options = build_sdk_options(
                sdk,
                self.config_values,
                ClaudeSession(session_id="claude-model-discovery"),
                settings_path=settings_path,
            )
            client = client_cls(options=options)
            await connect_client(client)
            try:
                get_server_info = getattr(client, "get_server_info", None)
                if not callable(get_server_info):
                    return None
                return await maybe_await(get_server_info())
            finally:
                await disconnect_client(client)
        finally:
            remove_gateway_settings_file(settings_path)


def cli_models_from_server_info(info: Any) -> tuple[dict[str, Any], ...] | None:
    if not isinstance(info, Mapping):
        return None
    raw_models = info.get("models")
    if not isinstance(raw_models, list):
        return None
    models: list[dict[str, Any]] = []
    seen: set[str] = set()
    for raw in raw_models:
        if not isinstance(raw, Mapping):
            continue
        value = raw.get("value")
        if not isinstance(value, str) or not value or value in seen:
            continue
        seen.add(value)
        models.append(dict(raw))
    return tuple(models) or None


def cli_commands_from_server_info(info: Any) -> tuple[dict[str, Any], ...] | None:
    """Normalize the CLI's ``commands`` list (slash commands, skills, plugins).

    Each entry carries ``name``/``description``/``argumentHint``/``aliases``/
    ``builtin``.  Internal commands (``__``-prefixed) are not exposed.
    """

    if not isinstance(info, Mapping):
        return None
    raw_commands = info.get("commands")
    if not isinstance(raw_commands, list):
        return None
    commands: list[dict[str, Any]] = []
    seen: set[str] = set()
    for raw in raw_commands:
        if not isinstance(raw, Mapping):
            continue
        name = raw.get("name")
        if not isinstance(name, str) or not name or name.startswith("__"):
            continue
        if name in seen:
            continue
        seen.add(name)
        aliases = tuple(
            alias
            for alias in (raw.get("aliases") or ())
            if isinstance(alias, str) and alias
        )
        commands.append(
            {
                "name": name,
                "description": raw.get("description")
                if isinstance(raw.get("description"), str)
                else "",
                "argumentHint": raw.get("argumentHint")
                if isinstance(raw.get("argumentHint"), str)
                else "",
                "aliases": aliases,
                "builtin": raw.get("builtin") is True,
            }
        )
    return tuple(commands) or None
