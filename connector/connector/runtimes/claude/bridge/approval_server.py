"""A loopback endpoint the session's plugin asks before it runs a tool.

``tool.check`` has ten seconds of its own time, and a person answering on a
phone does not fit in that. The engine's budget stops while a ``$`` call of the
hook is in flight, so the plugin holds an HTTP request open here while the
connector asks the phone through the usual interaction notice.

The endpoint is bound to loopback, carries a per-process token, and answers
nothing but the decision it was asked for.
"""

from __future__ import annotations

import asyncio
import json
import os
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from connector.logging import logger

ApprovalHandler = Callable[[Mapping[str, Any]], Awaitable[Mapping[str, Any]]]

MAX_BODY_BYTES = 256 * 1024
_REASONS = {200: "OK", 400: "Bad Request", 403: "Forbidden", 500: "Server Error"}


@dataclass(slots=True)
class BridgeApprovalServer:
    """Answers the plugin's permission question with what the phone decided."""

    handler: ApprovalHandler
    token: str
    endpoint_path: Path
    _server: asyncio.AbstractServer | None = field(default=None, init=False)
    _connections: set[asyncio.Task[Any]] = field(default_factory=set, init=False)

    @property
    def port(self) -> int:
        if self._server is None:
            return 0
        for sock in self._server.sockets or ():
            try:
                return int(sock.getsockname()[1])
            except OSError:
                continue
        return 0

    async def start(self) -> None:
        if self._server is not None:
            return
        self._server = await asyncio.start_server(self._handle, "127.0.0.1", 0)
        payload = {"port": self.port, "token": self.token}
        try:
            self.endpoint_path.parent.mkdir(parents=True, exist_ok=True)
            temp = self.endpoint_path.with_name(".approval.tmp")
            temp.write_text(json.dumps(payload), encoding="utf-8")
            os.replace(temp, self.endpoint_path)
        except OSError:
            logger.warning("plugin bridge: could not publish the approval endpoint")
        logger.info("plugin bridge approval endpoint listening on 127.0.0.1:{}", self.port)

    async def stop(self) -> None:
        server, self._server = self._server, None
        if server is not None:
            server.close()
            try:
                await server.wait_closed()
            except Exception:  # noqa: BLE001 - shutdown must not raise
                logger.exception("plugin bridge approval endpoint close failed")
        for task in tuple(self._connections):
            task.cancel()
        self._connections.clear()
        try:
            self.endpoint_path.unlink()
        except OSError:
            pass

    async def _handle(
        self,
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
    ) -> None:
        task = asyncio.current_task()
        if task is not None:
            self._connections.add(task)
        try:
            request = await self._read_request(reader)
            if request is None:
                await self._respond(writer, 400, {"error": "bad request"})
                return
            if request.get("token") != self.token:
                await self._respond(writer, 403, {"error": "forbidden"})
                return
            try:
                answer = await self.handler(request)
            except Exception:  # noqa: BLE001 - never leave the hook hanging
                logger.exception("plugin bridge approval handler failed")
                answer = {"decision": "deny", "reason": "approval failed"}
            await self._respond(writer, 200, _answer_payload(answer))
        except (asyncio.IncompleteReadError, ConnectionError):
            pass
        except Exception:  # noqa: BLE001
            logger.exception("plugin bridge approval request failed")
        finally:
            try:
                writer.close()
                await writer.wait_closed()
            except Exception:  # noqa: BLE001
                pass
            if task is not None:
                self._connections.discard(task)

    async def _read_request(
        self,
        reader: asyncio.StreamReader,
    ) -> dict[str, Any] | None:
        line = await reader.readline()
        if not line:
            return None
        length = 0
        while True:
            header = await reader.readline()
            if not header or header in (b"\r\n", b"\n"):
                break
            if header.lower().startswith(b"content-length:"):
                try:
                    length = int(header.split(b":", 1)[1].strip())
                except ValueError:
                    return None
        if length <= 0 or length > MAX_BODY_BYTES:
            return None
        body = await reader.readexactly(length)
        try:
            payload = json.loads(body)
        except ValueError:
            return None
        return payload if isinstance(payload, dict) else None

    async def _respond(
        self,
        writer: asyncio.StreamWriter,
        status: int,
        payload: Mapping[str, Any],
    ) -> None:
        body = json.dumps(payload).encode("utf-8")
        head = (
            f"HTTP/1.1 {status} {_REASONS.get(status, 'OK')}\r\n"
            "Content-Type: application/json\r\n"
            f"Content-Length: {len(body)}\r\n"
            "Connection: close\r\n\r\n"
        ).encode("ascii")
        writer.write(head + body)
        await writer.drain()


def _answer_payload(answer: Mapping[str, Any]) -> dict[str, Any]:
    """Keep the tool input a question/plan approval returns.

    The plugin hands ``updatedInput`` straight to the engine, which replaces the
    whole tool input.  The handler may use either spelling, so normalise it here
    instead of dropping a key the hook would silently ignore.
    """
    payload = dict(answer)
    updated = payload.get("updatedInput")
    if updated is None:
        updated = payload.pop("updated_input", None)
    if isinstance(updated, Mapping):
        payload["updatedInput"] = dict(updated)
    return payload
