// Package recording keeps the on-disk record of every session: the raw pane
// output in numbered segments, scriptreplay timing, meta.json and an event log.
// The layout is described in docs/PLAN.md; this package is its only reader and
// writer.
//
//	<root>/<name>-<YYYY-MM-DD-HHMM>[-N]/
//	  meta.json             written atomically (temp file + rename)
//	  stdout.NNNNNN.log     raw pane bytes
//	  stdout.NNNNNN.timing  "<delay-seconds> <bytes>" per write
//	  events.jsonl          one JSON object per line
//	  recorder.lock         flock held by the live recorder
package recording

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	dirMode  = 0o700
	fileMode = 0o600

	metaFile   = "meta.json"
	eventsFile = "events.jsonl"
	lockFile   = "recorder.lock"
)

// Meta is meta.json. Times are RFC3339 UTC, "" when unknown.
type Meta struct {
	Name           string `json:"name"`
	ID             string `json:"id"`
	Host           string `json:"host"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at"`
	FirstSegment   int    `json:"first_segment"`
	FirstSegmentAt string `json:"first_segment_at"`
}

// Root is where recordings live: $AGENT_TMUX_STATE_DIR (tests), else
// $XDG_STATE_HOME/agent-tmux, else ~/.local/state/agent-tmux.
func Root() (string, error) {
	if dir := os.Getenv("AGENT_TMUX_STATE_DIR"); dir != "" {
		return dir, nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "agent-tmux"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the recordings directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "agent-tmux"), nil
}

// Create makes a new recording directory for session name under root, named
// <name>-<local YYYY-MM-DD-HHMM> with -2, -3 ... on a collision (nothing is
// ever overwritten), and writes its initial meta.json. It returns the
// directory's absolute path.
func Create(root, name string, now time.Time) (string, error) {
	if err := os.MkdirAll(root, dirMode); err != nil {
		return "", fmt.Errorf("create recordings directory: %w", err)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	base := name + "-" + now.Local().Format("2006-01-02-1504")
	for n := 1; ; n++ {
		id := base
		if n > 1 {
			id += "-" + strconv.Itoa(n)
		}
		dir := filepath.Join(root, id)
		if err := os.Mkdir(dir, dirMode); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("create recording: %w", err)
		}
		host, _ := os.Hostname()
		started := formatTime(now)
		meta := Meta{Name: name, ID: id, Host: host, StartedAt: started, FirstSegment: 1, FirstSegmentAt: started}
		if err := WriteMeta(dir, meta); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		return dir, nil
	}
}

// ReadMeta reads dir's meta.json.
func ReadMeta(dir string) (Meta, error) {
	var meta Meta
	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("read %s: %w", filepath.Join(dir, metaFile), err)
	}
	return meta, nil
}

// WriteMeta replaces dir's meta.json atomically, so a reader never sees half
// a file.
func WriteMeta(dir string, meta Meta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".meta-*.json")
	if err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write meta.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, metaFile)); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}
	return nil
}

// Fields are the extra keys of an event, such as "actor" or "mode".
type Fields map[string]any

// LogEvent appends one event to dir's events.jsonl as a single write with
// O_APPEND, so lines from concurrent agent-tmux processes never interleave.
func LogEvent(dir, event string, fields Fields) error {
	line := Fields{}
	for k, v := range fields {
		line[k] = v
	}
	line["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	line["event"] = event
	data, err := json.Marshal(line)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("log %s event: %w", event, err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("log %s event: %w", event, err)
	}
	return nil
}

// NoteRecorderDied appends recorder-died unless the log already has it. The
// recorder never restarts, so once per recording is enough; whoever notices
// first (status, doctor, ls) writes it.
func NoteRecorderDied(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	// Two noticers at once must not both write it.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	if strings.Contains(string(data), `"event":"recorder-died"`) {
		return nil
	}
	return LogEvent(dir, "recorder-died", nil)
}

// RecorderRunning reports whether a recorder process holds dir's
// recorder.lock right now.
func RecorderRunning(dir string) bool {
	f, err := os.Open(filepath.Join(dir, lockFile))
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// WaitRecorder waits up to timeout for dir's recorder to start (running true)
// or to finish (running false), and reports whether it did.
func WaitRecorder(dir string, running bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if RecorderRunning(dir) == running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Finalize sets ended_at to at when it is still empty. It is for recordings
// whose recorder is gone; a live recorder writes ended_at itself.
func Finalize(dir string, at time.Time) error {
	meta, err := ReadMeta(dir)
	if err != nil {
		return err
	}
	if meta.EndedAt != "" {
		return nil
	}
	meta.EndedAt = formatTime(at)
	return WriteMeta(dir, meta)
}

// Recording is one recording directory as phase-2 readers need it.
type Recording struct {
	Dir      string
	Meta     Meta
	Bytes    int64    // size of the kept segments
	Segments []string // kept stdout.NNNNNN.log paths, oldest first
}

// Load reads one recording directory.
func Load(dir string) (Recording, error) {
	meta, err := ReadMeta(dir)
	if err != nil {
		return Recording{}, err
	}
	rec := Recording{Dir: dir, Meta: meta}
	numbers, err := segments(dir)
	if err != nil {
		return Recording{}, err
	}
	for _, n := range numbers {
		path := segmentPath(dir, n, ".log")
		info, err := os.Stat(path)
		if err != nil {
			continue // rotated away between the listing and now
		}
		rec.Segments = append(rec.Segments, path)
		rec.Bytes += info.Size()
	}
	return rec, nil
}

// List returns every recording under root, newest started_at first.
// Directories without a readable meta.json are skipped.
func List(root string) ([]Recording, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []Recording
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rec, err := Load(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Meta.StartedAt != recs[j].Meta.StartedAt {
			return recs[i].Meta.StartedAt > recs[j].Meta.StartedAt
		}
		return recs[i].Meta.ID > recs[j].Meta.ID
	})
	return recs, nil
}

// Find looks a recording up by id, else by session name (the newest one with
// that name wins).
func Find(root, key string) (Recording, error) {
	if key != "" && !strings.ContainsAny(key, `/\`) && key != "." && key != ".." {
		if rec, err := Load(filepath.Join(root, key)); err == nil {
			return rec, nil
		}
	}
	recs, err := List(root)
	if err != nil {
		return Recording{}, err
	}
	for _, rec := range recs {
		if rec.Meta.Name == key {
			return rec, nil
		}
	}
	return Recording{}, fmt.Errorf("no recording %q", key)
}

// FinalizeStale gives every recording that is not ended, not pointed at by a
// live session (live holds those directories) and has no running recorder an
// ended_at: the time its newest segment was last written.
func FinalizeStale(root string, live map[string]bool) error {
	recs, err := List(root)
	if err != nil {
		return err
	}
	var errs []error
	for _, rec := range recs {
		if rec.Meta.EndedAt != "" || live[rec.Dir] || RecorderRunning(rec.Dir) {
			continue
		}
		at := time.Time{}
		if n := len(rec.Segments); n > 0 {
			if info, err := os.Stat(rec.Segments[n-1]); err == nil {
				at = info.ModTime()
			}
		}
		if at.IsZero() {
			at, _ = time.Parse(time.RFC3339, rec.Meta.StartedAt)
		}
		if err := Finalize(rec.Dir, at); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// segments lists the numbers of dir's stdout.NNNNNN.log files, ascending.
func segments(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "stdout.") || !strings.HasSuffix(name, ".log") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "stdout."), ".log"))
		if err == nil && n > 0 {
			numbers = append(numbers, n)
		}
	}
	sort.Ints(numbers)
	return numbers, nil
}

func segmentPath(dir string, n int, ext string) string {
	return filepath.Join(dir, fmt.Sprintf("stdout.%06d%s", n, ext))
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
