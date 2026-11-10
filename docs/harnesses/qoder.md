# Qoder

Qoder is an interactive terminal harness. AO launches the installed `qodercli`
binary (falling back to `qoder`) in the session worktree. Chat stays on the
existing ACP driver and is not configured from this settings surface.

## Install

Install Qoder so `qoder` is on `PATH`. On Windows the native executable is
`%USERPROFILE%\.qoder\bin\qodercli\qodercli.exe`. AO does not install it
automatically. An existing install updates with:

```bash
qoder update
```

Verify:

```bash
qoder --version
```

## Settings

The settings screen offers Qoder in the harness list, the account tab as
documentation setup, the models tab, the health tab, and the update notice.
Worker, orchestrator, and reviewer pickers can select it. For a selected role
the person can set:

- model (`qodercli --model`; empty leaves the CLI default; a custom id is passed through)
- reasoning effort (`--reasoning-effort`: auto, none, low, medium, high, xhigh, max, ultracode)
- approval (`--permission-mode`: default, accept_edits, auto, bypass_permissions)

AO approval values stay `default`, `auto`, `accept-edits`, and
`bypass-permissions`. Qoder's `dont_ask` mode is not an AO permission.

AO does not write Qoder's own settings files. Sign in with `qoder login`.
