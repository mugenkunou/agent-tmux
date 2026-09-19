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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OwnerUnclaimed means no attached client currently holds input, so
// scripted Send/SendKeys/RunCommand calls are allowed. Any other value is
// the actor id of whoever last claimed control via Open or Take.
const OwnerUnclaimed = ""

var (
	sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	// ErrControlled is wrapped with the actual actor id holding control;
	// check with errors.Is and read the message for who it is.
	ErrControlled = errors.New("another actor controls the terminal; wait until they release it or run agent-tmux yield")
	// rcPattern matches the [RC:exitCode:seq] block prepended to PS1 by setupPS1.
	// The seq counter increments with each prompt draw, making completion detection
	// reliable even when exit code and working directory are unchanged.
	rcPattern = regexp.MustCompile(`\[RC:(\d+):(\d+)\]`)
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
	",#[bg=colour4]#[fg=black] OPEN - Ctrl-T takes control #[default]" +
	",#[bg=colour2]#[fg=black] CONTROLLED BY #{@agent-tmux-owner} - Ctrl-T releases #[default]} "

type Session struct {
	Name    string
	Owner   string
	Clients int
	Command string
	Path    string
}

type RunResult struct {
	Screen   string
	ExitCode int
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

func (m *Manager) Create(ctx context.Context, name, shell string, cols, rows int) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	if cols < 20 || rows < 5 {
		return errors.New("terminal size must be at least 20x5")
	}
	if shell == "" {
		return errors.New("shell cannot be empty")
	}

	if err := m.withLock("create", func() error {
		exists, err := m.HasSession(ctx, name)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("session %q already exists", name)
		}

		shellCommand := "exec " + shellQuote(shell) + " -l"
		if _, err := m.run(ctx, nil,
			"new-session", "-d", "-s", name,
			"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows),
			shellCommand,
		); err != nil {
			return err
		}

		settings := [][2]string{
			{"@agent-tmux-owner", OwnerUnclaimed},
			{"@agent-tmux-owner-tty", ""},
			{"@agent-tmux-generation", "1"},
			{"@agent-tmux-quit", "0"},
			{"@agent-tmux-created", time.Now().UTC().Format(time.RFC3339)},
			{"status-left", " agent-tmux:#S "},
			{"status-left-length", "40"},
			{"status-right", statusRightFormat},
			{"status-right-length", "60"},
		}
		for _, setting := range settings {
			if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), setting[0], setting[1]); err != nil {
				_, _ = m.run(context.Background(), nil, "kill-session", "-t", exact(name))
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return m.setupPS1(ctx, name)
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

func (m *Manager) RunCommand(ctx context.Context, name, command string, timeout time.Duration, history int) (RunResult, error) {
	if strings.TrimSpace(command) == "" {
		return RunResult{}, errors.New("command cannot be empty")
	}
	if timeout <= 0 {
		return RunResult{}, errors.New("timeout must be positive")
	}
	if history < 0 {
		return RunResult{}, errors.New("history must not be negative")
	}

	screenBefore, err := m.Screen(ctx, name, history)
	if err != nil {
		return RunResult{}, err
	}
	seqBefore := -1
	if m := rcPattern.FindStringSubmatch(screenBefore); m != nil {
		seqBefore, _ = strconv.Atoi(m[2])
	}

	if err := m.Send(ctx, name, command, true); err != nil {
		return RunResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var screen string
	for {
		screen, err = m.Screen(ctx, name, history)
		if err != nil {
			return RunResult{Screen: screen}, err
		}
		all := rcPattern.FindAllStringSubmatch(screen, -1)
		if len(all) > 0 {
			last := all[len(all)-1]
			seq, _ := strconv.Atoi(last[2])
			if seq > seqBefore {
				exitCode, parseErr := strconv.Atoi(last[1])
				if parseErr != nil {
					return RunResult{Screen: screen}, fmt.Errorf("parse command exit status %q: %w", last[1], parseErr)
				}
				return RunResult{Screen: screen, ExitCode: exitCode}, nil
			}
		}
		select {
		case <-ctx.Done():
			return RunResult{Screen: screen}, fmt.Errorf("command is still running or waiting for input: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// setupPS1 prepends [RC:$?:seq] to the shell's PS1 so RunCommand can detect
// command completion via the prompt without sentinel injection. _ATSEQ increments
// on every prompt draw, making completion detection reliable even when the exit
// code and working directory are unchanged between two consecutive commands.
// The keystrokes are buffered by the PTY and processed by the shell before any
// subsequent RunCommand arrives, so no confirmation wait is needed here.
func (m *Manager) setupPS1(ctx context.Context, name string) error {
	return m.Send(ctx, name, `_ATSEQ=0; export PS1='[RC:$?:$((_ATSEQ++))]'"${PS1}"`, true)
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
	return m.attach(ctx, name, true)
}

// Open attaches once and loops between a genuinely read-only observing phase
// and a writable controlling phase, toggled by the single Ctrl-T binding
// (bound to detach-client, which tmux honors even for read-only clients).
// onPhase is called with OwnerUnclaimed when the observing phase starts and
// with this process's own actor id when the controlling phase starts.
// Ctrl-Q ends the loop and returns control to the agent; it is only honored
// while the writable phase is active (see ensureToggleBinding), so quitting
// from the locked phase needs one Ctrl-T first. Closing the terminal or
// killing the session also ends the loop.
func (m *Manager) Open(ctx context.Context, name string, onPhase func(owner string)) error {
	if err := m.requireSession(ctx, name); err != nil {
		return err
	}
	if err := m.ensureToggleBinding(ctx); err != nil {
		return err
	}
	if err := m.release(ctx, name); err != nil {
		return err
	}
	if err := m.clearQuitFlag(ctx, name); err != nil {
		return err
	}

	id := actorID()
	tty := currentTTY()

	for {
		if exists, err := m.HasSession(ctx, name); err != nil {
			return err
		} else if !exists {
			return nil
		}
		if onPhase != nil {
			onPhase(OwnerUnclaimed)
		}
		if err := m.attach(ctx, name, true); err != nil {
			return err
		}
		if quit, err := m.quitRequested(ctx, name); err != nil {
			return err
		} else if quit {
			return m.clearQuitFlag(context.Background(), name)
		}

		if exists, err := m.HasSession(ctx, name); err != nil {
			return err
		} else if !exists {
			return nil
		}
		if err := m.claim(ctx, name, id, tty); err != nil {
			return err
		}
		if onPhase != nil {
			onPhase(id)
		}
		attachErr := m.attach(ctx, name, false)
		_ = m.release(context.Background(), name)
		if attachErr != nil {
			return attachErr
		}
		if quit, err := m.quitRequested(ctx, name); err != nil {
			return err
		} else if quit {
			return m.clearQuitFlag(context.Background(), name)
		}
	}
}

// quitRequested reports whether Ctrl-Q asked Open to end its loop.
func (m *Manager) quitRequested(ctx context.Context, name string) (bool, error) {
	output, err := m.run(ctx, nil, "show-option", "-qv", "-t", exact(name), "@agent-tmux-quit")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == "1", nil
}

func (m *Manager) clearQuitFlag(ctx context.Context, name string) error {
	_, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-quit", "0")
	return err
}

// ensureToggleBinding installs the two global keys attached clients use.
// Ctrl-T ends the current attach-session call regardless of read-only
// state, since detach-client is one of the two commands tmux still honors
// for a read-only client (Open supplies the actual lock/unlock semantics by
// re-attaching afterward). Ctrl-Q additionally marks the session's quit flag
// before detaching; tmux discards a bound command list wholesale unless it
// is purely detach-client or switch-client, so the quit mark is only
// reliably honored while the client is writable.
func (m *Manager) ensureToggleBinding(ctx context.Context) error {
	if _, err := m.run(ctx, nil, "bind-key", "-n", "C-t", "detach-client"); err != nil {
		return err
	}
	_, err := m.run(ctx, nil, "bind-key", "-n", "C-q",
		"set-option -t '#{session_name}' @agent-tmux-quit 1 ; detach-client")
	return err
}

func (m *Manager) Take(ctx context.Context, name string) error {
	id := actorID()
	tty := currentTTY()
	if err := m.claim(ctx, name, id, tty); err != nil {
		return err
	}
	attachErr := m.attach(ctx, name, false)
	releaseErr := m.release(context.Background(), name)
	if attachErr != nil {
		return attachErr
	}
	return releaseErr
}

func (m *Manager) Yield(ctx context.Context, name string) error {
	return m.release(ctx, name)
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
// the session's owner, so scripted Send/SendKeys/RunCommand calls are
// refused until release runs.
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
		generation := m.readGeneration(ctx, name)
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-owner", actorID); err != nil {
			return err
		}
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-owner-tty", tty); err != nil {
			return err
		}
		_, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-generation", strconv.Itoa(generation+1))
		return err
	})
}

// release clears the session's owner back to OwnerUnclaimed.
func (m *Manager) release(ctx context.Context, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	return m.withLock(name, func() error {
		if err := m.requireSession(ctx, name); err != nil {
			return err
		}
		generation := m.readGeneration(ctx, name)
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-owner", OwnerUnclaimed); err != nil {
			return err
		}
		if _, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-owner-tty", ""); err != nil {
			return err
		}
		_, err := m.run(ctx, nil, "set-option", "-t", exact(name), "@agent-tmux-generation", strconv.Itoa(generation+1))
		return err
	})
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
		rawOwner, err := m.run(ctx, nil, "show-option", "-qv", "-t", exact(name), "@agent-tmux-owner")
		if err != nil {
			return err
		}
		if owner := strings.TrimSpace(rawOwner); owner != OwnerUnclaimed {
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

func (m *Manager) attach(ctx context.Context, name string, readOnly bool) error {
	args := m.baseArgs()
	args = append(args, "attach-session")
	if readOnly {
		args = append(args, "-r")
	}
	args = append(args, "-t", exact(name))
	cmd := exec.CommandContext(ctx, m.tmux, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withoutEnv(os.Environ(), "TMUX")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("attach to %q: %w", name, err)
	}
	return nil
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
	return "#{session_name}\t#{@agent-tmux-owner}\t#{session_attached}\t#{pane_current_command}\t#{pane_current_path}"
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
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}
	lines := strings.Split(output, "\n")
	sessions := make([]Session, 0, len(lines))
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) != 5 {
			return nil, fmt.Errorf("cannot parse tmux session response %q", line)
		}
		clients, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("cannot parse client count %q: %w", fields[2], err)
		}
		sessions = append(sessions, Session{
			Name:    fields[0],
			Owner:   fields[1],
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
