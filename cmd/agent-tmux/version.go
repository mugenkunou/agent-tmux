package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	s := "agent-tmux " + v.Version
	switch {
	case v.Commit != "" && v.Date != "":
		s += fmt.Sprintf(" (%s, %s)", v.Commit, v.Date)
	case v.Commit != "":
		s += fmt.Sprintf(" (%s)", v.Commit)
	case v.Date != "":
		s += fmt.Sprintf(" (%s)", v.Date)
	}
	return s
}

// textWithLatest is the plain `version` output: the build line, then what the
// local cache says about upstream. It never touches the network.
func textWithLatest(info versionInfo, c *releaseCache) string {
	s := info.String()
	if c == nil || c.Tag == "" {
		return s + "\nlatest unknown yet; run 'agent-tmux version --check' to look it up"
	}
	switch info.Status {
	case statusCurrent:
		s += " (up to date)"
	case statusOutdated:
		s += fmt.Sprintf(" (latest %s \u2014 update available)\n%s", info.Latest, upgradeHint)
	case statusDev:
		s += fmt.Sprintf(" (local build is a dev build; upstream release is %s)", info.Latest)
	}
	return s
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
			fmt.Fprintln(inv.stdout, res.text())
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
		fmt.Fprintln(inv.stdout, textWithLatest(info, cache))
		return 0
	}
	return c
}

// latestReleaseURL is GitHub's "latest release" endpoint; a variable so tests
// can point it at an httptest server.
var latestReleaseURL = "https://api.github.com/repos/mugenkunou/agent-tmux/releases/latest"

const (
	checkTimeout = 3 * time.Second
	upgradeHint  = "upgrade: download from https://github.com/mugenkunou/agent-tmux/releases/latest"
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

func (r checkResult) text() string {
	switch r.Status {
	case statusCurrent:
		return fmt.Sprintf("agent-tmux %s (latest %s)", r.Version, r.Latest)
	case statusOutdated:
		return fmt.Sprintf("agent-tmux %s: update available, latest is %s\n%s", r.Version, r.Latest, upgradeHint)
	case statusDev:
		return fmt.Sprintf("agent-tmux %s: local build is a dev build; upstream release is %s", r.Version, r.Latest)
	}
	return fmt.Sprintf("agent-tmux %s: could not check latest release: %s", r.Version, strings.TrimSpace(r.Error))
}
