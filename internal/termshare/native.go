package termshare

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// OpenNative is Open without `tmux attach`. tmux attach draws the pane on
// the terminal's alternate screen, which has no scrollback, and ignores the
// mouse for read-only clients, so a watcher can never scroll. OpenNative
// instead runs a tmux control-mode client (`tmux -C`), prints the pane's
// history and screen once, then streams the bytes the pane's program writes
// straight to this terminal, the way ssh does. The terminal keeps its own
// scrollback, so the wheel, selection and the terminal's search all work,
// and the transcript stays in the tab afterwards.
//
// tmux still runs the PTY and the terminal emulation the agent reads; only
// the viewer changes. Input is read here: while watching it is dropped,
// while controlling it is sent to the pane with send-keys -H. Ctrl-T,
// Ctrl-Q and Ctrl-] mean the same as in Open.
//
// The pane has one size. OpenNative sets it to this terminal's size, so
// other viewers with a different size see wrapped or clipped lines.
func (m *Manager) OpenNative(ctx context.Context, name string, hooks OpenHooks) error {
	if err := m.requireSession(ctx, name); err != nil {
		return err
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("open --native needs a terminal on stdin and stdout")
	}
	pane, err := m.run(ctx, nil, "display-message", "-p", "-t", target(name), "#{pane_id}")
	if err != nil {
		return err
	}

	defer preserveTerminalTitle(os.Stdout)()
	defer surviveHangup()()
	if hooks.Phase != nil {
		hooks.Phase(OwnerUnclaimed)
	}

	saved, err := getTermios(os.Stdin)
	if err != nil {
		return err
	}
	defer setTermios(os.Stdin, saved)
	if err := setTermios(os.Stdin, passthroughMode(saved)); err != nil {
		return err
	}

	c := &nativeClient{m: m, name: name, pane: strings.TrimSpace(pane), id: actorID(), tty: currentTTY()}
	if err := c.start(ctx); err != nil {
		return err
	}
	defer c.stop()
	defer func() {
		if c.controlling {
			_ = m.release(context.Background(), name)
		}
	}()
	return c.loop(ctx, hooks, saved)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type nativeClient struct {
	m          *Manager
	name, pane string
	id, tty    string

	cmd   *exec.Cmd
	ctlIn io.WriteCloser
	done  chan struct{} // closed when the control client's output ends

	mu      sync.Mutex // guards pending and writes to ctlIn
	pending []*pendingCommand

	out       sync.Mutex  // serialises writes to the terminal
	rendering atomic.Bool // whether %output is drawn (off until a snapshot is drawn)
	alt       atomic.Bool // whether the terminal is on its alternate screen
	altCarry  []byte      // tail of the last output chunk, for sequences split across chunks

	controlling bool
	shownOwner  string
	lastBell    time.Time
	input       inputFilter
}

// pendingCommand is one line sent to the control client: it produces one
// %begin/%end block per command in it, and fn gets their payloads.
type pendingCommand struct {
	want   int
	blocks [][][]byte
	fn     func(blocks [][][]byte)
}

func (c *nativeClient) start(ctx context.Context) error {
	args := append(c.m.baseArgs(), "-C", "attach-session", "-t", exact(c.name))
	c.cmd = exec.Command(c.m.tmux, args...)
	c.cmd.Env = withoutEnv(os.Environ(), "TMUX")
	in, err := c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("attach to %q: %w", c.name, err)
	}
	c.ctlIn = in
	c.done = make(chan struct{})
	go c.read(out)
	return c.snapshot(true)
}

// stop detaches the control client (closing its stdin ends it) and puts the
// terminal back the way a shell expects it.
func (c *nativeClient) stop() {
	c.rendering.Store(false)
	_ = c.ctlIn.Close()
	exited := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
	}
	c.out.Lock()
	c.resetModes()
	c.out.Unlock()
}

// send writes one control-mode line made of cmds and calls fn with their
// output once all of them have answered. fn runs on the reader goroutine.
func (c *nativeClient) send(fn func(blocks [][][]byte), cmds ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, &pendingCommand{want: len(cmds), fn: fn})
	_, err := io.WriteString(c.ctlIn, strings.Join(cmds, " ; ")+"\n")
	return err
}

// snapshotFormat reads, in one go, what the terminal needs besides the text
// to look like the pane: history length, cursor, screen and input modes.
const snapshotFormat = "#{history_size} #{cursor_x} #{cursor_y} #{pane_height} #{alternate_on} " +
	"#{cursor_flag} #{keypad_cursor_flag} #{keypad_flag} #{wrap_flag} #{insert_flag} " +
	"#{mouse_standard_flag} #{mouse_button_flag} #{mouse_any_flag} #{mouse_sgr_flag} " +
	"#{scroll_region_upper} #{scroll_region_lower}"

// snapshot sizes the pane to this terminal and redraws it here, optionally
// printing the history first so it lands in the terminal's scrollback.
// tmux answers control-mode commands in order with the pane's output, so
// output before the answer is already in the capture and output after it
// is drawn on top: nothing is lost or doubled.
func (c *nativeClient) snapshot(withHistory bool) error {
	cols, rows, err := windowSize(os.Stdout)
	if err != nil {
		return err
	}
	cmds := []string{
		fmt.Sprintf("refresh-client -C %d,%d", cols, rows),
		fmt.Sprintf("display-message -p -t %s '%s'", c.pane, snapshotFormat),
	}
	if withHistory {
		cmds = append(cmds, "capture-pane -p -e -J -S - -E -1 -t "+c.pane)
	}
	cmds = append(cmds, "capture-pane -p -e -N -t "+c.pane)
	return c.send(func(blocks [][][]byte) { c.paint(blocks, withHistory) }, cmds...)
}

func (c *nativeClient) paint(blocks [][][]byte, withHistory bool) {
	var state []int
	if len(blocks[1]) > 0 {
		for _, field := range strings.Fields(string(blocks[1][0])) {
			n, _ := strconv.Atoi(field)
			state = append(state, n)
		}
	}
	if len(state) < 16 {
		state = append(state, make([]int, 16-len(state))...)
	}
	histSize, cx, cy, height, alt := state[0], state[1], state[2], state[3], state[4] == 1
	screen := blocks[len(blocks)-1]

	var b bytes.Buffer
	if withHistory && histSize > 0 {
		for _, line := range blocks[2] {
			b.Write(line)
			b.WriteString("\x1b[0m\r\n")
		}
	}
	if alt {
		b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
		for i, row := range screen {
			fmt.Fprintf(&b, "\x1b[%d;1H", i+1)
			b.Write(row)
			b.WriteString("\x1b[0m\x1b[K")
		}
	} else {
		for i, row := range screen {
			if i > 0 {
				b.WriteString("\r\n")
			}
			b.Write(row)
			b.WriteString("\x1b[0m\x1b[K")
		}
	}
	if upper, lower := state[14], state[15]; upper != 0 || (lower != 0 && lower != height-1) {
		fmt.Fprintf(&b, "\x1b[%d;%dr", upper+1, lower+1)
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", cy+1, cx+1)
	for i, seq := range []string{"", "", "", "", "", "\x1b[?25l", "\x1b[?1h", "\x1b=", "\x1b[?7l", "\x1b[4h",
		"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1006h"} {
		on := state[i] == 1
		switch i {
		case 5, 8: // cursor shown, autowrap: these default on, so emit when off
			on = !on
		}
		if seq != "" && on {
			b.WriteString(seq)
		}
	}

	c.out.Lock()
	_, _ = os.Stdout.Write(b.Bytes())
	c.alt.Store(alt)
	c.altCarry = nil
	c.rendering.Store(true)
	c.out.Unlock()
}

// resetModes undoes whatever modes the pane's program left the terminal in.
// The caller holds c.out.
func (c *nativeClient) resetModes() {
	seq := "\x1b[0m\x1b7\x1b[r\x1b8\x1b[?1l\x1b>\x1b[?25h\x1b[?7h\x1b[4l" +
		"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l"
	if c.alt.Load() {
		seq += "\x1b[?1049l"
		c.alt.Store(false)
	}
	_, _ = os.Stdout.WriteString(seq + "\r\n")
}

// read parses the control client's output: command answers go to their
// pendingCommand, %output from our pane is drawn, %exit ends the session.
func (c *nativeClient) read(out io.Reader) {
	defer close(c.done)
	r := bufio.NewReaderSize(out, 1<<16)
	var block [][]byte
	var end []byte // the %end line that closes the open block, nil if none
	ours := false  // whether the open block answers one of our commands
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		if end != nil {
			if bytes.Equal(line, end) || bytes.HasPrefix(line, []byte("%error ")) && bytes.Equal(line[len("%error"):], end[len("%end"):]) {
				if ours {
					c.answer(block)
				}
				block, end = nil, nil
				continue
			}
			block = append(block, append([]byte(nil), line...))
			continue
		}
		switch {
		case bytes.HasPrefix(line, []byte("%begin ")):
			rest := line[len("%begin"):]
			end = append([]byte("%end"), rest...)
			ours = bytes.HasSuffix(rest, []byte(" 1"))
		case bytes.HasPrefix(line, []byte("%output ")):
			rest := line[len("%output "):]
			id, data, _ := bytes.Cut(rest, []byte(" "))
			if string(id) == c.pane && c.rendering.Load() {
				c.draw(unescapeOutput(data))
			}
		case bytes.HasPrefix(line, []byte("%exit")):
			return
		}
	}
}

func (c *nativeClient) answer(block [][]byte) {
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return
	}
	p := c.pending[0]
	p.blocks = append(p.blocks, block)
	if len(p.blocks) < p.want {
		c.mu.Unlock()
		return
	}
	c.pending = c.pending[1:]
	c.mu.Unlock()
	if p.fn != nil {
		p.fn(p.blocks)
	}
}

var (
	altOn  = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?1047h"), []byte("\x1b[?47h")}
	altOff = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?1047l"), []byte("\x1b[?47l")}
)

func (c *nativeClient) draw(data []byte) {
	c.out.Lock()
	defer c.out.Unlock()
	if !c.rendering.Load() {
		return
	}
	_, _ = os.Stdout.Write(data)
	// Track the alternate screen so leaving can switch back to the normal one.
	scan := append(c.altCarry, data...)
	last, lastOn := -1, false
	for _, seqs := range [][][]byte{altOn, altOff} {
		for _, seq := range seqs {
			if i := bytes.LastIndex(scan, seq); i > last {
				last, lastOn = i, seqs[0][len(seqs[0])-1] == 'h'
			}
		}
	}
	if last >= 0 {
		c.alt.Store(lastOn)
	}
	if len(scan) > 8 {
		scan = scan[len(scan)-8:]
	}
	c.altCarry = append([]byte(nil), scan...)
}

// unescapeOutput decodes a %output payload: tmux writes bytes below 32 and
// the backslash as a backslash and three octal digits.
func unescapeOutput(data []byte) []byte {
	if bytes.IndexByte(data, '\\') < 0 {
		return append([]byte(nil), data...)
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] == '\\' && i+3 < len(data) && isOctal(data[i+1]) && isOctal(data[i+2]) && isOctal(data[i+3]) {
			out = append(out, (data[i+1]-'0')<<6|(data[i+2]-'0')<<3|(data[i+3]-'0'))
			i += 3
			continue
		}
		out = append(out, data[i])
	}
	return out
}

func isOctal(b byte) bool { return b >= '0' && b <= '7' }

func (c *nativeClient) loop(ctx context.Context, hooks OpenHooks, saved syscall.Termios) error {
	input := make(chan []byte)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(input)
				return
			}
			input <- append([]byte(nil), buf[:n]...)
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	c.showOwner(OwnerUnclaimed)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return nil
		case <-winch:
			if cols, rows, err := windowSize(os.Stdout); err == nil {
				_ = c.send(nil, fmt.Sprintf("refresh-client -C %d,%d", cols, rows))
			}
		case <-ticker.C:
			c.syncOwner(ctx)
		case chunk, ok := <-input:
			if !ok {
				return nil
			}
			action, keys := c.input.filter(chunk)
			if len(keys) > 0 {
				c.forward(ctx, keys)
			}
			switch action {
			case exitToggle:
				if err := c.toggle(ctx); err != nil {
					return err
				}
			case exitQuit:
				return nil
			case exitMenu:
				leave, err := c.menu(ctx, hooks, saved, input)
				if err != nil || leave {
					return err
				}
			}
		}
	}
}

// forward sends typed bytes to the pane while controlling, and rings the
// bell (at most every two seconds) while watching, as a hint that typing
// does nothing until Ctrl-T. Before sending, it asks tmux whether this
// process still owns the session, so a `yield` takes effect on the very
// next keystroke rather than at the next ownership poll.
func (c *nativeClient) forward(ctx context.Context, keys []byte) {
	if c.controlling {
		owner, held, err := c.m.holdsControl(ctx, c.name)
		if err != nil || !held {
			c.controlling = false
			c.showOwner(owner)
		}
	}
	if !c.controlling {
		if time.Since(c.lastBell) > 2*time.Second {
			c.lastBell = time.Now()
			c.out.Lock()
			_, _ = os.Stdout.WriteString("\a")
			c.out.Unlock()
		}
		return
	}
	for len(keys) > 0 {
		n := min(len(keys), 200)
		var b strings.Builder
		b.WriteString("send-keys -t " + c.pane + " -H")
		for _, k := range keys[:n] {
			fmt.Fprintf(&b, " %02x", k)
		}
		_ = c.send(nil, b.String())
		keys = keys[n:]
	}
}

func (c *nativeClient) toggle(ctx context.Context) error {
	if c.controlling {
		c.controlling = false
		if err := c.m.release(ctx, c.name); err != nil {
			return err
		}
		c.showOwner(OwnerUnclaimed)
		return nil
	}
	if err := c.m.claim(ctx, c.name, c.id, c.tty); err != nil {
		if !errors.Is(err, ErrControlled) {
			return err
		}
		c.notify("Ctrl-T refused: " + err.Error())
		return nil
	}
	c.controlling = true
	c.showOwner(c.id)
	return nil
}

// notify tells the user something without disturbing the pane's picture
// more than necessary: a bell, the message in the title for a moment, and
// on the normal screen a line of its own (it becomes part of scrollback).
// On the alternate screen a full-screen program owns every cell, so only
// the bell and title are used there.
func (c *nativeClient) notify(message string) {
	c.out.Lock()
	_, _ = os.Stdout.WriteString("\a\x1b]0;agent-tmux: " + message + "\a")
	if !c.alt.Load() {
		_, _ = os.Stdout.WriteString("\r\n[agent-tmux] " + message + "\r\n")
	}
	c.out.Unlock()
}

// syncOwner notices ownership changed elsewhere (a `take`, a `yield`) and
// follows it: the title tracks the owner, and input stops if this client
// no longer holds control.
func (c *nativeClient) syncOwner(ctx context.Context) {
	session, err := c.m.Status(ctx, c.name)
	if err != nil {
		return
	}
	if c.controlling && session.Owner != c.id {
		c.controlling = false
	}
	if session.Owner != c.shownOwner {
		c.showOwner(session.Owner)
	}
}

func (c *nativeClient) showOwner(owner string) {
	c.shownOwner = owner
	title := "\U0001F440 agent-tmux: " + c.name + " (watching)"
	if owner != OwnerUnclaimed {
		title = "⌨️ agent-tmux: " + c.name + " (" + owner + " controls)"
	}
	c.out.Lock()
	_, _ = os.Stdout.WriteString("\x1b]0;" + title + "\a")
	c.out.Unlock()
}

// menu stops drawing, releases control, and hands the terminal to the
// menu with output processing back on. If the menu does not end open, the
// pane is redrawn (without the history, which is already in scrollback).
// Output the pane produced while the menu was up is not drawn; it is still
// in tmux's history.
func (c *nativeClient) menu(ctx context.Context, hooks OpenHooks, saved syscall.Termios, input <-chan []byte) (bool, error) {
	if hooks.Menu == nil {
		return false, nil
	}
	c.out.Lock()
	c.rendering.Store(false)
	c.resetModes()
	c.out.Unlock()
	wasControlling := c.controlling
	if c.controlling {
		c.controlling = false
		_ = c.m.release(ctx, c.name)
	}
	c.showOwner(OwnerUnclaimed)

	_ = setTermios(os.Stdin, keyMode(saved))
	leave := hooks.Menu(func() (byte, error) {
		chunk, ok := <-input
		if !ok || len(chunk) == 0 {
			return 0, io.EOF
		}
		return chunk[0], nil
	})
	_ = setTermios(os.Stdin, passthroughMode(saved))
	if leave {
		return true, nil
	}
	if exists, err := c.m.HasSession(ctx, c.name); err != nil || !exists {
		return true, err
	}
	if wasControlling {
		if err := c.m.claim(ctx, c.name, c.id, c.tty); err == nil {
			c.controlling = true
			c.showOwner(c.id)
		} else if !errors.Is(err, ErrControlled) {
			return true, err
		}
		// If someone else took control while the menu was up, come back
		// watching; syncOwner shows who has it.
	}
	c.input = inputFilter{}
	return false, c.snapshot(false)
}

// inputFilter splits typed bytes into Open's keys and bytes for the pane,
// and drops replies the terminal sends to queries in the pane's output.
// Those queries reach this terminal because the output is passed through
// as is, but tmux has already answered them for the pane, so forwarding
// the terminal's reply too would type garbage into the program.
type inputFilter struct {
	state int
	seq   []byte
}

const (
	inGround = iota
	inEscape
	inCSI
	inString       // OSC, DCS, APC, PM, SOS: dropped up to the terminator
	inStringEscape // ESC seen inside a string
)

// filter returns the first Open key in chunk (exitDetached if none) and
// the bytes before it that should go to the pane. Bytes after the key are
// dropped: they were typed in the same instant as a key that changes mode.
func (f *inputFilter) filter(chunk []byte) (action int, keys []byte) {
	for _, b := range chunk {
		switch f.state {
		case inGround:
			switch b {
			case 0x14:
				return exitToggle, keys
			case 0x11:
				return exitQuit, keys
			case 0x1d:
				return exitMenu, keys
			case 0x1b:
				f.state, f.seq = inEscape, []byte{b}
			default:
				keys = append(keys, b)
			}
		case inEscape:
			f.seq = append(f.seq, b)
			switch b {
			case '[':
				f.state = inCSI
			case ']', 'P', '_', '^', 'X':
				f.state, f.seq = inString, nil
			default:
				keys = append(keys, f.seq...)
				f.state, f.seq = inGround, nil
			}
		case inCSI:
			f.seq = append(f.seq, b)
			if b >= 0x40 && b <= 0x7e {
				if !isTerminalReply(f.seq) {
					keys = append(keys, f.seq...)
				}
				f.state, f.seq = inGround, nil
			} else if len(f.seq) > 64 {
				keys = append(keys, f.seq...)
				f.state, f.seq = inGround, nil
			}
		case inString:
			switch b {
			case 0x07:
				f.state = inGround
			case 0x1b:
				f.state = inStringEscape
			}
		case inStringEscape:
			if b == '\\' {
				f.state = inGround
			} else {
				f.state = inString
			}
		}
	}
	// A lone ESC at the end of a read is the Escape key, not the start of
	// a sequence: terminals send a whole sequence in one write.
	if f.state == inEscape {
		keys = append(keys, f.seq...)
		f.state, f.seq = inGround, nil
	}
	return exitDetached, keys
}

// isTerminalReply reports whether a complete CSI sequence is a terminal's
// reply to a query rather than a key: device attributes (CSI ? … c,
// CSI > … c), mode reports (CSI … $ y), status reports (CSI … n), window
// reports (CSI … t), keyboard-protocol reports (CSI ? … u) and cursor
// position reports (CSI row ; col R, except row 1, which is also how a
// modified F3 key is sent).
func isTerminalReply(seq []byte) bool {
	params, final := string(seq[2:len(seq)-1]), seq[len(seq)-1]
	private := params != "" && strings.ContainsRune("?>=", rune(params[0]))
	switch final {
	case 'c':
		return private
	case 'y':
		return strings.Contains(params, "$")
	case 'n', 't':
		return true
	case 'u':
		return private
	case 'R':
		row, _, found := strings.Cut(params, ";")
		return found && row != "1"
	}
	return false
}
