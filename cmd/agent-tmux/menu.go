package main

import (
	"fmt"
)

// escapeMenu is what Ctrl-] opens inside `open`: a one-line menu shown with
// the session's display stopped, so it works however stuck the program in
// the pane is. readKey returns one keypress. It returns true when `open`
// should end.
func escapeMenu(inv *invocation, name string, readKey func() (byte, error)) bool {
	for {
		fmt.Fprintf(inv.stdout, "\r\nagent-tmux %s: [d]etach  [k]ill session  [r]estart shell  [c] send Ctrl-C  [any other key] back ", name)
		key, err := readKey()
		fmt.Fprint(inv.stdout, "\r\n")
		if err != nil {
			return false
		}
		switch key {
		case 'd', 'D', 'q', 'Q':
			fmt.Fprintf(inv.stdout, "detached; %s keeps running (agent-tmux open %s)\n", name, name)
			return true
		case 'k', 'K':
			if !confirm(inv, readKey, fmt.Sprintf("kill session %s and everything running in it?", name)) {
				continue
			}
			if err := inv.manager.Kill(inv.ctx, name); err != nil {
				fail(inv.stderr, err)
				continue
			}
			fmt.Fprintf(inv.stdout, "killed %s\n", name)
			return true
		case 'r', 'R':
			if !confirm(inv, readKey, fmt.Sprintf("kill what runs in %s and start a fresh shell?", name)) {
				continue
			}
			if err := inv.manager.Restart(inv.ctx, name); err != nil {
				fail(inv.stderr, err)
				continue
			}
			return false
		case 'c', 'C':
			if err := inv.manager.SendKeys(inv.ctx, name, []string{"C-c"}); err != nil {
				fail(inv.stderr, err)
				continue
			}
			return false
		default:
			return false
		}
	}
}

func confirm(inv *invocation, readKey func() (byte, error), question string) bool {
	fmt.Fprintf(inv.stdout, "%s [y/N] ", question)
	key, err := readKey()
	fmt.Fprint(inv.stdout, "\r\n")
	return err == nil && (key == 'y' || key == 'Y')
}
