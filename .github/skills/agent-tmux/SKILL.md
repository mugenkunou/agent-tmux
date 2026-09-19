---
name: agent-tmux
description: "Operate Linux through an attachable shared terminal. Use when running shell commands, SSH, sudo, debuggers, REPLs, installers, password or TOTP prompts, or any workflow where a user may need to watch or take over the terminal a caller is using."
argument-hint: "[session-name]"
---

# Agent Terminal

Use `agent-tmux` for all shell work so the user can observe and take control of the same Bash and child processes. There is no built-in notion of "agent" vs "human" — only whether a session's owner is unclaimed or held by a specific actor id; treat the user the same way any other caller is treated.

## Start

1. Assume `agent-tmux` is already installed and on `PATH`. Do not build it; do not run `make build` or reference `./bin/agent-tmux`.
2. Choose a short task-specific session name such as `work`, `incident`, or `deploy`.
3. Reuse an existing session when `agent-tmux status <session>` succeeds. Otherwise run `agent-tmux create <session>`.
4. Tell the user the session name and this single command:

   ```bash
   agent-tmux open <session>
   ```

   It attaches once, in the same terminal tab, starting unclaimed (watch-only). Pressing `Ctrl-T` claims control instantly for whoever is attached; pressing `Ctrl-T` again releases it. `Ctrl-Q` exits `open` and returns their terminal to a normal prompt without ending the session (press `Ctrl-T` first if currently watching). No second tab and no separate `take`/`yield` commands are needed for routine handoffs.

## Routine Commands

Use `run` for commands expected to finish:

```bash
agent-tmux run <session> 'cd /path/to/repository'
agent-tmux run <session> 'git status --short'
```

`run` executes in the existing Bash, so `cd`, exported variables, functions, and authentication state persist. Its exit code is the command exit code.

If `run` times out, it leaves the process running. Inspect before acting:

```bash
agent-tmux screen --history 200 <session>
```

Do not immediately retry a timed-out command; duplicate input may reach the wrong prompt.

## Interactive Commands

Use `send` when the command opens an interactive or long-lived program:

```bash
agent-tmux send <session> 'ssh user@host'
agent-tmux send <session> 'sudo -i'
agent-tmux send <session> 'gdb ./service'
```

Observe the current terminal with:

```bash
agent-tmux screen --history 200 <session>
agent-tmux wait --contains 'expected text' --timeout 15s <session>
```

Send control keys explicitly:

```bash
agent-tmux key <session> C-c
```

## Human Handoff

When the terminal requests a password, passphrase, TOTP, hardware-key action, approval, or other secret:

1. Stop sending input.
2. If the user has not already run `agent-tmux open <session>`, tell them to run it now.
3. Tell the user to press `Ctrl-T` to claim control, type the value directly, then press `Ctrl-T` again to release.
4. Do not request the secret in chat or through any agent tool.
5. Check `agent-tmux status <session>` before calling `send`, `run`, or `key`. If `owner` is not `(unclaimed)`, some other caller currently holds control — do not send input; report the current owner to the user instead.
6. After the user releases with `Ctrl-T`, confirm `status` reports `owner: (unclaimed)`, inspect `screen`, and continue from the resulting state.

If ownership is stuck on a claimed value outside of `open` (for example after an interrupted `take`), ask the user to run:

```bash
agent-tmux yield <session>
```

Never bypass a session whose `owner` is claimed by invoking tmux directly.

`watch` (read-only attach) and `take`/`yield` (manual detach-based handoff) remain available for advanced or scripted use, but `open` is the default recommendation because it needs no second tab and no manual command between watching and controlling.

## Finish

Leave useful long-running sessions available unless the user asked to close them. For disposable work, terminate explicitly:

```bash
agent-tmux kill <session>
```
