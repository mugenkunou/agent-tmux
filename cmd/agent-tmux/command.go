package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/mugenkunou/agent-tmux/internal/termshare"
)

// The command registry (commands() in commands.go) is the single source of
// truth for the CLI surface. Dispatch, flag parsing, argument-count
// validation, the usage text, and shell completion are ALL derived from the
// types below, so changing a command, flag, or argument in one place changes
// every consumer. Do not hand-maintain a second list of commands, flags, or
// argument shapes anywhere (including the shell completion scripts).

// argKind says what a positional argument holds, which is what completion
// uses to decide what to offer for it.
type argKind int

const (
	// argFree is arbitrary user input (a new session name, text to send);
	// nothing useful can be completed.
	argFree argKind = iota
	// argSession is the name of an existing session, completed live.
	argSession
	// argChoice is one of a fixed set, listed in positional.choices.
	argChoice
	// argRecording names a recording: a live session, a recording id, or
	// the session name of a recording.
	argRecording
)

// positional describes one positional argument. Positionals are required
// unless optional; optional ones come after every required one. A variadic
// one must be last and accepts one or more values (zero or more if optional).
type positional struct {
	name     string
	kind     argKind
	choices  []string
	variadic bool
	optional bool
}

// invocation is everything a command handler needs. args are the positional
// arguments left after flag parsing (or all arguments, for raw commands).
type invocation struct {
	ctx            context.Context
	manager        *termshare.Manager // nil when the command sets noManager
	args           []string
	stdout, stderr io.Writer
}

type command struct {
	name    string
	aliases []string // accepted on the command line, never advertised
	summary string
	// hidden commands are dispatchable but absent from help, completion
	// and the README command table (plumbing, not user-facing surface).
	hidden bool
	// noManager skips creating the tmux Manager, so the command still works
	// when tmux is missing (help, completion).
	noManager bool
	// raw hands every argument to the handler untouched: no flag parsing,
	// no argument validation. Only for plumbing whose arguments are
	// arbitrary words (__complete).
	raw bool

	// fs declares the command's flags. The handler reads values from the
	// same variables, and completion/usage enumerate the same FlagSet, so a
	// flag cannot exist in one place without the other.
	fs       *flag.FlagSet
	required []string            // flags that must be given
	hints    map[string][]string // flag name -> suggested values (completion only)
	flagArgs map[string]argKind  // flag name -> what its value names (completion only)
	args     []positional

	run func(inv *invocation) int
}

func newCommand(name, summary string) *command {
	return &command{
		name:     name,
		summary:  summary,
		fs:       flag.NewFlagSet(name, flag.ContinueOnError),
		hints:    map[string][]string{},
		flagArgs: map[string]argKind{},
	}
}

// mustHaveFlag turns a typo in a flag name into an immediate panic at
// construction time instead of a silently dead hint or requirement. Every
// command is constructed in tests, so such a mistake fails `make test`.
func (c *command) mustHaveFlag(name string) {
	if c.fs.Lookup(name) == nil {
		panic(fmt.Sprintf("command %q has no flag %q", c.name, name))
	}
}

// require marks flags as mandatory; they render without brackets in usage
// and a missing one is a usage error.
func (c *command) require(names ...string) {
	for _, name := range names {
		c.mustHaveFlag(name)
	}
	c.required = append(c.required, names...)
}

// suggest records example values for a flag's argument, offered by
// completion. They are hints, not validation.
func (c *command) suggest(flagName string, values ...string) {
	c.mustHaveFlag(flagName)
	c.hints[flagName] = values
}

// completeFlagAs says a flag's value names a session or recording, so
// completion offers live ones.
func (c *command) completeFlagAs(flagName string, kind argKind) {
	c.mustHaveFlag(flagName)
	c.flagArgs[flagName] = kind
}

func (c *command) isNamed(name string) bool {
	if c.name == name {
		return true
	}
	for _, alias := range c.aliases {
		if alias == name {
			return true
		}
	}
	return false
}

func findCommand(cmds []*command, name string) *command {
	for _, c := range cmds {
		if c.isNamed(name) {
			return c
		}
	}
	return nil
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// flagPlaceholder is the name of a flag's argument ("path" for
// "`path` of the login shell"), or "" for boolean flags. Flag usage strings
// must name their argument in backquotes; a test enforces it.
func flagPlaceholder(f *flag.Flag) string {
	if isBoolFlag(f) {
		return ""
	}
	name, _ := flag.UnquoteUsage(f)
	return name
}

func (c *command) isRequired(name string) bool {
	for _, r := range c.required {
		if r == name {
			return true
		}
	}
	return false
}

// usageLine renders the synopsis from the registry: required flags, then
// optional flags (alphabetical), then positionals.
func (c *command) usageLine() string {
	parts := []string{"agent-tmux", c.name}
	var optional []string
	c.fs.VisitAll(func(f *flag.Flag) {
		text := "--" + f.Name
		if p := flagPlaceholder(f); p != "" {
			text += " <" + p + ">"
		}
		if c.isRequired(f.Name) {
			parts = append(parts, text)
		} else {
			optional = append(optional, "["+text+"]")
		}
	})
	parts = append(parts, optional...)
	for _, p := range c.args {
		text := "<" + p.name + ">"
		if p.variadic {
			text += "..."
		}
		if p.optional {
			text = "[" + text + "]"
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

// parse applies flag parsing and validation. When done is true the caller
// must return code immediately (usage error or -h); otherwise rest holds the
// positional arguments.
func (c *command) parse(args []string, stderr io.Writer) (rest []string, code int, done bool) {
	if c.raw {
		return args, 0, false
	}
	c.fs.SetOutput(stderr)
	c.fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s\n", c.usageLine())
		c.fs.PrintDefaults()
	}
	if err := c.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0, true
		}
		return nil, 2, true
	}

	given := map[string]bool{}
	c.fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for _, name := range c.required {
		if !given[name] {
			fmt.Fprintf(stderr, "agent-tmux %s: --%s is required\nusage: %s\n", c.name, name, c.usageLine())
			return nil, 2, true
		}
	}

	rest = c.fs.Args()
	if !c.acceptsArgCount(len(rest)) {
		fmt.Fprintf(stderr, "usage: %s\n", c.usageLine())
		return nil, 2, true
	}
	return rest, 0, false
}

func (c *command) acceptsArgCount(n int) bool {
	required := 0
	for _, p := range c.args {
		if !p.optional {
			required++
		}
	}
	if n < required {
		return false
	}
	if len(c.args) > 0 && c.args[len(c.args)-1].variadic {
		return true
	}
	return n <= len(c.args)
}

// printUsage renders the help text entirely from the registry.
func printUsage(w io.Writer, cmds []*command) {
	fmt.Fprint(w, `agent-tmux shares one persistent terminal between scripted callers and an
attached terminal, with a single owner allowed to write at a time.

Usage:
`)
	for _, c := range cmds {
		if c.hidden {
			continue
		}
		fmt.Fprintf(w, "  %s\n      %s\n", c.usageLine(), c.summary)
	}
}
