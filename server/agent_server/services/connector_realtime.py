from __future__ import annotations

import base64
import binascii
from typing import Any

from agent_server.infra.terminal_broker import TerminalBroker
from agent_server.infra.terminal_stream_hub import TerminalStreamHub


class ConnectorRealtimeService:
    def __init__(
        self,
        terminal_broker: TerminalBroker,
        terminal_stream_hub: TerminalStreamHub,
    ) -> None:
        self._terminal_broker = terminal_broker
        self._terminal_stream_hub = terminal_stream_hub

    async def apply(
        self,
        *,
        connector_id: str,
        method: str,
        params: dict[str, Any],
    ) -> bool:
        if method == "terminal.output":
            await self._terminal_output(connector_id, params)
        elif method == "terminal.exited":
            await self._terminal_exited(connector_id, params)
        else:
            return False
        return True

    async def _terminal_output(
        self,
        connector_id: str,
        params: dict[str, Any],
    ) -> None:
        await self._terminal_stream_hub.publish_output(connector_id, params)
        terminal_id = params.get("terminalId")
        data_b64 = params.get("dataBase64")
        seq = params.get("seq")
        if not (
            isinstance(terminal_id, str)
            and isinstance(data_b64, str)
            and isinstance(seq, int)
        ):
            return
        try:
            data = base64.b64decode(data_b64)
        except (binascii.Error, ValueError):
            data = b""
        if data:
            await self._terminal_broker.on_output(terminal_id, data=data, seq=seq)

    async def _terminal_exited(
        self,
        connector_id: str,
        params: dict[str, Any],
    ) -> None:
        await self._terminal_stream_hub.publish_exit(connector_id, params)
        terminal_id = params.get("terminalId")
        if not isinstance(terminal_id, str):
            return
        exit_code = params.get("exitCode")
        reason = params.get("reason") if isinstance(params.get("reason"), str) else None
        await self._terminal_broker.on_exited(
            terminal_id,
            exit_code=exit_code if isinstance(exit_code, int) else None,
            reason=reason,
        )
