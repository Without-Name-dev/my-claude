Agents Anywhere connector
=========================

PAIR (once)
-----------
1. Double-click pair.bat
   A six-digit code appears and the window waits.
2. In the web app: 设备 → + → 使用命令行 → 给设备起名 → 继续 → 使用配对码
   Type that six-digit code and press 认领.
3. The window prints "Paired." and closes.

RUN
---
4. Double-click start.bat and leave the window open.
   The device turns green, and the Claude sessions on this computer
   appear under 项目 in the sidebar.

Different server?
   pair.bat http://192.168.1.10:8080

What it reads
   sessions   %USERPROFILE%\.claude\projects
   live turns %USERPROFILE%\.claude\phone-bridge  (the phone-pane plugin)

If the device shows online but a session is read-only, the phone-pane plugin
is not loaded in that Claude session — start Claude Code in a terminal there.
