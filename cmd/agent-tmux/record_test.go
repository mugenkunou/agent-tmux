package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mugenkunou/agent-tmux/internal/recording"
)

// A dead recorder shows in status and doctor, is logged once however often
// it is seen, and kill still leaves a finished recording.
func TestStatusAndDoctorReportADeadRecorder(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	runtimeDir, err := os.MkdirTemp("", "at-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("AGENT_TMUX_RUNTIME_DIR", runtimeDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := run(ctx, args, &out, &errOut); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut.String())
		}
		return out.String()
	}

	t.Cleanup(func() { run(context.Background(), []string{"kill", "rec"}, &bytes.Buffer{}, &bytes.Buffer{}) })
	// create attaches when stdin is a terminal; this test has none.
	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = stdin
	cli("create", "rec")
	os.Stdin = saved
	stdin.Close()
	stdinWriter.Close()
	status := cli("status", "rec")
	if !strings.Contains(status, "\nrecorder: running\n") {
		t.Fatalf("status of a new session:\n%s", status)
	}
	root, _ := recording.Root()
	if !strings.Contains(cli("doctor"), "recordings: "+root+"\n") {
		t.Error("doctor does not show the recordings directory")
	}
	var dir string
	for _, line := range strings.Split(status, "\n") {
		if after, ok := strings.CutPrefix(line, "recording: "); ok {
			dir = after
		}
	}
	if filepath.Dir(dir) != root {
		t.Fatalf("recording %q is not under %s", dir, root)
	}

	killRecorder(t, dir)
	for range 2 {
		if status := cli("status", "rec"); !strings.Contains(status, "\nrecorder: stopped") {
			t.Fatalf("status after the recorder died:\n%s", status)
		}
	}
	if doctor := cli("doctor"); !strings.Contains(doctor, "warning: rec: recorder stopped") {
		t.Errorf("doctor does not report the dead recorder:\n%s", doctor)
	}
	died := 0
	for _, e := range readEvents(t, dir) {
		if e["event"] == "recorder-died" {
			died++
		}
	}
	if died != 1 {
		t.Errorf("recorder-died logged %d times, want 1", died)
	}

	cli("kill", "rec")
	if meta, err := recording.ReadMeta(dir); err != nil || meta.EndedAt == "" {
		t.Errorf("meta after kill: %+v, %v", meta, err)
	}
}

// killRecorder SIGKILLs the recorder of dir, found by its command line.
func killRecorder(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if bytes.HasSuffix(cmdline, []byte("\x00_record\x00"+dir+"\x00")) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			if !recording.WaitRecorder(dir, false, 3*time.Second) {
				t.Fatal("recorder still holds its lock after SIGKILL")
			}
			return
		}
	}
	t.Fatalf("no recorder process for %s", dir)
}

// _record's flags reach the recorder: tiny segments rotate, and only --keep
// of them stay.
func TestRecordFlags(t *testing.T) {
	dir, err := recording.Create(t.TempDir(), "flags", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved; stdin.Close() }()
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"_record", "--segment-size", "4", "--keep", "2", dir}, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	for _, line := range []string{"aaa\n", "bbb\n", "ccc\n", "ddd\n"} {
		_, _ = writer.WriteString(line)
		time.Sleep(30 * time.Millisecond)
	}
	writer.Close()
	if code := <-done; code != 0 {
		t.Fatalf("_record exit %d", code)
	}
	rec, err := recording.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Segments) != 2 || rec.Meta.FirstSegment != 3 || rec.Meta.EndedAt == "" {
		t.Errorf("recording = %+v", rec)
	}
}
