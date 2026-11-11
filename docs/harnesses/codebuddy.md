# CodeBuddy

CodeBuddy Code is a Terminal UI harness. AO launches the user's `codebuddy`
binary (alias `cbc`) in the session worktree and leaves the session open.

## Install

AO looks for `codebuddy` or `cbc` on `PATH`, then:

- `/usr/local/bin/codebuddy`, `/opt/homebrew/bin/codebuddy`
- `~/.local/bin/codebuddy`
- `%LOCALAPPDATA%\codebuddy\bin\codebuddy.exe` on Windows

`codebuddy --version` prints a version such as `2.182.0`. An existing install
can update with `codebuddy update`. AO does not ship a download script for the
first install.

## Terminal launch

A fresh session is interactive `codebuddy` with no `--acp`, `--print`, or
`--serve`. AO injects the task after the TUI is ready.

- `--model` is set only when AO has an explicit model. There is no hardcoded default.
- `--effort` is set only when AO has one of `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`.
- `--permission-mode` is `default`, `acceptEdits`, `auto`, or `bypassPermissions`.
  The flag is process-local and is not written into CodeBuddy settings. Bypass
  is not mapped to `-y`. `plan` and `dontAsk` are CodeBuddy modes AO does not select.
- Standing instructions use `--append-system-prompt`, not `--system-prompt`.
- A caller-assigned native id uses `--session-id`. Resume uses `--session-id` and `--resume`.
- Extra workspace roots use repeated `--add-dir` only for directories AO has already granted.
  The TUI launch path does not invent roots.

Worker, orchestrator, and reviewer are AO session roles. They are not passed as
CodeBuddy `--agent` personas.

## Auth

AO does not write CodeBuddy's settings; sign in inside the CodeBuddy Code CLI.
The account row reads only whether a gateway credential is present in
`~/.codebuddy/settings.json` and never its value. No file, no gateway section,
or an unreadable file stays unknown.

## Chat

Chat mode is not enabled. The chat worker turns it on by registering a driver
for harness `codebuddy` in `backend/internal/adapters/chatdriver/registry.Build`.
That registration is what makes `SupportsChat` true. The TUI adapter does not
set it.
