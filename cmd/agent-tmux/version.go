package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Set at build time: -ldflags "-X main.version=v0.1.0 -X main.commit=abc1234 -X main.date=...".
// Left empty for plain `go build` and `go install`, which resolveVersion
// fills in from the build info Go embeds.
var (
	version string
	commit  string
	date    string
)

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	// Latest and Status come from the local release cache only; both are
	// omitted when there is no cache.
	Latest string `json:"latest,omitempty"`
	Status string `json:"status,omitempty"`
}

// resolveVersion prefers the ldflags values, then the module and VCS data
// from the build info (so `go install ...@v0.1.0` reports v0.1.0), then "dev".
func resolveVersion() versionInfo {
	info := versionInfo{Version: version, Commit: commit, Date: date}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return finishVersion(info)
	}
	if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = s.Value
			}
		}
	}
	return finishVersion(info)
}

func finishVersion(info versionInfo) versionInfo {
	if info.Version == "" {
		info.Version = "dev"
	}
	if len(info.Commit) > 7 {
		info.Commit = info.Commit[:7]
	}
	return info
}

func (v versionInfo) String() string {
	return "agent-tmux " + v.Version + v.detail()
}

// detail is the " (commit, date)" part of the build line, or "".
func (v versionInfo) detail() string {
	switch {
	case v.Commit != "" && v.Date != "":
		return fmt.Sprintf(" (%s, %s)", v.Commit, v.Date)
	case v.Commit != "":
		return fmt.Sprintf(" (%s)", v.Commit)
	case v.Date != "":
		return fmt.Sprintf(" (%s)", v.Date)
	}
	return ""
}

// palette colours human text. The zero value is plain.
type palette struct{ on bool }

const (
	ansiFaint  = "\x1b[2m"
	ansiYellow = "\x1b[33m"
	ansiReset  = "\x1b[0m"
)

func (p palette) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return code + s + ansiReset
}
func (p palette) grey(s string) string   { return p.wrap(ansiFaint, s) }
func (p palette) yellow(s string) string { return p.wrap(ansiYellow, s) }

// colorFor is a seam: colours only on a terminal, with NO_COLOR empty and
// TERM not "dumb".
var colorFor = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// mainLine is "agent-tmux <version> [(commit, date)]  <status>"; the status
// is already coloured by the caller.
func (v versionInfo) mainLine(p palette, status string) string {
	return "agent-tmux " + v.Version + p.grey(v.detail()) + "  " + status
}

// textWithLatest is the plain `version` output: one line, plus the upgrade
// block when outdated. It never touches the network.
func textWithLatest(info versionInfo, c *releaseCache, p palette) string {
	if c == nil || c.Tag == "" {
		return info.mainLine(p, p.grey("latest unknown \u00b7 run 'agent-tmux version --check'"))
	}
	return renderStatus(info, info.Status, info.Latest, p)
}

func renderStatus(info versionInfo, status, latest string, p palette) string {
	switch status {
	case statusCurrent:
		return info.mainLine(p, p.grey("up to date"))
	case statusOutdated:
		return info.mainLine(p, p.yellow("\u2192 "+latest+" available")) + "\n\n" + upgradeHint(p)
	case statusDev:
		return info.mainLine(p, p.grey("dev build \u00b7 upstream "+latest))
	}
	return info.mainLine(p, "")
}

func versionCommand() *command {
	c := newCommand("version", "print the agent-tmux version and, from a local cache, the latest release")
	c.noManager = true
	asJSON := c.fs.Bool("json", false, "print the version as JSON")
	check := c.fs.Bool("check", false, "force a fresh GitHub lookup of the latest release (bypasses and updates the cache)")
	c.internalFlags = []string{"refresh-cache"}
	c.run = func(inv *invocation) int {
		if c.internalGiven["refresh-cache"] {
			refreshCache(inv.ctx)
			return 0
		}
		info := resolveVersion()
		if *check {
			res := checkRelease(inv.ctx, info.Version)
			if res.Latest != "" {
				storeFetched(res.Latest, res.etag)
			}
			if *asJSON {
				out, err := json.Marshal(res)
				if err != nil {
					return fail(inv.stderr, err)
				}
				fmt.Fprintln(inv.stdout, string(out))
				return 0
			}
			fmt.Fprintln(inv.stdout, res.text(palette{colorFor(inv.stdout)}))
			return 0
		}
		cache := loadCache()
		if cache != nil && cache.Tag != "" {
			info.Status, _ = compareRelease(info.Version, cache.Tag)
			if info.Status != statusUnknown {
				info.Latest = cache.Tag
			}
		}
		maybeRefreshInBackground(cache)
		if *asJSON {
			out, err := json.Marshal(info)
			if err != nil {
				return fail(inv.stderr, err)
			}
			fmt.Fprintln(inv.stdout, string(out))
			return 0
		}
		fmt.Fprintln(inv.stdout, textWithLatest(info, cache, palette{colorFor(inv.stdout)}))
		return 0
	}
	return c
}

// latestReleaseURL is GitHub's "latest release" endpoint; a variable so tests
// can point it at an httptest server.
var latestReleaseURL = "https://api.github.com/repos/mugenkunou/agent-tmux/releases/latest"

const (
	checkTimeout = 3 * time.Second
)

// Statuses of `version --check`. The JSON shape is a contract with ws-tool.
const (
	statusCurrent  = "current"
	statusOutdated = "outdated"
	statusDev      = "dev"
	statusUnknown  = "unknown"
)

type checkResult struct {
	Version string `json:"version"`
	Latest  string `json:"latest"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	etag    string
}

var semverRE = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// parseSemver returns the core triple and whether there is a pre-release part.
func parseSemver(v string) (core [3]int, pre bool, ok bool) {
	m := semverRE.FindStringSubmatch(v)
	if m == nil {
		return core, false, false
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return core, false, false
		}
		core[i] = n
	}
	return core, m[4] != "", true
}

// fetchLatestTag looks up the latest release tag; the ETag is kept for the cache.
func fetchLatestTag(ctx context.Context) (tag, etag string, err error) {
	tag, etag, _, err = fetchLatest(ctx, "")
	return
}

// fetchLatest asks GitHub for the latest release. With a non-empty etag it
// revalidates, and a 304 comes back as notModified.
func fetchLatest(ctx context.Context, ifNoneMatch string) (tag, etag string, notModified bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return "", "", false, err
	}
	req.Header.Set("User-Agent", "agent-tmux-version-check")
	req.Header.Set("Accept", "application/vnd.github+json")
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && ifNoneMatch != "" {
		return "", ifNoneMatch, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", false, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", "", false, fmt.Errorf("bad response: %w", err)
	}
	if body.TagName == "" {
		return "", "", false, errors.New("response has no tag_name")
	}
	return body.TagName, resp.Header.Get("ETag"), false, nil
}

// compareRelease compares the installed version with a release tag. Only a
// clean release tag (vX.Y.Z[+build]) is ever reported outdated; "dev",
// git-describe strings and pre-releases are "dev". An unparseable latest tag
// is "unknown" with an error message.
func compareRelease(installed, latest string) (status, errMsg string) {
	lcore, _, lok := parseSemver(latest)
	if !lok {
		return statusUnknown, fmt.Sprintf("latest release tag %q is not a semantic version", latest)
	}
	icore, ipre, iok := parseSemver(installed)
	if !iok || ipre {
		return statusDev, ""
	}
	for i := 0; i < 3; i++ {
		if lcore[i] != icore[i] {
			if lcore[i] > icore[i] {
				return statusOutdated, ""
			}
			break
		}
	}
	return statusCurrent, ""
}

// checkRelease is the live lookup behind `version --check`; it never reads
// the cache.
func checkRelease(ctx context.Context, installed string) checkResult {
	res := checkResult{Version: installed}
	latest, etag, err := fetchLatestTag(ctx)
	if err != nil {
		res.Status, res.Error = statusUnknown, err.Error()
		return res
	}
	res.etag = etag
	res.Status, res.Error = compareRelease(installed, latest)
	if res.Status != statusUnknown {
		res.Latest = latest
	}
	return res
}

func (r checkResult) text(p palette) string {
	info := versionInfo{Version: r.Version}
	if r.Status == statusUnknown || r.Status == "" {
		return info.mainLine(p, p.grey("check failed: "+strings.TrimSpace(r.Error)))
	}
	return renderStatus(info, r.Status, r.Latest, p)
}

const releasesPage = "https://github.com/mugenkunou/agent-tmux/releases/latest"

// Seam for tests: the running binary's path.
var hintExe = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, nil
}

// upgradeURL picks the asset for the machine with shell substitution, so one
// block works on every OS and arch the releases cover.
const upgradeURL = `"` + releasesPage + `/download/agent-tmux-$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"`

// upgradeHint says how to replace this binary with the latest release.
func upgradeHint(p palette) string {
	exe, err := hintExe()
	if err != nil || exe == "" {
		exe = "<path of agent-tmux>"
	}
	const tmp = "agent-tmux.download"
	cmd := func(c string) string { return "  " + p.grey("$ "+c) }
	return p.yellow("upgrade") + "\n" +
		cmd("curl -fL -o "+tmp+" "+upgradeURL) + "\n" +
		cmd("chmod +x "+tmp) + "\n" +
		cmd("mv "+tmp+" "+exe)
}
