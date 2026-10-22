package main

import (
	"embed"
	"fmt"
	"sort"
)

// The shell scripts are deliberately dumb relays: they pass the words typed so
// far to `agent-tmux __complete` and display what comes back. They must never
// name a command, flag or session themselves; a test enforces that, so the
// registry stays the only place the CLI surface is defined.
//
//go:embed completions/*
var completionFS embed.FS

// completionScripts maps a shell name to its embedded script. A test checks
// this map and the completions/ directory agree.
var completionScripts = map[string]string{
	"bash": "completions/agent-tmux.bash",
	"zsh":  "completions/_agent-tmux",
	"fish": "completions/agent-tmux.fish",
}

func completionShells() []string {
	shells := make([]string, 0, len(completionScripts))
	for name := range completionScripts {
		shells = append(shells, name)
	}
	sort.Strings(shells)
	return shells
}

func completionCommand() *command {
	c := newCommand("completion", "print the shell completion script, e.g. eval \"$(agent-tmux completion bash)\"")
	c.noManager = true
	c.args = []positional{{name: "shell", kind: argChoice, choices: completionShells()}}
	c.run = func(inv *invocation) int {
		path, ok := completionScripts[inv.args[0]]
		if !ok {
			fmt.Fprintf(inv.stderr, "agent-tmux: unsupported shell %q; choose one of %v\n", inv.args[0], completionShells())
			return 2
		}
		script, err := completionFS.ReadFile(path)
		if err != nil {
			return fail(inv.stderr, err)
		}
		inv.stdout.Write(script)
		return 0
	}
	return c
}
