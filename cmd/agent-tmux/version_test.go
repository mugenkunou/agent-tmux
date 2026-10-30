package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func fakeLatest(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing User-Agent")
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	old := latestReleaseURL
	latestReleaseURL = srv.URL
	t.Cleanup(func() { latestReleaseURL = old; srv.Close() })
}

func TestVersionCheckStatuses(t *testing.T) {
	cases := []struct {
		name, installed, status, text string
		code                          int
		body                          string
	}{
		{"current", "v0.19.0", "current", "agent-tmux v0.19.0 (latest v0.19.0)", 200, `{"tag_name":"v0.19.0"}`},
		{"ahead", "v0.20.0", "current", "", 200, `{"tag_name":"v0.19.0"}`},
		{"outdated", "v0.19.0", "outdated", "agent-tmux v0.19.0: update available, latest is v0.20.0\nupgrade:", 200, `{"tag_name":"v0.20.0"}`},
		{"outdated numeric", "v0.9.0", "outdated", "", 200, `{"tag_name":"v0.10.0"}`},
		{"dev", "dev", "dev", "agent-tmux dev: local build is a dev build; upstream release is v0.20.0", 200, `{"tag_name":"v0.20.0"}`},
		{"describe", "v0.19.0-3-gabc1234-dirty", "dev", "", 200, `{"tag_name":"v0.20.0"}`},
		{"server error", "v0.19.0", "unknown", "agent-tmux v0.19.0: could not check latest release: GitHub returned 500", 500, `oops`},
		{"bad json", "v0.19.0", "unknown", "", 200, `not json`},
		{"bad tag", "v0.19.0", "unknown", "", 200, `{"tag_name":"nightly"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeLatest(t, c.code, c.body)
			res := checkRelease(context.Background(), c.installed)
			if res.Status != c.status {
				t.Fatalf("status %q, want %q (%+v)", res.Status, c.status, res)
			}
			if (res.Status == "unknown") != (res.Error != "") {
				t.Fatalf("error field wrong: %+v", res)
			}
			if !strings.HasPrefix(res.text(), c.text) {
				t.Fatalf("text %q, want prefix %q", res.text(), c.text)
			}
		})
	}
}

func TestVersionCheckNetworkFailureIsUnknown(t *testing.T) {
	old := latestReleaseURL
	latestReleaseURL = "http://127.0.0.1:1/"
	t.Cleanup(func() { latestReleaseURL = old })
	if res := checkRelease(context.Background(), "v0.19.0"); res.Status != "unknown" || res.Latest != "" || res.Error == "" {
		t.Fatalf("%+v", res)
	}
}

func runVersion(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := versionCommand()
	rest, code, done := cmd.parse(args, &errOut)
	if done || code != 0 {
		t.Fatalf("parse: %d %s", code, errOut.String())
	}
	code = cmd.run(&invocation{ctx: context.Background(), args: rest, stdout: &out, stderr: &errOut})
	return out.String(), code
}

func TestVersionCheckJSONShape(t *testing.T) {
	setVersionVars(t, "v0.19.0", "abc1234", "2026-10-29")
	fakeLatest(t, 200, `{"tag_name":"v0.20.0"}`)
	out, code := runVersion(t, "--check", "--json")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["version"] != "v0.19.0" || got["latest"] != "v0.20.0" || got["status"] != "outdated" {
		t.Fatalf("got %v", got)
	}

	fakeLatest(t, 500, "x")
	out, code = runVersion(t, "--check", "--json")
	if code != 0 {
		t.Fatalf("unknown must exit 0, got %d", code)
	}
	got = nil
	json.Unmarshal([]byte(out), &got)
	if len(got) != 4 || got["latest"] != "" || got["status"] != "unknown" || got["error"] == "" {
		t.Fatalf("got %v", got)
	}
}

func TestVersionWithoutCheckUnchanged(t *testing.T) {
	setVersionVars(t, "v0.1.0", "abc1234", "2026-10-29")
	out, _ := runVersion(t)
	if out != "agent-tmux v0.1.0 (abc1234, 2026-10-29)\n" {
		t.Fatalf("got %q", out)
	}
}
