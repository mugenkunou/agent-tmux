package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mugenkunou/agent-tmux/internal/recording"
	"github.com/mugenkunou/agent-tmux/internal/termshare"
)

// commands builds the registry. It returns fresh instances on every call
// because each command owns a flag.FlagSet whose variables its handler
// closes over; nothing here is shared global state.
//
// To add a command: add a constructor below and list it in this slice. To add
// or change a flag or positional argument: edit that command's constructor.
// Help, validation and shell completion follow automatically.
func commands() []*command {
	return []*command{
		createCommand(),
		lsCommand(),
		showCommand(),
		grepCommand(),
		rmCommand(),
		statusCommand(),
		screenCommand(),
		sendCommand(),
		keyCommand(),
		waitCommand(),
		watchCommand(),
		takeCommand(),
		openCommand(),
		yieldCommand(),
		restartCommand(),
		killCommand(),
		doctorCommand(),
		completionCommand(),
		skillCommand(),
		versionCommand(),
		recordCommand(),
		completeCommand(),
		helpCommand(),
	}
}

var sessionArg = positional{name: "session", kind: argSession}

// commonKeys are suggestions for `key`; tmux accepts many more, so this is a
// convenience list for completion, not validation.
var commonKeys = []string{
	"Enter", "Escape", "Tab", "Space", "BSpace",
	"Up", "Down", "Left", "Right",
	"C-c", "C-d", "C-z", "C-l", "C-r", "C-a", "C-e",
}

func createCommand() *command {
	c := newCommand("create", "start a persistent Bash session; attaches immediately when run from a terminal (same as 'open')")
	shell := c.fs.String("shell", "/bin/bash", "`path` of the login shell to start")
	cols := c.fs.Int("cols", 120, "initial terminal width in `columns`")
	rows := c.fs.Int("rows", 36, "initial terminal height in `rows`")
	history := c.fs.Int("history", termshare.DefaultHistoryLimit, "scrollback `lines` tmux keeps for the session")
	c.args = []positional{{name: "session", kind: argFree}}
	c.run = func(inv *invocation) int {
		name := inv.args[0]
		if err := inv.manager.CreateWithHistory(inv.ctx, name, *shell, *cols, *rows, *history); err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "created %s\n", name)
		// From a real terminal, dropping straight into the session is what the
		// user expects; scripted callers (no tty) can't attach and get the hint.
		if stdinIsTerminal() {
			return openSession(inv, name)
		}
		fmt.Fprintf(inv.stdout, "open:  agent-tmux open %s   (Ctrl-T toggles watching/control, Ctrl-] menu)\n", name)
		return 0
	}
	return c
}

// stdinIsTerminal reports whether the process was started with an
// interactive stdin, i.e. whether attaching is meaningful at all. /dev/null
// is a character device too, so this asks for a terminal, not a device.
func stdinIsTerminal() bool {
	return isTerminal(os.Stdin)
}

func ownerLabel(owner string) string {
	if owner == "" {
		return "(unclaimed)"
	}
	return owner
}

func statusCommand() *command {
	c := newCommand("status", "show one session's owner, clients, command, path, recording and recorder")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		session, err := inv.manager.Status(inv.ctx, inv.args[0])
		if err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "session: %s\nowner: %s\nclients: %d\ncommand: %s\npath: %s\n", session.Name, ownerLabel(session.Owner), session.Clients, session.Command, session.Path)
		fmt.Fprintf(inv.stdout, "recording: %s\nrecorder: %s\n", session.Recording, recorderLabel(session))
		return 0
	}
	return c
}

func screenCommand() *command {
	c := newCommand("screen", "print the current rendered terminal")
	history := c.fs.Int("history", 0, "include this many `lines` of scrollback")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		contents, err := inv.manager.Screen(inv.ctx, inv.args[0], *history)
		if err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprint(inv.stdout, contents)
		return 0
	}
	return c
}

func sendCommand() *command {
	c := newCommand("send", "send text (then Enter) to a session; works for routine commands and interactive programs alike")
	noEnter := c.fs.Bool("no-enter", false, "do not press Enter after sending text")
	c.args = []positional{sessionArg, {name: "text", kind: argFree, variadic: true}}
	c.run = func(inv *invocation) int {
		name := inv.args[0]
		text := strings.Join(inv.args[1:], " ")
		if err := inv.manager.Send(inv.ctx, name, text, !*noEnter); err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "sent to %s\n", name)
		return 0
	}
	return c
}

func keyCommand() *command {
	c := newCommand("key", "send tmux key names such as C-c, C-z or Escape")
	c.args = []positional{sessionArg, {name: "key", kind: argChoice, choices: commonKeys, variadic: true}}
	c.run = func(inv *invocation) int {
		if err := inv.manager.SendKeys(inv.ctx, inv.args[0], inv.args[1:]); err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "sent keys to %s\n", inv.args[0])
		return 0
	}
	return c
}

func waitCommand() *command {
	c := newCommand("wait", "wait until the expected text appears on screen")
	contains := c.fs.String("contains", "", "`text` expected on screen")
	timeout := c.fs.Duration("timeout", 30*time.Second, "maximum `duration` to wait")
	history := c.fs.Int("history", 200, "scrollback `lines` to inspect")
	c.require("contains")
	c.suggest("timeout", "5s", "30s", "1m", "5m")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		contents, err := inv.manager.WaitFor(inv.ctx, inv.args[0], *contains, *timeout, *history)
		if err != nil {
			if contents != "" {
				fmt.Fprint(inv.stdout, contents)
			}
			return fail(inv.stderr, err)
		}
		fmt.Fprint(inv.stdout, contents)
		return 0
	}
	return c
}

func watchCommand() *command {
	c := newCommand("watch", "attach a read-only terminal")
	c.aliases = []string{"attach"}
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		if err := inv.manager.Watch(inv.ctx, inv.args[0]); err != nil {
			return fail(inv.stderr, err)
		}
		return 0
	}
	return c
}

func takeCommand() *command {
	c := newCommand("take", "claim ownership and attach writable; released on detach")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		if err := inv.manager.Take(inv.ctx, inv.args[0]); err != nil {
			return fail(inv.stderr, err)
		}
		return 0
	}
	return c
}

func openCommand() *command {
	c := newCommand("open", "attach once; Ctrl-T toggles watching/control, Ctrl-] opens a menu (detach, kill, restart, Ctrl-C)")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int { return openSession(inv, inv.args[0]) }
	return c
}

func openSession(inv *invocation, name string) int {
	hooks := termshare.OpenHooks{
		Phase: func(owner string) {
			if owner == termshare.OwnerUnclaimed {
				fmt.Fprintf(inv.stdout, "-- watching %s: Ctrl-T takes control, Ctrl-Q exits, Ctrl-] menu --\n", name)
			} else {
				fmt.Fprintf(inv.stdout, "-- %s controls %s: Ctrl-T releases, Ctrl-Q exits, Ctrl-] menu --\n", owner, name)
			}
		},
		Menu: func() bool { return escapeMenu(inv, name) },
	}
	if err := inv.manager.Open(inv.ctx, name, hooks); err != nil {
		return fail(inv.stderr, err)
	}
	return 0
}

func yieldCommand() *command {
	c := newCommand("yield", "clear a stuck owner after an interrupted takeover")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		if err := inv.manager.Yield(inv.ctx, inv.args[0]); err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "%s is unclaimed; scripted send calls are allowed again\n", inv.args[0])
		return 0
	}
	return c
}

func restartCommand() *command {
	c := newCommand("restart", "kill whatever runs in a session (e.g. a frozen ssh) and start a fresh shell in its place")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		if err := inv.manager.Restart(inv.ctx, inv.args[0]); err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "restarted the shell in %s\n", inv.args[0])
		return 0
	}
	return c
}

func killCommand() *command {
	c := newCommand("kill", "terminate a session")
	c.args = []positional{sessionArg}
	c.run = func(inv *invocation) int {
		if err := inv.manager.Kill(inv.ctx, inv.args[0]); err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "killed %s\n", inv.args[0])
		return 0
	}
	return c
}

func doctorCommand() *command {
	c := newCommand("doctor", "check that tmux is found, show the socket path and recordings directory, and each session's recorder")
	c.run = func(inv *invocation) int {
		version, err := inv.manager.Version(inv.ctx)
		if err != nil {
			return fail(inv.stderr, err)
		}
		root, err := recording.Root()
		if err != nil {
			return fail(inv.stderr, err)
		}
		fmt.Fprintf(inv.stdout, "tmux: %s\nsocket: %s\nrecordings: %s\n", version, inv.manager.SocketPath(), root)
		sessions, err := inv.manager.List(inv.ctx)
		if err != nil {
			return fail(inv.stderr, err)
		}
		for _, s := range sessions {
			label := recorderLabel(s)
			if s.Recorder == termshare.RecorderStopped {
				fmt.Fprintf(inv.stdout, "warning: %s: recorder %s\n", s.Name, label)
			} else {
				fmt.Fprintf(inv.stdout, "session %s: recorder %s\n", s.Name, label)
			}
		}
		return 0
	}
	return c
}

func helpCommand() *command {
	c := newCommand("help", "show usage")
	c.aliases = []string{"-h", "--help"}
	c.hidden = true
	c.noManager = true
	c.run = func(inv *invocation) int {
		printUsage(inv.stdout, commands())
		return 0
	}
	return c
}
