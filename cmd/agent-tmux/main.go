package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"agent-tmux/internal/termshare"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches through the command registry (see command.go); it contains
// no per-command knowledge of its own.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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

	inv := &invocation{ctx: ctx, args: rest, stdout: stdout, stderr: stderr}
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

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "agent-tmux:", err)
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	return 1
}
