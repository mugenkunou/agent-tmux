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

Q: Without `agent-tmux`, how do AI coding tools (Claude CLI, Copilot CLI, Copilot in
the IDE) run shell commands today?
A: None attach a human to a real shared PTY by default. Each tool call is typically a
fresh, non-PTY subprocess (or, at best, a backgrounded shell reused per session id for
cwd/env persistence, purely as harness bookkeeping — not a terminal). Consequences:
no controlling terminal, so TTY-only prompts (`sudo`, `ssh` password/TOTP) fail or
hang; no way for a human to watch or type into that exact process; an IDE's "run in
terminal" may open a real terminal tab, but it's a separate surface from what the
agent reads, so handoff means switching tabs/focus, not one shared pane. This is
precisely the gap `agent-tmux` fills.

## Requirements

Q: Must the human use special client software?
A: No — an ordinary terminal, attached via `agent-tmux open/watch/take`.

Q: Is a second terminal tab acceptable for handover?
A: No. Explicitly rejected mid-project. One tab, one hotkey (`Ctrl-T`) toggles
watch/control in place.

Q: Can the agent ever see the secret being typed?
A: No. Human keystroke → PTY directly, never through chat or a tool call.

Q: Does this hide *all* secrets from the agent forever (e.g. ones later printed by
commands)?
A: No — explicitly out of scope. It only keeps secret keystrokes out of the scripted
input channel at the moment of entry. Ordinary command output the agent legitimately
reads afterward (`kubectl get secret -o yaml`, `env`, a decoded config file) can still
contain secrets; that's not a bug or a gap this design tries to close.

Q: Must this be SSH-specific?
A: No — any interactive program (`ssh`, `sudo -i`, `gdb`, `vim`, `kubectl exec`,
`docker exec`, DB consoles, ...).

Q: Is "agent" vs "human" a first-class concept in the architecture?
A: No — generalized to "callers" with self-reported actor ids; only one caller holds
the write lock at a time. See Ownership Model.

Q: How many callers can write at once?
A: One. Any number can read/watch concurrently.

Q: Should every AI agent invocation be forced through `agent-tmux`, all the time?
A: No — explicitly rejected. The user wants **both**: normal, everyday agent
sessions keep running bash however that tool normally does (unrestricted, fast,
no ceremony), and a **separate, deliberately-invoked** agent persona (named
`investigator`, §10) is the one routed exclusively through `agent-tmux`, for the
specific case of higher-stakes work (e.g. prod SSH + k8s recon) where a human may
need to see/take over the terminal. This is a two-tier design, not a global policy
change — don't "improve" this by making the default agent path mandatory-tmux.

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
- **Recommended id shape**: `<tool>-<host>-<user>-<instance-token>` (e.g.
  `copilot-cli-devbox-siva-4f2a`), set once at the start of a run and reused for every
  call in that run. `tool` and `host` disambiguate *which* client is which when two
  similar clients (same tool, same host, same user) are active — a bare `$USER` or a
  bare tool name alone can't tell them apart. `instance-token` is a short id generated
  once per run (not regenerated per call, and never shared as a fixed value across
  concurrently running instances of the same tool) so concurrent instances don't
  collide on an identical id.
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
- Set `AGENT_TMUX_ACTOR_ID` once per run using the `<tool>-<host>-<user>-<instance-token>`
  shape from §6, so `status` can tell apart two similar clients.
- Use `run` for anything expected to finish; `send`/`key` for interactive/long-lived
  programs; `screen`/`wait` to observe.
- Check `status` before calling `send`/`run`/`key`; if `owner` isn't `(unclaimed)`,
  something else currently holds the write lock — don't send input, surface who owns
  it instead.
- On a stall, classify what's on screen before reacting, rather than treating every
  stall identically:
  - Credential/secret prompts (password/passphrase/TOTP/hardware-key) → **silently
    yield**: stop sending input, tell the user to run `agent-tmux open <session>`,
    have them press `Ctrl-T`, type the secret, press `Ctrl-T` again — never ask for the
    secret in chat or through any tool call, and never ask permission first since this
    is expected/procedural.
  - Consequential/destructive confirmations (`yes/no`, `[y/N]`, "are you sure",
    unfamiliar host-key trust prompts) or any unrecognized stall → **seek
    confirmation**: surface the exact prompt text to the user in chat and ask what to
    answer, rather than assuming it's safe to proceed or that it's a secret. A plain
    non-secret answer may be relayed via `send` if the user is comfortable with that;
    otherwise fall back to the same `open`+`Ctrl-T` handoff.

Beyond the skill (advisory, for any agent doing routine shell work), the `agents/`
directory in this repo ships ready-made, tool-specific **custom agent** definitions —
all named `investigator` — for dedicated, higher-stakes work (e.g. a human explicitly
invoking a production-incident investigator): `investigator.vscode.agent.md`,
`investigator.copilot-cli.agent.md`, `investigator.claude.md`. Each embeds the same
command-classification and prompt-handling rules as a self-contained agent persona
using that tool's own frontmatter schema and tool names, since VS Code/Copilot CLI
share one `.agent.md` schema (different tool namespaces) while Claude Code uses its
own `.claude/agents/*.md` format. See README.md's "Agent Integration" section for
install locations.

Q: How does someone actually install `SKILL.md` and the `investigator` agent, rather
than just having the source files sit in this repo?
A: `make install-skill` / `make install-agents` / `make install-integration` (both).
These `ln -sf` the files from this repo into each tool's real config location
(`~/.claude/skills/agent-tmux/SKILL.md`, `~/.copilot/skills/agent-tmux/SKILL.md`,
`~/.config/Code/User/prompts/investigator.agent.md`, `~/.copilot/agents/investigator.agent.md`,
`~/.claude/agents/investigator.md`) — symlinked, not copied, so a `git pull` here keeps
every installed copy current with no manual re-sync step. Both skill dirs
(`~/.claude/skills/`, `~/.copilot/skills/`) use the shared, cross-tool SKILL.md
standard's folder-per-skill layout (`<skills-dir>/agent-tmux/SKILL.md`), confirmed
against Claude Code's and GitHub Copilot's own skills documentation — this repo had no
install path documented for either the skill or the agents before this was added.

## 11. Enforcement Strength Ladder (how "hard" can we make agents use this?)

Getting an AI coding agent to actually route shell work through `agent-tmux`, instead
of quietly falling back to its own raw bash tool, is a spectrum from "polite
suggestion" to "technically impossible to bypass." Ranked weakest → strongest,
evaluated for this project:

1. **Plain instructions/prompt text** — cheapest, no enforcement; the agent can ignore
   it under context pressure.
2. **`SKILL.md`** (what this repo ships, §10) — model-invoked advisory guidance,
   loaded automatically when relevant. Stronger than raw instructions because it's a
   standardized, tool-recognized mechanism, but still advisory — nothing stops a plain
   bash tool call.
3. **Custom agent with a restricted tool list** (`investigator`, §10) — meaningfully
   stronger: if the agent persona's frontmatter only exposes `bash`/`execute` wired to
   `agent-tmux`-style usage (via its system prompt) and the user explicitly invokes
   that persona, there's a real behavioral nudge, but the underlying tool is still
   "run a shell command" — a sufficiently determined model can still shell out around
   the convention inside that same tool call.
4. **MCP server exposing `agent-tmux` operations as the only shell-shaped tool, with
   the raw bash/terminal tool denied at the harness level** — the strongest *portable*
   lever: if raw bash isn't offered to the model at all, there's nothing to bypass to.
   Not implemented here; identified as the correct next step if hard enforcement is
   ever required, since it works the same way across tools that support MCP.
5. **Deterministic `PATH` shim** (e.g. a `ssh`/`bash` wrapper script ahead of the real
   binaries in `PATH` that redirects into `agent-tmux`) — enforces at the OS level
   regardless of what the agent *thinks* it's doing, but it's a blunt, global
   trap that also affects the human's own shell and any non-agent tooling; not
   pursued.
6. **Harness `preToolUse` hooks** — confirmed (via official docs) to exist for
   **Claude Code** (`hooks.PreToolUse` in subagent frontmatter) and **Copilot CLI**
   (`.github/hooks/*.json` or `~/.copilot/hooks/*.json`), and can approve/deny a tool
   call before it runs — true hard enforcement, per-tool, without needing a PATH
   trap. **Confirmed absent for VS Code Copilot chat** (docs.github.com/en/copilot/
   concepts/agents/hooks scopes hooks to Copilot CLI and the Copilot cloud agent only)
   — VS Code would need option 4 (MCP + tool-deny) to reach equivalent strength.
   Identified as feasible and the strongest *tool-native* lever for two of the three
   tools, but **not implemented** — explicitly deferred, out of scope for what was
   asked so far.
7. **After-the-fact review** (diff/log auditing once work is done) — weakest as a
   *preventive* control, but cheap and composable with any of the above as a safety
   net.

Current state of this repo: level 2 (`SKILL.md`, advisory, any agent) + level 3
(`investigator` custom agent, explicit invocation only) are implemented. Levels 4 and
6 are the known, deliberate next steps if the trust model in §6 (self-reported actor
id, cooperative isolation) ever needs to become a hard boundary instead.

Q: Why ship both a skill (advisory) and a custom agent (restricted persona) instead of
picking one?
A: They solve different problems. The skill makes *any* agent session that happens to
need shell/SSH work aware of `agent-tmux` with zero setup. The `investigator` agent is
for the case where the user wants to *deliberately switch into* a constrained,
tmux-routed persona for a specific task (e.g. a prod incident) — that requires
explicit invocation and a distinct identity (§6's actor-id shape), which a skill alone
can't provide since a skill doesn't change which tools an agent is allowed to call.

Q: Why is the custom agent named `investigator` and not something new like
`prod-incident`?
A: The user already had a pre-existing VS Code custom agent named `investigator` (raw
bash/ssh-based, no `agent-tmux` awareness). Rather than introduce a second, similarly-
named agent, the old one was preserved as `investigator-archive` (`disable-model-
invocation: true`, kept for reference/rollback) and the name `investigator` was
reclaimed for the new `agent-tmux`-routed version — one canonical name per tool,
no ambiguity about which "investigator" a user is invoking.

Q: Where do the skill/agent schemas actually differ across the three tools, in
practice?
A: VS Code Copilot and Copilot CLI **share** the same `.agent.md` frontmatter schema
(`name`, `description`, `tools`, `model`, `target`, `disable-model-invocation`,
`user-invocable`) — they differ only in which tool names exist in that namespace
(VS Code: `execute/runInTerminal`, `execute/sendToTerminal`, etc.; CLI: plain `bash`).
Claude Code subagents use their own format (`.claude/agents/*.md`: `name`,
`description`, `tools`, `model`, optionally `hooks`). This is why the repo ships three
separate agent files (`agents/investigator.{vscode,copilot-cli}.agent.md`,
`agents/investigator.claude.md`) rather than one shared file.

## 12. How To Rebuild This, In Order

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
9. Ship the `investigator` custom agent per §10/§11 for each supported tool, and add
   symlink-based install targets (`make install-skill`/`install-agents`) so the repo
   stays the single source of truth instead of hand-copied files drifting per tool.

Everything else in the current codebase (`README.md`, tests, `Makefile`) is a faithful
implementation of the above and can be regenerated; this file is the part that can't.

Q: Has this actually been built and installed for real (not just `make -n` dry-run),
end to end?
A: Yes — `make build` (produces `bin/agent-tmux`) and `make install` (`go install
./cmd/agent-tmux` → `~/go/bin/agent-tmux`, already on `PATH`) were both run for real
on the dev machine; `agent-tmux doctor` and `--help` were used to confirm the
installed binary resolves and finds tmux/the socket correctly. `make install-skill`
and `make install-agents` were likewise run for real, not just dry-run-checked — the
live symlinks listed in §10 are the proof.
