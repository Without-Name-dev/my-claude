"""Claude Code slash commands exposed to remote clients.

Commands are executed by typing the slash line into the live terminal, so the
catalog only lists entries that Claude Code itself understands.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from typing import Any

from connector.runtime_protocol import RuntimeCommand

_COMMAND_NAME = re.compile(r"/?[A-Za-z][A-Za-z0-9_-]*")

EFFORT_LEVELS = ("low", "medium", "high", "xhigh", "max")


def _command(
    name: str,
    description: str,
    *,
    accepts_args: bool = False,
    aliases: tuple[str, ...] = (),
    category: str = "session",
    args_schema: dict | None = None,
) -> RuntimeCommand:
    return RuntimeCommand(
        id=name,
        title=f"/{name}",
        description=description,
        aliases=aliases,
        category=category,
        accepts_args=accepts_args,
        args_schema=args_schema,
        metadata={"source": "claude.runtime"},
    )


def claude_command_catalog() -> tuple[RuntimeCommand, ...]:
    return (
        _command(
            "effort",
            "Set the reasoning effort for this session (low, medium, high, xhigh, max).",
            accepts_args=True,
            category="model",
            args_schema={"type": "string", "enum": list(EFFORT_LEVELS)},
        ),
        _command(
            "compact",
            "Compact the conversation, optionally with instructions.",
            accepts_args=True,
        ),
        _command("clear", "Clear the conversation history.", aliases=("reset",)),
        _command(
            "model",
            "Show or set the model for this session.",
            accepts_args=True,
            category="model",
        ),
        _command("cost", "Show token usage and cost for this session."),
        _command("context", "Show context window usage."),
        _command("status", "Show the current session status."),
        _command("export", "Export the conversation."),
        _command("rename", "Rename this session.", accepts_args=True),
        _command("init", "Initialize a CLAUDE.md for this project."),
        _command("memory", "Open the memory files for editing."),
        _command("permissions", "Manage tool permissions."),
        _command("agents", "Manage subagents."),
        _command("mcp", "Manage MCP servers."),
        _command("hooks", "Manage hooks."),
        _command("skills", "List available skills."),
        _command("review", "Review a pull request or the working tree.", accepts_args=True),
        _command("doctor", "Run Claude Code diagnostics."),
        _command("help", "Show help for Claude Code."),
        _command("vim", "Toggle vim editing mode."),
        _command("terminal-setup", "Configure terminal keybindings."),
    )


def commands_from_cli(
    entries: tuple[Mapping[str, Any], ...],
    query: str | None,
    limit: int,
) -> tuple[RuntimeCommand, ...]:
    """Build the command catalog from Claude Code's own initialize payload."""
    commands = tuple(_command_from_entry(entry) for entry in entries)
    return filter_claude_commands(commands, query, limit)


def _command_from_entry(entry: Mapping[str, Any]) -> RuntimeCommand:
    name = str(entry.get("name") or "")
    hint = str(entry.get("argumentHint") or "").strip()
    description = str(entry.get("description") or "").strip()
    aliases = tuple(
        str(alias) for alias in (entry.get("aliases") or ()) if str(alias)
    )
    return RuntimeCommand(
        id=name,
        title=f"/{name}",
        description=description or None,
        aliases=aliases,
        category="builtin" if entry.get("builtin") else "custom",
        accepts_args=bool(hint),
        args_schema={"type": "string", "description": hint} if hint else None,
        metadata={
            "source": "claude.cli",
            "builtin": bool(entry.get("builtin")),
            "argumentHint": hint,
        },
    )


def filter_claude_commands(
    commands: tuple[RuntimeCommand, ...],
    query: str | None,
    limit: int,
) -> tuple[RuntimeCommand, ...]:
    if isinstance(limit, int) and limit > 0:
        commands = commands[:limit]
    if not query:
        return commands
    needle = query.strip().lower().lstrip("/")
    if not needle:
        return commands
    matches = tuple(
        command
        for command in commands
        if needle in command.id.lower()
        or needle in command.title.lower()
        or (command.description or "").lower().find(needle) >= 0
    )
    return matches


def claude_command_input(
    command: str,
    raw: str | None,
    args: tuple[str, ...],
) -> tuple[str, str]:
    """Validate the requested slash command and return (name, argument text)."""
    if not isinstance(command, str) or not _COMMAND_NAME.fullmatch(command):
        raise ValueError("Invalid command name")
    name = command.removeprefix("/").lower()
    if not isinstance(args, (tuple, list)) or any(
        not isinstance(arg, str) for arg in args
    ):
        raise ValueError("Command arguments must be strings")
    if raw is not None:
        if not isinstance(raw, str) or len(raw) > 4096:
            raise ValueError("Command raw input must contain at most 4096 characters")
        match = re.fullmatch(r"\s*/([A-Za-z][A-Za-z0-9_-]*)(?:\s+([\s\S]*))?", raw)
        if not match or match[1].lower() != name:
            raise ValueError("Raw command must match the requested command")
        text = match[2] or ""
    else:
        text = " ".join(part for part in args if part)
    return name, text
