from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any

from sqlalchemy import select

from agent_server.infra.db import connectors, sessions


class AttachmentRepositoryMixin:
    @asynccontextmanager
    async def attachment_write_fence(
        self, session_id: str, *, user_id: str
    ) -> AsyncIterator[None]:
        # A device deletion waits for an in-flight upload, then removes it.
        # Uploads that arrive after deletion cannot leave orphaned blobs.
        async with self._engine.begin() as conn:
            row = (
                await conn.execute(
                    select(sessions.c.id)
                    .join(connectors, connectors.c.id == sessions.c.connector_id)
                    .where(
                        sessions.c.id == session_id,
                        connectors.c.user_id == user_id,
                        connectors.c.revoked == 0,
                    )
                    .with_for_update(read=True, of=sessions)
                )
            ).first()
            if row is None:
                raise KeyError(session_id)
            yield

    async def save_user_uploaded_file(
        self,
        *,
        session_id: str,
        user_id: str,
        name: str,
        data: bytes,
        media_type: str | None = None,
    ) -> dict[str, Any]:
        return await self.attachments.save_user_upload(
            session_id=session_id,
            user_id=user_id,
            data=data,
            name=name,
            media_type=media_type,
        )

    async def read_uploaded_file(
        self, *, session_id: str, file_id: str, user_id: str
    ) -> dict[str, Any]:
        return await self.attachments.read_user_file(
            session_id=session_id,
            file_id=file_id,
            user_id=user_id,
        )
