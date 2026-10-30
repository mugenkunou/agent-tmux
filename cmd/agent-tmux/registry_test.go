package main

import (
	"bytes"
	"context"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// These tests are the guard rails that keep the CLI surface, the help text and
// shell completion in lockstep. They are written against the registry, not
// against a hardcoded list, so adding a command or flag needs no test edit
// unless it breaks one of the rules below.

func TestRegistryIsWellFormed(t *testing.T) {
	names := map[string]string{}
	claim := func(name, owner string) {
		t.Helper()
		if name == "" || strings.ContainsAny(name, " \t\n=") {
			t.Errorf("%s: invalid name %q", owner, name)
		}
		if prev, dup := names[name]; dup {
			t.Errorf("%q is claimed by both %s and %s", name, prev, owner)
		}
		names[name] = owner
	}

	for _, c := range commands() {
		claim(c.name, "command "+c.name)
		for _, alias := range c.aliases {
			claim(alias, "alias of "+c.name)
		}
		if c.summary == "" {
			t.Errorf("%s: summary is empty (it is the help and completion description)", c.name)
		}
		if c.run == nil {
			t.Errorf("%s: no run handler", c.name)
		}
		if c.raw && len(c.args) > 0 {
			t.Errorf("%s: raw commands take no declared positionals", c.name)
		}

		c.fs.VisitAll(func(f *flag.Flag) {
			if strings.ContainsAny(f.Name, " \t\n=") || strings.HasPrefix(f.Name, "-") {
				t.Errorf("%s: invalid flag name %q", c.name, f.Name)
			}
			if f.Usage == "" {
				t.Errorf("%s --%s: usage is empty (it is the completion description)", c.name, f.Name)
			}
			if !isBoolFlag(f) && flagPlaceholder(f) == "" {
				t.Errorf("%s --%s: value flag must name its argument in backquotes, e.g. \"`path` of ...\"", c.name, f.Name)
			}
			if f.Name == "help" || f.Name == "h" {
				t.Errorf("%s: flag %q collides with the built-in help flag", c.name, f.Name)
			}
		})

		for i, p := range c.args {
			if p.name == "" {
				t.Errorf("%s: positional %d has no name", c.name, i)
			}
			if p.variadic && i != len(c.args)-1 {
				t.Errorf("%s: variadic positional %q must be last", c.name, p.name)
			}
			if p.optional && i+1 < len(c.args) && !c.args[i+1].optional {
				t.Errorf("%s: optional positional %q comes before a required one", c.name, p.name)
			}
			if p.kind == argChoice && len(p.choices) == 0 {
				t.Errorf("%s: choice positional %q has no choices", c.name, p.name)
			}
			if p.kind != argChoice && len(p.choices) > 0 {
				t.Errorf("%s: positional %q has choices but is not argChoice", c.name, p.name)
			}
		}
		// require/suggest panic at construction on unknown flags; also make
		// sure every hint is non-empty so it is not silently dead.
		for flagName, values := range c.hints {
			if len(values) == 0 {
				t.Errorf("%s: empty hints for --%s", c.name, flagName)
			}
		}
	}
}

// Help is generated, so this guards the generator: every visible command,
// every flag and every positional must appear in the usage text.
func TestUsageDocumentsEverything(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out, commands())
	text := out.String()
	for _, c := range commands() {
		if c.hidden {
			if strings.Contains(text, "agent-tmux "+c.name+" ") || strings.Contains(text, "agent-tmux "+c.name+"\n") {
				t.Errorf("hidden command %q leaked into help", c.name)
			}
			continue
		}
		if !strings.Contains(text, c.usageLine()) {
			t.Errorf("help is missing %q", c.usageLine())
		}
		c.fs.VisitAll(func(f *flag.Flag) {
			if !strings.Contains(c.usageLine(), "--"+f.Name) {
				t.Errorf("%s: usage line omits --%s", c.name, f.Name)
			}
		})
		for _, p := range c.args {
			if !strings.Contains(c.usageLine(), "<"+p.name+">") {
				t.Errorf("%s: usage line omits <%s>", c.name, p.name)
			}
		}
	}
}

// Every command must be reachable through run() by its name and each alias,
// proving dispatch is registry-driven and nothing is declared but unwired.
func TestEveryCommandIsDispatchable(t *testing.T) {
	for _, c := range commands() {
		for _, name := range append([]string{c.name}, c.aliases...) {
			var out, errOut bytes.Buffer
			// -h makes every parsed command print usage and stop before it
			// would touch tmux; raw/help commands are exercised separately.
			if c.raw || c.name == "help" {
				continue
			}
			code := run(context.Background(), []string{name, "-h"}, &out, &errOut)
			if code != 0 {
				t.Errorf("%s -h: exit %d, stderr: %s", name, code, errOut.String())
			}
			if !strings.Contains(errOut.String(), c.usageLine()) {
				t.Errorf("%s -h did not print its usage line %q; got %q", name, c.usageLine(), errOut.String())
			}
		}
	}
}

// Optional and variadic positionals decide how many arguments are accepted
// and how the usage line shows them.
func TestOptionalPositionals(t *testing.T) {
	c := newCommand("x", "test")
	c.args = []positional{{name: "a"}, {name: "b", optional: true}}
	for n, want := range map[int]bool{0: false, 1: true, 2: true, 3: false} {
		if got := c.acceptsArgCount(n); got != want {
			t.Errorf("required+optional, %d args: %v, want %v", n, got, want)
		}
	}
	if got := c.usageLine(); got != "agent-tmux x <a> [<b>]" {
		t.Errorf("usage %q", got)
	}
	c.args = []positional{{name: "s", variadic: true, optional: true}}
	for n, want := range map[int]bool{0: true, 1: true, 5: true} {
		if got := c.acceptsArgCount(n); got != want {
			t.Errorf("optional variadic, %d args: %v, want %v", n, got, want)
		}
	}
	if got := c.usageLine(); got != "agent-tmux x [<s>...]" {
		t.Errorf("usage %q", got)
	}
}

func TestArgumentValidationComesFromRegistry(t *testing.T) {
	for _, c := range commands() {
		if c.raw || c.hidden {
			continue
		}
		// One positional too many for fixed-arity commands, one too few
		// for those that take any.
		var bad []string
		switch {
		case len(c.args) == 0:
			bad = []string{"unexpected"}
		case c.args[len(c.args)-1].variadic:
			bad = nil // too few: no arguments at all
		default:
			for range len(c.args) + 1 {
				bad = append(bad, "x")
			}
		}
		for _, name := range c.required {
			// Satisfy required flags so only argument count is under test.
			f := c.fs.Lookup(name)
			value := "x"
			if f != nil && f.DefValue != "" {
				value = f.DefValue
			}
			bad = append([]string{"--" + name + "=" + value}, bad...)
		}
		var out, errOut bytes.Buffer
		if code := run(context.Background(), append([]string{c.name}, bad...), &out, &errOut); code != 2 {
			t.Errorf("%s %v: exit %d, want usage error 2 (stderr %q)", c.name, bad, code, errOut.String())
		}
	}
}

// The completion scripts must stay dumb relays. If one starts naming commands
// or flags it will silently go stale the next time the registry changes.
func TestScriptsDoNotDuplicateTheRegistry(t *testing.T) {
	scripts := readScripts(t)
	for shell, body := range scripts {
		code := stripComments(body)
		var words []string
		for _, c := range commands() {
			words = append(words, c.name)
			words = append(words, c.aliases...)
			c.fs.VisitAll(func(f *flag.Flag) { words = append(words, f.Name) })
		}
		for _, w := range words {
			if w == "completion" || w == "__complete" {
				continue // the relay is allowed to call its own plumbing
			}
			re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(w) + `($|[^A-Za-z0-9_-])`)
			if re.MatchString(code) {
				t.Errorf("%s script hardcodes registry word %q; derive it from __complete instead", shell, w)
			}
		}
		if !strings.Contains(code, "__complete") {
			t.Errorf("%s script does not call __complete", shell)
		}
	}
}

func TestEveryScriptIsEmbeddedAndMapped(t *testing.T) {
	entries, err := fs.ReadDir(completionFS, "completions")
	if err != nil {
		t.Fatal(err)
	}
	var onDisk, mapped []string
	for _, e := range entries {
		onDisk = append(onDisk, path.Join("completions", e.Name()))
	}
	for _, p := range completionScripts {
		mapped = append(mapped, p)
	}
	sort.Strings(onDisk)
	sort.Strings(mapped)
	if strings.Join(onDisk, ",") != strings.Join(mapped, ",") {
		t.Fatalf("completions/ and completionScripts disagree:\n  on disk: %v\n  mapped:  %v", onDisk, mapped)
	}

	// `completion <shell>` accepts exactly the shells it advertises.
	for _, shell := range completionShells() {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{"completion", shell}, &out, &errOut); code != 0 || out.Len() == 0 {
			t.Errorf("completion %s: exit %d, %d bytes, stderr %q", shell, code, out.Len(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"completion", "tcsh"}, &out, &errOut); code == 0 {
		t.Error("completion tcsh should fail")
	}
}

func readScripts(t *testing.T) map[string]string {
	t.Helper()
	scripts := map[string]string{}
	for shell, p := range completionScripts {
		body, err := completionFS.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		scripts[shell] = string(body)
	}
	return scripts
}

func stripComments(script string) string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func TestMain(m *testing.M) {
	// Sessions created in-process have tmux run os.Executable(), which is
	// this test binary, as the recorder: act as agent-tmux for it.
	if len(os.Args) > 1 && os.Args[1] == "_record" {
		os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	// Keep tests off the developer's real tmux server and recordings.
	dir, err := os.MkdirTemp("", "at-")
	if err != nil {
		panic(err)
	}
	state, err := os.MkdirTemp("", "at-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("AGENT_TMUX_RUNTIME_DIR", dir)
	os.Setenv("AGENT_TMUX_STATE_DIR", state)
	os.Setenv("XDG_DATA_HOME", state)  // rm's trash
	os.Setenv("XDG_CACHE_HOME", state) // version's release cache
	os.Unsetenv(noVersionCheckEv)
	startCmd = func(c *exec.Cmd) error { spawned = append(spawned, c); return nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(state)
	os.Exit(code)
}
