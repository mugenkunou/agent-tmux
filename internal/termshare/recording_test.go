package termshare

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mugenkunou/agent-tmux/internal/recording"
)

// Everything the pane prints is recorded from its first byte: login output
// (printed before anyone can send a command), send output, and output after a
// restart. kill returns with the recording finished.
func TestRecordingFollowsTheSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	// The login shell reads ~/.bash_profile at once; the tmux server is
	// started by this test, so it inherits this HOME.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("printf 'LOGIN_%s\\n' BANNER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", shortRuntimeDir(t))
	manager, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const name = "rec"
	if err := manager.CreateWithHistory(ctx, name, "/bin/bash", 80, 24, 1234); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Kill(context.Background(), name) })

	status, err := manager.Status(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if status.Recorder != RecorderRunning || status.Recording == "" {
		t.Fatalf("status = %+v, want a running recorder", status)
	}
	dir := status.Recording
	root, _ := recording.Root()
	if filepath.Dir(dir) != root {
		t.Errorf("recording %s is not under %s", dir, root)
	}
	waitLog(t, dir, "LOGIN_BANNER")

	// The real shell replaced the placeholder without losing what create
	// set up: working directory, history limit, start command.
	cwd, _ := os.Getwd()
	if status.Path != cwd {
		t.Errorf("shell started in %q, want %q", status.Path, cwd)
	}
	if got, _ := manager.run(ctx, nil, "display-message", "-p", "-t", target(name), "#{history_limit}|#{pane_start_command}"); strings.TrimSpace(got) != `1234|"exec '/bin/bash' -l"` {
		t.Errorf("history_limit|pane_start_command = %q", got)
	}

	if err := manager.Send(ctx, name, "echo SENT_$((6*7))", true); err != nil {
		t.Fatal(err)
	}
	waitLog(t, dir, "SENT_42")
	if err := manager.SendKeys(ctx, name, []string{"Enter"}); err != nil {
		t.Fatal(err)
	}

	if err := manager.Send(ctx, name, "cd / && sleep 1000", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.Restart(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := manager.Send(ctx, name, "echo AFTER_$((2*3)) $PWD", true); err != nil {
		t.Fatal(err)
	}
	waitLog(t, dir, "AFTER_6 "+cwd)
	if s, _ := manager.Status(ctx, name); s.Recorder != RecorderRunning {
		t.Errorf("recorder after restart: %q", s.Recorder)
	}

	if err := manager.Kill(ctx, name); err != nil {
		t.Fatal(err)
	}
	meta, err := recording.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if meta.EndedAt == "" || meta.Name != name || meta.ID != filepath.Base(dir) {
		t.Errorf("meta after kill = %+v", meta)
	}
	if recording.RecorderRunning(dir) {
		t.Error("recorder still running after kill returned")
	}
	got := eventNames(t, dir)
	want := "create send key send respawn send kill"
	if strings.Join(got, " ") != want {
		t.Errorf("events = %v, want %s", got, want)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "events.jsonl")); bytes.Contains(raw, []byte("SENT_")) {
		t.Error("events.jsonl contains sent text")
	}
}

// A dead recorder must show, not fail silently, and kill then finishes the
// recording itself. A recording left behind with no session and no recorder
// is finalized as stale.
func TestDeadRecorderIsVisibleAndFinalized(t *testing.T) {
	manager, ctx := newTestSession(t, "dead")
	status, err := manager.Status(ctx, "dead")
	if err != nil {
		t.Fatal(err)
	}
	dir := status.Recording
	killRecorder(t, dir)
	if s, _ := manager.Status(ctx, "dead"); s.Recorder != RecorderStopped {
		t.Fatalf("recorder state after its death = %q, want stopped", s.Recorder)
	}
	// tmux itself notices once the pane writes again.
	if err := manager.Send(ctx, "dead", "echo more", true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := manager.run(ctx, nil, "display-message", "-p", "-t", target("dead"), "#{pane_pipe}")
		if strings.TrimSpace(out) == "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pane_pipe stayed 1 after the recorder died")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := manager.Kill(ctx, "dead"); err != nil {
		t.Fatal(err)
	}
	if meta, _ := recording.ReadMeta(dir); meta.EndedAt == "" {
		t.Error("kill did not finalize a recording whose recorder was dead")
	}

	// Stale: the recorder died and the server went away without kill.
	if err := manager.Create(ctx, "stale", "/bin/bash", 80, 24); err != nil {
		t.Fatal(err)
	}
	stale := manager.recordingDir(ctx, "stale")
	killRecorder(t, stale)
	if _, err := manager.run(ctx, nil, "kill-server"); err != nil {
		t.Fatal(err)
	}
	if meta, _ := recording.ReadMeta(stale); meta.EndedAt != "" {
		t.Fatal("ended_at written by a dead recorder")
	}
	root, _ := recording.Root()
	if err := recording.FinalizeStale(root, nil); err != nil {
		t.Fatal(err)
	}
	if meta, _ := recording.ReadMeta(stale); meta.EndedAt == "" {
		t.Error("stale recording not finalized")
	}
}

// If create fails before the shell starts, nothing is left behind.
func TestFailedCreateLeavesNoRecording(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", shortRuntimeDir(t))
	root := t.TempDir()
	t.Setenv("AGENT_TMUX_STATE_DIR", root)
	manager, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	manager.executable = "/nonexistent/agent-tmux"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := manager.Create(ctx, "broken", "/bin/bash", 80, 24); err == nil || !strings.Contains(err.Error(), "recorder did not start") {
		t.Fatalf("create with a broken recorder: %v", err)
	}
	if exists, _ := manager.HasSession(ctx, "broken"); exists {
		t.Error("session left behind")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("recordings left behind: %v", entries)
	}
	_, _ = manager.run(ctx, nil, "kill-server")
}

// The status bar shows the recorder state. It can only be checked on a real
// client's output (PROJECT.md §4: display-message does not expand the nested
// formats of a stored option).
func TestStatusBarShowsRecorderState(t *testing.T) {
	manager, ctx := newTestSession(t, "bar")
	if err := manager.applyClientOptions(ctx, "bar"); err != nil {
		t.Fatal(err)
	}
	master, slave := openPTY(t)
	defer master.Close()
	cmd := exec.Command(manager.tmux, append(manager.baseArgs(), "-u", "attach-session", "-r", "-t", "bar")...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = append(withoutEnv(os.Environ(), "TMUX"), "TERM=xterm-256color")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var mu sync.Mutex
	var screen bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			mu.Lock()
			screen.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	waitOutput := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			found := bytes.Contains(screen.Bytes(), []byte(want))
			mu.Unlock()
			if found {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("client never showed %q; output:\n%q", want, screen.String())
	}
	waitOutput("● REC")
	waitOutput("OPEN - Ctrl-T takes control")

	killRecorder(t, manager.recordingDir(ctx, "bar"))
	if err := manager.Send(ctx, "bar", "echo poke", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := manager.run(ctx, nil, "refresh-client", "-S"); err != nil {
		t.Fatal(err)
	}
	waitOutput("REC STOPPED")
}

func TestTitleShowsRecorderState(t *testing.T) {
	manager, ctx := newTestSession(t, "rectitle")
	expand := func() string {
		out, err := manager.run(ctx, nil, "display-message", "-p", "-t", target("rectitle"), titleFormat)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	if got := expand(); !strings.HasSuffix(got, "(watching) ● REC") {
		t.Errorf("title while recording = %q", got)
	}
	killRecorder(t, manager.recordingDir(ctx, "rectitle"))
	_ = manager.Send(ctx, "rectitle", "echo poke", true)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.HasSuffix(expand(), "(watching) REC STOPPED") {
		if time.Now().After(deadline) {
			t.Fatalf("title after the recorder died = %q", expand())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := manager.run(ctx, nil, "set-option", "-t", "rectitle", "@agent-tmux-recording", ""); err != nil {
		t.Fatal(err)
	}
	if got := expand(); !strings.HasSuffix(got, "(watching)") {
		t.Errorf("title without a recording = %q", got)
	}
}

// killRecorder SIGKILLs the recorder of dir, found by its command line.
func killRecorder(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if bytes.HasSuffix(cmdline, []byte("\x00_record\x00"+dir+"\x00")) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			if !recording.WaitRecorder(dir, false, 3*time.Second) {
				t.Fatal("recorder still holds its lock after SIGKILL")
			}
			return
		}
	}
	t.Fatalf("no recorder process for %s", dir)
}

// waitLog waits until the recording's segments contain text.
func waitLog(t *testing.T, dir, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var all []byte
	for time.Now().Before(deadline) {
		rec, err := recording.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		all = nil
		for _, path := range rec.Segments {
			data, _ := os.ReadFile(path)
			all = append(all, data...)
		}
		if bytes.Contains(all, []byte(text)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("recording never contained %q; it has:\n%q", text, all)
}

func eventNames(t *testing.T, dir string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	decoder := json.NewDecoder(f)
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err == io.EOF {
			return names
		} else if err != nil {
			t.Fatal(err)
		}
		names = append(names, event["event"].(string))
	}
}

// tmux expands formats in the pipe-pane command, so a # in the recordings
// path (possible through XDG_STATE_HOME) must reach the recorder intact.
func TestRecordingPathWithHash(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	root := filepath.Join(t.TempDir(), "a#b #{session_name}")
	t.Setenv("AGENT_TMUX_STATE_DIR", root)
	manager, ctx := newTestSession(t, "hash")
	s, err := manager.Status(ctx, "hash")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(s.Recording) != root || s.Recorder != RecorderRunning {
		t.Fatalf("status = %+v, want a running recorder under %s", s, root)
	}
	if err := manager.Send(ctx, "hash", "echo HASH_$((1+1))", true); err != nil {
		t.Fatal(err)
	}
	waitLog(t, s.Recording, "HASH_2")
}
