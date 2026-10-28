# Plan: UX revamp and audit mode

Builds on `docs/recon-audit.md`.

## Status

| Item | State |
|---|---|
| 1a escape menu + exit-code key bindings | **done** |
| 1b stale-claim fix (agent no longer locked out) | **done** |
| 1c history-limit | **done** (50000 by default, `create --history`) |
| 2 audit mode + `view` | open, waiting on decisions 1–3 below |
| 3 passthrough `open` v2 (mouse scrolling) | **done** as `open --native` (experimental; becoming the default is still open) |

## Principles

1. **tmux is the engine, not the UI.** It holds the PTY and the terminal emulation
   the agent reads. What a human sees, scrolls and searches belongs to that viewer.
   Nothing a viewer does may change the shared pane's state (copy-mode, scroll position).
2. **The disk is the record, tmux memory is a cache.** Scrollback is capped and lossy.
   The audit log is append-only and survives `clear`, `kill` and crashes.
3. **Secrets never land on disk from the input side.** Output is captured byte-exact.
   Scripted input is captured verbatim. Human keystrokes are not captured.
4. **One escape that always works, whatever phase or state you're in.**

## Phase 1: fix the daily pain (small, on the current architecture)

**1a. `Ctrl-]` escape menu. DONE.** As built:
- Every key is bound to `detach-client -E 'exit N'`: Ctrl-T = 10, Ctrl-Q = 11,
  Ctrl-] = 12. All three fire from read-only clients too.
- The `@agent-tmux-quit` flag is gone, and Ctrl-Q now works while watching.
- Ctrl-] releases control, then shows outside tmux:
  `[d]etach  [k]ill session  [r]estart shell  [c] send Ctrl-C  [any other key] back`.
- Kill and restart ask y/N.
- New command `agent-tmux restart <name>` (`respawn-pane -k`).
- Not built yet: `[/] history` (comes with `view`, phase 2) and `[t]` (Ctrl-T already
  toggles control).
- Ctrl-T stays the toggle (decision 4 still open).

**1b. Stale-claim fix. DONE.** As built, it differs from the original idea:
- It records the claiming *process* (`@agent-tmux-owner-proc` = pid:start-time), not
  the tmux client. The client changes on every toggle; the process doesn't.
- `send`/`key` clear the claim when that process is gone or a zombie, and
  `status`/`list` show the session as unclaimed.
- `open`/`take` also catch SIGHUP, so closing the tab releases control immediately.
- `@agent-tmux-generation` is still unused.

**1c. `history-limit 50000`.** Set server-wide before the first session, and make it
configurable. This is a cap (about 110 MB worst case per session), not a record.

## Phase 2: audit mode

**What gets recorded, per session** (layout is ws-compatible, see the decision below):

```
<root>/<name>-<YYYY-MM-DD-HHMM>/        0700
  meta.json        tag, started_at, ended_at, duration_sec  (ws fields)
                   + source:"agent-tmux", session, host, audit_version, truncated
  stdout.log       raw pane output, byte-exact                0600
  stdout.timing    "<delay> <bytes>" lines (scriptreplay classic format)
  stdin.log        every scripted send, one line each (ws "commands")
  events.jsonl     create, attach, detach, claim, release, send, key, respawn,
                   kill, cap-reached, recorder-died — each with time and actor id
```

**Mechanism**
- `create` makes the pane with a placeholder command. It attaches
  `pipe-pane -o 'agent-tmux _record <dir>'` (a hidden subcommand), then
  `respawn-pane`s the real shell. That way login/motd output isn't lost to the race
  between creating the pane and starting the pipe. Respawn keeps the pipe (measured).
- Always check `#{pane_pipe}` before calling `pipe-pane`, since a second call turns it off.
- `_record` streams stdin to `stdout.log`, writes timing lines and tracks size. It never
  buffers more than a block. If it dies, tmux stops piping. `ls`, `doctor` and the
  status bar show the recorder state, so it never fails silently.
- `send`/`key` append to `stdin.log` and `events.jsonl` inside the CLI. They're exact,
  and they work even when the input goes to a nested ssh/sudo (unlike a history hook).
- `claim`/`release`/attach/detach events are written by the `open`/`take` wrapper.
  Human keystrokes themselves are not recorded. That decision goes in PROJECT.md,
  with the reason.
- **At the size cap** (default 200 MB): stop capturing output, write `cap-reached` to
  events, set `truncated` in meta, and turn the status bar red (`AUDIT CAPPED`).
  **Never kill the shell.** This is the opposite of ws, because the pane may be an
  in-progress prod session.
- `kill` finalizes `meta.json`. A recording found with no `ended_at` and no live
  session is finalized as stale (the ws pattern).

**Visibility.** status-right gains `● REC` while audit is on. The tab title gets the
same mark.

**Reading**
- `agent-tmux view <name>`: opens the escape-stripped `stdout.log` in `less -R +G` if
  the session is recorded (the whole session, even after `clear`). Otherwise
  `capture-pane -J -S - -E -` (about the last 50k lines). Starts at the bottom, with
  `/`, `n`, `q`. Nothing is held in Go memory: it streams.
- `Ctrl-] /` opens the same view from inside `open`; `q` returns to the live view.
- `agent-tmux replay <name>`: uses `scriptreplay` with the timing file. It's cheap
  because the timing is already recorded; build it when wanted.
- Listing, grep and delete: **use ws** (decision below) instead of rebuilding
  `ls/grep/rm`.

**Known output-log limitations, stated up front**
- Full-screen programs (vim, htop) and progress bars show up as redraw noise once
  stripped. The raw log plus `replay` is faithful. A rendered text log isn't available
  cheaply from tmux.
- The log contains anything commands print, `kubectl get secret` included. Files are
  0600 in 0700 directories, and nothing syncs them.

## Phase 3: `open` v2, a passthrough client (behind a flag first). DONE as `open --native`

Built as described below. Details and verified control-mode facts are in PROJECT.md §15.
`[/] history` in the menu and `Enter ~ .` were not built.

This is the only real fix for "scroll like a normal terminal while watching":
- Replace `tmux attach` with a control-mode client (`tmux -C`). On attach, it dumps the
  history so it lands in **your terminal's own scrollback**, then streams the pane's
  output to your terminal on the normal screen, like ssh does.
- You get the native wheel, selection and terminal search. The transcript stays in the
  tab after you leave.
- Go owns stdin, so the read-only gate becomes "drop input unless owner". The escape
  keys work in every phase, and so does ssh-style `Enter ~ .`. Toggling control no
  longer flickers. Human keystrokes still aren't logged, as a policy choice, even
  though Go now sees them.
- Costs:
  - the pane has one size, so it follows the most recent human viewer;
  - a second viewer at a different size sees wrapping glitches;
  - terminal modes have to be restored when attaching mid-session (alternate screen,
    cursor keys).

  tmux still does the emulation, so this isn't the custom terminal emulator that
  PROJECT.md rejected.

## Order and rough size

| Step | Size | Unblocks |
|---|---|---|
| 1a escape menu + exit codes | S | stuck-session pain, the Ctrl-Q limitation |
| 1b stale-claim fix | S | agent lockout after closing a tab |
| 1c history-limit | XS | |
| 2 audit recorder + events + `view` | M | audit mode, full-history search |
| 2b ws interop (format + a small ws patch) | S | ls/grep/rm/TUI for free |
| 3 passthrough `open` v2 | L | native mouse scrolling while watching |

## Decisions for you

1. **Audit default.** Recommendation: **on by default**, `create --no-audit` to opt
   out. It costs a few MB per session, and the incident you didn't plan to record is
   the one you'll want. The alternative is opt-in `create --audit`.
2. **Where recordings live.** Recommendation: **`~/Sessions/`** in the ws-compatible
   format, tagged `at-<name>-<timestamp>`, so `ws session ls/show/grep/rm` and the ws
   TUI cover them, plus a small ws patch so live ones show as recording. The
   alternative is `~/.local/state/agent-tmux/sessions/` with no ws integration.
3. **Human command lines.** Recommendation: **don't add a history hook.** The
   transcript already shows them, and a hook only sees the top-level shell anyway, not
   commands typed inside ssh. Add it later if `stdin.log` feels incomplete.
4. **Toggle key.** Keep Ctrl-T (it collides with bash transpose and fzf), or move
   everything under `Ctrl-]` (`Ctrl-] t`).
5. **Phase 3's single pane size.** Is it acceptable? You're probably the only human
   viewer most of the time.
