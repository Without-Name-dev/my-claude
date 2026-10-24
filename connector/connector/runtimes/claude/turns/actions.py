from __future__ import annotations

import asyncio
import re
import secrets
import time
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from connector.logging import logger
from connector.runtime_protocol import (
    RuntimeAttachment,
    RuntimeInvalidRequestError,
    RuntimeOperationResult,
    RuntimeSessionStateCache,
    RuntimeStatus,
)
from connector.runtimes.claude.bridge import plugin_bridge
from connector.runtimes.claude.domain.session import ClaudeExecution, ClaudeSession
from connector.runtimes.claude.history.syncer import ClaudeHistorySyncer
from connector.runtimes.claude.history.tailer import ClaudeTranscriptWatcher
from connector.runtimes.claude.notifications.projector import (
    ClaudeNotificationProjector,
)
from connector.runtimes.claude.sdk.client import interrupt_client
from connector.runtimes.claude.sdk.events import interrupted_terminal_event
from connector.runtimes.claude.sessions.cache import ClaudeSessionStore
from connector.runtimes.claude.terminal import attach as terminal_attach
from connector.runtimes.claude.timeline.messages import ClaudeToolBlock
from connector.runtimes.claude.turns.interactions import ClaudeInteractionController
from connector.runtimes.claude.turns.lifecycle import ClaudeTurnRunner
from connector.runtimes.claude.turns.selections import ClaudeSelectionController

# A bridge turn is bounded so a crashed plugin cannot pin the session as running.
BRIDGE_TURN_TIMEOUT_SECONDS = 3600.0
# How long to wait for the plugin to start the turn before giving up on it.
BRIDGE_START_TIMEOUT_SECONDS = 90.0
BRIDGE_POLL_SECONDS = 0.2
# 手机附件落到会话目录下的这个子目录，插件桥只送文本，模型靠 Read 看到文件。
PHONE_UPLOADS_DIRNAME = ".phone-uploads"
_UNSAFE_FILENAME = re.compile(r"[^\w.\-+]+")


@dataclass(slots=True)
class ClaudeTurnActionHandler:
    session_states: RuntimeSessionStateCache
    session_store: ClaudeSessionStore
    notifications: ClaudeNotificationProjector
    selections: ClaudeSelectionController
    interactions: ClaudeInteractionController
    runner: ClaudeTurnRunner
    history_syncer: ClaudeHistorySyncer
    _terminal_watchers: dict[str, ClaudeTranscriptWatcher] = field(
        default_factory=dict, init=False
    )

    async def stop(self) -> None:
        self.runner.stopping = True
        for session in self.session_store.sessions():
            executions = (session.queued_execution, session.execution)
            for execution in executions:
                if execution is not None:
                    execution.interrupt_source = "claude.runtime.stop"
                    execution.interrupt_reason = "runtime_stopped"
            for execution in executions:
                if execution is not None:
                    await self.interrupt_execution(
                        session=session,
                        execution=execution,
                        source="claude.runtime.stop",
                        reason="runtime_stopped",
                    )
        await self.runner.stop()

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
        session = self.session_for(session_id, external_session_id, cwd)
        try:
            effective_selections = await self.selections.effective_selections(
                session_id,
                selections,
            )
        except RuntimeInvalidRequestError as exc:
            return RuntimeOperationResult(
                ok=False,
                code="claude_invalid_selection",
                message=str(exc),
            )

        async with session.execution_lock:
            bridge_ready = (
                plugin_bridge.enabled()
                and plugin_bridge.available()
                and bool(session.external_session_id)
            )

            # A live session with the bridge plugin takes the message through its
            # queue: the plugin calls the engine's own prompt submit, so the turn
            # runs inside the terminal process and its text arrives token by token.
            # ``$.prompt.submit`` is itself queued, so a second message is accepted
            # while a turn runs; it must not replace the running execution.
            if bridge_ready:
                bridge_content = await self._bridge_content_with_attachments(
                    session=session,
                    content=content,
                    attachments=attachments,
                )
                submission = await plugin_bridge.submit(
                    bridge_content,
                    message_id=client_message_id,
                )
                if submission is not None:
                    logger.info(
                        "claude plugin bridge: session {} queued a turn",
                        session.session_id,
                    )
                    if session.execution is not None:
                        # 已有回合在跑：只入队，不创建也不覆盖 execution。
                        return RuntimeOperationResult(
                            ok=True,
                            result={
                                "turnId": session.execution.turn_id,
                                "externalSessionId": session.external_session_id,
                                "delivery": "plugin_bridge",
                                "queued": True,
                            },
                        )
                    return await self._start_bridge_turn(
                        session=session,
                        effective_selections=effective_selections,
                        content=bridge_content,
                        client_message_id=client_message_id,
                        submission=submission,
                    )
                logger.warning(
                    "claude plugin bridge could not queue for session {}; "
                    "falling back to the terminal",
                    session.session_id,
                )

            if session.execution is not None:
                return RuntimeOperationResult(
                    ok=False,
                    code="claude_turn_already_running",
                    message="Claude runtime already has an active turn for this session",
                )

            # If this Claude session is open in a live terminal, type the message
            # into that console instead of resuming it in a second SDK process.
            # The terminal then shows the message exactly like local input.
            terminal = (
                await terminal_attach.ensure_terminal_session(
                    session.external_session_id,
                    session.cwd,
                )
                if terminal_attach.enabled()
                and session.external_session_id
                and not attachments
                else None
            )
            if terminal is not None:
                injected = await terminal_attach.inject_text(terminal, content)
                if injected:
                    logger.info(
                        "claude terminal attach: session {} typed into terminal pid {}",
                        session.session_id,
                        terminal.get("pid"),
                    )
                    return await self._start_terminal_turn(
                        session=session,
                        effective_selections=effective_selections,
                        content=content,
                        client_message_id=client_message_id,
                    )
                logger.warning(
                    "claude terminal attach failed for session {} pid {}; "
                    "falling back to SDK runtime",
                    session.session_id,
                    terminal.get("pid"),
                )

            execution = ClaudeExecution(
                turn_id=f"turn_claude_{secrets.token_urlsafe(12)}"
            )
            session.execution = execution
            session.selections = effective_selections
            try:
                await self.notifications.session_state.session_state_update(
                    session,
                    "waiting",
                    selections=session.selections,
                    metadata={"source": "claude.turn.start"},
                )
                execution.task = asyncio.create_task(
                    self.runner.drive_turn(
                        session=session,
                        execution=execution,
                        content=content,
                        attachments=attachments,
                        client_message_id=client_message_id,
                    )
                )
            except BaseException:
                session.execution = None
                execution.finished.set()
                raise
        return RuntimeOperationResult(
            ok=True,
            result={
                "turnId": execution.turn_id,
                "externalSessionId": session.external_session_id,
            },
        )

    async def _bridge_content_with_attachments(
        self,
        *,
        session: ClaudeSession,
        content: str,
        attachments: tuple[RuntimeAttachment, ...],
    ) -> str:
        """把手机附件写进会话目录的 .phone-uploads 并改写 prompt。

        插件桥只收纯文本，模型要看到文件只能靠 Read 工具的路径。下载或落盘
        失败时直接跳过该附件，消息本身照常入队，不向用户报错。
        """
        if not attachments:
            return content
        cwd = session.cwd
        if not cwd:
            logger.warning(
                "claude plugin bridge: session {} has no cwd for attachments",
                session.session_id,
            )
            return content

        uploads_dir = Path(cwd) / PHONE_UPLOADS_DIRNAME
        saved_paths: list[str] = []
        for attachment in attachments:
            try:
                downloaded = await self.runner.host.attachment_download(
                    session.session_id,
                    attachment.file_id,
                )
            except Exception:  # noqa: BLE001 - 附件失败不能阻断消息
                logger.warning(
                    "claude plugin bridge: attachment download failed file_id={} "
                    "session_id={}",
                    attachment.file_id,
                    session.session_id,
                )
                continue
            filename = _safe_upload_name(
                downloaded.name or attachment.name or attachment.file_id
            )
            target = uploads_dir / f"{_safe_upload_name(attachment.file_id)}-{filename}"
            try:
                uploads_dir.mkdir(parents=True, exist_ok=True)
                target.write_bytes(downloaded.content)
            except OSError:
                logger.warning(
                    "claude plugin bridge: could not write attachment file_id={} "
                    "path={}",
                    attachment.file_id,
                    target,
                )
                continue
            saved_paths.append(str(target))

        if not saved_paths:
            return content
        if len(saved_paths) == 1:
            note = f"我上传了一个文件，路径是 {saved_paths[0]}"
        else:
            listing = "\n".join(f"- {path}" for path in saved_paths)
            note = f"我上传了文件，路径是：\n{listing}"
        return f"{content}\n\n{note}\n请先用 Read 工具读取后再回答。"

    async def _start_terminal_turn(
        self,
        *,
        session: ClaudeSession,
        effective_selections: Mapping[str, str | None],
        content: str,
        client_message_id: str | None,
    ) -> RuntimeOperationResult:
        """Record a turn whose message was typed into the live terminal.

        The message has already been delivered; this only tracks state so the
        phone shows the turn as running and returns to idle when the terminal
        finishes.
        """
        execution = ClaudeExecution(
            turn_id=f"turn_claude_{secrets.token_urlsafe(12)}"
        )
        execution.delivery = "terminal"
        session.execution = execution
        session.selections = dict(effective_selections)
        try:
            await self.notifications.session_state.session_state_update(
                session,
                "running",
                selections=session.selections,
                metadata={"source": "claude.turn.terminal"},
            )
            # Echo the user message so the phone can replace its optimistic item
            # and so history reconciliation can match the transcript entry.
            user_item = self.runner.timeline.message_item(
                session=session,
                turn_id=execution.turn_id,
                role="user",
                text=content,
                event="claude.turn.terminal.user",
                client_message_id=client_message_id,
            )
            await self.runner.publish_user_message(
                session=session,
                item=user_item,
                content=content,
                attachments=(),
                client_message_id=client_message_id,
                native_message_id=None,
            )
            watcher = ClaudeTranscriptWatcher(
                session.external_session_id or "",
                lambda: self.history_syncer.sync_session_timeline(
                    session.session_id,
                    session.external_session_id,
                ),
            )
            self._terminal_watchers[session.session_id] = watcher
            await watcher.start()
            execution.task = asyncio.create_task(
                self._watch_terminal_turn(session=session, execution=execution)
            )
        except BaseException:
            session.execution = None
            execution.finished.set()
            raise
        return RuntimeOperationResult(
            ok=True,
            result={
                "turnId": execution.turn_id,
                "externalSessionId": session.external_session_id,
                "delivery": "terminal",
            },
        )

    async def _watch_terminal_turn(
        self,
        *,
        session: ClaudeSession,
        execution: ClaudeExecution,
    ) -> None:
        try:
            await terminal_attach.wait_until_idle(session.external_session_id)
            await self.notifications.session_state.session_state_update(
                session,
                "idle",
                selections=session.selections,
                metadata={"source": "claude.turn.terminal.done"},
            )
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # pragma: no cover - defensive
            logger.warning("claude terminal turn watch failed: {}", exc)
        finally:
            watcher = self._terminal_watchers.pop(session.session_id, None)
            if watcher is not None:
                await watcher.stop()
            session.execution = None
            execution.finished.set()

    async def _start_bridge_turn(
        self,
        *,
        session: ClaudeSession,
        effective_selections: Mapping[str, str | None],
        content: str,
        client_message_id: str | None,
        submission: plugin_bridge.BridgeSubmission,
    ) -> RuntimeOperationResult:
        """Track a turn the session's own plugin raises inside its process.

        The message is already in the plugin's queue, so the terminal shows it
        and runs it like a typed prompt.  This only records the turn, echoes the
        user item, and follows the answer the plugin streams back.
        """
        execution = ClaudeExecution(
            turn_id=f"turn_claude_{secrets.token_urlsafe(12)}"
        )
        execution.delivery = "plugin_bridge"
        session.execution = execution
        session.selections = dict(effective_selections)
        try:
            await self.notifications.session_state.session_state_update(
                session,
                "running",
                selections=session.selections,
                metadata={"source": "claude.turn.bridge"},
            )
            user_item = self.runner.timeline.message_item(
                session=session,
                turn_id=execution.turn_id,
                role="user",
                text=content,
                event="claude.turn.bridge.user",
                client_message_id=client_message_id,
            )
            await self.runner.publish_user_message(
                session=session,
                item=user_item,
                content=content,
                attachments=(),
                client_message_id=client_message_id,
                native_message_id=None,
            )
            # Reserve the answer's place right behind the question. Tool items
            # are usually published before the first token arrives; without this
            # the reply lands after them and the phone draws the tool summary
            # above the message instead of below it. The projector keeps the
            # order it assigns here, so the later partials reuse it.
            self.runner.timeline.message_item(
                session=session,
                turn_id=execution.turn_id,
                role="assistant",
                text="",
                event="claude.turn.assistant.partial",
                status="running",
                item_id=f"bridge_{session.session_id}_{execution.turn_id}",
            )
            execution.task = asyncio.create_task(
                self._watch_bridge_turn(
                    session=session,
                    execution=execution,
                    submission=submission,
                )
            )
        except BaseException:
            session.execution = None
            execution.finished.set()
            raise
        return RuntimeOperationResult(
            ok=True,
            result={
                "turnId": execution.turn_id,
                "externalSessionId": session.external_session_id,
                "delivery": "plugin_bridge",
            },
        )

    async def _watch_bridge_turn(
        self,
        *,
        session: ClaudeSession,
        execution: ClaudeExecution,
        submission: plugin_bridge.BridgeSubmission,
    ) -> None:
        """Publish the answer as the plugin streams it, then reconcile history."""

        item_id = f"bridge_{session.session_id}_{execution.turn_id}"
        revision = 0
        published = ""
        published_tools: dict[str, bool] = {}
        reported_state: dict[str, Any] = {}
        follows = False
        start_deadline = time.monotonic() + BRIDGE_START_TIMEOUT_SECONDS
        deadline = time.monotonic() + BRIDGE_TURN_TIMEOUT_SECONDS

        try:
            while time.monotonic() < deadline:
                box = plugin_bridge.BridgeOutbox.read()
                # The plugin rewrites one outbox file per turn, so a snapshot
                # carrying the turn that was already there is the previous
                # answer, not this one.
                is_ours = (
                    box is not None
                    and box.turn_id != ""
                    and box.turn_id != submission.previous_turn_id
                )
                if is_ours and box is not None:
                    if not follows:
                        follows = True
                        logger.info(
                            "claude plugin bridge: following turn {} session {}",
                            box.turn_id,
                            session.session_id,
                        )
                    if box.text and box.text != published:
                        published = box.text
                        revision += 1
                        item = self.runner.timeline.message_item(
                            session=session,
                            turn_id=execution.turn_id,
                            role="assistant",
                            text=box.text,
                            event="claude.turn.assistant.partial",
                            status="running",
                            item_id=item_id,
                            revision=revision,
                        )
                        await self.notifications.timeline_activity.timeline_item_upsert(
                            item
                        )
                    await self._publish_bridge_tools(
                        session=session,
                        turn_id=execution.turn_id,
                        published=published_tools,
                    )
                    # model / effort / cwd / git / usage 由插件写进 outbox，
                    # 走已有的会话状态 metadata 上报，不新增字段。
                    state_metadata = box.state_metadata()
                    if state_metadata and state_metadata != reported_state:
                        reported_state = dict(state_metadata)
                        await self._publish_bridge_state(
                            session=session,
                            metadata=reported_state,
                        )
                    if box.finished:
                        break
                elif not follows and time.monotonic() >= start_deadline:
                    logger.warning(
                        "claude plugin bridge: no turn started for session {}",
                        session.session_id,
                    )
                    break
                if execution.finished.is_set():
                    return
                await asyncio.sleep(BRIDGE_POLL_SECONDS)
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # noqa: BLE001 - keep the session usable
            logger.warning("claude plugin bridge watch failed: {}", exc)
        finally:
            if not execution.finished.is_set():
                await self._finish_bridge_turn(
                    session=session,
                    execution=execution,
                    metadata=reported_state,
                )

    async def _publish_bridge_state(
        self,
        *,
        session: ClaudeSession,
        metadata: Mapping[str, Any],
    ) -> None:
        """Report the plugin's status snapshot without disturbing the live status."""
        state = self.notifications.session_states.get(session.session_id)
        status: RuntimeStatus = state.status if state is not None else "running"
        await self.notifications.session_state.session_state_update(
            session,
            status,
            metadata={"source": "claude.turn.bridge.state", **dict(metadata)},
        )

    async def _publish_bridge_tools(
        self,
        *,
        session: ClaudeSession,
        turn_id: str,
        published: dict[str, bool],
    ) -> None:
        """Mirror the plugin's tool calls as timeline items while they run.

        The plugin reports each call as it starts and again when the tool
        answered; the first lands as a running item, the second completes the
        same item, which is how the SDK path presents a call.
        """

        for entry in plugin_bridge.read_tools():
            tool_use_id = entry.get("id")
            if not isinstance(tool_use_id, str) or not tool_use_id:
                continue
            tool_name = entry.get("tool")
            tool_input = entry.get("input")
            done = bool(entry.get("done"))

            if tool_use_id not in published:
                call = ClaudeToolBlock(
                    block_type="tool_use",
                    tool_use_id=tool_use_id,
                    tool_name=tool_name if isinstance(tool_name, str) else None,
                    tool_input=tool_input,
                )
                item = self.runner.timeline.tool_item(
                    session=session,
                    turn_id=turn_id,
                    block=call,
                )
                await self.notifications.timeline_activity.timeline_item_upsert(item)
                published[tool_use_id] = False

            if done and not published[tool_use_id]:
                output = entry.get("output")
                result = ClaudeToolBlock(
                    block_type="tool_result",
                    tool_use_id=tool_use_id,
                    tool_name=tool_name if isinstance(tool_name, str) else None,
                    tool_input=tool_input,
                    tool_result=output if isinstance(output, str) else "",
                    is_error=bool(entry.get("isError")),
                )
                item = self.runner.timeline.tool_item(
                    session=session,
                    turn_id=turn_id,
                    block=result,
                )
                await self.notifications.timeline_activity.timeline_item_upsert(item)
                published[tool_use_id] = True

    async def _finish_bridge_turn(
        self,
        *,
        session: ClaudeSession,
        execution: ClaudeExecution,
        metadata: Mapping[str, Any] | None = None,
    ) -> None:
        """Reconcile the streamed answer with the transcript, then go idle."""

        # The history syncer refuses to touch a session it considers active, so
        # the turn has to stop owning the session before the reconcile runs.
        session.execution = None
        execution.finished.set()
        if session.external_session_id:
            try:
                await self.history_syncer.sync_session_timeline(
                    session.session_id,
                    session.external_session_id,
                )
            except Exception:  # noqa: BLE001
                logger.exception(
                    "claude plugin bridge history sync failed session_id={}",
                    session.session_id,
                )
        await self.notifications.session_state.session_state_update(
            session,
            "idle",
            selections=session.selections,
            metadata={
                "source": "claude.turn.bridge.done",
                **dict(metadata or {}),
            },
        )

    async def interrupt_session(
        self,
        session_id: str,
        reason: str | None = None,
    ) -> RuntimeOperationResult:
        session = self.session_store.get(session_id)
        if session is None:
            return RuntimeOperationResult(
                ok=True,
                result={"interrupted": False, "alreadyStopped": True},
            )
        async with session.execution_lock:
            execution = session.execution
            if execution is None:
                return RuntimeOperationResult(
                    ok=True,
                    result={"interrupted": False, "alreadyStopped": True},
                )
            if execution.delivery == "plugin_bridge":
                # The turn runs inside the Claude process, so the only way to
                # stop it is the plugin's own abort; nothing here owns the loop.
                requested = await plugin_bridge.request_abort()
                return RuntimeOperationResult(
                    ok=True,
                    result={
                        "interrupted": requested,
                        "delivery": "plugin_bridge",
                    },
                )
            execution.interrupt_source = "claude.session.interrupt"
            execution.interrupt_reason = reason
            queued = session.queued_execution
        if queued is not None:
            await self.interrupt_execution(
                session=session,
                execution=queued,
                source="claude.session.interrupt",
                reason=reason,
            )
        await self.interrupt_execution(
            session=session,
            execution=execution,
            source="claude.session.interrupt",
            reason=reason,
        )
        return RuntimeOperationResult(
            ok=True,
            result={"interrupted": True, "alreadyStopped": False},
        )

    async def interrupt_execution(
        self,
        session: ClaudeSession,
        execution: ClaudeExecution,
        source: str,
        reason: str | None,
    ) -> None:
        """Stop one owned execution and wait until all of its work has exited."""

        execution.interrupt_source = source
        execution.interrupt_reason = reason
        try:
            await interrupt_client(execution.client)
        except Exception:  # noqa: BLE001
            logger.debug(
                "Claude SDK interrupt raced with shutdown session_id={}",
                session.session_id,
            )
        task = execution.task
        if task is not None and not task.done():
            task.cancel()
        if task is not None:
            await asyncio.gather(task, return_exceptions=True)
        await self.runner.finish_execution(
            session=session,
            execution=execution,
            terminal=interrupted_terminal_event(reason),
        )

    def has_active_turn(self, session_id: str) -> bool:
        session = self.session_store.get(session_id)
        return session is not None and session.execution is not None

    def session_for(
        self,
        session_id: str,
        external_session_id: str | None,
        cwd: str | None,
    ) -> ClaudeSession:
        return self.session_store.ensure(
            session_id=session_id,
            external_session_id=external_session_id,
            cwd=cwd,
        )


def _safe_upload_name(name: str) -> str:
    """把服务端给的文件名收成一个安全路径片段。"""
    name = name.rsplit("/", 1)[-1].rsplit("\\", 1)[-1]
    sanitized = _UNSAFE_FILENAME.sub("_", name).strip("._") or ""
    return sanitized[:120] or "file"
