package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mugenkunou/agent-tmux/internal/recording"
	"github.com/mugenkunou/agent-tmux/internal/termshare"
)

// recordCommand is the recorder. `create` has tmux pipe the pane's output into
// `agent-tmux _record <dir>`; it runs until tmux closes the pipe (the session
// ended). The flags exist for tests.
func recordCommand() *command {
	c := newCommand("_record", "record pane output piped in by tmux into a recording directory")
	c.hidden = true
	c.noManager = true
	segmentSize := c.fs.Int64("segment-size", recording.DefaultSegmentSize, "start a new segment after this many `bytes`")
	keep := c.fs.Int("keep", recording.DefaultKeep, "keep this many `segments`, deleting older ones")
	c.args = []positional{{name: "dir", kind: argFree}}
	c.run = func(inv *invocation) int {
		// tmux sends the recorder no signals, but a hangup from elsewhere
		// must not cut a recording short.
		signal.Ignore(syscall.SIGHUP)
		if err := recording.Record(inv.args[0], os.Stdin, recording.Options{SegmentSize: *segmentSize, Keep: *keep}); err != nil {
			return fail(inv.stderr, err)
		}
		return 0
	}
	return c
}

// recorderLabel describes a session's recorder for status and doctor, and
// logs recorder-died the first time anyone sees it dead.
func recorderLabel(s termshare.Session) string {
	switch s.Recorder {
	case termshare.RecorderRunning:
		return "running"
	case termshare.RecorderStopped:
		if err := recording.NoteRecorderDied(s.Recording); err != nil {
			fmt.Fprintln(os.Stderr, "agent-tmux: warning:", err)
		}
		return "stopped (output is no longer recorded; kill the session and create a new one)"
	default:
		return "none (made by an older agent-tmux)"
	}
}
