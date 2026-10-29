package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func setVersionVars(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od := version, commit, date
	version, commit, date = v, c, d
	t.Cleanup(func() { version, commit, date = ov, oc, od })
}

func TestVersionHumanFromLdflags(t *testing.T) {
	setVersionVars(t, "v0.1.0", "abc1234def", "2026-10-29")
	got := resolveVersion().String()
	if got != "agent-tmux v0.1.0 (abc1234, 2026-10-29)" {
		t.Fatalf("got %q", got)
	}
}

func TestVersionJSON(t *testing.T) {
	setVersionVars(t, "v0.1.0", "abc1234", "2026-10-29T10:00:00Z")
	var out, errOut bytes.Buffer
	cmd := versionCommand()
	rest, code, done := cmd.parse([]string{"--json"}, &errOut)
	if done || code != 0 {
		t.Fatalf("parse: %d %s", code, errOut.String())
	}
	inv := &invocation{args: rest, stdout: &out, stderr: &errOut}
	if code := cmd.run(inv); code != 0 {
		t.Fatalf("code %d", code)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"version": "v0.1.0", "commit": "abc1234", "date": "2026-10-29T10:00:00Z"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestVersionFallbackNeverEmpty(t *testing.T) {
	setVersionVars(t, "", "", "")
	info := resolveVersion()
	if info.Version == "" {
		t.Fatal("version is empty")
	}
	if len(info.Commit) > 7 {
		t.Fatalf("commit not shortened: %q", info.Commit)
	}
	if !strings.HasPrefix(info.String(), "agent-tmux "+info.Version) {
		t.Fatalf("got %q", info.String())
	}
}

func TestVersionFinish(t *testing.T) {
	if got := finishVersion(versionInfo{}); got.Version != "dev" {
		t.Fatalf("got %q", got.Version)
	}
	if got := finishVersion(versionInfo{Version: "v1", Commit: "0123456789"}); got.Commit != "0123456" {
		t.Fatalf("got %q", got.Commit)
	}
}
