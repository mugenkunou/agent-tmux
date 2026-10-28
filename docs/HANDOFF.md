# Handoff: agent-tmux UX revamp

For an agent picking this up cold. Read this first, then `PROJECT.md` (the design
memory, especially §4 "Hard tmux Facts", §5 key bindings, §6 ownership, §15 native
open). `README.md` is the user-facing doc.

## What agent-tmux is, in one paragraph

A shared, persistent tmux-backed terminal for an AI agent and a human. The agent
drives it with one-shot CLI calls (`send`, `key`, `screen`, `wait`). A human attaches
with `open`, watches, and presses `Ctrl-T` to take control (to type a password, TOTP,
an approval). **Exactly one writer at a time** is the reason the project exists. Never
ship anything that lets two terminals, or a terminal and the agent, write at once.

## Where things stand (all committed on `master`)

| Change | State | Commit |
|---|---|---|
| `Ctrl-]` escape menu (detach / kill / restart shell / Ctrl-C), Ctrl-Q works while watching, `restart` command | done | `bbac0aa` |
| Stale claims cleared when the claiming process died (tab closed, killed) | done | `bbac0aa` |
| History limit 50000 per new session (`create --history`) | done | `61b1d83` |
| `open --native`: mouse-wheel scrolling via a tmux control-mode viewer | done, opt-in flag | `61b1d83` |
| **Exclusive control** across plain/native/take, `yield` detaches writers | done | this commit |
| Audit mode (record output/inputs/events to disk) and `view` | **not started**, waiting on user decisions | — |

The list of changes in plain words, with the user's open decisions, is
`docs/revamp-problems.md`. The fuller plan is `docs/plan-ux-audit.md`. Recon on ws-tool
sessions and tmux capture is in `docs/recon-audit.md`.

## The incident you must know about

After `open --native` shipped, the user had a plain `open` tab and a native `open` tab
on the same session, and **both were in control at once**. Cause: `claim` overwrote
`@agent-tmux-owner` without checking, and `open` called `release` on start (clearing
anyone's claim). The plain tab stayed attached writable (tmux enforces read-only only
at attach time), while the native tab claimed and forwarded keys. The user was
rightly furious: this broke the core promise.

Fix (this commit):
- `claim` refuses with `ErrControlled` while another *live* process holds control.
  Process identity is `@agent-tmux-owner-proc` = `pid:starttime`.
- `release` only clears a claim held by the calling process.
- `open` / `open --native` no longer release on start.
- A refused `Ctrl-T` shows a message: a tmux status message in plain mode; a bell,
  the title, plus a line in native mode.
- `yield` clears the owner *and* detaches every writable client with exit code 10, so
  a plain `open` re-attaches read-only.
- Native mode checks ownership with tmux before forwarding each keystroke.
- Regression test: `cmd/agent-tmux/exclusive_test.go`
  (`TestOnlyOneTerminalControlsAcrossOpenModes`). It runs one plain and one native
  `open` on real ptys and checks that only the holder's typing reaches the shell. It
  **fails on the pre-fix code** (verified).

Lesson: any new attach path must go through `claim`/`release` and be tested with two
terminals at once, in mixed modes.

## The two `open` modes (how the user wants them explained)

- **Plain `open`:** tmux draws the session and tmux blocks typing (`attach -r`).
  There's no scrollback, so the mouse wheel becomes ↑/↓, which in bash means command
  history while in control. Under VTE terminals the wheel on the alternate screen
  sends arrow keys.
- **`open --native`:** agent-tmux runs `tmux -C`, prints history and screen into the
  terminal's real scrollback, then streams `%output`. agent-tmux reads keys itself:
  dropped while watching, `send-keys -H` while controlling. The wheel, selection and
  search are native. Trade-off: one pane size for everyone (follows the latest native
  viewer).

## User preferences and decisions so far

- Wants taste, not churn. Explain in plain, simple words when asked; they got confused
  when requirements were mixed together.
- `--native` stays a flag; **the user will decide later** whether it becomes the
  default. They explicitly reverted a "make native default" request. Don't flip it.
- `Ctrl-T` stays the toggle for now (open decision).
- Open decisions for audit mode (see `docs/revamp-problems.md`): on by default?
  OK not to log human keystrokes (recommended, since passwords would leak)? Files in
  `~/Sessions` in ws-tool format, or a separate folder?
- Commit only when asked. Commit trailer used in this repo:
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Known gaps and ideas, not done

- `create` from a terminal always drops into **plain** `open`. There's no
  `create --native` yet; the user has to `Ctrl-Q` and run `open --native`. Proposed,
  not approved.
- Native mode: output while the `Ctrl-]` menu is open isn't drawn (it's in tmux
  history). Ownership changes made elsewhere update the title within 2 s; input is
  checked per keystroke.
- `@agent-tmux-generation` is still unused.
- Menu `[/] history` and `agent-tmux view` wait for the audit/`view` work.

## How to test

- `make test` (sets `GOTMPDIR`; `/tmp` may be `noexec`). The full suite takes about 15 s
  and needs tmux.
- Never attach tmux to the harness's own terminal. Use a fresh pty (helpers:
  `openPTY` in `internal/termshare/manager_test.go`, `openTestPTY` in
  `cmd/agent-tmux/exclusive_test.go`), with a short `AGENT_TMUX_RUNTIME_DIR`
  (socket paths have a length limit).
- For checking what a viewer *renders*, `.tmp/pylib` holds `pyte` (gitignored); use it
  from Python with `sys.path.insert(0, ".tmp/pylib")`.
