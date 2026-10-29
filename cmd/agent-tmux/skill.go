package main

import agenttmux "github.com/mugenkunou/agent-tmux"

// skillCommand prints the embedded SKILL.md. Like `completion`, the output
// comes from the binary itself, so a copy written from it can never describe a
// different version of the commands than the binary on PATH.
func skillCommand() *command {
	c := newCommand("skill", "print the agent skill (SKILL.md) for this binary, e.g. agent-tmux skill > <skills-dir>/agent-tmux/SKILL.md")
	c.noManager = true
	c.run = func(inv *invocation) int {
		inv.stdout.Write(agenttmux.Skill)
		return 0
	}
	return c
}
