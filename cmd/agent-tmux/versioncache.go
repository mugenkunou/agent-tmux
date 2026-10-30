package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// The release cache lets a plain `agent-tmux version` show the latest upstream
// release without ever waiting for the network. A detached child refreshes it.
const (
	cacheTTL         = 24 * time.Hour // a cache younger than this is fresh
	cacheRetryAfter  = time.Hour      // minimum gap between lookups, so failures back off
	noVersionCheckEv = "AGENT_TMUX_NO_VERSION_CHECK"
)

type releaseCache struct {
	Tag         string    `json:"tag"`
	ETag        string    `json:"etag,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	AttemptedAt time.Time `json:"attempted_at"`
}

// cachePath is ${XDG_CACHE_HOME:-~/.cache}/agent-tmux/release.json.
func cachePath() (string, error) {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "agent-tmux", "release.json"), nil
}

// loadCache returns the cache, or nil when it is missing or unreadable.
func loadCache() *releaseCache {
	p, err := cachePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var c releaseCache
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

// saveCache writes atomically (temp file + rename) so a reader never sees a
// half-written file. Errors are returned but callers ignore them.
func saveCache(c releaseCache) error {
	p, err := cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "release-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func versionCheckDisabled() bool {
	v := os.Getenv(noVersionCheckEv)
	return v != "" && v != "0"
}

// needsRefresh: no cache, or stale and not attempted within the last hour.
func needsRefresh(c *releaseCache, now time.Time) bool {
	if c == nil || c.Tag == "" {
		return c == nil || now.Sub(c.AttemptedAt) >= cacheRetryAfter
	}
	if now.Sub(c.CheckedAt) < cacheTTL {
		return false
	}
	return now.Sub(c.AttemptedAt) >= cacheRetryAfter
}

// startCmd launches a prepared command without waiting; a seam for tests.
var startCmd = func(c *exec.Cmd) error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

// spawnRefresh starts `agent-tmux version --refresh-cache` detached: own
// session, stdio on /dev/null, never waited for.
func spawnRefresh() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, "version", "--refresh-cache")
	c.Stdin, c.Stdout, c.Stderr = nil, nil, nil
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return startCmd(c)
}

// maybeRefreshInBackground records the attempt first (so concurrent or
// repeated runs do not stampede), then spawns the child.
func maybeRefreshInBackground(c *releaseCache) {
	if versionCheckDisabled() {
		return
	}
	now := time.Now()
	if !needsRefresh(c, now) {
		return
	}
	next := releaseCache{}
	if c != nil {
		next = *c
	}
	next.AttemptedAt = now
	if saveCache(next) != nil {
		return // cannot record the attempt: do not risk spawning on every run
	}
	_ = spawnRefresh()
}

// refreshCache is the body of the hidden --refresh-cache child: silent, one
// bounded lookup, writes the cache. Failure only records the attempt.
func refreshCache(ctx context.Context) {
	old := loadCache()
	next := releaseCache{}
	etag := ""
	if old != nil {
		next, etag = *old, old.ETag
	}
	now := time.Now()
	next.AttemptedAt = now
	tag, newETag, notModified, err := fetchLatest(ctx, etag)
	switch {
	case err != nil:
	case notModified && next.Tag != "":
		next.CheckedAt = now
	case !notModified:
		next.Tag, next.ETag, next.CheckedAt = tag, newETag, now
	}
	_ = saveCache(next)
}

// storeFetched is used by `version --check` to keep what it just fetched.
func storeFetched(tag, etag string) {
	now := time.Now()
	_ = saveCache(releaseCache{Tag: tag, ETag: etag, CheckedAt: now, AttemptedAt: now})
}
