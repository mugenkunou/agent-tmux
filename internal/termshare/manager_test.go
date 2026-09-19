package termshare

import (
	"context"
	"errors"
	"fmt"
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

	result, err := manager.RunCommand(ctx, name, `cd /tmp`, 5*time.Second, 100)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("change directory: result=%+v err=%v", result, err)
	}
	result, err = manager.RunCommand(ctx, name, `printf 'STATE:%s\n' "$PWD"`, 5*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Screen, "STATE:/tmp") {
		t.Fatalf("shell state did not persist; screen:\n%s", result.Screen)
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

func TestRunCommandReturnsExitStatus(t *testing.T) {
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
	name := fmt.Sprintf("exit-status-%d", time.Now().UnixNano())
	if err := manager.Create(ctx, name, "/bin/bash", 80, 24); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Kill(context.Background(), name) })

	result, err := manager.RunCommand(ctx, name, "false", 5*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", result.ExitCode)
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
