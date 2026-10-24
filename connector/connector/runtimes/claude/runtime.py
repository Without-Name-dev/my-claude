from __future__ import annotations

import asyncio
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

from connector.core.json_kv import JsonKeyValueStore
from connector.logging import logger
from connector.runtime_protocol import (
    AgentRuntime,
    PreparedSessionTimelineSync,
    RuntimeAttachment,
    RuntimeCapabilitySet,
    RuntimeCommand,
    RuntimeCommandResult,
    RuntimeConfig,
    RuntimeIdentity,
    RuntimeModelCatalog,
    RuntimeOperationResult,
    RuntimePermissionCatalog,
    RuntimeSessionStateCache,
    RuntimeTimelineSnapshot,
    RuntimeUnsupportedError,
    SessionMeta,
    SessionNotice,
    SessionState,
)
from connector.runtime_protocol.host import RuntimeHostClient
from connector.runtimes.claude.background import ClaudeBackgroundTasks
from connector.runtimes.claude.bridge import plugin_bridge

# How long one approval poll waits before telling the plugin "still pending".
# The plugin re-asks immediately; the host caps a single request at 30s.
BRIDGE_APPROVAL_POLL_SECONDS = 20.0
from connector.runtimes.claude.bridge.approval_server import BridgeApprovalServer
from connector.runtimes.claude.catalogs.reader import ClaudeCatalogReader
from connector.runtimes.claude.domain.capabilities import (
    ClaudeCapabilityContext,
    claude_runtime_capabilities,
    claude_session_capabilities,
)
from connector.runtimes.claude.domain.pending_messages import (
    ClaudePendingClientMessageRegistry,
)
from connector.runtimes.claude.domain.session import ClaudeSession
from connector.runtimes.claude.history.state import ClaudeHistoryCursorStore
from connector.runtimes.claude.history.syncer import ClaudeHistorySyncer
from connector.runtimes.claude.history.tailer import ClaudeTranscriptWatcher
from connector.runtimes.claude.notifications.notices import ClaudeNoticeRegistry
from connector.runtimes.claude.notifications.projector import (
    ClaudeNotificationProjector,
)
from connector.runtimes.claude.sdk.client import (
    ClaudeClientFactory,
    SdkLoader,
)
from connector.runtimes.claude.sessions.cache import ClaudeSessionStore
from connector.runtimes.claude.sessions.reader import ClaudeSessionReader
from connector.runtimes.claude.domain.commands import claude_command_input
from connector.runtimes.claude.sessions.sync_state import ClaudeSessionSyncStateStore
from connector.runtimes.claude.terminal import attach as terminal_attach
from connector.runtimes.claude.timeline.messages import ClaudeMessageProjector
from connector.runtimes.claude.turns.controller import ClaudeTurnController
from connector.runtimes.claude.watch import ClaudeTranscriptWatchService


@dataclass(slots=True)
class ClaudeRuntime(AgentRuntime):
    config: RuntimeConfig
    host: RuntimeHostClient
    sdk_loader: SdkLoader | None = None
    client_factory: ClaudeClientFactory | None = None
    client_message_kv: JsonKeyValueStore | None = None
    runtime_version: str = "native-0"
    _sessions: dict[str, ClaudeSession] = field(default_factory=dict, init=False)
    _session_states: RuntimeSessionStateCache = field(init=False)
    _session_store: ClaudeSessionStore = field(init=False)
    _session_reader: ClaudeSessionReader = field(init=False)
    _session_sync_states: ClaudeSessionSyncStateStore = field(init=False)
    _pending_messages: ClaudePendingClientMessageRegistry = field(init=False)
    _history_syncer: ClaudeHistorySyncer = field(init=False)
    _catalogs: ClaudeCatalogReader = field(init=False)
    _timeline: ClaudeMessageProjector = field(init=False)
    _notices: ClaudeNoticeRegistry = field(init=False)
    _notifications: ClaudeNotificationProjector = field(init=False)
    _turns: ClaudeTurnController = field(init=False)
    _command_watchers: dict[str, ClaudeTranscriptWatcher] = field(
        default_factory=dict, init=False
    )
    _transcript_watch: ClaudeTranscriptWatchService = field(init=False)
    _background: ClaudeBackgroundTasks = field(
        default_factory=ClaudeBackgroundTasks, init=False
    )
    _approval_server: BridgeApprovalServer | None = field(default=None, init=False)
    # tool_use_id -> (session_id, notice_id) for approvals the phone is holding.
    _bridge_approvals: dict[str, tuple[str, str]] = field(
        default_factory=dict, init=False
    )
    # tool_use_id -> the decision task the plugin's repeated polls share.
    _bridge_pending: dict[str, asyncio.Task[dict[str, Any]]] = field(
        default_factory=dict, init=False
    )

    def __post_init__(self) -> None:
        self._session_states = RuntimeSessionStateCache(
            "claude",
            self.host,
            on_state_updated=self.publish_session_capabilities_for_state,
        )
        self._session_store = ClaudeSessionStore(self._sessions)
        self._session_sync_states = ClaudeSessionSyncStateStore(self.host)
        self._pending_messages = ClaudePendingClientMessageRegistry(
            connector_id=getattr(
                self.host,
                "session_namespace",
                self.host.connector_id,
            ),
            kv_store=self.client_message_kv,
        )
        self._catalogs = ClaudeCatalogReader(
            config=self.config,
            sdk_loader=self.sdk_loader,
        )
        self._session_reader = ClaudeSessionReader(
            config=self.config,
            host=self.host,
            session_store=self._session_store,
            sdk_loader=self.sdk_loader,
            sync_states=self._session_sync_states,
            pending_messages=self._pending_messages,
        )
        self._history_syncer = ClaudeHistorySyncer(
            config=self.config,
            host=self.host,
            session_store=self._session_store,
            sdk_loader=self.sdk_loader,
            cursor_store=ClaudeHistoryCursorStore(self.host),
            sync_states=self._session_sync_states,
            pending_messages=self._pending_messages,
        )
        self._timeline = ClaudeMessageProjector()
        self._notices = ClaudeNoticeRegistry()
        self._notifications = ClaudeNotificationProjector(
            host=self.host,
            session_states=self._session_states,
            session_store=self._session_store,
            notices=self._notices,
        )
        self._turns = ClaudeTurnController(
            config=self.config,
            host=self.host,
            session_states=self._session_states,
            session_store=self._session_store,
            session_reader=self._session_reader,
            history_syncer=self._history_syncer,
            catalogs=self._catalogs,
            timeline=self._timeline,
            notices=self._notices,
            notifications=self._notifications,
            pending_messages=self._pending_messages,
            sdk_loader=self.sdk_loader,
            client_factory=self.client_factory,
        )
        self._transcript_watch = ClaudeTranscriptWatchService(
            list_sessions=self._watchable_sessions,
            sync_callback=self._transcript_sync_callback,
        )

    async def _watchable_sessions(self) -> list[tuple[str, str | None]]:
        sessions = await self.list_sessions(limit=500, force=True)
        return [(one.session_id, one.external_session_id) for one in sessions]

    @property
    def identity(self) -> RuntimeIdentity:
        return RuntimeIdentity(
            runtime="claude",
            runtime_version=self.runtime_version,
            display_name="Claude",
        )

    async def start(self) -> None:
        self._turns.runner.stopping = False
        await self._turns.runner.reconnect_sessions()
        self._transcript_watch.start()
        if plugin_bridge.enabled() and self._approval_server is None:
            self._approval_server = BridgeApprovalServer(
                handler=self._answer_bridge_approval,
                token=plugin_bridge.queue_token(),
                endpoint_path=plugin_bridge.approval_path(),
            )
            await self._approval_server.start()

    async def stop(self) -> None:
        server, self._approval_server = self._approval_server, None
        if server is not None:
            await server.stop()
        await self._transcript_watch.stop()
        await self._background.stop()
        await self._turns.stop()

    async def _answer_bridge_approval(
        self,
        payload: Mapping[str, Any],
    ) -> Mapping[str, Any]:
        """Ask the phone about a tool the plugin's turn wants to run.

        The plugin holds this request open, which freezes its own hook budget.
        The terminal's pane can answer the same question; it says so with a
        second request, which retracts the phone's card and settles this one.
        """
        tool_use_id = payload.get("toolUseId")
        if payload.get("action") == "cancel" and isinstance(tool_use_id, str):
            return await self._cancel_bridge_approval(payload, tool_use_id)

        session = None
        for candidate in self._session_store.sessions():
            execution = candidate.execution
            if execution is not None and execution.delivery == "plugin_bridge":
                session = candidate
                break
        if session is None or session.execution is None:
            return {"decision": "ask"}

        # The plugin re-asks with the same toolUseId until it has a verdict, so
        # one pending decision serves every poll. Without this, each retry would
        # raise a fresh card on the phone.
        key = tool_use_id if isinstance(tool_use_id, str) and tool_use_id else ""
        task = self._bridge_pending.get(key) if key else None
        if task is None:
            task = self._background.spawn(
                self._bridge_approval_decision(session, payload, tool_use_id)
            )
            if key:
                self._bridge_pending[key] = task
                task.add_done_callback(
                    lambda _finished, pending_key=key: self._bridge_pending.pop(
                        pending_key, None
                    )
                )
        try:
            return await asyncio.wait_for(
                asyncio.shield(task), timeout=BRIDGE_APPROVAL_POLL_SECONDS
            )
        except asyncio.TimeoutError:
            return {"decision": "pending"}

    async def _bridge_approval_decision(
        self,
        session: ClaudeSession,
        payload: Mapping[str, Any],
        tool_use_id: object,
    ) -> dict[str, Any]:
        """Raise the approval once and wait however long the person takes."""

        def remember(notice_id: str) -> None:
            if isinstance(tool_use_id, str) and tool_use_id:
                self._bridge_approvals[tool_use_id] = (session.session_id, notice_id)

        tool_name = payload.get("tool")
        tool_name_value = tool_name if isinstance(tool_name, str) else "tool"
        raw_tool_input = payload.get("input")
        # 原样交给审批通知与 request_tool_approval，不做裁剪：计划与问题都在这里。
        tool_input_value: Mapping[str, Any] = (
            raw_tool_input if isinstance(raw_tool_input, Mapping) else {}
        )
        try:
            decision = await self._turns.interactions.request_tool_approval(
                session=session,
                turn_id=session.execution.turn_id if session.execution else "",
                tool_name=tool_name_value,
                tool_input=tool_input_value,
                context={"bridge": True, "toolUseId": tool_use_id},
                on_notice=remember,
            )
        finally:
            if isinstance(tool_use_id, str):
                self._bridge_approvals.pop(tool_use_id, None)
        answer: dict[str, Any] = {
            "decision": "allow" if decision.allowed else "deny",
            "reason": decision.message or "",
        }
        if decision.allowed:
            updated_input = decision.updated_input
            if (
                updated_input is None
                and tool_name_value == "ExitPlanMode"
                and tool_input_value
            ):
                # updatedInput 是整体替换；计划审批至少把注入的 plan 原样回显。
                updated_input = dict(tool_input_value)
            if updated_input is not None:
                answer["updatedInput"] = dict(updated_input)
        return answer

    async def _cancel_bridge_approval(
        self,
        payload: Mapping[str, Any],
        tool_use_id: str,
    ) -> Mapping[str, Any]:
        """Settle an approval the terminal's pane already answered."""
        entry = self._bridge_approvals.get(tool_use_id)
        if entry is None:
            return {"decision": "ignored"}
        session_id, notice_id = entry
        wanted = payload.get("decision")
        await self._turns.interactions.respond_interaction(
            session_id=session_id,
            notice_id=notice_id,
            action_id="approve" if wanted == "allow" else "reject",
        )
        return {"decision": wanted if wanted in {"allow", "deny"} else "deny"}

    def _transcript_sync_callback(
        self,
        session_id: str,
        external_session_id: str,
    ):
        async def sync() -> None:
            try:
                await self._history_syncer.sync_session_timeline(
                    session_id,
                    external_session_id,
                )
            except asyncio.CancelledError:
                raise
            except Exception:  # noqa: BLE001 - the periodic scan retries
                logger.warning(
                    "claude transcript sync failed session_id={} external_session_id={}",
                    session_id,
                    external_session_id,
                )

        return sync

    async def get_config(self) -> RuntimeConfig:
        return self.config

    async def get_runtime_capabilities(self) -> RuntimeCapabilitySet:
        return claude_runtime_capabilities(
            ClaudeCapabilityContext(
                connector_id=self.host.connector_id,
                revision=self.config.revision,
            )
        )

    async def list_model_catalog(
        self,
        query: str | None = None,
        limit: int = 100,
    ) -> RuntimeModelCatalog:
        return await self._catalogs.list_model_catalog(query=query, limit=limit)

    async def list_permission_catalog(
        self,
        query: str | None = None,
        limit: int = 100,
    ) -> RuntimePermissionCatalog:
        return await self._catalogs.list_permission_catalog(query=query, limit=limit)

    async def get_session_state(
        self,
        session_id: str,
        external_session_id: str | None = None,
    ) -> SessionState | None:
        state = self._session_states.get(session_id)
        if state is not None:
            return state
        if external_session_id is not None:
            state = self._session_states.get_by_external_session_id(external_session_id)
            if state is not None:
                return state
        return await self._session_reader.get_session_state(
            session_id,
            external_session_id,
        )

    async def get_session_notices(
        self,
        session_id: str,
        external_session_id: str | None = None,
    ) -> tuple[SessionNotice, ...]:
        _ = external_session_id
        return self._notices.current_for_session(session_id)

    async def get_session_capabilities(
        self,
        session_id: str,
        external_session_id: str | None = None,
    ) -> RuntimeCapabilitySet:
        """Report session capabilities from facts already known to this process.

        Only the cached live status decides turn-based availability; a cold history
        read would return an idle state this set treats identically.
        """

        state = self._session_states.get(session_id)
        if state is None and external_session_id is not None:
            state = self._session_states.get_by_external_session_id(
                external_session_id
            )
        return claude_session_capabilities(
            ClaudeCapabilityContext(
                connector_id=self.host.connector_id,
                revision=self.config.revision,
                session_id=session_id,
                has_active_turn=claude_state_has_active_execution(state),
            )
        )

    async def publish_session_capabilities_for_state(
        self,
        state: SessionState,
    ) -> None:
        """Publish session capabilities after a Claude state transition."""

        await self.host.session_capabilities_update(
            claude_session_capabilities(
                ClaudeCapabilityContext(
                    connector_id=self.host.connector_id,
                    revision=self.config.revision,
                    session_id=state.session_id,
                    has_active_turn=claude_state_has_active_execution(state),
                )
            )
        )

    async def list_sessions(
        self,
        limit: int = 100,
        cursor: str | None = None,
        force: bool = False,
    ) -> tuple[SessionMeta, ...]:
        return await self._session_reader.list_sessions(
            limit=limit,
            cursor=cursor,
            force=force,
        )

    async def get_session_snapshot(
        self,
        session_id: str,
        external_session_id: str | None = None,
        limit: int | None = None,
    ) -> RuntimeTimelineSnapshot:
        return await self._session_reader.get_session_snapshot(
            session_id=session_id,
            external_session_id=external_session_id,
            limit=limit,
        )

    async def sync_session_timeline(
        self,
        session_id: str,
        external_session_id: str | None = None,
    ) -> bool:
        return await self._history_syncer.sync_session_timeline(
            session_id=session_id,
            external_session_id=external_session_id,
        )

    async def prepare_session_timeline_sync(
        self,
        session_id: str,
        external_session_id: str | None = None,
    ) -> PreparedSessionTimelineSync | None:
        return await self._history_syncer.prepare_session_timeline_sync(
            session_id=session_id,
            external_session_id=external_session_id,
        )

    async def create_and_start_session(
        self,
        session_id: str,
        content: str,
        title: str | None = None,
        cwd: str | None = None,
        selections: Mapping[str, str | None] | None = None,
        attachments: tuple[RuntimeAttachment, ...] = (),
        client_message_id: str | None = None,
        *,
        runtime_options: Mapping[str, Any] | None = None,
    ) -> RuntimeOperationResult:
        if runtime_options:
            raise RuntimeUnsupportedError("runtimeOptions")
        return await self._turns.create_and_start_session(
            session_id=session_id,
            content=content,
            title=title,
            cwd=cwd,
            selections=selections,
            attachments=attachments,
            client_message_id=client_message_id,
        )

    async def start_turn(
        self,
        session_id: str,
        external_session_id: str | None,
        content: str,
        selections: Mapping[str, str | None] | None = None,
        attachments: tuple[RuntimeAttachment, ...] = (),
        client_message_id: str | None = None,
        cwd: str | None = None,
    ) -> RuntimeOperationResult:
        return await self._turns.start_turn(
            session_id=session_id,
            external_session_id=external_session_id,
            content=content,
            selections=selections,
            attachments=attachments,
            client_message_id=client_message_id,
            cwd=cwd,
        )

    async def update_session_selections(
        self,
        session_id: str,
        external_session_id: str | None,
        selections: Mapping[str, str | None],
    ) -> RuntimeOperationResult:
        return await self._turns.update_session_selections(
            session_id=session_id,
            external_session_id=external_session_id,
            selections=selections,
        )

    async def list_commands(
        self,
        session_id: str,
        external_session_id: str | None = None,
        query: str | None = None,
        limit: int = 50,
    ) -> tuple[RuntimeCommand, ...]:
        _ = session_id, external_session_id
        return await self._catalogs.list_commands(query=query, limit=limit)

    async def execute_command(
        self,
        session_id: str,
        command: str,
        external_session_id: str | None = None,
        raw: str | None = None,
        args: tuple[str, ...] = (),
    ) -> RuntimeCommandResult:
        """Run a Claude slash command by typing it into the live terminal."""
        try:
            name, text = claude_command_input(command, raw, args)
        except (ValueError, TypeError) as exc:
            return RuntimeCommandResult(
                command=str(command),
                ok=False,
                code="invalid_command",
                message=str(exc),
            )

        session = self._session_store.get(session_id)
        resolved_session_id = external_session_id or (
            session.external_session_id if session is not None else None
        )

        # A session whose plugin is answering runs the command inside its own
        # process, which is both exact and cheaper than typing it in.
        if (
            resolved_session_id
            and plugin_bridge.enabled()
            and plugin_bridge.available()
        ):
            bridged = await self.run_bridge_command(
                session_id=session_id,
                command=name,
                args=" ".join(args) if args else None,
                external_session_id=resolved_session_id,
            )
            if bridged.ok:
                return RuntimeCommandResult(
                    command=name,
                    result={
                        "delivery": "plugin_bridge",
                        "executionState": "queued",
                        **(bridged.result or {}),
                    },
                )

        cwd = session.cwd if session is not None else None
        terminal = await terminal_attach.ensure_terminal_session(
            resolved_session_id,
            cwd,
        )
        if terminal is None:
            return RuntimeCommandResult(
                command=name,
                ok=False,
                code="command_unavailable",
                message="Open this session in a local terminal to use slash commands.",
            )

        payload = f"/{name}" + (f" {text}" if text else "")
        # Watch the transcript before typing; command output is written as a file
        # append and the watcher turns it into a timeline sync.
        watcher = await self._start_command_watcher(
            session_id,
            resolved_session_id,
        )
        if not await terminal_attach.inject_text(terminal, payload):
            if watcher is not None:
                await self._stop_command_watcher(session_id, watcher)
            return RuntimeCommandResult(
                command=name,
                ok=False,
                code="command_failed",
                message="Could not type the command into the terminal.",
            )
        logger.info(
            "claude command {} typed into terminal pid {}",
            payload,
            terminal.get("pid"),
        )
        # One delayed sync as a safety net for output that lands before the
        # watcher's first poll.
        self._background.spawn(
            self._sync_once_later(session_id, resolved_session_id, 1.5)
        )
        if watcher is not None:
            self._background.spawn(
                self._reap_command_watcher(session_id, watcher)
            )
        return RuntimeCommandResult(
            command=name,
            result={"delivery": "terminal", "executionState": "accepted"},
        )

    async def run_bridge_command(
        self,
        session_id: str,
        command: str,
        args: str | None = None,
        external_session_id: str | None = None,
    ) -> RuntimeOperationResult:
        """把一条斜杠命令排进插件队列，由终端内的插件自己执行。

        命令走 ``$.command.run``，只有会话 idle 时才真正运行；这里只负责入队，
        不等待结果，避免阻塞中断轮询。名字去掉前导斜杠，别名由插件解析。
        """
        if not (plugin_bridge.enabled() and plugin_bridge.available()):
            return RuntimeOperationResult(
                ok=False,
                code="claude_bridge_unavailable",
                message="The phone bridge plugin is not answering for this session.",
            )
        session = self._session_store.get(session_id)
        resolved_external_session_id = external_session_id or (
            session.external_session_id if session is not None else None
        )
        if not resolved_external_session_id:
            return RuntimeOperationResult(
                ok=False,
                code="claude_bridge_unavailable",
                message="This session is not attached to a live terminal.",
            )
        name = command.strip().lstrip("/").strip()
        if not name:
            return RuntimeOperationResult(
                ok=False,
                code="invalid_command",
                message="A command name is required",
            )
        queued = await plugin_bridge.submit_command(
            name,
            args=args,
            message_id=f"bridge-command-{session_id}",
        )
        if not queued:
            return RuntimeOperationResult(
                ok=False,
                code="claude_bridge_queue_failed",
                message="Could not queue the command for the phone bridge plugin.",
            )
        logger.info(
            "claude plugin bridge: session {} queued command /{}",
            session_id,
            name,
        )
        return RuntimeOperationResult(
            ok=True,
            result={
                "delivery": "plugin_bridge",
                "action": "command",
                "command": name,
            },
        )

    async def _start_command_watcher(
        self,
        session_id: str,
        external_session_id: str | None,
    ) -> ClaudeTranscriptWatcher | None:
        if not external_session_id:
            return None
        previous = self._command_watchers.pop(session_id, None)
        if previous is not None:
            await previous.stop()
        watcher = ClaudeTranscriptWatcher(
            external_session_id,
            lambda: self._history_syncer.sync_session_timeline(
                session_id,
                external_session_id,
            ),
            max_lifetime=15.0,
        )
        self._command_watchers[session_id] = watcher
        if not await watcher.start():
            self._command_watchers.pop(session_id, None)
            return None
        return watcher

    async def _reap_command_watcher(
        self,
        session_id: str,
        watcher: ClaudeTranscriptWatcher,
    ) -> None:
        await asyncio.sleep(16.0)
        if self._command_watchers.get(session_id) is watcher:
            self._command_watchers.pop(session_id, None)
        await watcher.stop()

    async def _stop_command_watcher(
        self,
        session_id: str,
        watcher: ClaudeTranscriptWatcher,
    ) -> None:
        if self._command_watchers.get(session_id) is watcher:
            self._command_watchers.pop(session_id, None)
        await watcher.stop()

    async def _sync_once_later(
        self,
        session_id: str,
        external_session_id: str | None,
        delay: float,
    ) -> None:
        await asyncio.sleep(delay)
        try:
            await self._history_syncer.sync_session_timeline(
                session_id,
                external_session_id,
            )
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # noqa: BLE001 - best effort refresh
            logger.warning(
                "claude command sync failed session_id={} error={}",
                session_id,
                exc,
            )

    async def interrupt_session(
        self,
        session_id: str,
        reason: str | None = None,
    ) -> RuntimeOperationResult:
        return await self._turns.interrupt_session(
            session_id=session_id,
            reason=reason,
        )

    async def respond_interaction(
        self,
        session_id: str,
        notice_id: str,
        action_id: str,
        input_data: Mapping[str, Any] | None = None,
    ) -> RuntimeOperationResult:
        return await self._turns.respond_interaction(
            session_id=session_id,
            notice_id=notice_id,
            action_id=action_id,
            input_data=input_data,
        )


def claude_state_has_active_execution(state: SessionState | None) -> bool:
    if state is None:
        return False
    return state.status in {
        "waiting",
        "pending",
        "running",
        "stopping",
        "waiting_approval",
        "blocked",
    }
