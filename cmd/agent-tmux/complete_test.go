package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mugenkunou/agent-tmux/internal/recording"
	"github.com/mugenkunou/agent-tmux/internal/termshare"
)

func noSessions() []termshare.Session { return nil }

func noRecordings() []recording.Recording { return nil }

func fixedRecordings(ids ...string) func() []recording.Recording {
	return func() []recording.Recording {
		var out []recording.Recording
		for _, id := range ids {
			name := id[:strings.Index(id, "-")]
			out = append(out, recording.Recording{Meta: recording.Meta{ID: id, Name: name}})
		}
		return out
	}
}

func fixedSessions(names ...string) func() []termshare.Session {
	return func() []termshare.Session {
		var out []termshare.Session
		for _, n := range names {
			out = append(out, termshare.Session{Name: n})
		}
		return out
	}
}

func values(cands []candidate) []string {
	out := []string{}
	for _, c := range cands {
		out = append(out, c.value)
	}
	sort.Strings(out)
	return out
}

func complVals(t *testing.T, sessions func() []termshare.Session, words ...string) []string {
	t.Helper()
	return values(complete(commands(), words, sessions, noRecordings))
}

// --- Derived-from-registry invariants (no hardcoded surface) --------------

func TestCompletesExactlyTheVisibleCommands(t *testing.T) {
	var want []string
	for _, c := range commands() {
		if !c.hidden {
			want = append(want, c.name)
		}
	}
	sort.Strings(want)
	if got := complVals(t, noSessions, ""); !reflect.DeepEqual(got, want) {
		t.Errorf("complete(\"\") = %v, want %v", got, want)
	}
}

func TestCompletesExactlyEachCommandsFlags(t *testing.T) {
	for _, c := range commands() {
		if c.hidden || c.raw {
			continue
		}
		want := []string{}
		c.fs.VisitAll(func(f *flag.Flag) { want = append(want, "--"+f.Name) })
		sort.Strings(want)
		if got := complVals(t, noSessions, c.name, "--"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s --<TAB> = %v, want %v", c.name, got, want)
		}
		// Aliases behave identically.
		for _, alias := range c.aliases {
			if got := complVals(t, noSessions, alias, "--"); !reflect.DeepEqual(got, want) {
				t.Errorf("%s --<TAB> = %v, want %v", alias, got, want)
			}
		}
	}
}

func TestSessionPositionalsOfferLiveSessionsAndOthersDoNot(t *testing.T) {
	sessions := fixedSessions("alpha", "beta")
	for _, c := range commands() {
		if c.hidden || c.raw {
			continue
		}
		for i, p := range c.args {
			words := []string{c.name}
			for j := 0; j < i; j++ {
				words = append(words, "filler")
			}
			words = append(words, "")
			got := complVals(t, sessions, words...)
			switch p.kind {
			case argSession, argRecording:
				if !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
					t.Errorf("%s arg %d (%s): got %v, want sessions", c.name, i, p.name, got)
				}
			case argChoice:
				want := append([]string{}, p.choices...)
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s arg %d (%s): got %v, want %v", c.name, i, p.name, got, want)
				}
			case argFree:
				if len(got) != 0 {
					t.Errorf("%s arg %d (%s): free-form arg offered %v", c.name, i, p.name, got)
				}
			}
		}
		// Past the last fixed positional: nothing, unless variadic.
		if len(c.args) > 0 && !c.args[len(c.args)-1].variadic {
			words := []string{c.name}
			for range c.args {
				words = append(words, "filler")
			}
			words = append(words, "")
			if got := complVals(t, sessions, words...); len(got) != 0 {
				t.Errorf("%s: offered %v beyond its last argument", c.name, got)
			}
		}
	}
}

func TestSessionsAreOnlyListedWhenNeeded(t *testing.T) {
	called := false
	probe := func() []termshare.Session { called = true; return nil }
	recProbe := func() []recording.Recording { called = true; return nil }
	complete(commands(), []string{""}, probe, recProbe)
	complete(commands(), []string{"send", "--"}, probe, recProbe)
	complete(commands(), []string{"create", ""}, probe, recProbe)
	complete(commands(), []string{"grep", "--limit", ""}, probe, recProbe)
	if called {
		t.Error("session lister ran for a position that cannot take a session")
	}
}

func TestFlagValueHintsFollowTheRegistry(t *testing.T) {
	for _, c := range commands() {
		for name, hints := range c.hints {
			sorted := append([]string{}, hints...)
			sort.Strings(sorted)
			if got := complVals(t, noSessions, append([]string{c.name}, "--"+name, "")...); !reflect.DeepEqual(got, sorted) {
				t.Errorf("%s --%s <TAB> = %v, want %v", c.name, name, got, sorted)
			}
			// "--flag=<TAB>" completes to the whole word.
			var withPrefix []string
			for _, h := range sorted {
				withPrefix = append(withPrefix, "--"+name+"="+h)
			}
			if got := complVals(t, noSessions, c.name, "--"+name+"="); !reflect.DeepEqual(got, withPrefix) {
				t.Errorf("%s --%s=<TAB> = %v, want %v", c.name, name, got, withPrefix)
			}
		}
	}
}

// --- Behaviour of the walker ----------------------------------------------

func TestWalkerCases(t *testing.T) {
	s := fixedSessions("alpha", "beta", "alpine")
	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"command prefix", []string{"k"}, []string{"key", "kill"}},
		{"unknown command", []string{"nope", ""}, []string{}},
		{"hidden command not completed", []string{"__"}, []string{}},
		{"session prefix narrows", []string{"kill", "al"}, []string{"alpha", "alpine"}},
		{"alias completes sessions", []string{"attach", "b"}, []string{"beta"}},
		{"flags before session arg", []string{"send", "--no-enter", ""}, []string{"alpha", "alpine", "beta"}},
		{"value flag swallows its argument", []string{"screen", "--history", "10", ""}, []string{"alpha", "alpine", "beta"}},
		{"bool flag does not swallow", []string{"send", "--no-enter", "al"}, []string{"alpha", "alpine"}},
		{"flag=value is one word", []string{"screen", "--history=10", "b"}, []string{"beta"}},
		{"used flag not offered again", []string{"send", "--no-enter", "--"}, []string{}},
		{"single dash matches long flag", []string{"send", "-n"}, []string{"--no-enter"}},
		{"dash after positional is data", []string{"send", "alpha", "-"}, []string{}},
		{"double dash ends flags", []string{"kill", "--", "-"}, []string{}},
		{"free-form value offers nothing", []string{"create", "--shell", ""}, []string{}},
		{"create offers no existing sessions", []string{"create", ""}, []string{}},
		{"key choices after session", []string{"key", "alpha", "C-"}, []string{"C-a", "C-c", "C-d", "C-e", "C-l", "C-r", "C-z"}},
		{"key stays variadic", []string{"key", "alpha", "C-c", "Es"}, []string{"Escape"}},
		{"send text is free", []string{"send", "alpha", "ls", ""}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := complVals(t, s, tc.words...)
			want := append([]string{}, tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("complete(%q) = %v, want %v", tc.words, got, want)
			}
		})
	}
}

// show, rm and grep --session name recordings: live sessions, recording ids
// and the names of ended recordings. Commands taking only a live session
// never offer recordings.
func TestRecordingArgumentsOfferRecordings(t *testing.T) {
	sessions := fixedSessions("web")
	recs := fixedRecordings("web-2026-10-29-1402", "db-2026-10-28-0900", "db-2026-10-27-0900")
	vals := func(words ...string) []string { return values(complete(commands(), words, sessions, recs)) }
	all := []string{"db", "db-2026-10-27-0900", "db-2026-10-28-0900", "web", "web-2026-10-29-1402"}
	for _, words := range [][]string{{"show", ""}, {"rm", ""}, {"rm", "x", ""}, {"rm", "--older-than", "7d", ""}, {"grep", "--session", ""}} {
		if got := vals(words...); !reflect.DeepEqual(got, all) {
			t.Errorf("%q = %v, want %v", words, got, all)
		}
	}
	if got := vals("show", "db-2026-10-2"); !reflect.DeepEqual(got, []string{"db-2026-10-27-0900", "db-2026-10-28-0900"}) {
		t.Errorf("id prefix = %v", got)
	}
	if got := vals("grep", "--session=d"); !reflect.DeepEqual(got, []string{"--session=db", "--session=db-2026-10-27-0900", "--session=db-2026-10-28-0900"}) {
		t.Errorf("--session=<prefix> = %v", got)
	}
	if got := vals("grep", "--session", "web", ""); len(got) != 0 {
		t.Errorf("grep text offered %v", got)
	}
	if got := vals("kill", ""); !reflect.DeepEqual(got, []string{"web"}) {
		t.Errorf("kill offered %v, want only live sessions", got)
	}
}

func TestEmptyWordsIsSafe(t *testing.T) {
	if got := complete(commands(), nil, noSessions, noRecordings); len(got) == 0 {
		t.Error("no words should behave like an empty first word and list commands")
	}
}

func TestCandidateWireFormat(t *testing.T) {
	if got := (candidate{"a", ""}).String(); got != "a" {
		t.Errorf("got %q", got)
	}
	if got := (candidate{"a", "x\ty\nz"}).String(); got != "a\tx y z" {
		t.Errorf("descriptions must be single-line, got %q", got)
	}
	d := sessionDescription(termshare.Session{Owner: "bob", Command: "ssh"})
	if !strings.Contains(d, "bob") || !strings.Contains(d, "ssh") {
		t.Errorf("session description %q lacks owner/command", d)
	}
}

// --- The __complete endpoint ----------------------------------------------

func TestCompleteEndpointNeverFailsWithoutTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no tmux anywhere
	for _, words := range [][]string{{"kill", ""}, {""}, {"nope", "x"}, {}} {
		var out, errOut bytes.Buffer
		code := run(context.Background(), append([]string{"__complete"}, words...), &out, &errOut)
		if code != 0 {
			t.Errorf("__complete %v: exit %d, stderr %q", words, code, errOut.String())
		}
		if errOut.Len() != 0 {
			t.Errorf("__complete %v wrote to stderr: %q", words, errOut.String())
		}
	}
	// Session-less positions still work, with no tmux needed.
	var out bytes.Buffer
	run(context.Background(), []string{"__complete", "ki"}, &out, &bytes.Buffer{})
	if !strings.Contains(out.String(), "kill") {
		t.Errorf("command completion should not need tmux, got %q", out.String())
	}
}

func TestHelpAndCompletionWorkWithoutTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{{"help"}, {"--help"}, {"completion", "bash"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 0 {
			t.Errorf("%v without tmux: exit %d, %q", args, code, errOut.String())
		}
	}
}

func TestCompleteEndpointListsLiveSessions(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir, err := os.MkdirTemp("", "at-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", dir)

	manager, err := termshare.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range []string{"complete-one", "complete-two", "other"} {
		if err := manager.Create(ctx, name, "/bin/bash", 80, 24); err != nil {
			t.Fatal(err)
		}
		name := name
		t.Cleanup(func() { _ = manager.Kill(context.Background(), name) })
	}

	var out bytes.Buffer
	if code := run(ctx, []string{"__complete", "kill", "complete-"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "complete-one\t") || !strings.HasPrefix(lines[1], "complete-two\t") {
		t.Errorf("unexpected candidates: %q", lines)
	}
}

// --- Real shell smoke test --------------------------------------------------

// Drives the actual bash script with a built binary, the way readline would,
// so the script/protocol cannot drift apart unnoticed.
func TestBashScriptEndToEnd(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "agent-tmux")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}

	// One recording, so recording arguments have something to offer.
	t.Setenv("AGENT_TMUX_STATE_DIR", t.TempDir())
	root, _ := recording.Root()
	recDir, err := recording.Create(root, "web", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	recID := filepath.Base(recDir)

	script, err := completionFS.ReadFile(completionScripts["bash"])
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(binDir, "agent-tmux.bash")
	if err := os.WriteFile(scriptPath, script, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		line    string // words, with the last being the one under the cursor
		want    string
		notWant string
	}{
		{"agent-tmux k", "key kill", ""},
		{"agent-tmux send --n", "--no-enter", ""},
		{"agent-tmux wait --timeout=", "", ""},             // '=' folded; must not crash
		{"agent-tmux wait --timeout ", "5s 30s 1m 5m", ""}, // hint after a value flag
		{"agent-tmux completion ", "bash fish zsh", ""},
		{"agent-tmux show w", recID + " web", ""},
		{"agent-tmux grep --session ", recID + " web", ""},
		{"agent-tmux rm --older-than ", "1d 7d 30d", ""},
		{"agent-tmux l", "ls", ""},
	}
	for _, tc := range cases {
		words := strings.Fields(tc.line)
		if strings.HasSuffix(tc.line, " ") {
			words = append(words, "")
		}
		harness := `source "$1"; shift
COMP_WORDS=("$@"); COMP_CWORD=$(( $# - 1 ))
_agent_tmux
printf '%s\n' "${COMPREPLY[@]}"`
		cmd := exec.Command(bash, append([]string{"-c", harness, "bash", scriptPath}, words...)...)
		cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%q: %v\n%s", tc.line, err, output)
			continue
		}
		got := strings.Fields(string(output))
		for _, w := range strings.Fields(tc.want) {
			if !contains(got, w) {
				t.Errorf("%q: %v missing %q", tc.line, got, w)
			}
		}
		if len(strings.Fields(tc.want)) > 0 && len(got) != len(strings.Fields(tc.want)) {
			t.Errorf("%q: got %v, want exactly %q", tc.line, got, tc.want)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
