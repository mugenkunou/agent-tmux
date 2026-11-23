package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/mugenkunou/agent-tmux/internal/termshare"
)

func main() {
	os.Exit(runWithStdin(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches through the command registry (see command.go); it contains
// no per-command knowledge of its own. Standard input is not connected.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWithStdin(ctx, args, nil, stdout, stderr)
}

func runWithStdin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmds := commands()
	if len(args) == 0 {
		printUsage(stderr, cmds)
		return 2
	}

	cmd := findCommand(cmds, args[0])
	if cmd == nil {
		fmt.Fprintf(stderr, "agent-tmux: unknown command %q\n\n", args[0])
		printUsage(stderr, cmds)
		return 2
	}

	rest, code, done := cmd.parse(args[1:], stderr)
	if done {
		return code
	}

	inv := &invocation{ctx: ctx, args: rest, stdin: stdin, stdout: stdout, stderr: stderr}
	if !cmd.noManager {
		manager, err := termshare.NewManager()
		if err != nil {
			fmt.Fprintln(stderr, "agent-tmux:", err)
			return 1
		}
		inv.manager = manager
	}
	return cmd.run(inv)
}

// exitRefused is the exit status for input the send policy refused, so a
// script can tell it apart from a failure to reach the session.
const exitRefused = 3

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "agent-tmux:", err)
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	if errors.Is(err, termshare.ErrRefused) {
		return exitRefused
	}
	return 1
}
