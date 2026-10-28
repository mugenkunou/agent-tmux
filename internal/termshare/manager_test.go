package termshare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestSessionLifecycleAndControlHandoff(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", shortRuntimeDir(t))
	manager, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const name = "integration"
	if err := manager.Create(ctx, name, "/bin/bash", 80, 24); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Kill(context.Background(), name) })

	if err := manager.Send(ctx, name, `cd /tmp && printf 'STATE:%s\n' "$PWD"`, true); err != nil {
		t.Fatal(err)
	}
	screen, err := manager.WaitFor(ctx, name, "STATE:/tmp", 5*time.Second, 100)
	if err != nil {
		t.Fatalf("shell state did not persist: %v; screen:\n%s", err, screen)
	}

	if err := manager.claim(ctx, name, "test-actor", ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.Send(ctx, name, "echo forbidden", true); !errors.Is(err, ErrControlled) {
		t.Fatalf("write while claimed: got %v, want %v", err, ErrControlled)
	}
	if err := manager.Yield(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := manager.Send(ctx, name, "printf 'YIELDED_OK\\n'", true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WaitFor(ctx, name, "YIELDED_OK", 5*time.Second, 100); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSessionName(t *testing.T) {
	for _, name := range []string{"work", "prod-debug_1", "a.b"} {
		if err := validateSessionName(name); err != nil {
			t.Errorf("valid name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "-bad", "has space", strings.Repeat("a", 65)} {
		if err := validateSessionName(name); err == nil {
			t.Errorf("invalid name %q accepted", name)
		}
	}
}

func TestActorIDPrefersEnvOverride(t *testing.T) {
	t.Setenv("AGENT_TMUX_ACTOR_ID", "alice-laptop")
	if got := actorID(); got != "alice-laptop" {
		t.Fatalf("actorID() = %q, want %q", got, "alice-laptop")
	}
}

func TestActorIDGeneratesDistinctFallback(t *testing.T) {
	t.Setenv("AGENT_TMUX_ACTOR_ID", "")
	first := actorID()
	second := actorID()
	if first == "" || second == "" {
		t.Fatal("actorID() returned an empty id")
	}
	if first == second {
		t.Fatalf("actorID() returned the same id twice: %q", first)
	}
}

func shortRuntimeDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "at-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

// The title must name the session and flip with ownership, and it must be set
// through tmux's own format so it tracks claim/release without extra plumbing.
func TestTitleFormatReflectsSessionAndOwnership(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", shortRuntimeDir(t))
	manager, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const name = "titled"
	if err := manager.Create(ctx, name, "/bin/bash", 80, 24); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Kill(context.Background(), name) })
	if err := manager.applyClientOptions(ctx, name); err != nil {
		t.Fatal(err)
	}

	expand := func() string {
		out, err := manager.run(ctx, nil, "display-message", "-p", "-t", target(name), titleFormat)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	if got := expand(); !strings.Contains(got, "agent-tmux: titled") || !strings.Contains(got, "watching") {
		t.Errorf("unclaimed title = %q", got)
	}
	if err := manager.claim(ctx, name, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if got := expand(); !strings.Contains(got, "agent-tmux: titled") || !strings.Contains(got, "alice controls") {
		t.Errorf("claimed title = %q", got)
	}
	if on, _ := manager.run(ctx, nil, "show-option", "-qv", "-t", exact(name), "set-titles"); strings.TrimSpace(on) != "on" {
		t.Errorf("set-titles = %q, want on", on)
	}
}

// Escape sequences must only ever go to a real terminal, never a pipe/file.
func TestPreserveTerminalTitleIgnoresNonTerminals(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	preserveTerminalTitle(file)()
	if info, _ := file.Stat(); info.Size() != 0 {
		t.Errorf("wrote %d bytes to a non-terminal", info.Size())
	}
}

func newTestSession(t *testing.T, name string) (*Manager, context.Context) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", shortRuntimeDir(t))
	manager, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := manager.Create(ctx, name, "/bin/bash", 80, 24); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Kill(context.Background(), name) })
	return manager, ctx
}

// A claim whose process is gone (its tab was closed before it could release)
// must not lock scripted callers out; a live claimant must.
func TestStaleClaimIsClearedOnSend(t *testing.T) {
	manager, ctx := newTestSession(t, "stale")

	if err := manager.claim(ctx, "stale", "alive", ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.Send(ctx, "stale", "true", true); !errors.Is(err, ErrControlled) {
		t.Fatalf("send under a live claim: got %v, want ErrControlled", err)
	}

	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.run(ctx, nil, "set-option", "-t", "stale", "@agent-tmux-owner-proc",
		strconv.Itoa(dead.Process.Pid)+":1"); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.Status(ctx, "stale"); err != nil || status.Owner != OwnerUnclaimed {
		t.Fatalf("status of a stale claim: owner %q, err %v; want unclaimed", status.Owner, err)
	}
	if err := manager.Send(ctx, "stale", "printf 'STALE_OK\\n'", true); err != nil {
		t.Fatalf("send after the claimant died: %v", err)
	}
	if _, err := manager.WaitFor(ctx, "stale", "STALE_OK", 5*time.Second, 50); err != nil {
		t.Fatal(err)
	}
	if owner, _ := manager.run(ctx, nil, "show-option", "-qv", "-t", "stale", "@agent-tmux-owner"); strings.TrimSpace(owner) != "" {
		t.Errorf("stale owner not cleared: %q", owner)
	}
}

// Ctrl-T, Ctrl-Q and Ctrl-] must end even a read-only client, each with its
// own exit code: that is what makes the menu and quitting work while watching.
func TestKeysEndReadOnlyAttachWithTheirExitCode(t *testing.T) {
	manager, ctx := newTestSession(t, "keys")
	if err := manager.ensureBindings(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  byte
		want int
	}{{0x14, exitToggle}, {0x11, exitQuit}, {0x1d, exitMenu}} {
		code := attachInPTY(t, manager, "keys", tc.key)
		if code != tc.want {
			t.Errorf("key %#x: exit %d, want %d", tc.key, code, tc.want)
		}
	}
}

// attachInPTY attaches read-only on a fresh pty (never the test's own
// terminal, see PROJECT.md §8), presses key, and returns the key exit code.
func attachInPTY(t *testing.T, manager *Manager, name string, key byte) int {
	t.Helper()
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()
	cmd := exec.Command(manager.tmux, append(manager.baseArgs(), "attach-session", "-r", "-t", name)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = append(withoutEnv(os.Environ(), "TMUX"), "TERM=xterm-256color")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, master) }()
	time.Sleep(700 * time.Millisecond)
	if _, err := master.Write([]byte{key}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code, err := attachExit(err)
		if err != nil {
			t.Fatalf("attach failed: %v", err)
		}
		return code
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("key %#x did not end the read-only attach", key)
		return -1
	}
}

func openPTY(t *testing.T) (master, slave *os.File) {
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
	size := struct{ rows, cols, x, y uint16 }{24, 80, 0, 0}
	syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
	return master, slave
}

// Restart must replace a program that ignores input with a fresh shell,
// keeping the session.
func TestRestartReplacesAStuckProgram(t *testing.T) {
	manager, ctx := newTestSession(t, "restart")
	if err := manager.Send(ctx, "restart", "sleep 1000", true); err != nil {
		t.Fatal(err)
	}
	waitCommand := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if s, err := manager.Status(ctx, "restart"); err == nil && s.Command == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		s, _ := manager.Status(ctx, "restart")
		t.Fatalf("pane command = %q, want %q", s.Command, want)
	}
	waitCommand("sleep")
	if err := manager.Restart(ctx, "restart"); err != nil {
		t.Fatal(err)
	}
	waitCommand("bash")
}
