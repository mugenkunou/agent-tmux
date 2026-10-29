package termshare

import (
	"fmt"
	"os"
	"testing"

	"github.com/mugenkunou/agent-tmux/internal/recording"
)

// TestMain doubles as the recorder: Create has tmux run this program
// (os.Executable, which here is the test binary) as `<exe> _record <dir>`.
// Recordings go to a temp dir, never the real ~/.local/state.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "_record" {
		if err := recording.Record(os.Args[2], os.Stdin, recording.Options{}); err != nil {
			fmt.Fprintln(os.Stderr, "_record:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "at-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("AGENT_TMUX_STATE_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
