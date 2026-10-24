"""The plugin bridge: a live Claude session driven through its own plugin."""

from connector.runtimes.claude.bridge.plugin_bridge import (
    BridgeOutbox,
    BridgeSubmission,
    available,
    bridge_dir,
    enabled,
    request_abort,
    submit,
)

__all__ = [
    "BridgeOutbox",
    "BridgeSubmission",
    "available",
    "bridge_dir",
    "enabled",
    "request_abort",
    "submit",
]
