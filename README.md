# agent-tmux

`agent-tmux` gives any number of callers — scripts, automation, people at a keyboard — one shared, persistent Bash terminal, with a single owner allowed to write at a time.

```text
                          read-only output
                       ┌────────────────────> scripted caller
                       │
isolated tmux server ──┴── PTY ── bash ── ssh / sudo / gdb
                       │
                       └────────────────────> attached terminal
                                      view or claim ownership
```

Anyone attached can watch at any time, or temporarily claim the input lock to enter a password, TOTP, approval, or command directly. Secret input goes straight from the attached terminal to the PTY and is never passed through a scripted caller.

## Requirements

- Linux
- tmux
- Go 1.23 or newer to build

## Install

From a release (Linux amd64 and arm64): download the `.tar.gz` for your CPU from the
[releases page](https://github.com/mugenkunou/agent-tmux/releases), check it against
`checksums.txt`, and put `agent-tmux` on your `PATH`. Or with Go:

```bash
go install github.com/mugenkunou/agent-tmux/cmd/agent-tmux@latest
agent-tmux version
```

From a clone:

```bash
make install
agent-tmux doctor
```

This builds the binary, copies it into Go's install dir (`GOBIN`, else `GOPATH/bin`;
override with `make install BINDIR=~/.local/bin`), checks that `agent-tmux` on `PATH`
resolves to that copy, and installs shell completions. The skill and the `investigator`
agent are left to you (see [Agent Integration](#agent-integration)). Re-run it after a `git pull` to pick up
a new binary. Run `make` on its own to list every target.

To build without installing anything:

```bash
make build
./bin/agent-tmux doctor
```

## Shell Completion

`make install` also installs completions for bash, zsh and fish: commands, flags,
flag-value hints, **live session names** (with owner and running command shown
where the shell supports descriptions), and, for `show`, `rm` and `grep --session`,
recording ids and names too. To set it up by hand instead:

```bash
eval "$(agent-tmux completion bash)"          # ~/.bashrc
eval "$(agent-tmux completion zsh)"           # ~/.zshrc, after compinit
agent-tmux completion fish | source           # ~/.config/fish/config.fish
```

Completion never needs tmux to be running, and never prints errors into your prompt.

## Daily Workflow

A scripted caller creates one Bash session:

```bash
agent-tmux create work
```

Run from a real terminal, `create` also attaches immediately (identical to running
`open` right after) — the session is on screen the moment it exists. Scripted callers
(without a terminal) get the same behavior as before: the session is created detached
and a hint to run `open` is printed.

Interactive commands are started without waiting for them to exit:

```bash
agent-tmux send work 'ssh deploy@prod'
agent-tmux screen --history 100 work
```

Every command is sent the same way, whether it's a routine local command or one
running deep inside an SSH/sudo session: `send` the text, then `screen`/`wait` to see
what happened. There's no separate "run and wait for the exit code" fast path — see
[PROJECT.md §14](PROJECT.md#14-why-there-is-no-run-command) for why.

Someone can observe and take over from a real terminal, with a single key:

```bash
agent-tmux open work
```

`open` attaches once. It starts unclaimed: typed input does not reach the shell until someone claims it. Pressing `Ctrl-T` claims control instantly for whoever is attached, unless another terminal already holds it, in which case `Ctrl-T` is refused with a message naming the holder; pressing `Ctrl-T` again releases it. Only one terminal can be in control at a time, whatever mix of `open` and `take` is attached. While attached, the terminal tab/window title shows which session it is and who has input —
`👀 agent-tmux: work (watching)` or `⌨️ agent-tmux: work (<actor> controls)` — so the right
tab is easy to find, and the previous title is restored when you leave. This relies on your
terminal honoring title escape sequences (most do); the emoji need a font that renders them.

`Ctrl-Q` ends `open` entirely and returns the terminal to a normal prompt, without killing the session — reattach later with `agent-tmux open work` again. No second tab and no detach chord to remember for routine handoffs.

```text
UNCLAIMED work            CONTROLLED BY <actor> work
Ctrl-T claims  ───────> Ctrl-T releases
        ▲                        │
        └────────────────────────┘
      Ctrl-Q exits open, Ctrl-] opens the menu (both from either phase)
```

The mouse wheel doesn't scroll the session's output inside `open`: tmux ignores the mouse
for a watching terminal (see [PROJECT.md §9](PROJECT.md#9-known-limitations-deliberate-not-oversights)).
To read further back, press `Ctrl-]` then `/` (the whole recording in `less`), or run
`agent-tmux show work` from another terminal.

### When the session is stuck

`Ctrl-]` always gets you out, whether you are watching or in control and however
frozen the program in the session is (an ssh connection hung by a network drop, a
program ignoring Ctrl-C). It leaves tmux and shows a one-line menu:

```text
agent-tmux work: [d]etach  [k]ill session  [r]estart shell  [c] send Ctrl-C  [/] history  [any other key] back
```

- `d` leaves `open`; the session keeps running.
- `k` kills the whole session (asks first).
- `r` kills whatever runs in the session and starts a fresh shell in its place, keeping
  the session and its name (asks first). The same thing from another terminal:
  `agent-tmux restart work`.
- `c` sends Ctrl-C to the session, then returns to it.
- `/` shows the session's whole recording in `less` (`agent-tmux show`); quitting
  `less` returns to the session.

Any other key returns to the session. Control is always released before the menu
shows, so the agent is never locked out while you decide.

Closing the terminal tab while in control releases control too. If the `open` or
`take` process dies without releasing (killed, crashed), the claim is treated as stale
and cleared the next time a scripted caller sends input or anyone runs `status`.

When SSH requests a password or TOTP, whoever is watching presses `Ctrl-T`, types the secret, and presses `Ctrl-T` again. Scripted callers can resume in the same authenticated shell once ownership clears.

### Advanced: manual watch/take/yield

For scripting or edge cases, the original two-command flow is still available:

```bash
agent-tmux watch work
```

When SSH requests a password or TOTP, someone takes control:

```bash
agent-tmux take work
```

Type the secret, then press `Ctrl+B`, `D` to detach. Ownership automatically clears, and scripted callers continue in the same authenticated SSH shell.

```text
scripted caller writes ──> password prompt ──> take claims ──> authenticates
     ▲                                                              │
     └────────────── same shell, same SSH connection <─────────────┘
```

If takeover is interrupted instead of cleanly detached (only needed when the claiming
process is somehow still alive, since a dead claimant's claim is cleared automatically):

```bash
agent-tmux yield work
```

Finish the session explicitly:

```bash
agent-tmux kill work
```

## Recording

Every session is recorded, from its first byte (login banners included) to `kill`.
There is no flag and no opt-out. What is recorded is the pane's **output**, exactly as
the terminal received it. Input is never recorded on its own: what you type shows up
only where the terminal echoed it, so passwords and TOTP codes (which don't echo) stay
out. `send`, `key`, `claim`, `release`, attach and detach are logged as events, without
the text sent.

Recordings live in `~/.local/state/agent-tmux/` (`$XDG_STATE_HOME/agent-tmux` if set;
`agent-tmux doctor` prints it), one directory per session, named
`<name>-<YYYY-MM-DD-HHMM>` (`-2`, `-3` ... on a collision). That name is the recording's
**id**. Directories are `0700`, files `0600`:

```text
web-2026-10-29-1402/
  meta.json              name, id, host, started_at, ended_at, first_segment, first_segment_at
  stdout.000001.log      raw output, byte-exact
  stdout.000001.timing   "<delay> <bytes>" per write (scriptreplay format)
  events.jsonl           create, attach, detach, claim, release, send, key, respawn, kill, rotated, recorder-died
```

A recording outlives its session: `kill` ends the shell and finishes `meta.json`, the
files stay. Output goes to 64 MB segments; once more than 8 exist the oldest is deleted
(about 512 MB per session at most). Each rotation is logged, and `show` then starts with
`-- history starts at <time>; older output was rotated away --`.

The status bar and tab title show `● REC` while the recorder runs and `REC STOPPED` if
it died (`status`, `ls` and `doctor` say so too; the session keeps working, unrecorded).

```bash
agent-tmux ls                      # live sessions and recordings
agent-tmux show work               # whole recording; less on a terminal, plain text when piped
agent-tmux show --raw work > out   # the exact bytes, escape sequences and all
agent-tmux grep 'connection refused'
agent-tmux rm --older-than 30d     # move ended recordings to the trash
```

`show`, `rm` and `grep --session` take a live session name, a recording id, or a
session name, which means its newest recording (for `rm`, every ended recording with
that name). `rm` moves recordings to the XDG trash (`~/.local/share/Trash`) and refuses
a live session's recording: kill the session first.

Known limits: full-screen programs (vim, htop) and progress bars come out as redraw
noise in `show` and `grep`, since those print plain text; the raw log with
`scriptreplay` is faithful
(`scriptreplay -t stdout.000001.timing stdout.000001.log` in the recording's directory). A recording holds whatever commands printed, secrets
included if a command printed them. Files are `0600` and nothing syncs them anywhere.

### JSON output

`ls --json` (live sessions by name, then ended recordings, newest first; every field
always present, zero values when it does not apply; `{"sessions":[]}` when empty):

```json
{"sessions":[
  {"name":"web","id":"web-2026-10-29-1402","state":"live","recorder":"running",
   "owner":"","clients":1,"command":"bash","path":"/home/u/src","host":"box",
   "started_at":"2026-10-29T08:32:11Z","ended_at":"","history_starts_at":"2026-10-29T08:32:11Z",
   "bytes":1048576}
]}
```

`state` is `live` or `ended`. `recorder` is `running`, `stopped` (its recorder died),
`none` (a session made by an older version, not recorded) or `""` for an ended
recording. Times are RFC3339 UTC, `""` when unknown; `bytes` is the size of the kept
segments.

`grep --json` (exit 0 with or without matches; plain `grep` exits 1 with none):

```json
{"matches":[{"name":"web","id":"web-2026-10-29-1402","line":1234,"text":"..."}],"truncated":false}
```

`line` is the line number in the plain text that `show` prints for that recording.

`version --json`:

```json
{"version":"v0.1.0","commit":"abc1234","date":"2026-10-29T10:00:00Z"}
```

`version --check` always asks GitHub (3s timeout, no cache) for the latest release and
compares it with this binary. Anything that is not a clean `vX.Y.Z` release (`dev`, a
`git describe` string, a pre-release) is a dev build and is never reported as outdated.
It exits 0 for every status; a failed lookup is status `unknown`.

```text
agent-tmux v0.19.0 (latest v0.19.0)
agent-tmux v0.19.0: update available, latest is v0.20.0
agent-tmux dev: local build is a dev build; upstream release is v0.20.0
agent-tmux v0.19.0: could not check latest release: <error>
```

`version --check --json` prints one object: `version` (installed), `latest` (`""` when
unknown), `status` (`current`, `outdated`, `dev` or `unknown`) and `error` (only with
`unknown`):

```json
{"version":"v0.19.0","latest":"v0.20.0","status":"outdated"}
{"version":"v0.19.0","latest":"","status":"unknown","error":"GitHub returned 500 Internal Server Error"}
```

## Commands

| Command | Purpose |
| --- | --- |
| `create` | Start a persistent Bash session (50000 lines of scrollback, `--history` to change); from a terminal, attaches immediately (same as `open`) |
| `send` | Send text, useful for routine commands and interactive programs alike |
| `key` | Send keys such as `C-c`, `C-z`, or `Escape` |
| `screen` | Read the current rendered terminal |
| `wait` | Wait until expected text appears |
| `open` | Attach once; `Ctrl-T` toggles unclaimed/claimed, `Ctrl-Q` exits, `Ctrl-]` opens the menu |
| `watch` | Attach a read-only terminal |
| `take` | Claim ownership and attach a writable terminal |
| `yield` | Take control away from whoever holds it and put their terminal back to watching |
| `ls` | List live sessions and recordings (`--json` for scripts) |
| `status` | Inspect one session: owner, clients, command, path, recording, recorder |
| `show` | Print a session's whole recording; pages with `less` on a terminal, plain text when piped, `--raw` for the exact bytes |
| `grep` | Search recordings, newest first (`--json`, `--limit`, `--session`) |
| `rm` | Move ended recordings to the trash (by id or name, or `--older-than 30d`) |
| `restart` | Kill whatever runs in a session (e.g. a frozen ssh) and start a fresh shell in its place |
| `kill` | Terminate a session (its recording stays) |
| `doctor` | Check tmux, show the socket and recordings paths and each session's recorder |
| `completion` | Print the shell completion script for `bash`, `zsh` or `fish` |
| `skill` | Print `SKILL.md`, matching this binary, for a skill manager to keep |
| `version` | Print the version, commit and build date (`--json` for scripts); `--check` asks GitHub for the latest release |

## Agent Integration

`SKILL.md` teaches an AI agent to route ordinary shell work through `agent-tmux`. For
dedicated, higher-stakes work (production incident investigation over SSH/sudo/kubectl,
where every command should be classified and secrets/approvals always hand off to a
human) the `agents/` directory ships a ready-made custom agent named `investigator`,
one file per tool, since each has its own schema.

`make install` installs neither. Have the
installed binary print the skill into your skills folder (or your skill manager's), the
same way `completion` prints its scripts:

```bash
mkdir -p ~/.claude/skills/agent-tmux
agent-tmux skill > ~/.claude/skills/agent-tmux/SKILL.md
```

The copy matches the binary on `PATH`; run it again after each `make install-bin`.

To link the files from this repo instead, without touching the binary:

```bash
make install-integration   # SKILL.md + the investigator agent, all three tools
make install-skill         # SKILL.md only
make install-agents        # investigator agent only
```

These symlink files from this repo into place, so `git pull` here keeps the installed
copies current — nothing is duplicated by hand. Re-running any of them repoints
existing links at this checkout, which also fixes links left dangling by a moved clone.

| Content | File | Installed to |
| --- | --- | --- |
| Skill | `SKILL.md` | `~/.claude/skills/agent-tmux/SKILL.md` (Claude Code) and `~/.copilot/skills/agent-tmux/SKILL.md` (Copilot CLI) |
| Agent | `agents/investigator.vscode.agent.md` | `~/.config/Code/User/prompts/investigator.agent.md` (GitHub Copilot in VS Code) |
| Agent | `agents/investigator.copilot-cli.agent.md` | `~/.copilot/agents/investigator.agent.md` (GitHub Copilot CLI) |
| Agent | `agents/investigator.claude.md` | `~/.claude/agents/investigator.md` (Claude Code) |

Both skill and agent installs are user-level (apply across every repo you work in),
mirroring `AGENT_TMUX_ACTOR_ID`'s "set once per logical caller" model. Repo-level
placement (`.github/skills/`, `.github/agents/`, `.claude/skills/`, `.claude/agents/`)
is also supported by these tools if you'd rather scope either to one project — copy
the same files there manually if so.

VS Code Copilot's own skill-discovery path (as opposed to custom agents, which are
handled by `install-agents` above) is still settling upstream at the time of writing;
if it doesn't pick up `SKILL.md` from a shared Copilot location automatically, check
VS Code's Skills UI/settings for the current expected directory.

Every install target defines `name: investigator` (or `name: "investigator"`,
depending on the tool's YAML style) — none of these files claim any other name, so
they won't collide with a differently named custom agent you already have.

## Ownership Model

There is no built-in notion of "agent" versus "human" — only whether a session is unclaimed or held by a specific actor:

- Any number of clients may read concurrently.
- `watch` is enforced read-only by tmux; `open` starts the same way, using tmux's native read-only client flag rather than a custom key table (an earlier key-table-swallow design was tried and empirically disproven: unbound keys were still forwarded to the pane).
- Only the current owner may write through `agent-tmux`; scripted `send`/`key` calls are refused while any actor holds ownership (`status` shows who).
- `take` and `open`'s control phase set the owner to an actor id before attaching, under the session lock. A claim is refused (`ErrControlled`) while another live process holds it, so two attached terminals can never both be writable. Releasing only clears a claim this process holds.
- `yield` is the override: it clears the owner and detaches every writable client, so an `open` in control falls back to watching.
- Scripted writes attempted while someone else owns the session fail instead of being queued.
- `Ctrl-T`, `Ctrl-Q` and `Ctrl-]` are bound server-wide to `detach-client -E 'exit N'`, a different N per key. `detach-client` is the one command tmux still honors for a read-only client, and the client's exit code tells `open` which key was pressed. `open` alternates read-only and writable `attach-session` calls around those detach points, claiming/releasing ownership on each transition.
- Detaching from `take`, or either phase of `open`, clears the owner back to unclaimed. So does closing the tab: `open`/`take` survive the hangup long enough to release.
- A claim records the claiming process (pid and start time). If that process is gone, the claim is stale: `send`/`key` clear it instead of refusing, and `status`/`ls` show the session as unclaimed.

### Actor identity

Each caller is identified by an `actor id`, not a role:

- Set `AGENT_TMUX_ACTOR_ID` once per logical caller (a shell session, an automation process) so repeated `open`/`take` calls from it consistently show the same id in `status` and the on-screen banner.
- If unset, `open`/`take` generate a fresh id (`$USER-<random>`) per invocation — fine for one-off use, but it won't stay consistent across separate invocations from the same caller.
- The claimant's controlling tty is recorded alongside the actor id (`@agent-tmux-owner-tty`, best-effort) as corroborating, kernel-verified evidence next to the self-reported id.
- **This is accountability, not authentication.** `actor id` is self-reported; nothing stops any process reaching the socket from claiming any id, as noted below.

The tmux socket and lock files live in a mode `0700` per-user runtime directory. This is cooperative isolation, not a sandbox: another process running as the same Unix user can still invoke tmux directly, or claim ownership under any actor id it likes. Terminal output is recorded (see [Recording](#recording)), readable only by your Unix user.

## Persistence Boundary

The tmux server owns the local Bash PTY, so the session survives every caller disconnecting. It does not survive a local reboot or tmux server crash. An SSH connection inside the session still depends on the network and remote host.

## Development

```bash
make test
```

### Changing commands, flags or arguments

`cmd/agent-tmux/commands.go` is the single definition of the CLI. Dispatch, flag
parsing, argument-count validation, `help` output and shell completion are all
generated from it, so there is nothing else to update: add or edit a command's
constructor and everything follows. Never hardcode command, flag or session names in
the completion scripts. Tests enforce this (`registry_test.go`, `complete_test.go`):
flags must have a description and name their value in backquotes, positionals declare
what they hold (`argSession` completes live sessions, `argRecording` recordings too), and the scripts must remain
dumb relays to `agent-tmux __complete`.

The integration tests launch real tmux sessions and verify persistent shell state and ownership-based write exclusion.

### Secret scanning

A `pre-commit` hook runs [gitleaks](https://github.com/gitleaks/gitleaks) against staged changes and blocks the commit if it finds a likely secret. Enable it once per clone:

```bash
make hooks
```

Requires `gitleaks` on `PATH`; the hook config lives in `.gitleaks.toml`. If it's missing, the hook logs a warning and lets the commit through rather than failing closed.
