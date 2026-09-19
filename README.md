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

## Build

```bash
make build
./bin/agent-tmux doctor
```

To put the binary on `PATH`:

```bash
make install
```

## Daily Workflow

A scripted caller creates one Bash session:

```bash
agent-tmux create work
```

Routine commands run synchronously and preserve shell state:

```bash
agent-tmux run work 'cd /srv/app'
agent-tmux run work 'git status --short'
```

Interactive commands are started without waiting for them to exit:

```bash
agent-tmux send work 'ssh deploy@prod'
agent-tmux screen --history 100 work
```

Someone can observe and take over from a real terminal, with a single key:

```bash
agent-tmux open work
```

`open` attaches once. It starts unclaimed: typed input does not reach the shell until someone claims it. Pressing `Ctrl-T` claims control instantly for whoever is attached; pressing `Ctrl-T` again releases it. `Ctrl-Q` ends `open` entirely and returns the terminal to a normal prompt, without killing the session — reattach later with `agent-tmux open work` again. No second tab and no detach chord to remember for routine handoffs.

```text
UNCLAIMED work            CONTROLLED BY <actor> work
Ctrl-T claims  ───────> Ctrl-T releases
        ▲                        │
        └────────────────────────┘
                Ctrl-Q (from either phase; press Ctrl-T
                first if currently unclaimed) exits open
```

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

If takeover is interrupted instead of cleanly detached:

```bash
agent-tmux yield work
```

Finish the session explicitly:

```bash
agent-tmux kill work
```

## Commands

| Command | Purpose |
| --- | --- |
| `create` | Start a persistent Bash session |
| `run` | Run a routine command and wait for its exit status |
| `send` | Send text, useful for interactive programs |
| `key` | Send keys such as `C-c`, `C-z`, or `Escape` |
| `screen` | Read the current rendered terminal |
| `wait` | Wait until expected text appears |
| `open` | Attach once; `Ctrl-T` toggles unclaimed/claimed in the same tab |
| `watch` | Attach a read-only terminal |
| `take` | Claim ownership and attach a writable terminal |
| `yield` | Clear the current owner |
| `status` / `list` | Inspect sessions and their current owner |
| `kill` | Terminate a session |

`run` does not cancel a command when its timeout expires. This is intentional: the process may be waiting for interactive input. Inspect it with `screen`, then use `open` or `take` when needed.

`open` currently needs one `Ctrl-T` before `Ctrl-Q` if the session is unclaimed, since tmux discards any bound command chain other than a bare `detach-client`/`switch-client` while a client is read-only — `Ctrl-Q` sets the quit flag as part of a chain, which only reliably fires once the client is writable.

## Ownership Model

There is no built-in notion of "agent" versus "human" — only whether a session is unclaimed or held by a specific actor:

- Any number of clients may read concurrently.
- `watch` is enforced read-only by tmux; `open` starts the same way, using tmux's native read-only client flag rather than a custom key table (an earlier key-table-swallow design was tried and empirically disproven: unbound keys were still forwarded to the pane).
- Only the current owner may write through `agent-tmux`; scripted `send`/`run`/`key` calls are refused while any actor holds ownership (`status` shows who).
- `take` and `open`'s control phase atomically set the owner to an actor id before attaching.
- Scripted writes attempted while someone else owns the session fail instead of being queued.
- `Ctrl-T` is bound once, server-wide, to `detach-client` — the one command tmux still honors for a read-only client. `open` alternates read-only and writable `attach-session` calls around that single detach point, claiming/releasing ownership on each transition.
- Detaching from `take`, or either phase of `open`, clears the owner back to unclaimed.

### Actor identity

Each caller is identified by an `actor id`, not a role:

- Set `AGENT_TMUX_ACTOR_ID` once per logical caller (a shell session, an automation process) so repeated `open`/`take` calls from it consistently show the same id in `status` and the on-screen banner.
- If unset, `open`/`take` generate a fresh id (`$USER-<random>`) per invocation — fine for one-off use, but it won't stay consistent across separate invocations from the same caller.
- The claimant's controlling tty is recorded alongside the actor id (`@agent-tmux-owner-tty`, best-effort) as corroborating, kernel-verified evidence next to the self-reported id.
- **This is accountability, not authentication.** `actor id` is self-reported; nothing stops any process reaching the socket from claiming any id, as noted below.

The tmux socket and lock files live in a mode `0700` per-user runtime directory. This is cooperative isolation, not a sandbox: another process running as the same Unix user can still invoke tmux directly, or claim ownership under any actor id it likes. Terminal output is not logged by `agent-tmux`.

## Persistence Boundary

The tmux server owns the local Bash PTY, so the session survives every caller disconnecting. It does not survive a local reboot or tmux server crash. An SSH connection inside the session still depends on the network and remote host.

## Development

```bash
make test
```

The integration tests launch real tmux sessions and verify persistent shell state, command exit codes, and ownership-based write exclusion.

### Secret scanning

A `pre-commit` hook runs [gitleaks](https://github.com/gitleaks/gitleaks) against staged changes and blocks the commit if it finds a likely secret. Enable it once per clone:

```bash
make hooks
```

Requires `gitleaks` on `PATH`; the hook config lives in `.gitleaks.toml`. If it's missing, the hook logs a warning and lets the commit through rather than failing closed.
