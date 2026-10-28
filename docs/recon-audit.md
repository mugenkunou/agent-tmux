# Recon: audit mode, ws sessions, and what tmux gives us

Notes gathered before planning the UX and audit revamp. Everything marked
**measured** was run on this machine (tmux 3.4, util-linux 2.39.3) in an isolated
pty or a throwaway tmux server, never a real session.

## 1. What `ws session` is (ws-tool)

Source: `~/Workspace/experiments/ws-tool`, mainly `internal/session/session.go`,
`cmd/session.go`, and HANDBOOK.md §`ws session`.

**How it works**
- `ws session record [tag]` runs `script -q --flush -e -O <dir>/stdout.log -c <shell>`
  around one interactive bash or zsh. The terminal you're sitting in is the one recorded.
- It injects an rc file that loads your normal rc, then adds two prompt hooks:
  - **command capture:** before each prompt, the newest history entry is appended to
    `stdin.log`;
  - **indicator:** `● ws:rec` is prepended to the prompt.
- On disk, one directory per session at `~/Sessions/<tag>/`:
  - `meta.json`: `tag`, `started_at`, `ended_at`, `duration_sec`;
  - `stdout.log`: the raw PTY output, escape codes included;
  - `stdin.log`: one command line per line.
- One recording at a time per machine, tracked by a marker file at
  `~/.local/state/ws-tool/session-active.json` that holds the recorder's PID. A marker
  whose PID is dead is stale: the old session is finalized, then the new one starts.
- Size cap is `session.max_session_mb` (default 100). It warns at 80%. At the limit it
  **kills the recorder**, which ends the shell.
- Reading:
  - `show` strips escape codes and `\r` (`StripEscapes`, standard library only);
  - `grep` is a case-insensitive substring search over every session, newest first,
    commands before transcript;
  - `rm` soft-deletes to the XDG trash; `--older-than` prunes by age.
- TUI: a list of sessions, a transcript viewer, and `:grep`.

**Decisions in ws worth copying**
- **Raw keystrokes are never logged.** The code says so outright (`scriptArgs`):
  `script -I` would capture passwords typed at prompts that don't echo. Commands come from
  shell history instead. This matches agent-tmux's core promise.
- A plain-text view built by stripping escape codes. A rendered view isn't attempted.
- A size cap with a warning before it's reached.
- Stale-marker recovery, finalized from metadata.
- A tag that already exists gets `-2`, `-3`, … appended; nothing is overwritten.
- Deletion goes through the trash.

**Where ws sessions don't fit agent-tmux**

| | ws session | agent-tmux needs |
|---|---|---|
| What's recorded | the terminal you're sitting in | a pane that outlives every client |
| How many | one per machine | many at once |
| Who types | one human | scripted callers and humans, in turns |
| Command capture | history hook, top-level shell only (nothing inside ssh) | exact scripted input already passes through `send`/`key` |
| At the size cap | kills the shell | must never kill a prod ssh session |
| Permissions | dirs 0755, files 0644 | 0700 / 0600 (logs of prod sessions are sensitive) |

`script(1)` records whatever terminal it sits in. An agent-tmux pane has no terminal
of its own once everyone detaches, and wrapping each `open` in `script` would record
only the time a human was attached. **So the mechanism can't be reused, but the
on-disk format and the reading tools can** (see §4).

## 2. What tmux gives us for capture (measured)

**`pipe-pane -o -t <pane> '<cmd>'`** sends every byte the pane outputs to `<cmd>`'s
stdin, whether or not any client is attached.

| Check | Result |
|---|---|
| Keeps running across `respawn-pane -k` (the shell is replaced, the pane stays) | yes, still piping |
| Survives the pane entering and leaving copy-mode | yes |
| Survives `clear` / `printf '\e[3J'` | yes; it's a stream, so already-written bytes stay in the file |
| Running `pipe-pane -o` a second time on a piped pane | **turns piping off** (`pane_pipe` 1 → 0). Check `#{pane_pipe}` first. |
| Typed passwords (no echo) | never in the output stream, so never captured |

**tmux scrollback is not an audit store** (measured in the side thread):
- tmux uses about 2 KB of RAM per kept line. 500k lines took the server to 1.1 GB.
  The same text on disk was 38 MB, or 2.8 MB gzipped.
- `clear` sets history to 0 lines, but the memory is never returned to the OS.
- All sessions share one server, so one runaway `cat` grows memory for all of them.
- History is gone after `kill`, a crash or a reboot.
- `history-limit` is currently **2000** for every session: `-f /dev/null` skips your
  config, and the code never sets it.

**Other relevant facts (measured earlier)**
- Clients attached with `attach -r` get **no** mouse events and no bound commands
  other than `detach-client` / `switch-client`. Scrolling while watching is impossible
  under `tmux attach`.
- `detach-client -E 'exit N'` **does** fire from a read-only client, and `tmux attach`
  exits with code N. This is now how `open` reads its keys (plan item 1a, done).
- While a pane is in copy-mode, a scripted `send-keys -l` into it **hung** (more than
  5 s) when a client was attached. Any design that puts the shared pane into copy-mode
  for one viewer's scrolling breaks the agent.

**`script(1)` timing format**: `script -T` writes `<delay> <bytes>` lines, and
`scriptreplay` (installed) plays them back. A pipe-pane recorder can write the same
classic format itself at almost no cost, and that makes replay free later.

## 3. What "every stdin and stdout" can honestly mean here

Input reaches the pane through three channels, and they need different treatment:

| Channel | Example | Capture? |
|---|---|---|
| Pane output | everything shown, including the echo of anything typed | **yes**, byte-exact, via pipe-pane |
| Scripted input | `agent-tmux send/key` | **yes**, verbatim. The CLI already sees the exact text, actor id and time. These callers never handle secrets by design. |
| Human keystrokes while in control | typing commands, passwords, TOTP | **no raw bytes.** Log the claim/release window (who, when, how long). What was typed and echoed is already in the output. |

Recording raw human keystrokes would put every password typed at sudo/ssh prompts
into a file, which is exactly what agent-tmux exists to prevent. ws-tool reached the
same decision. It should be a written-down decision, not an omission.

## 4. The overlap worth using

If agent-tmux writes each recorded session as a ws-compatible directory
(`meta.json` + `stdout.log` + `stdin.log`), then `ws session ls/show/grep/rm` and the
ws TUI **work on agent-tmux recordings with no new code in either tool**:
- ws's `List` reads any directory with a parseable `meta.json`;
- unknown JSON fields are ignored, so extras like `source`, `events.jsonl` and timing
  files are fine.

Gaps if it's placed directly under `~/Sessions`:
- ws treats a session as live only if its own marker says so, so a live agent-tmux
  recording would show as `done` with a 0 s duration. The fix is small and on the ws
  side: treat `ended_at` empty plus a live writer as recording, or show `—`.
- ws creates `~/Sessions` with mode 0755. agent-tmux would still make its own session
  directories 0700 and files 0600.
- `Cmds` in ws counts lines in `stdin.log`. For agent-tmux that would be scripted
  inputs only; human commands appear in the transcript, not there.
