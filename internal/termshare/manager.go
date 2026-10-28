package termshare

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OwnerUnclaimed means no attached client currently holds input, so
// scripted Send/SendKeys calls are allowed. Any other value is
// the actor id of whoever last claimed control via Open or Take.
const OwnerUnclaimed = ""

var (
	sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	// ErrControlled is wrapped with the actual actor id holding control;
	// check with errors.Is and read the message for who it is.
	ErrControlled = errors.New("another actor controls the terminal; wait until they release it or run agent-tmux yield")
)

// statusRightFormat renders a banner naming the actor that currently owns
// input (or that the session is open/unclaimed) and the key that changes
// it, so ownership is visible without a separate status command.
//
// Each style tag carries exactly one attribute (no comma inside `#[...]`):
// a comma inside a style tag is otherwise indistinguishable from the
// true/false separators of the surrounding `#{?cond,true,false}`, which
// silently drops the entire banner instead of raising a parse error.
const statusRightFormat = " #{?#{==:#{@agent-tmux-owner},}" +
	",#[bg=colour4]#[fg=black] OPEN - Ctrl-T takes control - Ctrl-] menu #[default]" +
	",#[bg=colour2]#[fg=black] CONTROLLED BY #{@agent-tmux-owner} - Ctrl-T releases - Ctrl-] menu #[default]} "

// Exit codes an attached tmux client ends with. Each key Open listens for is
// bound to `detach-client -E 'exit N'`: tmux honors detach-client even for a
// read-only client, and -E makes the client exit with N, so the caller learns
// which key ended the attach without any side channel. Any other detach
// (another process detached the client, the session was killed) exits 0.
const (
	exitDetached = 0
	exitToggle   = 10 // Ctrl-T: switch between watching and controlling
	exitQuit     = 11 // Ctrl-Q: leave open, keep the session
	exitMenu     = 12 // Ctrl-]: the escape menu
)

// keyBindings are the server-wide keys, installed by every attaching command
// so a server started by an older version picks up the current ones.
var keyBindings = []struct {
	key  string
	exit int
}{
	{"C-t", exitToggle},
	{"C-q", exitQuit},
	{"C-]", exitMenu},
}

// titleFormat is the terminal tab/window title shown while attached, so the
// right tab is findable among many and its state is visible without looking
// at the status bar: an eye while unclaimed (watching), a keyboard while an
// actor holds input. Like statusRightFormat it avoids commas outside the
// conditional's own separators and uses no style tags.
const titleFormat = "#{?#{==:#{@agent-tmux-owner},}" +
	",\U0001F440 agent-tmux: #S (watching)" +
	",\u2328\uFE0F agent-tmux: #S (#{@agent-tmux-owner} controls)}"

type Session struct {
	Name    string
	Owner   string
	Clients int
	Command string
	Path    string
}

type Manager struct {
	tmux       string
	runtimeDir string
	socketPath string
}

func NewManager() (*Manager, error) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return nil, errors.New("tmux is required but was not found in PATH")
	}

	runtimeDir := os.Getenv("AGENT_TMUX_RUNTIME_DIR")
	if runtimeDir == "" {
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			runtimeDir = filepath.Join(xdg, "agent-tmux")
		} else {
			runtimeDir = filepath.Join(os.TempDir(), fmt.Sprintf("agent-tmux-%d", os.Getuid()))
		}
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime directory: %w", err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure runtime directory: %w", err)
	}

	return &Manager{
		tmux:       tmux,
		runtimeDir: runtimeDir,
		socketPath: filepath.Join(runtimeDir, "tmux.sock"),
	}, nil
}

func (m *Manager) SocketPath() string {
	return m.socketPath
}

func (m *Manager) Version(ctx context.Context) (string, error) {
	output, err := m.run(ctx, nil, "-V")
	return strings.TrimSpace(output), err
}

// DefaultHistoryLimit is how many lines of scrollback a new session keeps in
// tmux. tmux's own default is 2000. Memory is spent only as lines arrive,
// roughly 2 KB per kept line, so a full 50000-line history is about 100 MB.
const DefaultHistoryLimit = 50000

func (m *Manager) Create(ctx context.Context, name, shell string, cols, rows int) error {
	return m.CreateWithHistory(ctx, name, shell, cols, rows, DefaultHistoryLimit)
}

// CreateWithHistory is Create with the session's scrollback size in lines.
func (m *Manager) CreateWithHistory(ctx context.Context, name, shell string, cols, rows, history int) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	if cols < 20 || rows < 5 {
		return errors.New("terminal size must be at least 20x5")
	}
	if shell == "" {
		return errors.New("shell cannot be empty")
	}
	if history < 0 {
		return errors.New("history must not be negative")
	}

	return m.withLock("create", func() error {
		exists, err := m.HasSession(ctx, name)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("session %q already exists", name)
		}

		// A pane takes its history-limit from the global option when it is
		// created, so set it in the same tmux invocation, right before
		// new-session (start-server makes this work on a cold server too).
		// The create lock keeps concurrent creates from mixing values.
		shellCommand := "exec " + shellQuote(shell) + " -l"
		if _, err := m.run(ctx, nil,
			"start-server", ";",
			"set-option", "-g", "history-limit", strconv.Itoa(history), ";",
			"new-session", "-d", "-s", name,
			"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows),
			shellCommand,
		); err != nil {
			return err
		}

		settings := [][2]string{
			{"@agent-tmux-owner", OwnerUnclaimed},
			{"@agent-tmux-owner-tty", ""},
			{"@agent-tmux-owner-proc", ""},
			{"@agent-tmux-generation", "1"},
			{"@agent-tmux-created", time.Now().UTC().Format(time.RFC3339)},
			{"status-left", " agent-tmux:#S "},
			{"status-left-length", "40"},
		}
		for _, setting := range settings {
			if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), setting[0], setting[1]); err != nil {
				_, _ = m.run(context.Background(), nil, "kill-session", "-t", exact(name))
				return err
			}
		}
		return nil
	})
}

func (m *Manager) HasSession(ctx context.Context, name string) (bool, error) {
	if err := validateSessionName(name); err != nil {
		return false, err
	}
	if _, err := os.Stat(m.socketPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	_, err := m.run(ctx, nil, "has-session", "-t", exact(name))
	if err == nil {
		return true, nil
	}
	var commandError *CommandError
	if errors.As(err, &commandError) && (strings.Contains(commandError.Output, "can't find session") || strings.Contains(commandError.Output, "no server running")) {
		return false, nil
	}
	return false, err
}

func (m *Manager) List(ctx context.Context) ([]Session, error) {
	if _, err := os.Stat(m.socketPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	output, err := m.run(ctx, nil, "list-sessions", "-F", sessionFormat())
	if err != nil {
		var commandError *CommandError
		if errors.As(err, &commandError) && strings.Contains(commandError.Output, "no server running") {
			return nil, nil
		}
		return nil, err
	}
	return parseSessions(output)
}

func (m *Manager) Status(ctx context.Context, name string) (Session, error) {
	if err := validateSessionName(name); err != nil {
		return Session{}, err
	}
	output, err := m.run(ctx, nil, "display-message", "-p", "-t", target(name), sessionFormat())
	if err != nil {
		return Session{}, err
	}
	sessions, err := parseSessions(output)
	if err != nil {
		return Session{}, err
	}
	if len(sessions) != 1 {
		return Session{}, fmt.Errorf("unexpected status response for %q", name)
	}
	return sessions[0], nil
}

func (m *Manager) Screen(ctx context.Context, name string, history int) (string, error) {
	if err := validateSessionName(name); err != nil {
		return "", err
	}
	if history < 0 {
		return "", errors.New("history must not be negative")
	}
	args := []string{"capture-pane", "-p", "-J", "-t", target(name)}
	if history > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(history))
	}
	return m.run(ctx, nil, args...)
}

func (m *Manager) Send(ctx context.Context, name, text string, enter bool) error {
	if text == "" {
		return errors.New("text cannot be empty")
	}
	if strings.IndexByte(text, 0) >= 0 {
		return errors.New("text cannot contain a NUL byte")
	}
	return m.withAgentControl(ctx, name, func() error {
		if _, err := m.run(ctx, nil, "send-keys", "-t", target(name), "-l", "--", text); err != nil {
			return err
		}
		if enter {
			_, err := m.run(ctx, nil, "send-keys", "-t", target(name), "Enter")
			return err
		}
		return nil
	})
}

func (m *Manager) SendKeys(ctx context.Context, name string, keys []string) error {
	if len(keys) == 0 {
		return errors.New("at least one key is required")
	}
	return m.withAgentControl(ctx, name, func() error {
		args := []string{"send-keys", "-t", target(name), "--"}
		args = append(args, keys...)
		_, err := m.run(ctx, nil, args...)
		return err
	})
}

func (m *Manager) WaitFor(ctx context.Context, name, text string, timeout time.Duration, history int) (string, error) {
	if timeout <= 0 {
		return "", errors.New("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var contents string
	for {
		var err error
		contents, err = m.Screen(ctx, name, history)
		if err != nil {
			return contents, err
		}
		if strings.Contains(contents, text) {
			return contents, nil
		}
		select {
		case <-ctx.Done():
			return contents, fmt.Errorf("waiting for %q: %w", text, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) Watch(ctx context.Context, name string) error {
	if err := m.requireSession(ctx, name); err != nil {
		return err
	}
	if err := m.ensureBindings(ctx); err != nil {
		return err
	}
	defer preserveTerminalTitle(os.Stdout)()
	_, err := m.attach(ctx, name, true, "")
	return err
}

// OpenHooks lets the caller show what Open is doing. Both are optional.
type OpenHooks struct {
	// Phase is called as each attach starts: with OwnerUnclaimed when
	// watching, with this process's actor id when controlling.
	Phase func(owner string)
	// Menu is called after Ctrl-], with control released and the terminal
	// in a mode where output prints normally. It reads keypresses only
	// through readKey. It returns true to end Open, false to go back to the
	// phase Ctrl-] was pressed from.
	Menu func(readKey func() (byte, error)) (leave bool)
}

// Open attaches once and loops between a genuinely read-only watching phase
// and a writable controlling phase. Ctrl-T toggles between them, Ctrl-Q ends
// Open, and Ctrl-] hands over to hooks.Menu; all three work from either
// phase (see keyBindings). Closing the terminal or killing the session also
// ends the loop, and control is always released when the writable attach
// ends, however it ends.
func (m *Manager) Open(ctx context.Context, name string, hooks OpenHooks) error {
	if err := m.requireSession(ctx, name); err != nil {
		return err
	}
	if err := m.ensureBindings(ctx); err != nil {
		return err
	}
	defer preserveTerminalTitle(os.Stdout)()
	defer surviveHangup()()

	id := actorID()
	tty := currentTTY()
	controlling := false
	for {
		if exists, err := m.HasSession(ctx, name); err != nil {
			return err
		} else if !exists {
			return nil
		}
		owner, notice := OwnerUnclaimed, ""
		if controlling {
			if err := m.claim(ctx, name, id, tty); err != nil {
				if !errors.Is(err, ErrControlled) {
					return err
				}
				// Someone else holds control: stay watching, and say so.
				controlling = false
				notice = "Ctrl-T refused: " + err.Error()
			} else {
				owner = id
			}
		}
		if hooks.Phase != nil {
			hooks.Phase(owner)
		}
		code, attachErr := m.attach(ctx, name, !controlling, notice)
		if controlling {
			_ = m.release(context.Background(), name)
		}
		if attachErr != nil {
			return attachErr
		}
		switch code {
		case exitToggle:
			controlling = !controlling
		case exitMenu:
			if hooks.Menu != nil && hooks.Menu(func() (byte, error) { return ReadKey(os.Stdin) }) {
				return nil
			}
		default:
			return nil
		}
	}
}

// ensureBindings installs keyBindings server-wide. A binding must be a bare
// detach-client to fire for a read-only client (a chained command list is
// dropped whole), which is why each key reports itself through the client's
// exit code instead of setting state.
func (m *Manager) ensureBindings(ctx context.Context) error {
	for _, binding := range keyBindings {
		if _, err := m.run(ctx, nil, "bind-key", "-n", binding.key,
			"detach-client", "-E", "exit "+strconv.Itoa(binding.exit)); err != nil {
			return err
		}
	}
	return nil
}

// surviveHangup keeps this process alive through SIGHUP while a client is
// attached. Closing the terminal tab hangs up both this process and its tmux
// client; the client exits, and this process lives long enough to release
// the control it holds instead of dying with it. The returned function
// restores the default handling.
func surviveHangup() (restore func()) {
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	return func() { signal.Stop(hangup) }
}

func (m *Manager) Take(ctx context.Context, name string) error {
	id := actorID()
	tty := currentTTY()
	if err := m.ensureBindings(ctx); err != nil {
		return err
	}
	if err := m.claim(ctx, name, id, tty); err != nil {
		return err
	}
	defer preserveTerminalTitle(os.Stdout)()
	defer surviveHangup()()
	_, attachErr := m.attach(ctx, name, false, "")
	releaseErr := m.release(context.Background(), name)
	if attachErr != nil {
		return attachErr
	}
	return releaseErr
}

// Yield takes control away from whoever holds it, live or not, and puts
// their writable tmux client back to watching (a plain `open` re-attaches
// read-only; a native `open` checks ownership before every keystroke it
// forwards). It is the manual override; normal handoffs never need it.
func (m *Manager) Yield(ctx context.Context, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	return m.withLock(name, func() error {
		if err := m.requireSession(ctx, name); err != nil {
			return err
		}
		if err := m.clearOwner(ctx, name); err != nil {
			return err
		}
		return m.detachWriters(ctx, name)
	})
}

// detachWriters ends every writable, non-control tmux client on the
// session with exitToggle, so a plain `open` holding it falls back to
// watching. The caller holds the session lock.
func (m *Manager) detachWriters(ctx context.Context, name string) error {
	out, err := m.run(ctx, nil, "list-clients", "-t", exact(name), "-F",
		"#{client_name}\t#{client_readonly}\t#{client_control_mode}")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[1] == "1" || fields[2] == "1" {
			continue
		}
		_, _ = m.run(ctx, nil, "detach-client", "-t", fields[0], "-E", "exit "+strconv.Itoa(exitToggle))
	}
	return nil
}

// Restart kills whatever runs in the session's pane and starts the session's
// shell again in its place. The session, its name and its attached clients
// stay. It is the way out of a program that Ctrl-C cannot reach, such as an
// ssh connection frozen by a network drop.
func (m *Manager) Restart(ctx context.Context, name string) error {
	if err := m.requireSession(ctx, name); err != nil {
		return err
	}
	_, err := m.run(ctx, nil, "respawn-pane", "-k", "-t", target(name))
	return err
}

func (m *Manager) Kill(ctx context.Context, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	_, err := m.run(ctx, nil, "kill-session", "-t", exact(name))
	if removeErr := os.Remove(m.lockPath(name)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
		err = removeErr
	}
	return err
}

// claim records actorID (and its best-effort tty, for the audit trail) as
// the session's owner, so scripted Send/SendKeys calls are
// refused until release runs. It also records this process's identity: if
// the process goes away without releasing (killed, or its terminal closed
// before it could), the claim is stale and withAgentControl clears it.
//
// Claims are exclusive: while another live process holds control, claim
// fails with ErrControlled. This is the rule that keeps two attached
// terminals (in any mix of plain and native open, or take) from typing into
// the session at once. Re-claiming from the process that already holds it
// succeeds.
func (m *Manager) claim(ctx context.Context, name, actorID, tty string) error {
	if actorID == "" {
		return errors.New("actor id cannot be empty")
	}
	if err := validateSessionName(name); err != nil {
		return err
	}
	return m.withLock(name, func() error {
		if err := m.requireSession(ctx, name); err != nil {
			return err
		}
		owner, proc, err := m.readOwner(ctx, name)
		if err != nil {
			return err
		}
		if owner != OwnerUnclaimed && proc != processIdentity(os.Getpid()) && !claimStale(proc) {
			return fmt.Errorf("actor %q controls the terminal: %w", owner, ErrControlled)
		}
		generation := m.readGeneration(ctx, name)
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-owner", actorID); err != nil {
			return err
		}
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-owner-tty", tty); err != nil {
			return err
		}
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-owner-proc", processIdentity(os.Getpid())); err != nil {
			return err
		}
		_, err = m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-generation", strconv.Itoa(generation+1))
		return err
	})
}

// release clears the session's owner back to OwnerUnclaimed, but only if
// this process holds it: a terminal that never had control must not be
// able to drop someone else's.
func (m *Manager) release(ctx context.Context, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	return m.withLock(name, func() error {
		if err := m.requireSession(ctx, name); err != nil {
			return err
		}
		_, proc, err := m.readOwner(ctx, name)
		if err != nil {
			return err
		}
		if proc != processIdentity(os.Getpid()) {
			return nil
		}
		return m.clearOwner(ctx, name)
	})
}

// readOwner returns the session's owner and the process identity that
// claimed it.
func (m *Manager) readOwner(ctx context.Context, name string) (owner, proc string, err error) {
	raw, err := m.run(ctx, nil, "display-message", "-p", "-t", target(name), "#{@agent-tmux-owner}\t#{@agent-tmux-owner-proc}")
	if err != nil {
		return "", "", err
	}
	owner, proc, _ = strings.Cut(strings.TrimRight(raw, "\n"), "\t")
	return owner, proc, nil
}

// holdsControl reports whether this process currently owns the session.
func (m *Manager) holdsControl(ctx context.Context, name string) (owner string, held bool, err error) {
	var proc string
	owner, proc, err = m.readOwner(ctx, name)
	if err != nil {
		return "", false, err
	}
	if owner != OwnerUnclaimed && claimStale(proc) {
		owner = OwnerUnclaimed
	}
	return owner, owner != OwnerUnclaimed && proc == processIdentity(os.Getpid()), nil
}

// clearOwner does release's work; the caller holds the session lock.
func (m *Manager) clearOwner(ctx context.Context, name string) error {
	generation := m.readGeneration(ctx, name)
	for _, option := range []string{"@agent-tmux-owner", "@agent-tmux-owner-tty", "@agent-tmux-owner-proc"} {
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), option, ""); err != nil {
			return err
		}
	}
	_, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-generation", strconv.Itoa(generation+1))
	return err
}

func (m *Manager) readGeneration(ctx context.Context, name string) int {
	generation := 0
	if output, err := m.run(ctx, nil, "show-option", "-qv", "-t", exact(name), "@agent-tmux-generation"); err == nil {
		generation, _ = strconv.Atoi(strings.TrimSpace(output))
	}
	return generation
}

func (m *Manager) withAgentControl(ctx context.Context, name string, action func() error) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	return m.withLock(name, func() error {
		if err := m.requireSession(ctx, name); err != nil {
			return err
		}
		owner, proc, err := m.readOwner(ctx, name)
		if err != nil {
			return err
		}
		if owner != OwnerUnclaimed && claimStale(proc) {
			// Whoever claimed control is gone without releasing it (for
			// example, its terminal tab was closed): nobody can be typing.
			if err := m.clearOwner(ctx, name); err != nil {
				return err
			}
			owner = OwnerUnclaimed
		}
		if owner != OwnerUnclaimed {
			return fmt.Errorf("actor %q controls the terminal: %w", owner, ErrControlled)
		}
		return action()
	})
}

func (m *Manager) requireSession(ctx context.Context, name string) error {
	exists, err := m.HasSession(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("session %q does not exist", name)
	}
	return nil
}

// Xterm window-manipulation sequences that push/pop the terminal's title on
// its own stack (CSI 22;0 t / CSI 23;0 t). tmux sets the title while attached
// but, under typical TERM values, never restores it on detach, so without this
// the tab would keep saying "agent-tmux: ..." after the user has left. Terminals
// that lack a title stack (or don't implement these) ignore them harmlessly,
// leaving the title as it was before this change.
const (
	pushTitle = "\x1b[22;0t"
	popTitle  = "\x1b[23;0t"
)

// preserveTerminalTitle saves the terminal's current title and returns a
// function that restores it. It does nothing unless w is a real terminal, so
// scripted callers and pipes never receive stray escape bytes.
func preserveTerminalTitle(w *os.File) (restore func()) {
	info, err := w.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}
	_, _ = w.WriteString(pushTitle)
	return func() { _, _ = w.WriteString(popTitle) }
}

// applyClientOptions sets what an attached client shows: the terminal title
// from titleFormat and the ownership banner from statusRightFormat. It runs on
// every attach rather than only in Create so sessions made by an older
// version pick up the current text too; set-option is idempotent. tmux
// re-expands both formats on redraw, so they track ownership changes.
func (m *Manager) applyClientOptions(ctx context.Context, name string) error {
	for _, setting := range [][2]string{
		{"set-titles", "on"},
		{"set-titles-string", titleFormat},
		{"status-right", statusRightFormat},
		{"status-right-length", "90"},
	} {
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

// attach runs a tmux client on this terminal until it ends, and returns the
// exit code naming the key that ended it (see exitToggle and friends), or
// exitDetached for any other clean detach. A non-empty notice is shown on
// the client's status line for a few seconds.
func (m *Manager) attach(ctx context.Context, name string, readOnly bool, notice string) (int, error) {
	if err := m.applyClientOptions(ctx, name); err != nil {
		return 0, err
	}
	args := m.baseArgs()
	args = append(args, "attach-session")
	if readOnly {
		args = append(args, "-r")
	}
	args = append(args, "-t", exact(name))
	if notice != "" {
		// display-message expands formats; escape # so the text is literal.
		args = append(args, ";", "display-message", "-d", "5000", strings.ReplaceAll(notice, "#", "##"))
	}
	cmd := exec.CommandContext(ctx, m.tmux, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withoutEnv(os.Environ(), "TMUX")
	code, err := attachExit(cmd.Run())
	if err != nil {
		return 0, fmt.Errorf("attach to %q: %w", name, err)
	}
	return code, nil
}

// attachExit turns a finished tmux client's error into the key exit code it
// carries, or passes a genuine failure through.
func attachExit(err error) (int, error) {
	if err == nil {
		return exitDetached, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		for _, binding := range keyBindings {
			if exitErr.ExitCode() == binding.exit {
				return binding.exit, nil
			}
		}
	}
	return 0, err
}

func (m *Manager) withLock(name string, action func() error) error {
	lock, err := os.OpenFile(m.lockPath(name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open session lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock session: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return action()
}

func (m *Manager) lockPath(name string) string {
	return filepath.Join(m.runtimeDir, name+".lock")
}

func (m *Manager) run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	commandArgs := append(m.baseArgs(), args...)
	cmd := exec.CommandContext(ctx, m.tmux, commandArgs...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return output.String(), &CommandError{Args: args, Output: strings.TrimSpace(output.String()), Err: err}
	}
	return output.String(), nil
}

func (m *Manager) baseArgs() []string {
	return []string{"-f", "/dev/null", "-S", m.socketPath}
}

type CommandError struct {
	Args   []string
	Output string
	Err    error
}

func (e *CommandError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("tmux %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("tmux %s: %s", strings.Join(e.Args, " "), e.Output)
}

func (e *CommandError) Unwrap() error {
	return e.Err
}

func validateSessionName(name string) error {
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid session name %q; use 1-64 letters, numbers, dots, dashes, or underscores", name)
	}
	return nil
}

func exact(name string) string {
	return name
}

func target(name string) string {
	return exact(name) + ":0.0"
}

func sessionFormat() string {
	return "#{session_name}\t#{@agent-tmux-owner}\t#{session_attached}\t#{pane_current_command}\t#{pane_current_path}\t#{@agent-tmux-owner-proc}"
}

// processIdentity names a running process as "<pid>:<start time>", so a pid
// later reused by an unrelated process does not pass for the original. It
// is empty when the process does not exist or has already exited.
func processIdentity(pid int) string {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The command name (field 2) is parenthesised and may contain spaces;
	// the fields after its closing paren start at field 3, and the start
	// time is field 22.
	closing := bytes.LastIndexByte(stat, ')')
	if closing < 0 {
		return ""
	}
	fields := strings.Fields(string(stat[closing+1:]))
	if len(fields) < 20 || fields[0] == "Z" {
		// A zombie has exited; only its parent has not collected it yet.
		return ""
	}
	return strconv.Itoa(pid) + ":" + fields[19]
}

// claimStale reports whether the process recorded by claim is gone. A claim
// with no recorded process (made by an older version) is never stale.
func claimStale(proc string) bool {
	if proc == "" {
		return false
	}
	pidText, _, _ := strings.Cut(proc, ":")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return false
	}
	return processIdentity(pid) != proc
}

// actorID identifies the caller for ownership purposes. Set
// AGENT_TMUX_ACTOR_ID once per logical caller (a shell session, an
// automation process) so repeated invocations share one id; otherwise a
// fresh one is generated per process. This is self-reported, not
// authenticated — see the security notes in README.md.
func actorID() string {
	if id := strings.TrimSpace(os.Getenv("AGENT_TMUX_ACTOR_ID")); id != "" {
		return id
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Sprintf("actor-%d", os.Getpid())
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "actor"
	}
	return fmt.Sprintf("%s-%s", user, hex.EncodeToString(suffix))
}

// currentTTY best-effort identifies the caller's controlling terminal, for
// the audit trail alongside actorID. Empty when there isn't one (e.g. a
// scripted invocation with no attached terminal).
func currentTTY() string {
	target, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		return ""
	}
	return target
}

func parseSessions(output string) ([]Session, error) {
	// Trim only line breaks: the last field may be empty, and TrimSpace
	// would eat the tab in front of it.
	output = strings.Trim(output, "\r\n")
	if output == "" {
		return nil, nil
	}
	lines := strings.Split(output, "\n")
	sessions := make([]Session, 0, len(lines))
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", 6)
		if len(fields) != 6 {
			return nil, fmt.Errorf("cannot parse tmux session response %q", line)
		}
		clients, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("cannot parse client count %q: %w", fields[2], err)
		}
		owner := fields[1]
		if claimStale(fields[5]) {
			owner = OwnerUnclaimed
		}
		sessions = append(sessions, Session{
			Name:    fields[0],
			Owner:   owner,
			Clients: clients,
			Command: fields[3],
			Path:    fields[4],
		})
	}
	return sessions, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func withoutEnv(environment []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
