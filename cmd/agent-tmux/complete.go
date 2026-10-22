package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"agent-tmux/internal/termshare"
)

// candidate is one completion suggestion. The wire format printed by
// __complete is "value" or "value<TAB>description", one per line.
type candidate struct {
	value string
	desc  string
}

func (c candidate) String() string {
	if c.desc == "" {
		return c.value
	}
	// Descriptions must stay on one line; tabs/newlines would corrupt the
	// line-oriented protocol the shell scripts rely on.
	desc := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(c.desc)
	return c.value + "\t" + desc
}

// complete computes candidates for the words typed after the program name;
// the last word is the one being completed (possibly empty). It derives
// everything from the registry: command names, each command's FlagSet and its
// positional argument kinds. The only external input is the session lister,
// injected so the engine is testable without tmux and only invoked when a
// session name is actually needed.
//
// Candidates are never escaped: session names are validated to
// [A-Za-z0-9._-] (see termshare.validateSessionName) and flag/command names are
// plain words, so none contains whitespace or shell metacharacters.
func complete(cmds []*command, words []string, sessions func() []termshare.Session) []candidate {
	if len(words) == 0 {
		words = []string{""}
	}
	current := words[len(words)-1]
	prior := words[:len(words)-1]

	if len(prior) == 0 {
		var out []candidate
		for _, c := range cmds {
			if !c.hidden && strings.HasPrefix(c.name, current) {
				out = append(out, candidate{c.name, c.summary})
			}
		}
		return out
	}

	cmd := findCommand(cmds, prior[0])
	if cmd == nil || cmd.hidden || cmd.raw {
		return nil
	}

	// Walk the words already typed the way Go's flag package would: flags
	// are recognised only until the first positional argument, "--" ends
	// them, and a non-boolean flag swallows the following word as its value.
	var (
		positionals int
		flagsDone   bool
		pending     *flag.Flag
		seen        = map[string]bool{}
	)
	for _, word := range prior[1:] {
		if pending != nil {
			pending = nil
			continue
		}
		if !flagsDone && len(word) > 1 && word[0] == '-' {
			if word == "--" {
				flagsDone = true
				continue
			}
			name, _, hasValue := strings.Cut(strings.TrimLeft(word, "-"), "=")
			seen[name] = true
			if f := cmd.fs.Lookup(name); f != nil && !hasValue && !isBoolFlag(f) {
				pending = f
			}
			continue
		}
		flagsDone = true
		positionals++
	}

	if pending != nil {
		return matching(cmd.hints[pending.Name], current)
	}

	if !flagsDone && strings.HasPrefix(current, "-") {
		// "--flag=val" in a single word (zsh, fish): complete the value and
		// return the whole word. bash splits at "=", so the script folds it
		// into the pending-flag case above instead.
		if eq := strings.Index(current, "="); eq >= 0 {
			f := cmd.fs.Lookup(strings.TrimLeft(current[:eq], "-"))
			if f == nil {
				return nil
			}
			var out []candidate
			for _, c := range matching(cmd.hints[f.Name], current[eq+1:]) {
				out = append(out, candidate{value: current[:eq+1] + c.value})
			}
			return out
		}
		// Go's flag package accepts -flag and --flag alike; match on the bare
		// name so "-n" finds --no-enter, and always offer the "--" spelling.
		typed := strings.TrimLeft(current, "-")
		var out []candidate
		cmd.fs.VisitAll(func(f *flag.Flag) {
			if !seen[f.Name] && strings.HasPrefix(f.Name, typed) {
				_, usage := flag.UnquoteUsage(f) // strips the `placeholder` backquotes
				out = append(out, candidate{"--" + f.Name, usage})
			}
		})
		return out
	}

	spec, ok := cmd.positionalAt(positionals)
	if !ok {
		return nil
	}
	switch spec.kind {
	case argSession:
		var out []candidate
		for _, s := range sessions() {
			if strings.HasPrefix(s.Name, current) {
				out = append(out, candidate{s.Name, sessionDescription(s)})
			}
		}
		return out
	case argChoice:
		return matching(spec.choices, current)
	}
	return nil
}

// positionalAt returns the spec governing the i'th positional argument; a
// variadic final spec governs every position from its own index onward.
func (c *command) positionalAt(i int) (positional, bool) {
	if i < len(c.args) {
		return c.args[i], true
	}
	if n := len(c.args); n > 0 && c.args[n-1].variadic {
		return c.args[n-1], true
	}
	return positional{}, false
}

func matching(values []string, prefix string) []candidate {
	var out []candidate
	for _, v := range values {
		if strings.HasPrefix(v, prefix) {
			out = append(out, candidate{value: v})
		}
	}
	return out
}

func sessionDescription(s termshare.Session) string {
	state := "unclaimed"
	if s.Owner != "" {
		state = "controlled by " + s.Owner
	}
	if s.Command != "" {
		state += ", running " + s.Command
	}
	return state
}

// liveSessions lists sessions on demand. Any failure (no tmux, no server,
// slow tmux) yields an empty list: completion must never print errors into
// the user's prompt or hang it.
func liveSessions(ctx context.Context) func() []termshare.Session {
	return func() []termshare.Session {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		manager, err := termshare.NewManager()
		if err != nil {
			return nil
		}
		sessions, err := manager.List(ctx)
		if err != nil {
			return nil
		}
		return sessions
	}
}

// completeCommand is the hidden protocol endpoint the shell scripts call:
//
//	agent-tmux __complete <word>... <word-being-completed>
//
// It always exits 0 so a failure can never break the user's shell.
func completeCommand() *command {
	c := newCommand("__complete", "print completion candidates for the words typed so far")
	c.hidden = true
	c.raw = true
	c.noManager = true
	c.run = func(inv *invocation) (code int) {
		defer func() {
			if recover() != nil {
				code = 0
			}
		}()
		for _, cand := range complete(commands(), inv.args, liveSessions(inv.ctx)) {
			fmt.Fprintln(inv.stdout, cand)
		}
		return 0
	}
	return c
}
