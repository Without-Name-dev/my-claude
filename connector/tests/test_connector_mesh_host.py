from __future__ import annotations

import asyncio
import io
import json
import os
from pathlib import Path

import pytest
from connector.mesh.host import serve_host
from connector.mesh.hub import CONNECTOR_ROLE, CONTROL_SERVICE, HOST_ROLE, LOGS_SERVICE, LocalHub
from connector.mesh.logs import LogEntry
from meshcall import Mesh, MeshCallError, Router
from pydantic import BaseModel, ConfigDict


class Params(BaseModel):
    model_config = ConfigDict(extra="allow")


class Lines(io.StringIO):
    def __init__(self) -> None:
        super().__init__()
        self.ready = asyncio.Event()

    def flush(self) -> None:
        self.ready.set()


def test_rpc_host_announces_its_router_serves_control_and_exits_on_stdin_close(tmp_path) -> None:
    async def exercise() -> tuple[dict, str, dict, bool]:
        read_fd, write_fd = os.pipe()
        stdout = Lines()
        host = asyncio.create_task(
            serve_host(config_path=tmp_path / "connector.json", stdin=os.fdopen(read_fd, "rb"), stdout=stdout),
        )
        await asyncio.wait_for(stdout.ready.wait(), 20)
        ready = json.loads(stdout.getvalue())
        router = (
            Router(unix_path=ready["endpoint"]["unixPath"])
            if "unixPath" in ready["endpoint"]
            else Router(pipe=ready["endpoint"]["pipe"])
        )
        async with Mesh(router, token=ready["key"]) as mesh:
            state = (await mesh.call_unary("aa.local.v2.Control", "getState", Params(), Params, timeout=10)).model_dump()
            stream = mesh.call_server_stream("aa.local.v2.Logs", "tail", Params(since=0), LogEntry)
            async with stream:
                first = await anext(stream)
        os.close(write_fd)
        await asyncio.wait_for(host, 20)
        socket_left = "unixPath" in ready["endpoint"] and Path(ready["endpoint"]["unixPath"]).exists()
        return ready, first.message, state, socket_left

    ready, message, state, socket_left = asyncio.run(exercise())

    assert ready["kind"] == "connector.ready"
    assert ready["key"].startswith("mk_host_")
    assert state["status"] == "stopped"
    assert "local router listening" in message
    assert socket_left is False


def test_hub_keys_are_random_per_process_and_scoped_by_role() -> None:
    first, second = LocalHub(), LocalHub()
    assert first.key("host") != second.key("host")
    entries = {entry["id"]: entry for entry in first.auth_document()["api_keys"]}
    assert entries["host"]["roles"] == ["client"]
    assert CONTROL_SERVICE in entries[CONNECTOR_ROLE]["register"]
    assert LOGS_SERVICE in entries[CONNECTOR_ROLE]["register"]
    assert all(first.key(role) not in json.dumps(entries) for role in entries)


def test_log_rpc_persists_and_deduplicates(tmp_path) -> None:
    from connector.logging import install_rpc_log_sink
    from connector.mesh.logs import LogHub, LogPage, LogsService

    async def exercise():
        hub = LocalHub()
        logs = LogHub(tmp_path / 'logs')
        sink = install_rpc_log_sink(logs.notify, level='DEBUG')
        try:
            await hub.start()
            await hub.mount(LogsService(logs))
            async with Mesh(hub.router, token=hub.key(HOST_ROLE)) as client:
                payload = {'time': 'now', 'level': 'info', 'name': 'host', 'message': 'rpc persisted', 'producer': 'instance', 'eventId': '1'}
                for _ in range(2):
                    await client.call_unary('aa.local.v2.Logs', 'write', Params(entries=[payload]), Params, timeout=5)
                for _ in range(3):
                    page = await client.call_unary('aa.local.v2.Logs', 'read', Params(source='bridge'), LogPage, timeout=5)
                    assert len(page.items) == 1
                with pytest.raises(MeshCallError) as invalid:
                    await client.call_unary('aa.local.v2.Logs', 'write', Params(entries=[payload] * 65), Params, timeout=5)
                assert invalid.value.code == 'invalid_argument'
            # Logs' access logs never enter the store, including validation failures.
            assert not any('RPC call aa.local.v2.Logs.' in item.message for item in logs.read().items)
        finally:
            await hub.stop()
            await sink.close()
            logs.close()
        restored = LogHub(tmp_path / 'logs')
        assert restored.read(source='bridge').items[0].message == 'rpc persisted'
        restored.close()
    asyncio.run(exercise())
