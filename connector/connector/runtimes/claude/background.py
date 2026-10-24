"""Background work the runtime starts and must not leak on shutdown."""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable
from dataclasses import dataclass, field
from typing import Any

from connector.logging import logger


@dataclass(slots=True)
class ClaudeBackgroundTasks:
    """Own the fire-and-forget tasks a runtime helper starts.

    Keeping the ownership here means the runtime asks for work to run instead of
    reaching for the event loop itself, and nothing outlives ``stop()``.
    """

    _tasks: set[asyncio.Task[Any]] = field(default_factory=set, init=False)

    def spawn(self, awaitable: Awaitable[Any]) -> asyncio.Task[Any]:
        task = asyncio.ensure_future(awaitable)
        self._tasks.add(task)
        task.add_done_callback(self._discard)
        return task

    def _discard(self, task: asyncio.Task[Any]) -> None:
        self._tasks.discard(task)
        if task.cancelled():
            return
        error = task.exception()
        if error is not None:
            logger.warning("claude background task failed: {}", error)

    async def stop(self) -> None:
        tasks, self._tasks = tuple(self._tasks), set()
        for task in tasks:
            task.cancel()
        for task in tasks:
            try:
                await task
            except asyncio.CancelledError:
                pass
            except Exception:  # noqa: BLE001 - shutdown must not raise
                logger.exception("claude background task raised during shutdown")
