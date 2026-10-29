package main

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
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

func versionCommand() *command {
	c := newCommand("version", "print the agent-tmux version")
	c.noManager = true
	asJSON := c.fs.Bool("json", false, "print the version as JSON")
	c.run = func(inv *invocation) int {
		info := resolveVersion()
		if *asJSON {
			out, err := json.Marshal(info)
			if err != nil {
				return fail(inv.stderr, err)
			}
			fmt.Fprintln(inv.stdout, string(out))
			return 0
		}
		fmt.Fprintln(inv.stdout, info)
		return 0
	}
	return c
}
