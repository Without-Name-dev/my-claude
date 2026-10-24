from __future__ import annotations

from connector.runtime_protocol import RuntimeProvider
from connector.runtimes.claude.provider import ClaudeProvider


def default_runtime_providers() -> tuple[RuntimeProvider, ...]:
    return (ClaudeProvider(),)
