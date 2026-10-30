package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// No test may touch the real cache or fork a real child: TestMain (in
// registry_test.go) points the cache at a temp dir and swaps startCmd for this
// recorder.
var spawned []*exec.Cmd

func freshCacheDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	spawned = nil
}

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

func TestVersionNoCacheHintsAndSpawns(t *testing.T) {
	freshCacheDir(t)
	setVersionVars(t, "v0.1.0", "abc1234", "2026-10-29")
	out, _ := runVersion(t)
	want := "agent-tmux v0.1.0 (abc1234, 2026-10-29)\nlatest unknown yet; run 'agent-tmux version --check' to look it up\n"
	if out != want {
		t.Fatalf("got %q", out)
	}
	if len(spawned) != 1 {
		t.Fatalf("spawned %d, want 1", len(spawned))
	}
	c := spawned[0]
	if len(c.Args) != 3 || c.Args[1] != "version" || c.Args[2] != "--refresh-cache" {
		t.Fatalf("args %v", c.Args)
	}
	if c.SysProcAttr == nil || !c.SysProcAttr.Setsid || c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Fatalf("not detached: %+v", c)
	}
	// The attempt is recorded, so an immediate second run does not spawn again.
	runVersion(t)
	if len(spawned) != 1 {
		t.Fatalf("respawned within the backoff: %d", len(spawned))
	}
}

func TestVersionEnvDisablesSpawn(t *testing.T) {
	freshCacheDir(t)
	t.Setenv(noVersionCheckEv, "1")
	runVersion(t)
	if len(spawned) != 0 {
		t.Fatal("spawned despite env")
	}
	if loadCache() != nil {
		t.Fatal("wrote cache despite env")
	}
	t.Setenv(noVersionCheckEv, "0")
	runVersion(t)
	if len(spawned) != 1 {
		t.Fatal("0 must not disable")
	}
}

func TestVersionFromCache(t *testing.T) {
	cases := []struct{ installed, tag, status, text string }{
		{"v0.20.0", "v0.21.0", "outdated", "agent-tmux v0.20.0 (latest v0.21.0 \u2014 update available)\nupgrade:"},
		{"v0.21.0", "v0.21.0", "current", "agent-tmux v0.21.0 (up to date)\n"},
		{"dev", "v0.21.0", "dev", "agent-tmux dev (local build is a dev build; upstream release is v0.21.0)\n"},
		{"v0.20.0-3-gabc1234", "v0.21.0", "dev", "agent-tmux v0.20.0-3-gabc1234 (local build"},
	}
	for _, c := range cases {
		t.Run(c.installed, func(t *testing.T) {
			freshCacheDir(t)
			setVersionVars(t, c.installed, "", "")
			now := time.Now()
			saveCache(releaseCache{Tag: c.tag, CheckedAt: now, AttemptedAt: now})
			out, _ := runVersion(t)
			if !strings.HasPrefix(out, c.text) || strings.Contains(out, "unknown yet") {
				t.Fatalf("got %q", out)
			}
			if len(spawned) != 0 {
				t.Fatal("spawned for a fresh cache")
			}
			j, _ := runVersion(t, "--json")
			var got map[string]string
			json.Unmarshal([]byte(j), &got)
			if got["version"] != c.installed || got["latest"] != c.tag || got["status"] != c.status {
				t.Fatalf("json %v", got)
			}
		})
	}
}

func TestVersionJSONOmitsLatestWithoutCache(t *testing.T) {
	freshCacheDir(t)
	t.Setenv(noVersionCheckEv, "1")
	j, _ := runVersion(t, "--json")
	var got map[string]any
	json.Unmarshal([]byte(j), &got)
	if _, ok := got["latest"]; ok {
		t.Fatalf("got %v", got)
	}
	if _, ok := got["status"]; ok || got["version"] == nil {
		t.Fatalf("got %v", got)
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	h := func(n float64) time.Time { return now.Add(-time.Duration(n * float64(time.Hour))) }
	cases := []struct {
		name string
		c    *releaseCache
		want bool
	}{
		{"missing", nil, true},
		{"fresh", &releaseCache{Tag: "v1.0.0", CheckedAt: h(2), AttemptedAt: h(2)}, false},
		{"stale", &releaseCache{Tag: "v1.0.0", CheckedAt: h(25), AttemptedAt: h(25)}, true},
		{"stale, tried recently", &releaseCache{Tag: "v1.0.0", CheckedAt: h(25), AttemptedAt: h(0.5)}, false},
		{"never succeeded, tried recently", &releaseCache{AttemptedAt: h(0.5)}, false},
		{"never succeeded, tried long ago", &releaseCache{AttemptedAt: h(2)}, true},
	}
	for _, c := range cases {
		if got := needsRefresh(c.c, now); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestRefreshCacheChildWritesSilently(t *testing.T) {
	freshCacheDir(t)
	setVersionVars(t, "v0.1.0", "", "")
	var gotINM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotINM = r.Header.Get("If-None-Match")
		if gotINM == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		w.Write([]byte(`{"tag_name":"v0.9.0"}`))
	}))
	defer srv.Close()
	old := latestReleaseURL
	latestReleaseURL = srv.URL
	t.Cleanup(func() { latestReleaseURL = old })

	out, code := runVersion(t, "--refresh-cache")
	if code != 0 || out != "" {
		t.Fatalf("code %d out %q", code, out)
	}
	c := loadCache()
	if c == nil || c.Tag != "v0.9.0" || c.ETag != `"abc"` || c.CheckedAt.IsZero() {
		t.Fatalf("cache %+v", c)
	}
	// Revalidation: 304 keeps the tag and bumps checked_at.
	c.CheckedAt = time.Now().Add(-48 * time.Hour)
	saveCache(*c)
	runVersion(t, "--refresh-cache")
	c2 := loadCache()
	if gotINM != `"abc"` || c2.Tag != "v0.9.0" || time.Since(c2.CheckedAt) > time.Minute {
		t.Fatalf("after 304: %+v (INM %q)", c2, gotINM)
	}
	// Failure: silent, exit 0, keeps the old tag, records the attempt only.
	fakeLatest(t, 500, "x")
	c2.CheckedAt = time.Now().Add(-48 * time.Hour)
	c2.AttemptedAt = c2.CheckedAt
	saveCache(*c2)
	out, code = runVersion(t, "--refresh-cache")
	c3 := loadCache()
	if code != 0 || out != "" || c3.Tag != "v0.9.0" || time.Since(c3.AttemptedAt) > time.Minute || time.Since(c3.CheckedAt) < 24*time.Hour {
		t.Fatalf("after failure: %d %q %+v", code, out, c3)
	}
}

func TestVersionCheckWritesCache(t *testing.T) {
	freshCacheDir(t)
	fakeLatest(t, 200, `{"tag_name":"v0.20.0"}`)
	runVersion(t, "--check")
	if c := loadCache(); c == nil || c.Tag != "v0.20.0" {
		t.Fatalf("cache %+v", c)
	}
	freshCacheDir(t)
	fakeLatest(t, 500, "x")
	runVersion(t, "--check")
	if loadCache() != nil {
		t.Fatal("failed check wrote a cache")
	}
}

func TestRefreshFlagIsHidden(t *testing.T) {
	c := versionCommand()
	if strings.Contains(c.usageLine(), "refresh-cache") || c.fs.Lookup("refresh-cache") != nil {
		t.Fatal("refresh-cache leaked into the public surface")
	}
}
