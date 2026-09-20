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
