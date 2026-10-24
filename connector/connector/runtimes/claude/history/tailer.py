"""Watch one Claude transcript and report appends promptly.

Claude Code appends each message to
``~/.claude/projects/<project>/<session-id>.jsonl``.  Polling that single file
for a size/mtime change costs microseconds, which is far cheaper than the
periodic full session scan and gives sub-second notification latency.

The watcher does not parse the transcript itself; the caller runs the normal
history sync, so projection and de-duplication stay in one place.
"""

from __future__ import annotations

import asyncio
import os
import time
from collections.abc import Awaitable, Callable
from pathlib import Path

from connector.logging import logger


def claude_projects_dir() -> Path:
    base = os.environ.get("CLAUDE_CONFIG_DIR")
    root = Path(base) if base else Path.home() / ".claude"
    return root / "projects"


def find_transcript_path(session_id: str) -> Path | None:
    if not session_id:
        return None
    root = claude_projects_dir()
    try:
        projects = list(root.iterdir())
    except OSError:
        return None
    for project in projects:
        candidate = project / f"{session_id}.jsonl"
        try:
            if candidate.is_file():
                return candidate
        except OSError:
            continue
    return None


class ClaudeTranscriptWatcher:
    """Debounced size/mtime watcher for a single transcript file."""

    def __init__(
        self,
        session_id: str,
        on_change: Callable[[], Awaitable[None]],
        *,
        poll_interval: float = 0.25,
        debounce: float = 0.35,
        max_lifetime: float = 3600.0,
    ) -> None:
        self._session_id = session_id
        self._on_change = on_change
        self._poll_interval = poll_interval
        self._debounce = debounce
        self._max_lifetime = max_lifetime
        self._task: asyncio.Task[None] | None = None
        self._stopping = False

    async def start(self) -> bool:
        if self._task is not None:
            return True
        path = find_transcript_path(self._session_id)
        if path is None:
            logger.debug(
                "claude transcript watcher: no transcript yet session_id={}",
                self._session_id,
            )
            return False
        self._task = asyncio.create_task(self._run(path))
        return True

    async def stop(self) -> None:
        self._stopping = True
        task = self._task
        self._task = None
        if task is None:
            return
        task.cancel()
        try:
            await task
        except (asyncio.CancelledError, Exception):  # noqa: BLE001 - shutdown only
            pass

    async def _run(self, path: Path) -> None:
        started = time.monotonic()
        try:
            stat = path.stat()
        except OSError:
            return
        last = (stat.st_size, stat.st_mtime_ns)
        pending_since: float | None = None
        while not self._stopping:
            if time.monotonic() - started >= self._max_lifetime:
                return
            await asyncio.sleep(self._poll_interval)
            try:
                stat = path.stat()
            except OSError:
                # File was replaced or removed; re-resolve once.
                resolved = find_transcript_path(self._session_id)
                if resolved is None:
                    return
                path = resolved
                try:
                    stat = path.stat()
                except OSError:
                    continue
                last = (stat.st_size, stat.st_mtime_ns)
                continue
            current = (stat.st_size, stat.st_mtime_ns)
            if current != last:
                last = current
                pending_since = time.monotonic()
                continue
            if pending_since is None:
                continue
            if time.monotonic() - pending_since < self._debounce:
                continue
            pending_since = None
            try:
                await self._on_change()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 - one bad sync must not stop the watcher
                logger.warning(
                    "claude transcript watcher sync failed session_id={} error={}",
                    self._session_id,
                    exc,
                )
