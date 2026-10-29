package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mugenkunou/agent-tmux/internal/recording"
	"github.com/mugenkunou/agent-tmux/internal/termshare"
	"github.com/mugenkunou/agent-tmux/internal/transcript"
)

type cliResult struct {
	out, err string
	code     int
}

func runCLI(ctx context.Context, args ...string) cliResult {
	var out, errOut bytes.Buffer
	code := run(ctx, args, &out, &errOut)
	return cliResult{out.String(), errOut.String(), code}
}

// historyEnv gives a test its own tmux server, recordings root and trash.
func historyEnv(t *testing.T) (root string, manager *termshare.Manager, ctx context.Context) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	runtimeDir, err := os.MkdirTemp("", "at-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", runtimeDir)
	root = t.TempDir()
	t.Setenv("AGENT_TMUX_STATE_DIR", root)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	manager, err = termshare.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() { _, _ = exec.Command("tmux", "-S", manager.SocketPath(), "kill-server").CombinedOutput() })
	return root, manager, ctx
}

// session creates a session, runs command in it and waits until its
// recording holds want.
func session(t *testing.T, ctx context.Context, manager *termshare.Manager, name, command, want string) string {
	t.Helper()
	if err := manager.Create(ctx, name, "/bin/bash", 100, 30); err != nil {
		t.Fatal(err)
	}
	s, err := manager.Status(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Send(ctx, name, command, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(recorded(t, s.Recording), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never recorded %q", name, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s.Recording
}

func recorded(t *testing.T, dir string) string {
	t.Helper()
	rec, err := recording.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all []byte
	for _, p := range rec.Segments {
		data, _ := os.ReadFile(p)
		all = append(all, data...)
	}
	return string(all)
}

// fakeEnded writes an ended recording whose first two segments were rotated
// away, started and ended at the given times.
func fakeEnded(t *testing.T, root, name string, started, ended time.Time, output string) string {
	t.Helper()
	dir, err := recording.Create(root, name, started)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.000003.log"), []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := recording.ReadMeta(dir)
	meta.FirstSegment = 3
	meta.FirstSegmentAt = started.Add(time.Hour).UTC().Format(time.RFC3339)
	meta.EndedAt = ended.UTC().Format(time.RFC3339)
	if err := recording.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLsShowsLiveAndEnded(t *testing.T) {
	root, manager, ctx := historyEnv(t)
	if r := runCLI(ctx, "ls", "--json"); r.code != 0 || r.out != "{\"sessions\":[]}\n" {
		t.Fatalf("empty ls --json = %q (exit %d)", r.out, r.code)
	}
	if r := runCLI(ctx, "ls"); r.out != "no sessions\n" {
		t.Fatalf("empty ls = %q", r.out)
	}

	webDir := session(t, ctx, manager, "web", "echo WEB_$((1+1))", "WEB_2")
	session(t, ctx, manager, "api", "echo API_$((1+1))", "API_2")
	oldDir := session(t, ctx, manager, "old", "echo OLD_$((1+1))", "OLD_2")
	if err := manager.Kill(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	ancient := fakeEnded(t, root, "ancient", time.Now().Add(-72*time.Hour), time.Now().Add(-48*time.Hour), "x\n")
	// A recording whose session vanished without kill: ls finalizes it.
	stale := fakeEnded(t, root, "stale", time.Now().Add(-96*time.Hour), time.Now(), "y\n")
	meta, _ := recording.ReadMeta(stale)
	meta.EndedAt = ""
	_ = recording.WriteMeta(stale, meta)

	r := runCLI(ctx, "ls", "--json")
	if r.code != 0 {
		t.Fatalf("ls --json: exit %d: %s", r.code, r.err)
	}
	var raw struct{ Sessions []map[string]any }
	if err := json.Unmarshal([]byte(r.out), &raw); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields("name id state recorder owner clients command path host started_at ended_at history_starts_at bytes")
	var order []string
	for _, e := range raw.Sessions {
		for _, f := range fields {
			if _, ok := e[f]; !ok {
				t.Errorf("entry %v lacks %q", e["name"], f)
			}
		}
		if len(e) != len(fields) {
			t.Errorf("entry %v has %d fields, want %d", e["name"], len(e), len(fields))
		}
		order = append(order, e["name"].(string)+":"+e["state"].(string))
	}
	// Live by name, then ended newest started_at first.
	if got := strings.Join(order, " "); got != "api:live web:live old:ended ancient:ended stale:ended" {
		t.Errorf("ls order = %s", got)
	}
	web, old := raw.Sessions[1], raw.Sessions[2]
	if web["id"] != filepath.Base(webDir) || web["recorder"] != "running" || web["path"] == "" || web["command"] == "" ||
		web["started_at"] == "" || web["ended_at"] != "" || web["history_starts_at"] == "" || web["bytes"].(float64) <= 0 {
		t.Errorf("live entry = %v", web)
	}
	if old["id"] != filepath.Base(oldDir) || old["recorder"] != "" || old["owner"] != "" || old["clients"] != float64(0) ||
		old["command"] != "" || old["path"] != "" || old["ended_at"] == "" || old["bytes"].(float64) <= 0 {
		t.Errorf("ended entry = %v", old)
	}
	if raw.Sessions[4]["ended_at"] == "" {
		t.Error("ls did not finalize the stale recording")
	}
	_ = ancient

	human := runCLI(ctx, "ls").out
	lines := strings.Split(strings.TrimSpace(human), "\n")
	if len(lines) != 6 || strings.Join(strings.Fields(lines[0]), " ") != "SESSION STATE REC OWNER CLIENTS STARTED" {
		t.Fatalf("ls:\n%s", human)
	}
	if f := strings.Fields(lines[2]); f[0] != "web" || f[1] != "live" || f[2] != "running" || f[3] != "(unclaimed)" || f[4] != "0" {
		t.Errorf("live row %q", lines[2])
	}
	if f := strings.Fields(lines[3]); f[0] != filepath.Base(oldDir) || f[1] != "ended" {
		t.Errorf("ended row %q", lines[3])
	}
}

func TestShowResolvesAndRenders(t *testing.T) {
	root, manager, ctx := historyEnv(t)
	webDir := session(t, ctx, manager, "web", "printf '\\033[31mRED_%s\\033[0m\\n' $((1+1))", "RED_2")
	oldDir := session(t, ctx, manager, "old", "echo FIRST_$((1+1))", "FIRST_2")
	_ = manager.Kill(ctx, "old")
	newDir := session(t, ctx, manager, "old", "echo SECOND_$((1+1))", "SECOND_2")
	_ = manager.Kill(ctx, "old")
	rotated := fakeEnded(t, root, "rot", time.Now().Add(-2*time.Hour), time.Now(), "kept\r\n")

	plain := runCLI(ctx, "show", "web")
	if plain.code != 0 || !strings.Contains(plain.out, "RED_2\n") || strings.Contains(plain.out, "\x1b") {
		t.Errorf("show web (piped) = %q, exit %d, %s", plain.out, plain.code, plain.err)
	}
	if raw := runCLI(ctx, "show", "--raw", "web"); raw.out != recorded(t, webDir) || !strings.Contains(raw.out, "\x1b[31m") {
		t.Errorf("show --raw is not the recorded bytes: %q", raw.out)
	}
	// A name of ended recordings means the newest; an id means that one.
	if r := runCLI(ctx, "show", "old"); !strings.Contains(r.out, "SECOND_2") || strings.Contains(r.out, "FIRST_2") {
		t.Errorf("show old = %q", r.out)
	}
	if r := runCLI(ctx, "show", filepath.Base(oldDir)); !strings.Contains(r.out, "FIRST_2") {
		t.Errorf("show <id> = %q", r.out)
	}
	_ = newDir
	r := runCLI(ctx, "show", "rot")
	if !strings.HasPrefix(r.out, "-- history starts at ") || !strings.HasSuffix(r.out, "older output was rotated away --\nkept\n") {
		t.Errorf("show of a rotated recording = %q", r.out)
	}
	if r := runCLI(ctx, "show", "--raw", "rot"); r.out != "kept\r\n" {
		t.Errorf("show --raw of a rotated recording = %q", r.out)
	}
	if r := runCLI(ctx, "show", "nope"); r.code != 1 || !strings.Contains(r.err, "no recording") {
		t.Errorf("show nope: exit %d, %q", r.code, r.err)
	}
	_ = rotated
}

func TestGrep(t *testing.T) {
	_, manager, ctx := historyEnv(t)
	oldDir := session(t, ctx, manager, "old", "echo NEEDLE_$((1+1)) one", "NEEDLE_2 one")
	_ = manager.Kill(ctx, "old")
	time.Sleep(1100 * time.Millisecond) // started_at has second precision
	webDir := session(t, ctx, manager, "web", "echo NEEDLE_$((1+1)) two; echo needle_$((1+1)) three", "needle_2 three")

	r := runCLI(ctx, "grep", "needle_2")
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	webID, oldID := filepath.Base(webDir), filepath.Base(oldDir)
	if r.code != 0 || len(lines) != 3 ||
		!strings.HasPrefix(lines[0], webID+":") || !strings.HasSuffix(lines[0], ": needle_2 three") ||
		!strings.HasSuffix(lines[1], ": NEEDLE_2 two") ||
		!strings.HasPrefix(lines[2], oldID+":") || !strings.HasSuffix(lines[2], ": NEEDLE_2 one") {
		t.Fatalf("grep (exit %d):\n%s%s", r.code, r.out, r.err)
	}
	// The line number is the line in the plain rendering of the recording.
	var rendered bytes.Buffer
	_ = transcript.Render(&rendered, strings.NewReader(recorded(t, webDir)), false)
	n, _ := strconv.Atoi(strings.Split(lines[0], ":")[1])
	if got := strings.Split(rendered.String(), "\n")[n-1]; got != "needle_2 three" {
		t.Errorf("line %d of the rendering is %q", n, got)
	}

	// Words are joined with one space.
	if r := runCLI(ctx, "grep", "NEEDLE_2", "one"); r.code != 0 || strings.Count(r.out, "\n") != 1 {
		t.Errorf("grep two words = %q", r.out)
	}

	var res struct {
		Matches []struct {
			Name, ID, Text string
			Line           int
		}
		Truncated bool
	}
	r = runCLI(ctx, "grep", "--json", "--limit", "2", "needle_2")
	if err := json.Unmarshal([]byte(r.out), &res); err != nil || r.code != 0 {
		t.Fatalf("grep --json: %v, exit %d, %q", err, r.code, r.out)
	}
	if len(res.Matches) != 2 || !res.Truncated || res.Matches[0].Name != "web" || res.Matches[0].ID != webID || res.Matches[0].Line < 1 {
		t.Errorf("grep --json --limit 2 = %+v", res)
	}
	r = runCLI(ctx, "grep", "--json", "--limit", "3", "needle_2")
	if json.Unmarshal([]byte(r.out), &res); len(res.Matches) != 3 || res.Truncated {
		t.Errorf("grep --json --limit 3 = %+v", res)
	}
	if r := runCLI(ctx, "grep", "--session", "old", "needle_2"); r.code != 0 || strings.Count(r.out, "\n") != 1 || !strings.HasPrefix(r.out, oldID) {
		t.Errorf("grep --session old = %q", r.out)
	}
	if r := runCLI(ctx, "grep", "no-such-text"); r.code != 1 || r.out != "" {
		t.Errorf("grep without matches: exit %d, %q", r.code, r.out)
	}
	if r := runCLI(ctx, "grep", "--json", "no-such-text"); r.code != 0 || r.out != "{\"matches\":[],\"truncated\":false}\n" {
		t.Errorf("grep --json without matches: exit %d, %q", r.code, r.out)
	}
	if r := runCLI(ctx, "grep", "--limit", "0", "x"); r.code != 2 {
		t.Errorf("grep --limit 0: exit %d", r.code)
	}
}

func TestRm(t *testing.T) {
	root, manager, ctx := historyEnv(t)
	session(t, ctx, manager, "web", "echo W", "W")
	webDir, _ := manager.Status(ctx, "web")
	session(t, ctx, manager, "old", "echo O", "O")
	_ = manager.Kill(ctx, "old")
	oldRec, _ := recording.Find(root, "old")
	ancient := fakeEnded(t, root, "ancient", time.Now().Add(-72*time.Hour), time.Now().Add(-48*time.Hour), "x\n")
	exists := func(dir string) bool { _, err := os.Stat(dir); return err == nil }

	if r := runCLI(ctx, "rm"); r.code != 2 {
		t.Errorf("rm with nothing: exit %d", r.code)
	}
	if r := runCLI(ctx, "rm", "--older-than", "soon"); r.code != 2 {
		t.Errorf("rm --older-than soon: exit %d", r.code)
	}
	// A live session anywhere in the call refuses the whole call.
	for _, args := range [][]string{{"web"}, {oldRec.Meta.ID, filepath.Base(webDir.Recording)}} {
		r := runCLI(ctx, append([]string{"rm"}, args...)...)
		if r.code != 1 || !strings.Contains(r.err, "web is live; kill it first (agent-tmux kill web)") {
			t.Errorf("rm %v: exit %d, %q", args, r.code, r.err)
		}
	}
	if !exists(oldRec.Dir) || !exists(webDir.Recording) {
		t.Fatal("a refused rm moved something")
	}
	if r := runCLI(ctx, "rm", "nope"); r.code != 1 {
		t.Errorf("rm nope: exit %d", r.code)
	}

	// --older-than alone: only what ended long enough ago.
	r := runCLI(ctx, "rm", "--older-than", "1d")
	if r.code != 0 || r.out != "moved to trash: "+filepath.Base(ancient)+"\n" || exists(ancient) || !exists(oldRec.Dir) {
		t.Fatalf("rm --older-than 1d: exit %d, %q %q", r.code, r.out, r.err)
	}
	trashed := filepath.Join(os.Getenv("XDG_DATA_HOME"), "Trash", "files", filepath.Base(ancient))
	if !exists(filepath.Join(trashed, "meta.json")) {
		t.Error("recording not in the trash")
	}
	// With names, --older-than filters them.
	if r := runCLI(ctx, "rm", "--older-than", "1d", "old"); r.code != 0 || r.out != "" || !exists(oldRec.Dir) {
		t.Errorf("rm --older-than 1d old: exit %d, %q", r.code, r.out)
	}
	if r := runCLI(ctx, "rm", "old"); r.code != 0 || r.out != "moved to trash: "+oldRec.Meta.ID+"\n" || exists(oldRec.Dir) {
		t.Errorf("rm old: exit %d, %q %q", r.code, r.out, r.err)
	}
	if !exists(webDir.Recording) {
		t.Error("the live recording was moved")
	}
}

// On a terminal, show pages the colored rendering with less, and Ctrl-] [/]
// shows the same from inside open and then re-attaches. Without less it
// prints the colored rendering.
func TestShowOnATerminal(t *testing.T) {
	if _, err := exec.LookPath("less"); err != nil {
		t.Skip("less is not installed")
	}
	_, manager, ctx := historyEnv(t)
	binary := filepath.Join(t.TempDir(), "agent-tmux")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	session(t, ctx, manager, "web", "printf '\\033[31mRED_%s\\033[0m\\n' $((1+1))", "RED_2")
	runtimeDir := os.Getenv("AGENT_TMUX_RUNTIME_DIR")

	tab := startOpen(t, binary, runtimeDir, "viewer", "open", "web")
	tab.collect()
	time.Sleep(1200 * time.Millisecond)
	tab.write(t, "\x1d") // Ctrl-]
	tab.waitFor(t, "[/] history")
	tab.write(t, "/")
	tab.waitFor(t, "\x1b[31mRED_2")
	tab.reset()
	tab.write(t, "q") // leave less: open re-attaches
	tab.waitFor(t, "-- watching web")
	time.Sleep(1200 * time.Millisecond)
	tab.write(t, "\x11")
	tab.wait(t)

	// No less on PATH: the colored rendering is printed.
	t.Setenv("PATH", filepath.Dir(binary))
	plain := startOpen(t, binary, runtimeDir, "viewer", "show", "web")
	plain.collect()
	plain.waitFor(t, "\x1b[31mRED_2")
	plain.wait(t)
}
