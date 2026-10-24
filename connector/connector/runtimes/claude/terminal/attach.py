"""Deliver phone messages into a live Claude Code terminal.

Claude Code registers every interactive session under ``~/.claude/sessions``
(``<pid>.json`` plus a ``<pid>.<hash>.key``).  Sessions started from a terminal
own a real console, so on Windows we can type into it exactly like a person
would: ``AttachConsole`` + ``WriteConsoleInputW`` + Enter.  That is the only
delivery path that both executes in the *same* process the user is looking at
and renders the message in the terminal UI.

The alternative delivery path (Claude Code's ``cc-msg`` named pipe) is recorded
with ``isMeta: true`` and is therefore invisible in the TUI, so it is only used
by the SDK runtime, never here.

Everything in this module is stdlib-only so it can be unit tested without the
connector's dependencies.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import threading
import time
from collections.abc import Mapping
from pathlib import Path
from typing import Any

from connector.logging import logger

TERMINAL_ATTACH_ENV = "AGENT_CONNECTOR_CLAUDE_TERMINAL_ATTACH"
TERMINAL_SPAWN_ENV = "AGENT_CONNECTOR_CLAUDE_TERMINAL_SPAWN"
_DISABLED_VALUES = {"0", "false", "no", "off"}


def enabled() -> bool:
    """Terminal attach is Windows-only and can be switched off per machine."""
    if sys.platform != "win32":
        return False
    return os.environ.get(TERMINAL_ATTACH_ENV, "1").strip().lower() not in _DISABLED_VALUES


def sessions_dir() -> Path:
    base = os.environ.get("CLAUDE_CONFIG_DIR")
    root = Path(base) if base else Path.home() / ".claude"
    return root / "sessions"


def _pid_alive(pid: int) -> bool:
    if sys.platform == "win32":
        import ctypes

        process_query_limited_information = 0x1000
        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.OpenProcess.argtypes = [ctypes.c_ulong, ctypes.c_int, ctypes.c_ulong]
        kernel32.OpenProcess.restype = ctypes.c_void_p
        kernel32.CloseHandle.argtypes = [ctypes.c_void_p]
        handle = kernel32.OpenProcess(
            process_query_limited_information, 0, int(pid)
        )
        if not handle:
            return False
        kernel32.CloseHandle(handle)
        return True
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def list_terminal_sessions() -> list[dict[str, Any]]:
    directory = sessions_dir()
    try:
        files = list(directory.glob("*.json"))
    except OSError:
        return []
    found: list[dict[str, Any]] = []
    for path in files:
        try:
            info = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if info.get("kind") != "interactive":
            continue
        if info.get("status") == "exited":
            continue
        pid = int(info.get("pid") or 0)
        if pid <= 0 or not _pid_alive(pid):
            continue
        found.append(info)
    return found


def find_terminal_session(session_id: str | None) -> dict[str, Any] | None:
    """Return the live terminal registration for a Claude session id."""
    if not session_id:
        return None
    for info in list_terminal_sessions():
        if info.get("sessionId") == session_id:
            return info
    return None


def spawn_enabled() -> bool:
    """Whether we may open a local terminal for a session that is not running."""
    if sys.platform != "win32":
        return False
    return os.environ.get(TERMINAL_SPAWN_ENV, "1").strip().lower() not in _DISABLED_VALUES


def _claude_executable() -> str | None:
    explicit = os.environ.get("CLAUDE_CODE_PATH")
    if explicit and Path(explicit).exists():
        return explicit
    found = shutil.which("claude.exe")
    if found:
        return found
    appdata = os.environ.get("APPDATA")
    if appdata:
        guess = (
            Path(appdata)
            / "npm"
            / "node_modules"
            / "@anthropic-ai"
            / "claude-code"
            / "bin"
            / "claude.exe"
        )
        if guess.exists():
            return str(guess)
    return None


def _cwd_trusted(cwd: str | None) -> bool:
    """Claude Code stops at a trust dialog for unknown folders, so avoid them."""
    if not cwd:
        return False
    try:
        resolved = Path(cwd).resolve()
        home = Path.home().resolve()
    except OSError:
        resolved = home = None
    # The home directory never shows the trust dialog, and `hasTrustDialogAccepted`
    # is absent/false there even when Claude Code runs from it every day.
    if resolved is not None and home is not None and resolved == home:
        return True
    try:
        data = json.loads((Path.home() / ".claude.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    projects = data.get("projects") or {}
    if not isinstance(projects, dict):
        return False
    folded = {str(key).casefold(): value for key, value in projects.items()}
    for key in {cwd, cwd.replace("\\", "/"), cwd.replace("/", "\\")}:
        entry = folded.get(key.casefold())
        if isinstance(entry, dict) and entry.get("hasTrustDialogAccepted"):
            return True
    return False


_TRUST_LOCK = threading.Lock()


def _mark_cwd_trusted(cwd: str) -> bool:
    """Record the "Yes, I trust this folder" decision for a spawned session.

    A spawned ``claude --resume`` is interactive, so Claude Code blocks on the
    workspace trust dialog and no CLI flag answers it. The connector opens the
    window for a session that already lives in this folder, so it records the
    same decision the user would have made, for that one folder only.

    Authorised explicitly by the user (2026-10-23). Never call this with a path
    that did not come from an existing session's own cwd.
    """

    if not cwd or not os.path.isabs(cwd):
        return False
    path = Path.home() / ".claude.json"
    key = os.path.normpath(cwd).replace("\\", "/")
    with _TRUST_LOCK:
        try:
            raw = path.read_text(encoding="utf-8")
            data = json.loads(raw) if raw.strip() else {}
        except (OSError, ValueError):
            return False
        if not isinstance(data, dict):
            return False
        projects = data.get("projects")
        if not isinstance(projects, dict):
            projects = {}
            data["projects"] = projects
        folded = {str(existing).casefold(): str(existing) for existing in projects}
        existing_key = folded.get(key.casefold())
        entry = projects.get(existing_key) if existing_key is not None else None
        if not isinstance(entry, dict):
            entry = {}
            projects[existing_key or key] = entry
        if entry.get("hasTrustDialogAccepted") is True:
            return True
        entry["hasTrustDialogAccepted"] = True
        try:
            temp = path.with_name(".claude.json.connector-tmp")
            temp.write_text(
                json.dumps(data, ensure_ascii=False, separators=(",", ":")),
                encoding="utf-8",
            )
            os.replace(temp, path)
        except OSError:
            return False
        logger.info("marked workspace trusted for spawned session cwd={}", key)
        return True


_SPAWNING: set[str] = set()


async def ensure_terminal_session(
    session_id: str | None,
    cwd: str | None,
    *,
    timeout: float = 40.0,
    poll_interval: float = 0.5,
) -> dict[str, Any] | None:
    """Return the live terminal for ``session_id``, starting one when needed.

    When the phone opens a session that is not running locally, this launches a
    new console window with ``claude --resume <session-id>`` so the desktop and
    the phone share exactly the same session.
    """
    if not session_id:
        return None
    info = find_terminal_session(session_id)
    if info is not None:
        return info
    if not spawn_enabled() or session_id in _SPAWNING:
        return None
    if not _cwd_trusted(cwd):
        # An interactive `claude --resume` blocks on the workspace trust dialog
        # and no CLI flag answers it. The user authorised the connector to
        # record the same decision, scoped to the session's own cwd.
        if cwd is None or not _mark_cwd_trusted(cwd):
            return None
    executable = _claude_executable()
    if executable is None:
        return None

    import asyncio

    _SPAWNING.add(session_id)
    try:
        env = dict(os.environ)
        env.pop("CLAUDE_CODE_CHILD_SESSION", None)
        creationflags = getattr(subprocess, "CREATE_NEW_CONSOLE", 0)
        subprocess.Popen(
            [executable, "--resume", str(session_id)],
            cwd=cwd,
            env=env,
            creationflags=creationflags,
            close_fds=True,
        )
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            await asyncio.sleep(poll_interval)
            info = find_terminal_session(session_id)
            if info is not None and str(info.get("status") or "") in {"idle", "busy"}:
                return info
        return None
    except OSError:
        return None
    finally:
        _SPAWNING.discard(session_id)


# --------------------------------------------------------------------------
# Windows console input
# --------------------------------------------------------------------------

if sys.platform == "win32":  # pragma: no cover - exercised on Windows only
    import ctypes
    from ctypes import wintypes

    _kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    _user32 = ctypes.WinDLL("user32", use_last_error=True)

    class _KEY_EVENT_RECORD(ctypes.Structure):
        _fields_ = [
            ("bKeyDown", wintypes.BOOL),
            ("wRepeatCount", wintypes.WORD),
            ("wVirtualKeyCode", wintypes.WORD),
            ("wVirtualScanCode", wintypes.WORD),
            ("UnicodeChar", wintypes.WCHAR),
            ("dwControlKeyState", wintypes.DWORD),
        ]

    class _INPUT_UNION(ctypes.Union):
        _fields_ = [("KeyEvent", _KEY_EVENT_RECORD)]

    class _INPUT_RECORD(ctypes.Structure):
        _fields_ = [
            ("EventType", wintypes.WORD),
            ("Event", _INPUT_UNION),
        ]

    _KEY_EVENT = 0x0001
    _VK_RETURN = 0x0D
    _GENERIC_READ_WRITE = 0xC0000000
    _FILE_SHARE_READ_WRITE = 0x00000003
    _OPEN_EXISTING = 3
    _INVALID_HANDLE_VALUE = -1

    _kernel32.FreeConsole.argtypes = []
    _kernel32.FreeConsole.restype = wintypes.BOOL
    _kernel32.AttachConsole.argtypes = [wintypes.DWORD]
    _kernel32.AttachConsole.restype = wintypes.BOOL
    _kernel32.CreateFileW.argtypes = [
        wintypes.LPCWSTR,
        wintypes.DWORD,
        wintypes.DWORD,
        wintypes.LPVOID,
        wintypes.DWORD,
        wintypes.DWORD,
        wintypes.HANDLE,
    ]
    _kernel32.CreateFileW.restype = wintypes.HANDLE
    _kernel32.WriteConsoleInputW.argtypes = [
        wintypes.HANDLE,
        ctypes.POINTER(_INPUT_RECORD),
        wintypes.DWORD,
        ctypes.POINTER(wintypes.DWORD),
    ]
    _kernel32.WriteConsoleInputW.restype = wintypes.BOOL
    _kernel32.CloseHandle.argtypes = [wintypes.HANDLE]
    _kernel32.CloseHandle.restype = wintypes.BOOL
    _user32.VkKeyScanW.argtypes = [wintypes.WCHAR]
    _user32.VkKeyScanW.restype = ctypes.c_short

    def _build_records(text: str, press_enter: bool) -> tuple[Any, int]:
        items: list[Any] = []
        for char in text:
            scan = _user32.VkKeyScanW(char)
            vk = (scan & 0xFF) if scan != -1 else 0
            for key_down in (1, 0):
                record = _INPUT_RECORD()
                record.EventType = _KEY_EVENT
                record.Event.KeyEvent.bKeyDown = key_down
                record.Event.KeyEvent.wRepeatCount = 1
                record.Event.KeyEvent.wVirtualKeyCode = vk
                record.Event.KeyEvent.UnicodeChar = char
                items.append(record)
        if press_enter:
            for key_down in (1, 0):
                record = _INPUT_RECORD()
                record.EventType = _KEY_EVENT
                record.Event.KeyEvent.bKeyDown = key_down
                record.Event.KeyEvent.wRepeatCount = 1
                record.Event.KeyEvent.wVirtualKeyCode = _VK_RETURN
                record.Event.KeyEvent.UnicodeChar = "\r"
                items.append(record)
        array = (_INPUT_RECORD * len(items))(*items)
        return array, len(items)

    LAST_ERROR: str | None = None
    _INVALID = ctypes.c_void_p(-1).value

    def _inject_sync(pid: int, text: str) -> bool:
        global LAST_ERROR
        LAST_ERROR = None
        _kernel32.FreeConsole()
        if not _kernel32.AttachConsole(pid):
            LAST_ERROR = f"AttachConsole({pid}) failed err={ctypes.get_last_error()}"
            return False
        handle = None
        try:
            handle = _kernel32.CreateFileW(
                "CONIN$",
                _GENERIC_READ_WRITE,
                _FILE_SHARE_READ_WRITE,
                None,
                _OPEN_EXISTING,
                0,
                None,
            )
            if not handle or int(handle) == _INVALID:
                LAST_ERROR = f"CreateFileW(CONIN$) failed err={ctypes.get_last_error()}"
                return False
            records, count = _build_records(text, press_enter=True)
            written = wintypes.DWORD(0)
            ok = _kernel32.WriteConsoleInputW(
                handle, records, count, ctypes.byref(written)
            )
            if not ok:
                LAST_ERROR = f"WriteConsoleInputW failed err={ctypes.get_last_error()}"
                return False
            if written.value != count:
                LAST_ERROR = f"partial write {written.value}/{count}"
                return False
            return True
        finally:
            if handle and int(handle) != _INVALID:
                _kernel32.CloseHandle(handle)
            _kernel32.FreeConsole()

else:  # pragma: no cover - non-Windows

    def _inject_sync(pid: int, text: str) -> bool:
        return False


async def inject_text(info: Mapping[str, Any], text: str) -> bool:
    """Type ``text`` into the terminal owning ``info['pid']`` and press Enter."""
    if not text:
        return False
    pid = int(info.get("pid") or 0)
    if pid <= 0:
        return False
    import asyncio

    return await asyncio.to_thread(_inject_sync, pid, text)


async def wait_until_idle(
    session_id: str | None,
    *,
    startup_timeout: float = 15.0,
    idle_timeout: float = 1800.0,
    poll_interval: float = 0.5,
) -> None:
    """Wait for the attached terminal session to finish the turn."""
    if not session_id:
        return
    import asyncio

    deadline = time.monotonic() + idle_timeout
    saw_busy = False
    idle_streak = 0
    startup_deadline = time.monotonic() + startup_timeout
    while time.monotonic() < deadline:
        info = find_terminal_session(session_id)
        if info is None:
            return
        status = str(info.get("status") or "")
        if status == "busy":
            saw_busy = True
            idle_streak = 0
        elif status == "idle" and saw_busy:
            # Require the idle state to persist so the gap between two queued
            # turns is not mistaken for the end of this one.
            idle_streak += 1
            if idle_streak >= 3:
                return
        elif status == "idle" and time.monotonic() >= startup_deadline:
            # The message never flipped the session to busy (queued or fast).
            return
        await asyncio.sleep(poll_interval)


if __name__ == "__main__":  # manual smoke test: python -m ... <session-id> <text>
    import asyncio

    target = sys.argv[1] if len(sys.argv) > 1 else ""
    payload = " ".join(sys.argv[2:]) or "terminal attach smoke test"
    info = find_terminal_session(target)
    print(json.dumps(info, ensure_ascii=False, indent=2) if info else "session not found")
    if info:
        injected = asyncio.run(inject_text(info, payload))
        print("injected:", injected)
        if not injected:
            print("reason:", LAST_ERROR if sys.platform == "win32" else "non-windows")
