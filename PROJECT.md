# agent-tmux — Project Memory (Q&A)

> If this repo burns down, take this file. Code is disposable; this isn't.
> Format: question, answer. Some answers are **TBD** — left open on purpose.

## Problem

Q: What problem does this tool solve?
A: An AI agent runs a persistent Bash session and needs to run `ssh`/`sudo`/etc.
When those need interactive input (password, TOTP), the agent must not see it — a
human must type it into the *same* terminal, *same tab*, then hand control back with
one keystroke.

Q: Why not just have the agent run `ssh user@host "cmd"` per command?
A: Loses shell state (cwd, env, an authenticated session) between commands, and gives
no way to supply interactive input at all.

## Requirements

Q: Must the human use special client software?
A: No — an ordinary terminal, attached via `agent-tmux open/watch/take`.

Q: Is a second terminal tab acceptable for handover?
A: No. Explicitly rejected mid-project. One tab, one hotkey (`Ctrl-T`) toggles
watch/control in place.

Q: Can the agent ever see the secret being typed?
A: No. Human keystroke → PTY directly, never through chat or a tool call.

Q: Must this be SSH-specific?
A: No — any interactive program (`ssh`, `sudo -i`, `gdb`, `vim`, `kubectl exec`,
`docker exec`, DB consoles, ...).

Q: Is "agent" vs "human" a first-class concept in the architecture?
A: No — generalized to "callers" with self-reported actor ids; only one caller holds
the write lock at a time. See Ownership Model.

Q: How many callers can write at once?
A: One. Any number can read/watch concurrently.

## Rejected Architectures

Q: Why not a custom PTY broker (`openpty` + a VT100 emulator)?
A: Correct in theory, too much code for the problem — tmux already gives PTY
ownership, multi-client attach, and reattachment. Revisit only if tmux becomes a real
limit.

Q: Why not SSH `ControlMaster`?
A: Only reuses transport/auth; each channel is still an independent shell, not a
shared pty.

Q: Why not remote tmux/screen on the target host?
A: Solves *remote* SSH-drop resilience, not the actual (local) problem, and requires
tmux on every remote host. Kept as a separate, optional concern, not the core design.

Q: Why not just use Upterm/tmate?
A: Good at sharing a terminal; neither arbitrates a single writer between a scripted
caller and an interactive one — would need to build that layer on top anyway.

Q: Does a custom tmux key-table ("swallow every key but one") block typing?
A: **No — empirically disproven.** Built and tested: unbound keys in a switched-to
table were still forwarded to the pane. Do not retry without new evidence.

Q: What actually blocks typing, then?
A: tmux's real `attach -r` (read-only) flag. Confirmed empirically — bytes never
reach the pane.

## Hard tmux Facts (grounded, not assumed)

Q: While a client is read-only, which bound commands still fire?
A: Only `detach-client` and `switch-client` — as a whole binding. A chain like
`set-option ...; detach-client` does **nothing** while read-only; it only fires once
writable.

Q: Why did the colored status banner render as nothing?
A: A comma inside a `#[...]` style tag (`#[bg=colour2,fg=black]`) is
indistinguishable from the outer `#{?cond,true,false}` separators — drops the whole
banner, no error. Fix: one attribute per tag (`#[bg=colour2]#[fg=black]`).

Q: Can you preview a rendered status line with `display-message -F`?
A: No — it doesn't recursively re-expand nested `#{...}` inside a stored option's
value. Must attach a real client and read its raw output bytes.

Q: Does `capture-pane` show the status line?
A: Never — it only captures the pane grid, not per-client status rendering.

Q: Does `-t '=name'` (exact-match target) work?
A: Not on tmux 3.4 — "no such session." Plain `-t name` worked.

Q: How do you pass a `;`-chained command to `bind-key`?
A: As ONE argv string (`"set-option ... ; detach-client"`), not separate argv
elements — otherwise the bind fails outright ("no current client").

Q: Do `#{session_name}`-style formats expand inside a bound command?
A: Yes, at keypress time, resolved to whichever session the pressing client is on —
this is what lets one global (`-n`) binding work correctly across every session on the
shared server.

## Working Architecture

Q: What is the terminal engine?
A: One private tmux server per user, its own socket
(`$XDG_RUNTIME_DIR/agent-tmux/tmux.sock`, dir mode `0700`), separate from the user's
normal tmux.

Q: Does the agent hold a live tmux client?
A: No. `send`/`run`/`key`/`screen`/`wait` are one-shot `tmux` CLI invocations that
exit immediately. No persistent "agent" client exists.

Q: What does `run` do to get a synchronous exit code out of an async terminal?
A: Injects `eval '<cmd>'; printf '\n__AGENT_TMUX_DONE_<token>:%s\n' "$?"`, polls
`capture-pane` for that marker, parses the code, strips the marker from returned text.

Q: How does `open` give one-tab, one-key handoff?
A: Loop: `attach -r` (blocks typing) → `Ctrl-T` → `detach-client` fires (always
honored) → wrapper claims ownership → `attach` writable → user types → `Ctrl-T` again
→ detach → wrapper releases ownership → loop back to read-only.

Q: What are the two global keybindings, exactly?
A:

```text
bind-key -n C-t detach-client
bind-key -n C-q "set-option -t '#{session_name}' @agent-tmux-quit 1 ; detach-client"
```

- `Ctrl-T` is the toggle. It is *always* just `detach-client` — nothing else — which is
  exactly why it works identically whether the client is currently read-only or
  writable (§4.2).
- `Ctrl-Q` requests the loop actually end (return to a normal shell prompt, without
  killing the session). Because it's a *chain*, it is **only reliably honored while the
  client is writable** (§4.2) — from the read-only/unclaimed phase you must press
  `Ctrl-T` once first, then `Ctrl-Q`. This is a known, documented, accepted limitation,
  not a bug to "fix" without solving the underlying tmux restriction in §4.2.
- The Go wrapper checks the `@agent-tmux-quit` flag after every phase and exits the
  loop (clearing the flag) when it's set.

## 6. The Ownership / Identity Model

No "agent" or "human" role is hardcoded anywhere. There are only **callers**, some of
which briefly hold an ownership claim.

- **`@agent-tmux-owner`** (tmux session option): empty string = **unclaimed** (any
  scripted `send`/`run`/`key` call is allowed). Non-empty = the **actor id** of whoever
  currently holds the write lock; all scripted calls are refused
  (`ErrControlled`, wrapping the actor id in the error message) until it clears.
- **Actor id** (`actorID()`): read from `AGENT_TMUX_ACTOR_ID` if the caller set it
  (recommended — set once per logical caller, e.g. once per shell session or once per
  automation process, so repeated `open`/`take` calls consistently show the same id).
  If unset, a fresh `$USER-<8 random hex chars>` is generated **per process** — fine for
  one-off use, but won't be stable across separate invocations.
- **`@agent-tmux-owner-tty`**: best-effort, kernel-verified corroborating evidence
  (`/proc/self/fd/0` symlink target) recorded alongside the self-reported actor id, for
  an audit trail. Not used for enforcement, just visibility.
- **`@agent-tmux-generation`**: monotonically incremented on every claim/release.
  Reserved for future staleness detection; not currently checked by anything.
- **Trust model, stated explicitly:** this is *cooperative isolation, not a sandbox*.
  The socket directory is mode `0700` (same Unix user only) — that is the entire
  enforcement boundary. Actor id is **self-reported**; nothing stops any process that
  can reach the socket from claiming any id, or from bypassing `agent-tmux` and calling
  `tmux` directly against the same socket. If you need real authentication/authorization
  between genuinely different parties, that is out of scope for this design and would
  need a different security model entirely (separate OS users, capability tokens, or
  similar) — treat that as a deliberate non-goal, not an oversight.

### Data available to differentiate callers (why actor id was chosen)

| Signal | Trust | Available for scripted (no-tty) calls? | Stable across many calls? |
| --- | --- | --- | --- |
| `client_pid` / `client_tty` (tmux, only for a live attach) | kernel-verified | No | Only while attached |
| OS `pid` | kernel-verified | Yes | No — new every invocation |
| OS `uid` | kernel-verified | Yes | Useless — identical for every caller on this socket |
| Self-issued `actor_id` (env var or generated) | self-reported | Yes | Yes, if the caller sets `AGENT_TMUX_ACTOR_ID` once |

`actor_id` won on being the only thing that is both universal (works for one-shot calls
with no controlling terminal) and stable across repeated calls from the same logical
caller. `client_tty` is kept as corroborating evidence precisely because it's the more
trustworthy of the two, even though it can't stand alone.

## 7. Status Bar (visible ownership, no separate command needed)

```text
status-left:  " agent-tmux:#S "          (length 40, so the session name isn't truncated)
status-right: " #{?#{==:#{@agent-tmux-owner},},
                  #[bg=colour4]#[fg=black] OPEN - Ctrl-T takes control #[default],
                  #[bg=colour2]#[fg=black] CONTROLLED BY #{@agent-tmux-owner} - Ctrl-T releases #[default]} "
              (length 60)
```

Verified live (via an isolated pty, not just a syntax check — see §8) to render as:

```text
OPEN - Ctrl-T takes control
CONTROLLED BY alice-9f2c - Ctrl-T releases
```

Remember the comma rule from §4.4: every `#[...]` tag above carries exactly one
attribute. Do not "simplify" this by combining `bg`/`fg` into one tag with a comma —
that exact change is what silently breaks it.

## 8. Testing Technique That Actually Works (and dev-box gotchas)

- **Never drive a tmux-attach test through the harness's own controlling terminal**
  (e.g. via `script(1)` without isolating it). tmux's alt-screen switching bleeds into
  and corrupts the harness's own display, producing useless/garbled output. Instead:
  spawn the attach process with `stdin`/`stdout`/`stderr` wired to a **freshly opened
  pty** (`pty.openpty()` in Python, or the Go equivalent), write raw bytes to the master
  fd to simulate keystrokes, and read from the master fd to observe output. Never
  render it to a real terminal. Always explicitly set a realistic window size via
  `TIOCSWINSZ` — a default/unset pty size can hide real rendering behavior (this is
  exactly why the status-right bug in §4.4 wasn't caught until a properly-sized pty was
  used).
- **`/tmp` may be mounted `noexec`** on the dev box. `go test`/`go build` will fail with
  "permission denied" if their temp binaries land there. Fix: set `GOTMPDIR` to a
  workspace-local directory (the `Makefile` does this for `make build`/`make test`).
- **Deeply nested temp directories break the tmux Unix socket path.** `mktemp -d`
  chains that end up very long will make tmux fail with "File name too long" when
  it tries to bind the socket (Unix domain socket paths are capped around 100+ bytes
  on Linux). Use short-lived, short-named temp directories specifically for anything
  that becomes a tmux socket path (see `shortRuntimeDir` in the test file).

## 9. Known Limitations (deliberate, not oversights)

- `Ctrl-Q` (exit `open`) only reliably fires from the writable/claimed phase (§4.2, §5).
  There is no fix for this without a different tmux-level primitive; don't attempt the
  key-table workaround again (§3).
- No real authentication — actor ids are self-reported (§6). Fine for one trusted user
  on one machine; not fine as a multi-tenant boundary.
- Session-level, not pane-level, ownership: one writer for the whole session, not
  independent ownership per pane/window.
- No persisted audit log file — only live tmux options (`@agent-tmux-owner`,
  `@agent-tmux-owner-tty`) that vanish when the session ends. Would be a good next step
  if audit history across sessions is ever needed.
- No remote/relay attachment — this is strictly local, same-machine, same Unix user.
  Remote sharing (à la Upterm/tmate) was deliberately out of scope.
- Persistence boundary: the session survives any client disconnecting, but not a local
  reboot or a tmux server crash. An `ssh` session *inside* it still depends on the
  network and the remote host as usual.

## 10. The Agent-Facing Skill

A companion `SKILL.md` teaches an AI agent to route **all** shell work through
`agent-tmux` instead of ad hoc `ssh`/shell tool calls:

- Assume `agent-tmux` is already installed and on `PATH`. **Never build it** from the
  skill (this was an explicit correction — the skill used to try `make build`).
- Use `run` for anything expected to finish; `send`/`key` for interactive/long-lived
  programs; `screen`/`wait` to observe.
- On any password/passphrase/TOTP/hardware-key/approval prompt: stop sending input
  immediately, tell the user to run `agent-tmux open <session>`, have them press
  `Ctrl-T`, type the secret, press `Ctrl-T` again — never ask for the secret in chat or
  through any tool call.
- Check `status` before calling `send`/`run`/`key`; if `owner` isn't `(unclaimed)`,
  something else currently holds the write lock — don't send input, surface who owns
  it instead.

## 11. How To Rebuild This, In Order

If you're starting from zero with only this file:

1. Confirm `tmux` is installed; build a thin Go (or any language) CLI that always
   invokes `tmux` against one dedicated, mode-`0700` private socket.
2. Implement `create` (new-session + the option defaults in §6/§7), `screen`,
   `send`/`key` (fire-and-forget), and `run` (the completion-marker-and-poll trick in
   §5) first — these don't require any attach/detach logic.
3. Implement the ownership primitives (`claim`/`release`, the `@agent-tmux-owner*`
   options, `withAgentControl` refusing writes while claimed) before touching any
   attach logic.
4. Implement `watch` (`attach -r`) and `take` (claim → attach writable → release on
   exit, however it exits) — verify with the isolated-pty technique in §8 that
   read-only truly blocks typing before building anything on top of it.
5. Install the two global keybindings from §5 exactly as written (`Ctrl-T` = bare
   `detach-client`; `Ctrl-Q` = the quit-flag chain) — verify each empirically with an
   isolated pty per §8 before assuming either works, especially under read-only.
6. Implement `open`'s loop on top of steps 4–5.
7. Add the status bar from §7, respecting the one-attribute-per-style-tag rule (§4.4),
   and verify the *rendered* output with a real attached client, not `display-message`.
8. Write the agent-facing skill per §10, pointing at an already-installed binary.

Everything else in the current codebase (`README.md`, tests, `Makefile`) is a faithful
implementation of the above and can be regenerated; this file is the part that can't.
