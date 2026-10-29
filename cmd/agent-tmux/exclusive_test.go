package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/mugenkunou/agent-tmux/internal/termshare"
)

// One writer at a time is the reason agent-tmux exists. This drives two
// real `open` processes on two separate ptys and checks that whichever holds
// control is the only one whose typing reaches the shell, that the other's
// Ctrl-T is refused rather than taking over, and that `yield` puts the
// writer back to watching.
func TestOnlyOneTerminalControls(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "agent-tmux")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	runtimeDir, err := os.MkdirTemp("", "at-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", runtimeDir)

	manager, err := termshare.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const name = "exclusive"
	if err := manager.Create(ctx, name, "/bin/bash", 100, 30); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Kill(context.Background(), name) })

	first := startOpen(t, binary, runtimeDir, "first-tab", "open", name)
	second := startOpen(t, binary, runtimeDir, "second-tab", "open", name)
	time.Sleep(1500 * time.Millisecond)

	owner := func() string {
		t.Helper()
		s, err := manager.Status(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return s.Owner
	}
	settle := func() { time.Sleep(900 * time.Millisecond) }
	seq := 0
	// typed reports whether a line typed into tab reaches the shell.
	typed := func(tab *openTab) bool {
		t.Helper()
		seq++
		marker := fmt.Sprintf("MARK_%s_%d", strings.ReplaceAll(tab.actor, "-", "_"), seq)
		tab.write(t, "echo "+marker+"\r")
		settle()
		screen, err := manager.Screen(ctx, name, 200)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(screen, marker)
	}

	// The first tab takes control; the second tab's Ctrl-T is refused.
	first.write(t, "\x14")
	settle()
	if got := owner(); got != "first-tab" {
		t.Fatalf("after first Ctrl-T: owner %q, want first-tab", got)
	}
	second.write(t, "\x14")
	settle()
	if got := owner(); got != "first-tab" {
		t.Fatalf("second Ctrl-T took control from a live holder: owner %q", got)
	}
	if typed(second) {
		t.Fatal("second tab typed into the session while the first tab held control")
	}
	if !typed(first) {
		t.Fatal("the controlling first tab could not type")
	}

	// Swap: first releases, second takes, first's Ctrl-T is refused.
	first.write(t, "\x14")
	settle()
	if got := owner(); got != termshare.OwnerUnclaimed {
		t.Fatalf("after first released: owner %q, want unclaimed", got)
	}
	second.write(t, "\x14")
	settle()
	if got := owner(); got != "second-tab" {
		t.Fatalf("after second Ctrl-T: owner %q, want second-tab", got)
	}
	first.write(t, "\x14")
	settle()
	if got := owner(); got != "second-tab" {
		t.Fatalf("first Ctrl-T took control from a live holder: owner %q", got)
	}
	if typed(first) {
		t.Fatal("first tab typed into the session while the second tab held control")
	}
	if !typed(second) {
		t.Fatal("the controlling second tab could not type")
	}

	// yield takes control away; the second tab re-attaches read-only.
	if err := manager.Yield(ctx, name); err != nil {
		t.Fatal(err)
	}
	settle()
	if got := owner(); got != termshare.OwnerUnclaimed {
		t.Fatalf("after yield: owner %q, want unclaimed", got)
	}
	if typed(second) {
		t.Fatal("second tab kept typing after yield")
	}

	first.write(t, "\x11")
	second.write(t, "\x11")
	first.wait(t)
	second.wait(t)

	// Every handoff above is in the recording's event log.
	status, err := manager.Status(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	logged := map[string]int{}
	for _, e := range readEvents(t, status.Recording) {
		key := fmt.Sprint(e["event"], " ", e["actor"])
		if e["mode"] != nil {
			key += " " + fmt.Sprint(e["mode"])
		}
		if e["by"] != nil {
			key += " by " + fmt.Sprint(e["by"])
		}
		logged[key]++
	}
	for _, want := range []string{
		"attach first-tab watch", "attach first-tab control", "detach first-tab control",
		"attach second-tab watch", "attach second-tab control", "detach second-tab watch",
		"claim first-tab", "release first-tab",
		"claim second-tab", "release second-tab by yield",
	} {
		if logged[want] == 0 {
			t.Errorf("event %q not logged; have %v", want, logged)
		}
	}
	if logged["claim first-tab"] != 1 || logged["claim second-tab"] != 1 {
		t.Errorf("a refused Ctrl-T was logged as a claim: %v", logged)
	}
}

func readEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("bad event line %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

type openTab struct {
	actor  string
	master *os.File
	cmd    *exec.Cmd

	mu     sync.Mutex
	output bytes.Buffer // filled once collect is called
	drain  chan bool    // collect: keep the output instead of discarding it
}

func startOpen(t *testing.T, binary, runtimeDir, actor string, args ...string) *openTab {
	t.Helper()
	master, slave := openTestPTY(t)
	cmd := exec.Command(binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	env := []string{"TERM=xterm-256color", "AGENT_TMUX_RUNTIME_DIR=" + runtimeDir, "AGENT_TMUX_ACTOR_ID=" + actor,
		"AGENT_TMUX_STATE_DIR=" + os.Getenv("AGENT_TMUX_STATE_DIR")}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "AGENT_TMUX_") && !strings.HasPrefix(kv, "TERM=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	tab := &openTab{actor: actor, master: master, cmd: cmd, drain: make(chan bool, 1)}
	go func() {
		keep := false
		buf := make([]byte, 4096)
		for {
			select {
			case keep = <-tab.drain:
			default:
			}
			n, err := master.Read(buf)
			if keep {
				tab.mu.Lock()
				tab.output.Write(buf[:n])
				tab.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = master.Close()
	})
	return tab
}

func (tab *openTab) write(t *testing.T, s string) {
	t.Helper()
	if _, err := tab.master.Write([]byte(s)); err != nil {
		t.Fatalf("%s: write: %v", tab.actor, err)
	}
}

// collect keeps the tab's output from now on, for waitFor.
func (tab *openTab) collect() { tab.drain <- true }

// reset forgets the output collected so far.
func (tab *openTab) reset() {
	tab.mu.Lock()
	tab.output.Reset()
	tab.mu.Unlock()
}

// waitFor waits until the tab's output contains want.
func (tab *openTab) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tab.mu.Lock()
		found := bytes.Contains(tab.output.Bytes(), []byte(want))
		tab.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			tab.mu.Lock()
			defer tab.mu.Unlock()
			t.Fatalf("%s: output never contained %q:\n%q", tab.actor, want, tab.output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (tab *openTab) wait(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- tab.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("%s: open did not exit on Ctrl-Q", tab.actor)
		_ = tab.cmd.Process.Kill()
	}
}

// openTestPTY opens a pty pair sized 100x30. Tests attach on their own pty,
// never the test runner's terminal (PROJECT.md §8).
func openTestPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pty available:", err)
	}
	unlock := 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatal(errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	size := struct{ rows, cols, x, y uint16 }{30, 100, 0, 0}
	syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
	return master, slave
}
