---
name: agent-tmux
description: "Operate Linux through an attachable shared terminal. Use when running shell commands, SSH, sudo, debuggers, REPLs, installers, password or TOTP prompts, or any workflow where a user may need to watch or take over the terminal a caller is using."
argument-hint: "[session-name]"
---

# Agent Terminal

Use `agent-tmux` for all shell work so the user can observe and take control of the same Bash and child processes. There is no built-in notion of "agent" vs "human" — only whether a session's owner is unclaimed or held by a specific actor id; treat the user the same way any other caller is treated.

This protects the terminal's live input channel at the moment something needs a human — it does not hide secrets that ordinary command output may reveal later (e.g. `kubectl get secret -o yaml`, `env`, decoded config). That is explicitly out of scope; don't rely on this tool for it.

## Start

1. Assume `agent-tmux` is already installed and on `PATH`.
2. Set `AGENT_TMUX_ACTOR_ID` once, before the first `agent-tmux` call this run, using
   `<tool>-<host>-<user>-<instance-token>` (e.g. `copilot-cli-devbox-siva-4f2a`), where
   `instance-token` is a short id generated once at startup and reused for every call in
   this run. This lets `status` tell apart two similar clients — same tool, same host,
   same user — instead of colliding on a bare tool name or `$USER`. Never regenerate it
   mid-run, and never share a fixed value across concurrently running instances of the
   same tool.
3. Choose a short task-specific session name such as `work`, `incident`, or `deploy`.
4. Reuse an existing session when `agent-tmux status <session>` succeeds. Otherwise run `agent-tmux create <session>`.
5. Tell the user the session name and this single command:

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

## Recognizing When the Terminal Needs a Human

Before every `send`/`run`/`key`, check `agent-tmux status <session>` first. If `owner` is not `(unclaimed)`, some other caller currently holds control — do not send input; report the current owner to the user instead.

Not every stall means the same thing, so classify before reacting:

1. **Detect a stall.** If a command that should still be producing output stops for a
   noticeable pause, don't retry input — capture the current state instead:

   ```bash
   agent-tmux screen --history 50 <session>
   ```

2. **Classify what's on screen:**

   - **Credential/secret entry** — password, passphrase, TOTP/verification code,
     hardware-key touch, private key passphrase prompts (`password:`, `passphrase`,
     `verification code`, `token:`, or an unechoed prompt) → **silently yield** (3a).
   - **Consequential or destructive confirmation** — `(yes/no)`, `[y/N]`, "are you
     sure", "this will overwrite/delete/restart/drain", an unfamiliar host-key trust
     prompt, or anything with a side effect that isn't a secret → **seek confirmation**
     (3b).
   - **Unrecognized stall** — doesn't match either pattern → treat as **seek
     confirmation** (3b). Never assume it's safe to relay blind input, and never assume
     it's a secret prompt either.

3a. **Silently yield** (credential/secret prompts — no need to ask permission first,
   this is expected and procedural):

   1. Stop sending input immediately.
   2. If the user hasn't already run `agent-tmux open <session>`, tell them to run it now.
   3. Tell them to press `Ctrl-T` to claim control, type the value directly, then press `Ctrl-T` again to release.
   4. Never request the secret in chat or relay it through any tool call.
   5. Stop here and end your turn. Do **not** poll `agent-tmux status <session>` in a
      loop waiting for release — while owner is set, `send`/`run`/`key` are refused
      anyway, so looping only burns turns/tool calls without doing anything useful.
   6. Resume only when the user explicitly tells you they're done (e.g. "done",
      "continue", "go ahead") — treat that message, not a background retry, as the
      trigger. At that point check `agent-tmux status <session>` once, confirm
      `owner: (unclaimed)`, inspect `screen`, and continue from the resulting state.

3b. **Seek confirmation** (consequential or unrecognized prompts):

   1. Stop sending input.
   2. Surface the exact on-screen prompt text to the user in chat and ask what to
      answer — don't guess, and don't auto-proceed.
   3. If the answer is a plain, non-secret token (e.g. `yes`, a menu choice) and the
      user is fine with the agent relaying it, send it via
      `agent-tmux send <session> '<answer>'`.
   4. If the user prefers to type it themselves, or the prompt shouldn't be relayed as
      plain text, fall back to the same `open` + `Ctrl-T` handoff as 3a.
   5. Never proceed past a destructive-looking prompt on an assumption.

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
