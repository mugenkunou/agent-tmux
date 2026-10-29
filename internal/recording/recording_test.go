package recording

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Nothing in these tests may write to the real ~/.local/state.
	dir, err := os.MkdirTemp("", "at-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("AGENT_TMUX_STATE_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestRootOrder(t *testing.T) {
	t.Setenv("AGENT_TMUX_STATE_DIR", "/state")
	t.Setenv("XDG_STATE_HOME", "/xdg")
	t.Setenv("HOME", "/home/u")
	for _, tc := range []struct{ unset, want string }{
		{"", "/state"},
		{"AGENT_TMUX_STATE_DIR", "/xdg/agent-tmux"},
		{"XDG_STATE_HOME", "/home/u/.local/state/agent-tmux"},
	} {
		if tc.unset != "" {
			t.Setenv(tc.unset, "")
		}
		if got, err := Root(); err != nil || got != tc.want {
			t.Errorf("Root() = %q, %v; want %q", got, err, tc.want)
		}
	}
}

func TestCreateNamesCollisionsAndPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "recs")
	now := time.Date(2026, 10, 29, 14, 2, 0, 0, time.Local)
	var ids []string
	for range 3 {
		dir, err := Create(root, "web", now)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, filepath.Base(dir))
	}
	want := []string{"web-2026-10-29-1402", "web-2026-10-29-1402-2", "web-2026-10-29-1402-3"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	assertMode(t, root, dirMode)
	dir := filepath.Join(root, want[0])
	assertMode(t, dir, dirMode)
	assertMode(t, filepath.Join(dir, metaFile), fileMode)
	meta, err := ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "web" || meta.ID != want[0] || meta.StartedAt != now.UTC().Format(time.RFC3339) ||
		meta.EndedAt != "" || meta.FirstSegment != 1 || meta.FirstSegmentAt != meta.StartedAt {
		t.Errorf("initial meta = %+v", meta)
	}

	if err := LogEvent(dir, "create", Fields{"actor": "a"}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(dir, eventsFile), fileMode)
	events := readEvents(t, dir)
	if len(events) != 1 || events[0]["event"] != "create" || events[0]["actor"] != "a" || events[0]["at"] == "" {
		t.Errorf("events = %v", events)
	}
}

// Raw bytes go to the log unchanged, the timing lines add up to the log size,
// and ended_at is written when the input ends.
func TestRecordIsByteExactWithTimingAndEnd(t *testing.T) {
	dir := newRecording(t)
	input := "login banner\r\n\x1b[31mred\x1b[0m\r\n$ "
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Record(dir, pr, Options{}) }()
	for _, part := range []string{input[:5], input[5:20], input[20:]} {
		if _, err := pw.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !WaitRecorder(dir, true, time.Second) {
		t.Fatal("recorder does not hold its lock")
	}
	if meta, _ := ReadMeta(dir); meta.EndedAt != "" {
		t.Fatal("ended_at set while recording")
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(segmentPath(dir, 1, ".log"))
	if err != nil || string(got) != input {
		t.Fatalf("log = %q, %v; want %q", got, err, input)
	}
	if sum := timingSum(t, dir, 1); sum != len(input) {
		t.Errorf("timing sums to %d bytes, log has %d", sum, len(input))
	}
	assertMode(t, segmentPath(dir, 1, ".log"), fileMode)
	assertMode(t, segmentPath(dir, 1, ".timing"), fileMode)
	assertMode(t, filepath.Join(dir, lockFile), fileMode)
	if meta, _ := ReadMeta(dir); meta.EndedAt == "" {
		t.Error("ended_at not written on EOF")
	}
	if RecorderRunning(dir) {
		t.Error("lock still held after the recorder finished")
	}
}

// A block that would overflow the segment is split at a newline when it has
// one, and the timing of each segment adds up to its log.
func TestRotationPrefersANewline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parts  []string
		first  string
		second string
	}{
		{"last newline that fits", []string{"aaaa\n", "bb\ncc\ndd"}, "aaaa\nbb\n", "cc\ndd"},
		{"first newline past the size", []string{"aaaaaaa\n", "bb\ncc\ndd", "ee"}, "aaaaaaa\nbb\n", "cc\nddee"},
		{"no newline: at the size", []string{"0123456", "789abcdef"}, "0123456789", "abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newRecording(t)
			if err := Record(dir, &chunks{parts: tc.parts}, Options{SegmentSize: 10}); err != nil {
				t.Fatal(err)
			}
			for n, want := range map[int]string{1: tc.first, 2: tc.second} {
				got, _ := os.ReadFile(segmentPath(dir, n, ".log"))
				if string(got) != want {
					t.Errorf("segment %d = %q, want %q", n, got, want)
				}
				if sum := timingSum(t, dir, n); sum != len(want) {
					t.Errorf("segment %d timing sums to %d, want %d", n, sum, len(want))
				}
			}
			if _, err := os.Stat(segmentPath(dir, 3, ".log")); !os.IsNotExist(err) {
				t.Error("an empty third segment was opened")
			}
			events := readEvents(t, dir)
			if len(events) != 1 || events[0]["event"] != "rotated" || events[0]["segment"] != float64(2) {
				t.Errorf("events = %v", events)
			}
		})
	}
}

// Only Keep segments stay; the oldest are deleted and meta.json says where the
// kept history starts.
func TestKeepDeletesOldestAndMovesFirstSegment(t *testing.T) {
	dir := newRecording(t)
	var parts []string
	for i := range 6 {
		parts = append(parts, fmt.Sprintf("line%d\n", i))
	}
	in := &chunks{parts: parts, pause: 1100 * time.Millisecond, pauseAt: 3}
	if err := Record(dir, in, Options{SegmentSize: 6, Keep: 3}); err != nil {
		t.Fatal(err)
	}
	// Each 6-byte line fills a segment, so segments 1..6 were written and
	// 4..6 are kept.
	rec, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range rec.Segments {
		names = append(names, filepath.Base(p))
	}
	if strings.Join(names, " ") != "stdout.000004.log stdout.000005.log stdout.000006.log" {
		t.Fatalf("kept segments = %v", names)
	}
	for _, n := range []int{1, 2, 3} {
		if _, err := os.Stat(segmentPath(dir, n, ".timing")); !os.IsNotExist(err) {
			t.Errorf("segment %d timing not deleted", n)
		}
	}
	if rec.Bytes != 18 {
		t.Errorf("bytes = %d, want 18", rec.Bytes)
	}
	if rec.Meta.FirstSegment != 4 {
		t.Errorf("first_segment = %d, want 4", rec.Meta.FirstSegment)
	}
	// Segment 4 opened after the pause; segment 1 before it.
	start, _ := time.Parse(time.RFC3339, rec.Meta.StartedAt)
	firstAt, _ := time.Parse(time.RFC3339, rec.Meta.FirstSegmentAt)
	if !firstAt.After(start) {
		t.Errorf("first_segment_at %s is not after started_at %s", rec.Meta.FirstSegmentAt, rec.Meta.StartedAt)
	}
	rotated := 0
	for _, e := range readEvents(t, dir) {
		if e["event"] == "rotated" {
			rotated++
		}
	}
	if rotated != 5 {
		t.Errorf("rotated events = %d, want 5", rotated)
	}
}

// A second recorder on the same recording must give up, not write alongside.
func TestSecondRecorderIsRefused(t *testing.T) {
	dir := newRecording(t)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Record(dir, pr, Options{}) }()
	if !WaitRecorder(dir, true, time.Second) {
		t.Fatal("first recorder did not lock")
	}
	if err := Record(dir, strings.NewReader("x"), Options{}); err == nil {
		t.Error("second recorder ran")
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeStale(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stale, _ := Create(root, "stale", now)
	live, _ := Create(root, "live", now)
	ended, _ := Create(root, "ended", now)
	if err := os.WriteFile(segmentPath(stale, 1, ".log"), []byte("x"), fileMode); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(segmentPath(stale, 1, ".log"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := Finalize(ended, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeStale(root, map[string]bool{live: true}); err != nil {
		t.Fatal(err)
	}
	if m, _ := ReadMeta(stale); m.EndedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("stale ended_at = %q", m.EndedAt)
	}
	if m, _ := ReadMeta(live); m.EndedAt != "" {
		t.Errorf("live recording finalized: %q", m.EndedAt)
	}
	if m, _ := ReadMeta(ended); m.EndedAt != "2025-01-01T00:00:00Z" {
		t.Errorf("ended recording changed: %q", m.EndedAt)
	}

	// A recording whose recorder still runs is left alone.
	running, _ := Create(root, "running", now)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Record(running, pr, Options{}) }()
	WaitRecorder(running, true, time.Second)
	if err := FinalizeStale(root, nil); err != nil {
		t.Fatal(err)
	}
	if m, _ := ReadMeta(running); m.EndedAt != "" {
		t.Error("finalized a recording with a running recorder")
	}
	pw.Close()
	<-done
}

func TestListAndFind(t *testing.T) {
	root := t.TempDir()
	older, _ := Create(root, "web", time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local))
	newer, _ := Create(root, "web", time.Date(2026, 1, 2, 10, 0, 0, 0, time.Local))
	other, _ := Create(root, "db", time.Date(2026, 1, 3, 10, 0, 0, 0, time.Local))
	if err := os.Mkdir(filepath.Join(root, "junk"), dirMode); err != nil {
		t.Fatal(err)
	}
	recs, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, r := range recs {
		dirs = append(dirs, r.Dir)
	}
	if strings.Join(dirs, " ") != strings.Join([]string{other, newer, older}, " ") {
		t.Errorf("List order = %v", dirs)
	}
	if rec, err := Find(root, "web"); err != nil || rec.Dir != newer {
		t.Errorf("Find(name) = %v, %v; want %s", rec.Dir, err, newer)
	}
	if rec, err := Find(root, filepath.Base(older)); err != nil || rec.Dir != older {
		t.Errorf("Find(id) = %v, %v; want %s", rec.Dir, err, older)
	}
	if _, err := Find(root, "nope"); err == nil {
		t.Error("Find(unknown) succeeded")
	}
	if _, err := Find(root, "../"+filepath.Base(root)); err == nil {
		t.Error("Find escaped the root")
	}
	if recs, err := List(filepath.Join(root, "missing")); err != nil || len(recs) != 0 {
		t.Errorf("List(missing root) = %v, %v", recs, err)
	}
}

func TestNoteRecorderDiedOnce(t *testing.T) {
	dir := newRecording(t)
	for range 3 {
		if err := NoteRecorderDied(dir); err != nil {
			t.Fatal(err)
		}
	}
	if events := readEvents(t, dir); len(events) != 1 || events[0]["event"] != "recorder-died" {
		t.Errorf("events = %v", events)
	}
}

// RecorderRunning must see a lock held by another process, which is how
// status and kill see the recorder that tmux started.
func TestRecorderRunningSeesOtherProcess(t *testing.T) {
	dir := newRecording(t)
	lock := filepath.Join(dir, lockFile)
	cmd := exec.Command("flock", lock, "sleep", "30")
	// flock(1) runs sleep as a child holding the lock: kill both.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skip("flock(1) not available:", err)
	}
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	t.Cleanup(func() { kill(); _ = cmd.Wait() })
	if !WaitRecorder(dir, true, 2*time.Second) {
		t.Fatal("lock held by another process not seen")
	}
	kill()
	if !WaitRecorder(dir, false, 2*time.Second) {
		t.Fatal("lock still seen after its holder died")
	}
}

// chunks returns one part per Read, so tests control block boundaries.
type chunks struct {
	parts   []string
	pause   time.Duration // sleep before part pauseAt
	pauseAt int
	i       int
}

func (c *chunks) Read(p []byte) (int, error) {
	if c.i >= len(c.parts) {
		return 0, io.EOF
	}
	if c.pause > 0 && c.i == c.pauseAt {
		time.Sleep(c.pause)
	}
	n := copy(p, c.parts[c.i])
	c.i++
	return n, nil
}

func newRecording(t *testing.T) string {
	t.Helper()
	dir, err := Create(t.TempDir(), "s", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func timingSum(t *testing.T, dir string, n int) int {
	t.Helper()
	f, err := os.Open(segmentPath(dir, n, ".timing"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			t.Fatalf("bad timing line %q", scanner.Text())
		}
		if _, err := strconv.ParseFloat(fields[0], 64); err != nil {
			t.Fatalf("bad delay in %q", scanner.Text())
		}
		bytes, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("bad byte count in %q", scanner.Text())
		}
		sum += bytes
	}
	return sum
}

func readEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, eventsFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad event line %q: %v", line, err)
		}
		events = append(events, e)
	}
	return events
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode %o, want %o", path, got, want)
	}
}
