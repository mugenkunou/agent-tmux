package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"agent-tmux/internal/termshare"
)

// One writer at a time is the reason agent-tmux exists. This drives two
// real `open` processes on two separate ptys, one plain (tmux attach) and
// one native (control mode), and checks that whichever holds control is
// the only one whose typing reaches the shell, that the other's Ctrl-T is
// refused rather than taking over, and that `yield` stops a native writer
// on its next keystroke.
func TestOnlyOneTerminalControlsAcrossOpenModes(t *testing.T) {
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

	plain := startOpen(t, binary, runtimeDir, "plain-tab", "open", name)
	native := startOpen(t, binary, runtimeDir, "native-tab", "open", "--native", name)
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

	// The plain tab takes control; the native tab's Ctrl-T is refused.
	plain.write(t, "\x14")
	settle()
	if got := owner(); got != "plain-tab" {
		t.Fatalf("after plain Ctrl-T: owner %q, want plain-tab", got)
	}
	native.write(t, "\x14")
	settle()
	if got := owner(); got != "plain-tab" {
		t.Fatalf("native Ctrl-T took control from a live holder: owner %q", got)
	}
	if typed(native) {
		t.Fatal("native tab typed into the session while the plain tab held control")
	}
	if !typed(plain) {
		t.Fatal("the controlling plain tab could not type")
	}

	// Swap: plain releases, native takes, plain's Ctrl-T is refused.
	plain.write(t, "\x14")
	settle()
	if got := owner(); got != termshare.OwnerUnclaimed {
		t.Fatalf("after plain released: owner %q, want unclaimed", got)
	}
	native.write(t, "\x14")
	settle()
	if got := owner(); got != "native-tab" {
		t.Fatalf("after native Ctrl-T: owner %q, want native-tab", got)
	}
	plain.write(t, "\x14")
	settle()
	if got := owner(); got != "native-tab" {
		t.Fatalf("plain Ctrl-T took control from a live holder: owner %q", got)
	}
	if typed(plain) {
		t.Fatal("plain tab typed into the session while the native tab held control")
	}
	if !typed(native) {
		t.Fatal("the controlling native tab could not type")
	}

	// yield takes control away; the native tab's next keystrokes are dropped.
	if err := manager.Yield(ctx, name); err != nil {
		t.Fatal(err)
	}
	if typed(native) {
		t.Fatal("native tab kept typing after yield")
	}

	plain.write(t, "\x11")
	native.write(t, "\x11")
	plain.wait(t)
	native.wait(t)
}

type openTab struct {
	actor  string
	master *os.File
	cmd    *exec.Cmd
}

func startOpen(t *testing.T, binary, runtimeDir, actor string, args ...string) *openTab {
	t.Helper()
	master, slave := openTestPTY(t)
	cmd := exec.Command(binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	env := []string{"TERM=xterm-256color", "AGENT_TMUX_RUNTIME_DIR=" + runtimeDir, "AGENT_TMUX_ACTOR_ID=" + actor}
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
	go func() { _, _ = io.Copy(io.Discard, master) }()
	tab := &openTab{actor: actor, master: master, cmd: cmd}
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
