package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mugenkunou/agent-tmux/internal/age"
	"github.com/mugenkunou/agent-tmux/internal/recording"
	"github.com/mugenkunou/agent-tmux/internal/termshare"
	"github.com/mugenkunou/agent-tmux/internal/transcript"
	"github.com/mugenkunou/agent-tmux/internal/trash"
)

// The commands here read recordings (ls, show, grep) or remove them (rm).
// They work without tmux too: with no tmux or no server, nothing is live and
// only recordings are shown.

var recordingArg = positional{name: "session", kind: argRecording}

// lsEntry is one row of `ls --json`. Every field is always present; zero
// values when it does not apply (see the CLI contract in README.md).
type lsEntry struct {
	Name            string `json:"name"`
	ID              string `json:"id"`
	State           string `json:"state"`
	Recorder        string `json:"recorder"`
	Owner           string `json:"owner"`
	Clients         int    `json:"clients"`
	Command         string `json:"command"`
	Path            string `json:"path"`
	Host            string `json:"host"`
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at"`
	HistoryStartsAt string `json:"history_starts_at"`
	Bytes           int64  `json:"bytes"`
}

func lsCommand() *command {
	c := newCommand("ls", "list live sessions and recordings: state, recorder, owner, clients, start time")
	c.noManager = true
	asJSON := c.fs.Bool("json", false, "print JSON")
	c.run = func(inv *invocation) int {
		entries, err := listAll(inv)
		if err != nil {
			return fail(inv.stderr, err)
		}
		if *asJSON {
			if entries == nil {
				entries = []lsEntry{}
			}
			return printJSON(inv, map[string]any{"sessions": entries})
		}
		if len(entries) == 0 {
			fmt.Fprintln(inv.stdout, "no sessions")
			return 0
		}
		w := tabwriter.NewWriter(inv.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SESSION\tSTATE\tREC\tOWNER\tCLIENTS\tSTARTED")
		for _, e := range entries {
			if e.State == "live" {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", e.Name, e.State, e.Recorder, ownerLabel(e.Owner), e.Clients, localTime(e.StartedAt))
			} else {
				// An ended recording is named by its id: that is what show
				// and rm take, and names repeat.
				fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t%s\n", e.ID, e.State, localTime(e.StartedAt))
			}
		}
		w.Flush()
		return 0
	}
	return c
}

// listAll is ls: live sessions by name, then the other recordings, newest
// first. Stale recordings are finalized first, and a dead recorder is noted.
func listAll(inv *invocation) ([]lsEntry, error) {
	sessions, err := currentSessions(inv)
	if err != nil {
		return nil, err
	}
	root, err := recording.Root()
	if err != nil {
		return nil, err
	}
	live := liveDirs(sessions)
	if err := recording.FinalizeStale(root, live); err != nil {
		fmt.Fprintln(inv.stderr, "agent-tmux: warning:", err)
	}
	recs, err := recording.List(root)
	if err != nil {
		return nil, err
	}
	byDir := map[string]recording.Recording{}
	for _, r := range recs {
		byDir[r.Dir] = r
	}

	var entries []lsEntry
	host, _ := os.Hostname()
	for _, s := range sessions {
		if s.Recorder == termshare.RecorderStopped {
			recorderLabel(s) // logs recorder-died once
		}
		e := lsEntry{Name: s.Name, State: "live", Recorder: s.Recorder, Owner: s.Owner,
			Clients: s.Clients, Command: s.Command, Path: s.Path, Host: host}
		if r, ok := byDir[s.Recording]; ok {
			e.ID, e.Host, e.StartedAt = r.Meta.ID, r.Meta.Host, r.Meta.StartedAt
			e.HistoryStartsAt, e.Bytes = r.Meta.FirstSegmentAt, r.Bytes
		}
		entries = append(entries, e)
	}
	for _, r := range recs {
		if live[r.Dir] {
			continue
		}
		entries = append(entries, lsEntry{Name: r.Meta.Name, ID: r.Meta.ID, State: "ended", Host: r.Meta.Host,
			StartedAt: r.Meta.StartedAt, EndedAt: r.Meta.EndedAt, HistoryStartsAt: r.Meta.FirstSegmentAt, Bytes: r.Bytes})
	}
	return entries, nil
}

func showCommand() *command {
	c := newCommand("show", "print a session's whole recording (live or ended); pages with less on a terminal, plain text when piped")
	c.noManager = true
	raw := c.fs.Bool("raw", false, "print the recorded bytes exactly, escape sequences and all")
	c.args = []positional{recordingArg}
	c.run = func(inv *invocation) int {
		rec, err := resolveRecording(inv, inv.args[0])
		if err != nil {
			return fail(inv.stderr, err)
		}
		if err := showRecording(inv, rec, *raw); err != nil {
			return fail(inv.stderr, err)
		}
		return 0
	}
	return c
}

// showRecording writes rec: raw bytes, or rendered text paged through less
// (with colors) when stdout is a terminal, or plain text otherwise. It
// streams; a recording is never held in memory.
func showRecording(inv *invocation, rec recording.Recording, raw bool) error {
	src := newSegmentReader(rec.Segments)
	defer src.Close()
	if raw {
		_, err := io.Copy(inv.stdout, src)
		return err
	}
	notice := ""
	if rec.Meta.FirstSegment > 1 {
		notice = fmt.Sprintf("-- history starts at %s; older output was rotated away --\n", localTimeSeconds(rec.Meta.FirstSegmentAt))
	}
	out, ok := inv.stdout.(*os.File)
	if !ok || !isTerminal(out) {
		io.WriteString(inv.stdout, notice)
		return transcript.Render(inv.stdout, src, false)
	}
	less, err := exec.LookPath("less")
	if err != nil {
		io.WriteString(out, notice)
		return transcript.Render(out, src, true)
	}
	pager := exec.Command(less, "-R", "+G")
	pager.Stdout, pager.Stderr = out, inv.stderr
	in, err := pager.StdinPipe()
	if err != nil {
		return err
	}
	// Ctrl-C belongs to less while it runs; it must not end this process.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	if err := pager.Start(); err != nil {
		return err
	}
	// Quitting less early makes these writes fail; that is not an error.
	io.WriteString(in, notice)
	_ = transcript.Render(in, src, true)
	in.Close()
	_ = pager.Wait()
	return nil
}

func grepCommand() *command {
	c := newCommand("grep", "search recordings for text (case-insensitive), newest first; exit 1 when nothing matches")
	c.noManager = true
	asJSON := c.fs.Bool("json", false, "print JSON")
	limit := c.fs.Int("limit", 100, "print at most this many `matches`")
	session := c.fs.String("session", "", "search only this `session` (live name, recording id, or name)")
	c.suggest("limit", "10", "100", "1000")
	c.completeFlagAs("session", argRecording)
	c.args = []positional{{name: "text", kind: argFree, variadic: true}}
	c.run = func(inv *invocation) int {
		if *limit < 1 {
			fmt.Fprintf(inv.stderr, "agent-tmux grep: --limit must be at least 1\nusage: %s\n", c.usageLine())
			return 2
		}
		needle := strings.Join(inv.args, " ")
		matches, truncated, err := grepRecordings(inv, needle, *session, *limit)
		if err != nil {
			fail(inv.stderr, err)
			return 2
		}
		if *asJSON {
			if matches == nil {
				matches = []grepMatch{}
			}
			return printJSON(inv, map[string]any{"matches": matches, "truncated": truncated})
		}
		for _, m := range matches {
			fmt.Fprintf(inv.stdout, "%s:%d: %s\n", m.ID, m.Line, m.Text)
		}
		if len(matches) == 0 {
			return 1
		}
		return 0
	}
	return c
}

type grepMatch struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// grepRecordings searches recordings newest first, newest line first, and
// keeps the first limit matches. truncated reports that more exist.
func grepRecordings(inv *invocation, needle, session string, limit int) (matches []grepMatch, truncated bool, err error) {
	var recs []recording.Recording
	if session != "" {
		rec, err := resolveRecording(inv, session)
		if err != nil {
			return nil, false, err
		}
		recs = []recording.Recording{rec}
	} else {
		root, err := recording.Root()
		if err != nil {
			return nil, false, err
		}
		if recs, err = recording.List(root); err != nil {
			return nil, false, err
		}
	}
	for _, rec := range recs {
		left := limit - len(matches)
		if left == 0 {
			// Full: only learn whether anything else would have matched.
			left = 1
		}
		src := newSegmentReader(rec.Segments)
		found, more, err := transcript.Search(src, needle, left)
		src.Close()
		if err != nil {
			return nil, false, fmt.Errorf("search %s: %w", rec.Meta.ID, err)
		}
		if len(matches) == limit {
			if len(found) > 0 {
				return matches, true, nil
			}
			continue
		}
		for _, m := range found {
			matches = append(matches, grepMatch{Name: rec.Meta.Name, ID: rec.Meta.ID, Line: m.Line, Text: m.Text})
		}
		if more {
			return matches, true, nil
		}
	}
	return matches, false, nil
}

func rmCommand() *command {
	c := newCommand("rm", "move ended recordings to the trash, by id or session name, or all older than an age")
	c.noManager = true
	olderThan := c.fs.String("older-than", "", "only recordings that ended more than this `age` ago (30d, 12h, 2w)")
	c.suggest("older-than", "1d", "7d", "30d")
	c.args = []positional{{name: "session", kind: argRecording, variadic: true, optional: true}}
	c.run = func(inv *invocation) int {
		if *olderThan == "" && len(inv.args) == 0 {
			fmt.Fprintf(inv.stderr, "agent-tmux rm: name a recording or give --older-than\nusage: %s\n", c.usageLine())
			return 2
		}
		var cutoff time.Time
		if *olderThan != "" {
			d, err := age.Parse(*olderThan)
			if err != nil {
				fmt.Fprintf(inv.stderr, "agent-tmux rm: --older-than: %v\nusage: %s\n", err, c.usageLine())
				return 2
			}
			cutoff = time.Now().Add(-d)
		}
		selected, err := selectForRemoval(inv, inv.args, cutoff)
		if err != nil {
			return fail(inv.stderr, err)
		}
		for _, rec := range selected {
			if _, err := trash.Move(rec.Dir); err != nil {
				return fail(inv.stderr, fmt.Errorf("move %s to the trash: %w", rec.Meta.ID, err))
			}
			fmt.Fprintf(inv.stdout, "moved to trash: %s\n", rec.Meta.ID)
		}
		return 0
	}
	return c
}

// selectForRemoval picks the ended recordings named by args (ids, or session
// names meaning every ended recording of that name), or all ended ones when
// args is empty, keeping those that ended before cutoff when it is set. It
// fails, selecting nothing, if any argument names a live session.
func selectForRemoval(inv *invocation, args []string, cutoff time.Time) ([]recording.Recording, error) {
	sessions, err := currentSessions(inv)
	if err != nil {
		return nil, err
	}
	root, err := recording.Root()
	if err != nil {
		return nil, err
	}
	live := liveDirs(sessions)
	if err := recording.FinalizeStale(root, live); err != nil {
		fmt.Fprintln(inv.stderr, "agent-tmux: warning:", err)
	}
	recs, err := recording.List(root)
	if err != nil {
		return nil, err
	}
	// Still live: pointed at by a session here, or not ended (a session on
	// another tmux server, whose recorder still runs).
	isLive := func(r recording.Recording) bool { return live[r.Dir] || r.Meta.EndedAt == "" }
	refuse := func(name string) error {
		return fmt.Errorf("%s is live; kill it first (agent-tmux kill %s)", name, name)
	}

	var picked []recording.Recording
	if len(args) == 0 {
		for _, r := range recs {
			if !isLive(r) {
				picked = append(picked, r)
			}
		}
	}
	for _, arg := range args {
		for _, s := range sessions {
			if s.Name == arg {
				return nil, refuse(arg)
			}
		}
		found := false
		for _, r := range recs {
			if r.Meta.ID == arg {
				if isLive(r) {
					return nil, refuse(r.Meta.Name)
				}
				picked, found = append(picked, r), true
			}
		}
		if found {
			continue
		}
		for _, r := range recs {
			if r.Meta.Name == arg {
				if isLive(r) {
					continue
				}
				picked, found = append(picked, r), true
			}
		}
		if !found {
			return nil, fmt.Errorf("no ended recording %q", arg)
		}
	}

	var out []recording.Recording
	seen := map[string]bool{}
	for _, r := range picked {
		if seen[r.Dir] {
			continue
		}
		seen[r.Dir] = true
		if !cutoff.IsZero() {
			ended, err := time.Parse(time.RFC3339, r.Meta.EndedAt)
			if err != nil || !ended.Before(cutoff) {
				continue
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Meta.StartedAt > out[j].Meta.StartedAt })
	return out, nil
}

// resolveRecording finds what show and grep --session name: a live session,
// else a recording id, else the newest recording of that session name.
func resolveRecording(inv *invocation, key string) (recording.Recording, error) {
	sessions, err := currentSessions(inv)
	if err != nil {
		return recording.Recording{}, err
	}
	for _, s := range sessions {
		if s.Name == key {
			if s.Recording == "" {
				return recording.Recording{}, fmt.Errorf("%s has no recording (it was made by an older agent-tmux)", key)
			}
			return recording.Load(s.Recording)
		}
	}
	root, err := recording.Root()
	if err != nil {
		return recording.Recording{}, err
	}
	return recording.Find(root, key)
}

// currentSessions lists the live sessions; none when tmux is not installed.
func currentSessions(inv *invocation) ([]termshare.Session, error) {
	manager := inv.manager
	if manager == nil {
		var err error
		if manager, err = termshare.NewManager(); err != nil {
			return nil, nil
		}
	}
	sessions, err := manager.List(inv.ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Name < sessions[j].Name })
	return sessions, nil
}

func liveDirs(sessions []termshare.Session) map[string]bool {
	live := map[string]bool{}
	for _, s := range sessions {
		if s.Recording != "" {
			live[s.Recording] = true
		}
	}
	return live
}

// segmentReader reads a recording's kept segments as one stream, opening
// one file at a time. A segment rotated away meanwhile is skipped.
type segmentReader struct {
	paths []string
	cur   *os.File
}

func newSegmentReader(paths []string) *segmentReader { return &segmentReader{paths: paths} }

func (r *segmentReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if len(r.paths) == 0 {
				return 0, io.EOF
			}
			f, err := os.Open(r.paths[0])
			r.paths = r.paths[1:]
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return 0, err
			}
			r.cur = f
		}
		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			r.cur.Close()
			r.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (r *segmentReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}

func printJSON(inv *invocation, v any) int {
	enc := json.NewEncoder(inv.stdout)
	if err := enc.Encode(v); err != nil {
		return fail(inv.stderr, err)
	}
	return 0
}

// isTerminal reports whether f is a terminal (not just any character device
// such as /dev/null).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return termios(f.Fd(), syscall.TCGETS, &t) == nil
}

// localTime shows an RFC3339 time in local time to the minute, "-" if unknown.
func localTime(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func localTimeSeconds(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return "an unknown time"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
