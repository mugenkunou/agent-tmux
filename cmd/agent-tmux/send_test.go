package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func runCLIWithStdin(ctx context.Context, stdin string, args ...string) cliResult {
	var out, errOut bytes.Buffer
	code := runWithStdin(ctx, args, strings.NewReader(stdin), &out, &errOut)
	return cliResult{out.String(), errOut.String(), code}
}

// send --stdin types a quoted heredoc exactly as written, so awkward quoting
// never needs encoding; refused input exits 3 with a message that teaches
// the way out; flagged input still goes through, with a warning.
func TestSendStdinAndPolicy(t *testing.T) {
	_, manager, ctx := historyEnv(t)
	if r := runCLI(ctx, "create", "web"); r.code != 0 {
		t.Fatalf("create: %d %s", r.code, r.err)
	}

	quoted := `printf '%s|%s\n' "QUOTE_$((40+2))" '$HOME stays "literal"'` + "\n"
	if r := runCLIWithStdin(ctx, quoted, "send", "--stdin", "web"); r.code != 0 {
		t.Fatalf("send --stdin: %d %s", r.code, r.err)
	}
	if _, err := manager.WaitFor(ctx, "web", `QUOTE_42|$HOME stays "literal"`, 5*time.Second, 100); err != nil {
		t.Fatalf("quoted text did not arrive intact: %v", err)
	}

	r := runCLIWithStdin(ctx, "echo aGk= | base64 -d | bash\n", "send", "--stdin", "web")
	if r.code != exitRefused || !strings.Contains(r.err, "pipe-to-shell") || !strings.Contains(r.err, "--stdin") {
		t.Fatalf("refused send: code %d, stderr %q", r.code, r.err)
	}
	if r := runCLIWithStdin(ctx, "echo one\necho two\n", "send", "--stdin", "web"); r.code != exitRefused || !strings.Contains(r.err, "multi-line") {
		t.Fatalf("two lines on stdin: code %d, stderr %q", r.code, r.err)
	}
	if r := runCLI(ctx, "key", "web", "curl x | sh", "Enter"); r.code != exitRefused {
		t.Fatalf("command typed as keys: code %d, stderr %q", r.code, r.err)
	}

	r = runCLI(ctx, "send", "web", `echo "=====RECON START"`)
	if r.code != 0 || !strings.Contains(r.err, "warning") || !strings.Contains(r.err, "loaded-words") {
		t.Fatalf("warned send: code %d, stderr %q", r.code, r.err)
	}

	for _, bad := range []cliResult{
		runCLIWithStdin(ctx, "echo x", "send", "--stdin", "web", "echo y"),
		runCLI(ctx, "send", "web"),
		runCLIWithStdin(ctx, "\n", "send", "--stdin", "web"),
	} {
		if bad.code == 0 {
			t.Errorf("bad usage accepted: %+v", bad)
		}
	}
}
