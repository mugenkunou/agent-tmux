package termshare

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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
	if err := manager.applyTitle(ctx, name); err != nil {
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
