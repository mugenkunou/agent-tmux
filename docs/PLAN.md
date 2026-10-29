# agent-tmux plan: always-on recording, ws integration, releases

Single source for the pending work. Read `PROJECT.md` first (§4 "Hard tmux Facts",
§5 key bindings, §6 ownership, §9 known limitations). `README.md` is the user-facing doc.

## 1. Where things stand

Done on `master`:
- `Ctrl-]` escape menu (detach / kill / restart shell / Ctrl-C), `Ctrl-Q` while watching, `restart`.
- Stale claims cleared when the claiming process dies (`@agent-tmux-owner-proc` = `pid:starttime`).
- History limit 50000 lines per new session (`create --history`).
- `open --native` removed (`64b66da`). One viewing mode: plain `tmux attach` for `open`, `watch` and `take`.
- Exclusive control across `open`/`take` (`f5fd618`). `claim` refuses while another live process holds control, `release` only clears your own claim, `yield` detaches writers.

Implemented, uncommitted (2026-10-29), all of §4 steps 1–7:
- agent-tmux: recorder (`internal/recording`, hidden `_record`), `ls`/`show`/`grep`/`rm`/`version`,
  `● REC` indicator, `[/] history` menu entry, GoReleaser + Actions workflow, module renamed to
  `github.com/mugenkunou/agent-tmux` (needed for `go install`). Docs: README, SKILL.md, PROJECT.md §4/§16.
- ws: old recorder and `~/Sessions` removed, `ws session` execs agent-tmux, completion delegated,
  TUI sessions page on `ls --json`, version nudge, `ws skill install --providers` + `ws skill check`.

Decisions made while implementing (the plan was silent):
- "Recorder running" = `#{pane_pipe}` is 1 **and** `recorder.lock` is held; `pane_pipe` lags a dead
  recorder until the pane prints again.
- The recorder writes `ended_at` itself on EOF; `kill` waits up to 2 s for it. `ls` finalizes only
  recordings whose recorder lock is free.
- `rm <name>` refuses if `<name>` is live, even when ended recordings share the name; use the id.
- ws has no `doctor` (grammar forbids umbrella verbs), so the nudge shows in `ws version` and the TUI.
- Provider skills are copied by an explicit `ws skill install --providers`, not silently on every
  run, so `ws skill check` can report drift. The workspace skills folder is `agent/skills`.

Left to do:
- Run `goreleaser check` once (not validated here), commit, then push tag `v0.1.0`.
- Manual: delete the `"session": {...}` block from `~/Workspace/ws.json`; `ws config edit` rejects it.

## 2. Core promise and the incident behind it

**Exactly one writer at a time.** After `open --native` shipped (since removed), a plain
tab and a native tab on one session were both in control, because `claim` overwrote the
owner unchecked. Lesson: every attach path goes through `claim`/`release` and gets a
two-terminal test (`cmd/agent-tmux/exclusive_test.go`).

## 3. Architecture

**agent-tmux is the engine. ws is a thin front end.** ws never reads agent-tmux's files.

### Recording (agent-tmux)
- Every session is recorded. No flag, no opt-out. Sessions are used sparingly, so disk use stays small.
- `create` makes the pane with a placeholder command, attaches
  `pipe-pane -o 'agent-tmux _record <dir>'` (hidden subcommand), then `respawn-pane`s the
  real shell, so login output isn't lost to a race. Respawn keeps the pipe (measured).
- Always check `#{pane_pipe}` before `pipe-pane`: a second call turns piping off.
- `_record` is the only writer. It never buffers more than a block. If it dies, tmux stops
  piping, so `ls`/`doctor`/status bar show recorder state; it must never fail silently.
- Layout, under `~/.local/state/agent-tmux/` (dirs 0700, files 0600):
  ```
  <name>-<YYYY-MM-DD-HHMM>/
    meta.json         name, started_at, ended_at, host, first_segment
    stdout.NNNNNN.log raw pane output, byte-exact, ~64 MB per segment
    stdout.NNNNNN.timing  "<delay> <bytes>" lines (scriptreplay format)
    events.jsonl      create, attach, detach, claim, release, send, key, respawn,
                      kill, rotated, recorder-died
  ```
- A name collision gets `-2`, `-3`... Nothing is overwritten.
- A recording outlives its session. `kill` finalizes `meta.json` but keeps the files.
  A recording with no `ended_at` and no live session is finalized as stale on the next `ls`.
- Input is never recorded on its own, human or scripted: the output pipe is the
  transcript, and it already shows whatever echoed (passwords and TOTP don't echo, so
  they stay out). `send`/`key`/`claim`/`release`/attach/detach are logged as events,
  without the text.

### Rotation
- Silent, never stops capture, never touches the shell.
- At 64 MB the recorder closes the segment, opens the next, and unlinks the oldest once
  more than **8** segments exist (cap about 512 MB per session). Unlink runs in a goroutine
  so the recorder never stalls. Rotate at a newline when one is nearby.
- Each rotation appends `rotated` to `events.jsonl` and bumps `first_segment` in
  `meta.json`, so `show` can say "history starts at 14:02". Loss is never invisible.
- A user who generates multi-GB output owns losing the old history.

### Verbs (agent-tmux owns the vocabulary, ws has none)
- `create` (not `record`: everything is recorded, so recording is a property).
- `kill` (not `stop`). Ends the shell, recording stays.
- `ls` replaces `list`. **No alias**, no backward compatibility.
- `show`, `grep`, `rm`, `open`, `send`, `key`, `screen`, `wait`, `restart`, `yield` stay.
- **`view` is dropped.** `show` pages through `less -R +G` on a tty and prints plain,
  escape-stripped text when piped (like `git log`). One verb for humans and agents.
  The `Ctrl-]` menu gets `[/] history`, which runs `show`.
- `show` on a live session uses the files, so it covers the whole recording, even after `clear`.
- `grep`: case-insensitive substring over transcripts, newest first, capped. Transcript only.
- `rm`: moves to the XDG trash (port the implementation from ws), supports `--older-than`,
  **refuses a live session** (kill first).
- Every read verb that ws uses has `--json`.
- Known limit: full-screen programs (vim, htop) and progress bars are redraw noise once
  stripped. The raw log plus `scriptreplay` is faithful. The log can contain anything
  commands print; files are 0600 and nothing syncs them.
- Status bar and tab title show `● REC` while recording.

### Shell completion
`ws session <TAB>` delegates to agent-tmux's completion. Built now. It must work for
bash and zsh, and complete session names from `ls --json`.

### ws side
- `ws session <args>` is `exec agent-tmux <args>`. No session CLI of its own, so no flags,
  schema or help text to keep in step. (The noun `session` stays.)
- **The TUI sessions page is the real ws surface.** It's a client of `agent-tmux ls --json`:
  - agent-tmux missing: page is greyed out with install steps.
  - lists live and recorded sessions together, with a state column;
  - actions map to agent-tmux verbs: `open`, `show` (suspend TUI, run pager), `grep`
    (render `grep --json` results in the TUI), `kill`, `rm` (to trash).
  - "create and attach" uses `open`.
- ws deletes all of its recorder code: `script`, rc hook, marker file, stdin hook,
  `session.max_session_mb`, config keys, docs, tests, TUI data source, and every
  `~/Sessions` reference. Afterwards `grep -ri sessions` on ws finds nothing.
  No migration; the existing workspace is fixed manually.
- **Skills:** ws keeps an explicit short list of tools (`agent-tmux` first) and takes the
  output of `<bin> skill` into the workspace skills folder (`workspace/agents/skills`),
  linking it to agent tools as it does today. Explicit list, never a PATH scan.
  `ws skill check` flags a copy that no longer matches the installed binary.
  agent-tmux already stopped installing skills itself (`1b41b8a`, `7624d13`).

### Compatibility policy
No contract integer, no matrix, no runtime compatibility checks. We start fresh.
One convention: from now on change verbs, flags and JSON fields **additively only**, so
an older ws keeps working with a newer agent-tmux.

### Version nudge (ws only)
- ws TUI (and `ws doctor`) asks `api.github.com/repos/mugenkunou/agent-tmux/releases/latest`,
  compares with `agent-tmux version`, and shows one line "agent-tmux X is available"
  with the install steps. That is all.
- Cached 24 h in `$XDG_CACHE_HOME`, ETag conditional request (a 304 costs no rate limit),
  async, fails silently, env var to turn it off. Never runs during session commands.
- ws's own release/version check is out of scope.

### Install steps shown in the nudge
Two lines: download from the release page, and
`go install github.com/mugenkunou/agent-tmux/cmd/agent-tmux@latest`. No `install.sh` yet.

### Release pipeline (agent-tmux)
- GitHub Actions on tag `v*`: install tmux, `make test`, then **GoReleaser** builds
  **linux amd64 and arm64 only**, with checksums, and publishes the release.
- Version via `-ldflags`, with a `debug.ReadBuildInfo` fallback so `go install` builds
  report one. New command `agent-tmux version` (with `--json`).
- First tag: `v0.1.0`.
- Before choosing targets, confirm the stale-claim code (reads `/proc` for start time)
  is fine on both arches. It's Linux-only by design.

## 4. Work, in order

1. **Recorder:** `_record`, `create` flow, segments, rotation, `meta.json`, `events.jsonl`, stale finalization.
2. **Verbs:** `list`→`ls`, `ls --json`, `show` with auto-paging, `grep`, `rm` to XDG trash, `version`, completion for the new verbs.
3. **Indicator and menu:** `● REC`, `Ctrl-]` `[/] history`. Two-terminal test for anything touching attach.
4. **Release:** GoReleaser config, Actions workflow, `v0.1.0`.
5. **ws cleanup:** remove the old recorder and all `~/Sessions` traces; add the `exec` passthrough and completion delegation.
6. **ws TUI:** sessions page, grey/install state, version nudge.
7. **ws skills:** skill-provider list, `ws skill check`.

## 5. Open items

- Toggle key: keep `Ctrl-T` (collides with bash transpose and fzf), or move under `Ctrl-]`. Still open.
- `create` from a terminal drops into `open`.
- `@agent-tmux-generation` is unused.

## 6. How to test

- `make test` (sets `GOTMPDIR`; `/tmp` may be `noexec`). About 15 s, needs tmux.
- Never attach tmux to the harness's own terminal. Use a fresh pty (`openPTY` in
  `internal/termshare/manager_test.go`, `openTestPTY` in `cmd/agent-tmux/exclusive_test.go`)
  with a short `AGENT_TMUX_RUNTIME_DIR` (socket paths have a length limit).
- To check what a viewer renders, `.tmp/pylib` holds `pyte` (gitignored):
  `sys.path.insert(0, ".tmp/pylib")`.

## 7. Working with the user

- Wants taste, not churn. Explain in plain, simple words. Don't mix requirements together.
- Commit only when asked.
- Facts measured earlier (tmux 3.4): `attach -r` clients get no mouse events; `detach-client -E 'exit N'` fires from
  read-only clients; a `tmux attach` client gets redrawn screens, not every line (about
  250 of 5000), so the terminal's own scrollback can't work under it; a scripted
  `send-keys -l` into a pane in copy-mode hangs, so never put
  the shared pane into copy-mode for one viewer; tmux costs about 2 KB RAM per history
  line, so scrollback is a cache, not an audit store.
