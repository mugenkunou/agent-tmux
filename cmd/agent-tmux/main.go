package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"agent-tmux/internal/termshare"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	manager, err := termshare.NewManager()
	if err != nil {
		fmt.Fprintln(stderr, "agent-tmux:", err)
		return 1
	}

	switch args[0] {
	case "create":
		return create(ctx, manager, args[1:], stdout, stderr)
	case "list":
		return list(ctx, manager, stdout, stderr)
	case "status":
		return status(ctx, manager, args[1:], stdout, stderr)
	case "screen":
		return screen(ctx, manager, args[1:], stdout, stderr)
	case "send":
		return send(ctx, manager, args[1:], stdout, stderr)
	case "key":
		return key(ctx, manager, args[1:], stdout, stderr)
	case "wait":
		return waitFor(ctx, manager, args[1:], stdout, stderr)
	case "watch", "attach":
		return attach(ctx, manager, args[1:], true, stderr)
	case "take":
		return attach(ctx, manager, args[1:], false, stderr)
	case "open":
		return open(ctx, manager, args[1:], stdout, stderr)
	case "yield":
		return yield(ctx, manager, args[1:], stdout, stderr)
	case "kill":
		return kill(ctx, manager, args[1:], stdout, stderr)
	case "doctor":
		return doctor(ctx, manager, stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "agent-tmux: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func create(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	shell := flags.String("shell", "/bin/bash", "login shell to start")
	cols := flags.Int("cols", 120, "initial terminal width")
	rows := flags.Int("rows", 36, "initial terminal height")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agent-tmux create [options] <session>")
		return 2
	}
	name := flags.Arg(0)
	if err := manager.Create(ctx, name, *shell, *cols, *rows); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "created %s\n", name)
	fmt.Fprintf(stdout, "open:  agent-tmux open %s   (Ctrl-T toggles watching/control)\n", name)
	return 0
}

func list(ctx context.Context, manager *termshare.Manager, stdout, stderr io.Writer) int {
	sessions, err := manager.List(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "no sessions")
		return 0
	}
	fmt.Fprintln(stdout, "SESSION\tOWNER\tCLIENTS\tCOMMAND\tPATH")
	for _, session := range sessions {
		owner := session.Owner
		if owner == "" {
			owner = "(unclaimed)"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\t%s\n", session.Name, owner, session.Clients, session.Command, session.Path)
	}
	return 0
}

func status(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: agent-tmux status <session>")
		return 2
	}
	session, err := manager.Status(ctx, args[0])
	if err != nil {
		return fail(stderr, err)
	}
	owner := session.Owner
	if owner == "" {
		owner = "(unclaimed)"
	}
	fmt.Fprintf(stdout, "session: %s\nowner: %s\nclients: %d\ncommand: %s\npath: %s\n", session.Name, owner, session.Clients, session.Command, session.Path)
	return 0
}

func screen(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("screen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	history := flags.Int("history", 0, "include this many lines of scrollback")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agent-tmux screen [--history N] <session>")
		return 2
	}
	contents, err := manager.Screen(ctx, flags.Arg(0), *history)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprint(stdout, contents)
	return 0
}

func send(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	flags.SetOutput(stderr)
	noEnter := flags.Bool("no-enter", false, "do not press Enter after sending text")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() < 2 {
		fmt.Fprintln(stderr, "usage: agent-tmux send [--no-enter] <session> <text>")
		return 2
	}
	name := flags.Arg(0)
	text := strings.Join(flags.Args()[1:], " ")
	if err := manager.Send(ctx, name, text, !*noEnter); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "sent to %s\n", name)
	return 0
}

func key(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: agent-tmux key <session> <key> [key ...]")
		return 2
	}
	if err := manager.SendKeys(ctx, args[0], args[1:]); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "sent keys to %s\n", args[0])
	return 0
}

func waitFor(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(stderr)
	contains := flags.String("contains", "", "text expected on screen")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum wait")
	history := flags.Int("history", 200, "scrollback lines to inspect")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *contains == "" {
		fmt.Fprintln(stderr, "usage: agent-tmux wait --contains <text> [--timeout 30s] <session>")
		return 2
	}
	contents, err := manager.WaitFor(ctx, flags.Arg(0), *contains, *timeout, *history)
	if err != nil {
		if contents != "" {
			fmt.Fprint(stdout, contents)
		}
		return fail(stderr, err)
	}
	fmt.Fprint(stdout, contents)
	return 0
}

func attach(ctx context.Context, manager *termshare.Manager, args []string, readOnly bool, stderr io.Writer) int {
	if len(args) != 1 {
		if readOnly {
			fmt.Fprintln(stderr, "usage: agent-tmux watch <session>")
		} else {
			fmt.Fprintln(stderr, "usage: agent-tmux take <session>")
		}
		return 2
	}
	var err error
	if readOnly {
		err = manager.Watch(ctx, args[0])
	} else {
		err = manager.Take(ctx, args[0])
	}
	if err != nil {
		return fail(stderr, err)
	}
	return 0
}

func yield(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: agent-tmux yield <session>")
		return 2
	}
	if err := manager.Yield(ctx, args[0]); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s is unclaimed; scripted send calls are allowed again\n", args[0])
	return 0
}

func open(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: agent-tmux open <session>")
		return 2
	}
	name := args[0]
	onPhase := func(owner string) {
		if owner == termshare.OwnerUnclaimed {
			fmt.Fprintf(stdout, "-- watching %s: press Ctrl-T to take control --\n", name)
		} else {
			fmt.Fprintf(stdout, "-- %s controls %s: press Ctrl-T to release, Ctrl-Q to exit --\n", owner, name)
		}
	}
	if err := manager.Open(ctx, name, onPhase); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func kill(ctx context.Context, manager *termshare.Manager, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: agent-tmux kill <session>")
		return 2
	}
	if err := manager.Kill(ctx, args[0]); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "killed %s\n", args[0])
	return 0
}

func doctor(ctx context.Context, manager *termshare.Manager, stdout, stderr io.Writer) int {
	version, err := manager.Version(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "tmux: %s\nsocket: %s\n", version, manager.SocketPath())
	return 0
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "agent-tmux:", err)
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	return 1
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `agent-tmux shares one persistent terminal between scripted callers and an
attached terminal, with a single owner allowed to write at a time.

Usage:
  agent-tmux create [--shell /bin/bash] <session>
  agent-tmux list
  agent-tmux status <session>
  agent-tmux screen [--history N] <session>
  agent-tmux send [--no-enter] <session> <text>
  agent-tmux key <session> <key> [key ...]
  agent-tmux wait --contains <text> [--timeout 30s] <session>
  agent-tmux watch <session>       # read-only attachment
  agent-tmux take <session>        # claim ownership, attach writable, release on detach
  agent-tmux open <session>        # attach once; Ctrl-T toggles watching/control in place
  agent-tmux yield <session>       # clear a stuck owner after an interrupted takeover
  agent-tmux kill <session>
  agent-tmux doctor`)
}
