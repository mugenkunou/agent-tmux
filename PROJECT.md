# agent-tmux — Project Memory (Q&A)

> If this repo burns down, take this file. Code is disposable; this isn't.
> Format: question, answer. Some answers are **TBD** — left open on purpose.

## Problem

Q: What is this tool for, fundamentally?
A: Letting an AI agent and a human **co-work on the same remote-debugging session** —
production incidents, SSH'd-into boxes, `sudo -i` shells, `kubectl exec`, interactive
debuggers — not just running commands in a local terminal. Local, non-interactive
command execution is the easy sub-case any subprocess call already handles; the
reason this project exists is the case where the process behind the pane is
frequently a *nested* remote shell, and a human may need to see or take over that
exact shell at any moment. Any feature that only works for the local top-level shell
and silently breaks once you're nested (SSH, sudo, a debugger) is failing at the
actual point of this tool — see §14 for a concrete instance of that mistake and why
it was reverted.

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
A: No. `send`/`key`/`screen`/`wait` are one-shot `tmux` CLI invocations that
exit immediately. No persistent "agent" client exists.

Q: Is there a synchronous "run and get the exit code" call?
A: No — deliberately removed. See §14 for the full history (there used to be a
`run` command backed by a `PS1` marker) and why every caller now uses
`send` + `screen`/`wait` uniformly instead, local or nested.

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
  scripted `send`/`key` call is allowed). Non-empty = the **actor id** of whoever
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
- Use `send` for every command — routine or interactive/long-lived — followed by
  `screen`/`wait` to observe the result; there is no separate "run and get the exit
  code" call (§14).
- Check `status` before calling `send`/`key`; if `owner` isn't `(unclaimed)`,
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

Q: While waiting for a human to finish typing a secret, should the agent keep calling
`agent-tmux status` in a loop to detect release?
A: No — explicitly corrected. Looping `status` calls burns turns/tool calls for no
benefit (the human isn't necessarily fast, and `send`/`key` are refused the
whole time regardless). Both `SKILL.md` and the `investigator` agents now instruct:
stop and **end the turn** once yielding, and only check `status`/`screen` again after
the user explicitly says they're done (e.g. "done", "continue") — the user's message
is the resume trigger, not a background poll.

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
   `send`/`key` (fire-and-forget), and `wait` (poll `capture-pane` for expected text)
   first — these don't require any attach/detach logic. Do not implement a `run`
   command backed by a PS1/prompt marker; see §14 for why that path is closed.
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

## 13. Completion Detection Evolution (why the sentinel was replaced)

Q: What was wrong with the original sentinel approach in shared sessions?
A: The `__AGENT_TMUX_DONE_<token>:0` marker is printed to stdout and appears verbatim
on screen. When a human watches the shared terminal (the whole point of agent-tmux),
they see noise like `eval 'hostname'; __agent_tmux_status=$?; printf ...` on every
command. In environments with a monitoring team watching SSH sessions, this looks
suspicious and unprofessional. The root cause: tmux gives only two primitives —
`send-keys` (write keystrokes) and `capture-pane` (read the screen). The screen is
the only return channel. Any completion signal must either appear on screen (noisy) or
travel on a separate channel.

Q: Why not write the exit code to a temp file on the remote instead of printing it?
A: Works for local sessions — agent-tmux and the shell share the same filesystem, so
polling a file in `/tmp` is trivial. Rejected for SSH sessions: the file lives on the
**remote** machine; agent-tmux runs on the **local** machine. Reading it requires a
separate SSH call per 100 ms poll, trading terminal noise for slow, network-dependent
polling. Also rejected on policy grounds: writing temp files to a remote production
server is not acceptable in monitored environments.

Q: What other out-of-band channels were considered?
A:
- **Window title** (`\033]0;RC:$?\007` OSC sequence, read via `tmux display-message
  "#{pane_title}"`): truly invisible — signal never enters the scrollable screen area.
  Requires `PROMPT_COMMAND` or PS1 modification. Cleanest option technically; rejected
  because `PROMPT_COMMAND` modifications are flagged by security monitoring tools on
  corporate machines.
- **Zero-width Unicode / Private Use Area chars in PS1**: invisible to the human,
  present in `capture-pane` byte stream. Fragile across terminal emulators and
  monitoring tools that strip non-ASCII. Not pursued.
- **Prompt detection via existing PS1**: simply watch for the shell prompt to
  reappear. Rejected in isolation because the prompt varies per machine and user —
  agent-tmux has no way to know what pattern to look for without controlling PS1.

Q: What constraint made the line-count anchor approach fail?
A: `capture-pane -S -N` returns exactly `N + terminal_height` lines, padded with blank
lines to fill the terminal. `anchorLines = strings.Count(screenBefore, "\n")` is
therefore always near the maximum line count. After the command runs, the new prompt
appears at a line index **below** `anchorLines` (because blank padding lines absorb
new content). The search starting at `lines[anchorLines:]` reliably skips the prompt
we are looking for. This was confirmed empirically — the screen output showed `[RC:0]`
clearly present, but the poll loop timed out anyway.

Q: What is the current detection mechanism and why does it work?
A: `setupPS1` (called once in `Create`) sends:
```bash
_ATSEQ=0; export PS1='[RC:$?:$((_ATSEQ++))]'"${PS1}"
```
`_ATSEQ` starts at 0 and increments on every prompt draw. For each `RunCommand` call:
1. Capture screen → find last `[RC:X:Y]` → record `seqBefore = Y`.
2. Send the raw command (no wrapper at all).
3. Poll `capture-pane` every 100 ms → find last `[RC:X':Y']` → when `Y' > seqBefore`,
   command is done and `X'` is the exit code.
The seq counter bypasses position entirely. Even if the terminal scrolls or the exit
code and working directory are identical to the previous command, `Y'` is strictly
greater — the new prompt is always distinguishable from the old one.

Q: Why is `$((_ATSEQ++))` inside PS1 reliable?
A: Bash evaluates `$((expr))` arithmetic expansions inside PS1 at each prompt draw —
confirmed empirically. `_ATSEQ++` is post-increment: the current value is returned and
the variable is incremented for next time. Starting at 0, prompt 0 shows `0`, prompt 1
shows `1`, etc. Works in any bash version that supports arithmetic expansion (all
modern versions).

Q: Why modify PS1 rather than use PROMPT_COMMAND for the same effect?
A: `PROMPT_COMMAND` is a well-known bash hook for running code after every command.
Security monitoring tools on corporate machines explicitly watch for and flag
modifications to it. Setting `PS1` is what every developer does (custom prompts are
universal); no monitoring tool flags it. The two mechanisms have identical timing —
both fire after each command completes and before the next prompt is drawn — so PS1 is
strictly better from a stealth perspective.

Q: How is the existing PS1 preserved when `setupPS1` runs?
A: `'[RC:$?:$((_ATSEQ++))]'"${PS1}"` — the single-quoted prefix keeps `$?` and
`$((expr))` as **literals** (not expanded at assignment time), while the
double-quoted `"${PS1}"` **expands the current PS1 value** at the moment `setupPS1`
runs — after the shell's login files (`/etc/profile`, `~/.bash_profile`, etc.) have
already set their own PS1. The result is prepend-only: the human's existing prompt
appearance is unchanged, just prefixed with `[RC:0:3]` or similar.

Q: Confirmed gotcha: `agent-tmux attach` vs `agent-tmux open`.
A: `agent-tmux attach <session>` calls `tmux attach-session -r` (read-only). `Ctrl-T`
and `Ctrl-Q` are **`open`-specific** — they only work when `agent-tmux open <session>`
is used, because `open` installs the global key bindings and runs its toggle loop.
Using `attach` by mistake leaves the user in a read-only session with no working exit
key. Recovery: press `Ctrl-B d` (standard tmux detach, always honored in read-only),
then re-run with `agent-tmux open <session>` instead. Do not document `attach` as a
user-facing command in SKILL.md or any agent-facing guide.

## 14. Why There Is No `run` Command

Q: What is this project actually for — a local terminal convenience, or something
else?
A: **agent-tmux exists for agents and humans to co-work on remote debugging
sessions** — production incidents, SSH'd-into boxes, `sudo -i` shells, `kubectl exec`,
debuggers, anything where the process a human might need to see or take over is
frequently *not* the local shell agent-tmux started, but something nested several
hops inside it. Local-only command execution was never the point; if that were the
whole problem, a plain subprocess would suffice and this tool wouldn't exist. Any
design decision that only works for the local top-level shell and silently degrades
once you `ssh` in is a bug against the actual purpose of this project, not an
acceptable edge case.

Q: What was `run`, and why did it exist?
A: `RunCommand` (CLI: `agent-tmux run <session> '<cmd>'`) was a single-call
convenience: send a command, poll the screen for a `[RC:$?:seq]` marker injected into
the shell's `PS1` (§13), and return the exit code once a newer marker appeared. It
existed so a caller didn't have to separately `send` + `wait`/`screen` for the common
case of an ordinary, quick, local command.

Q: What was the actual flaw, and why does it matter for this project specifically?
A: `setupPS1` (§13) is sent exactly once, to the **local** shell, at `Create` time. It
is a property of that one shell process — not of "whatever program currently has the
pane." The instant a caller `send`s `ssh user@host` (or `sudo -i`, or anything that
execs into a different shell), the pane is now driven by a shell that never received
that `export PS1=...`. `run`'s marker never reappears, so every subsequent `run` call
against that session times out with "command is still running or waiting for input"
— even though the remote command completed instantly. Since this project's entire
reason to exist is exactly that nested-shell case (SSH/sudo/debugger sessions during
remote debugging), a mechanism that quietly stops working the moment you're inside
one is not a minor gap — it fails on the primary use case, not an edge case.

Q: Why not just re-inject the same PS1 marker into the remote shell after `ssh`
completes?
A: Considered and rejected. It would need to detect "a new shell just started" (no
reliable, generic signal for that over a raw PTY — see §13's rejected "prompt
detection via existing PS1"), then re-run `setupPS1` remotely, then keep track of
*which* shell in the nesting stack is the "current" one so `run` polls the right
marker generation. That's meaningfully more state and more failure modes (what if the
re-injection races the login banner? what if the remote shell isn't bash?) in service
of preserving one convenience call. Simpler to remove the assumption entirely than to
keep patching around it.

Q: So what replaces `run`?
A: Nothing — by design. Every caller (agent or human-authored script) uses the same
two primitives for every command, local or nested: `send` the text, then
`screen`/`wait --contains <expected-text>` to read the result. If the exit code
matters, `send 'echo $?'` as its own follow-up and read it off `screen`. This is
exactly what a human already does when watching a shared terminal — there's nothing
for a monitoring tool to flag, and nothing that assumes anything about which shell,
local or remote, is currently attached to the pane.

Q: Doesn't this make every command more expensive (two calls instead of one, and a
polling/settle heuristic instead of a hard completion signal)?
A: Yes, and that's an accepted, deliberate cost. A single unified mechanism that is
*correct* in the primary use case (remote, nested shells) is worth more than a faster
mechanism that is *wrong* in exactly that case. `wait --contains` already requires the
caller to know something about expected output — true for interactive commands
today, and no worse for routine ones. Where output is unpredictable, polling `screen`
until it stops changing is the same judgment call a human watching the pane would
make; it is a heuristic, not a guarantee, but it never silently returns a wrong
answer the way `run` did once nested.

Q: Does removing `run` change the ownership/ACL rules?
A: No. `send`/`key` are still refused while another actor holds ownership, exactly as
`send`/`run`/`key` were before. Removing `run` only removes the PS1 injection
(`setupPS1`, the `[RC:...]` regex, and the `RunResult`/exit-code plumbing) and the
`run` CLI verb; `Create` no longer touches `PS1` at all, so a freshly created
session's prompt is whatever the user's own shell config would normally produce —
one less thing agent-tmux changes about the environment it's given.
