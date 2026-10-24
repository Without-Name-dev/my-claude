"""One live watcher per Claude session, refreshed as sessions appear.

The periodic connector scan stays the safety net; these watchers are what make a
reply reach the phone in well under a second. A session already answered by a
terminal turn has its own watcher, and the history syncer de-duplicates by
cursor, so overlapping coverage is harmless.
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass, field

from connector.logging import logger
from connector.runtimes.claude.history.tailer import ClaudeTranscriptWatcher

# Returns (session_id, external_session_id) for every session worth watching.
SessionLister = Callable[[], Awaitable[Sequence[tuple[str, str | None]]]]
# Builds the callback that syncs one session's timeline.
SyncFactory = Callable[[str, str], Callable[[], Awaitable[None]]]

REFRESH_SECONDS = 30.0
MAX_WATCHER_LIFETIME_SECONDS = 86_400.0


@dataclass(slots=True)
class ClaudeTranscriptWatchService:
    """Attach a watcher to each session and keep the set current."""

    list_sessions: SessionLister
    sync_callback: SyncFactory
    poll_interval: float = 1.0
    debounce: float = 0.5
    refresh_seconds: float = REFRESH_SECONDS
    _watchers: dict[str, ClaudeTranscriptWatcher] = field(
        default_factory=dict, init=False
    )
    _task: asyncio.Task[None] | None = field(default=None, init=False)

    def start(self) -> None:
        if self._task is None:
            self._task = asyncio.create_task(self._run())

    async def stop(self) -> None:
        task, self._task = self._task, None
        if task is not None:
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass
        watchers, self._watchers = self._watchers, {}
        for watcher in watchers.values():
            await watcher.stop()

    @property
    def watching(self) -> dict[str, ClaudeTranscriptWatcher]:
        return self._watchers

    async def _run(self) -> None:
        while True:
            try:
                for session_id, external in await self.list_sessions():
                    if not external or external in self._watchers:
                        continue
                    watcher = ClaudeTranscriptWatcher(
                        external,
                        self.sync_callback(session_id, external),
                        poll_interval=self.poll_interval,
                        debounce=self.debounce,
                        max_lifetime=MAX_WATCHER_LIFETIME_SECONDS,
                    )
                    self._watchers[external] = watcher
                    await watcher.start()
            except asyncio.CancelledError:
                raise
            except Exception:  # noqa: BLE001 - keep watching other sessions
                logger.exception("claude transcript watch refresh failed")
            await asyncio.sleep(self.refresh_seconds)
