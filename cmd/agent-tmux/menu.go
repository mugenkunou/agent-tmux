package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// escapeMenu is what Ctrl-] opens inside `open`: a one-line menu shown with
// tmux detached, so it works however stuck the program in the pane is. It
// returns true when `open` should end.
func escapeMenu(inv *invocation, name string) bool {
	for {
		fmt.Fprintf(inv.stdout, "\r\nagent-tmux %s: [d]etach  [k]ill session  [r]estart shell  [c] send Ctrl-C  [any other key] back ", name)
		key, err := readKey(os.Stdin)
		fmt.Fprint(inv.stdout, "\r\n")
		if err != nil {
			return false
		}
		switch key {
		case 'd', 'D', 'q', 'Q':
			fmt.Fprintf(inv.stdout, "detached; %s keeps running (agent-tmux open %s)\n", name, name)
			return true
		case 'k', 'K':
			if !confirm(inv, fmt.Sprintf("kill session %s and everything running in it?", name)) {
				continue
			}
			if err := inv.manager.Kill(inv.ctx, name); err != nil {
				fail(inv.stderr, err)
				continue
			}
			fmt.Fprintf(inv.stdout, "killed %s\n", name)
			return true
		case 'r', 'R':
			if !confirm(inv, fmt.Sprintf("kill what runs in %s and start a fresh shell?", name)) {
				continue
			}
			if err := inv.manager.Restart(inv.ctx, name); err != nil {
				fail(inv.stderr, err)
				continue
			}
			return false
		case 'c', 'C':
			if err := inv.manager.SendKeys(inv.ctx, name, []string{"C-c"}); err != nil {
				fail(inv.stderr, err)
				continue
			}
			return false
		default:
			return false
		}
	}
}

func confirm(inv *invocation, question string) bool {
	fmt.Fprintf(inv.stdout, "%s [y/N] ", question)
	key, err := readKey(os.Stdin)
	fmt.Fprint(inv.stdout, "\r\n")
	return err == nil && (key == 'y' || key == 'Y')
}

// readKey reads one keypress from a terminal without waiting for Enter or
// echoing it. Signal keys arrive as plain bytes (Ctrl-C is 3, which callers
// treat as "back"). Bytes left over from a multi-byte key, such as an arrow,
// are discarded so they do not leak into the session on re-attach.
func readKey(f *os.File) (byte, error) {
	fd := f.Fd()
	var saved syscall.Termios
	if err := termios(fd, syscall.TCGETS, &saved); err != nil {
		return 0, err
	}
	defer termios(fd, syscall.TCSETS, &saved)

	raw := saved
	raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := termios(fd, syscall.TCSETS, &raw); err != nil {
		return 0, err
	}
	buf := make([]byte, 1)
	if _, err := f.Read(buf); err != nil {
		return 0, err
	}

	raw.Cc[syscall.VMIN] = 0
	if termios(fd, syscall.TCSETS, &raw) == nil {
		rest := make([]byte, 64)
		for {
			if n, _ := f.Read(rest); n == 0 {
				break
			}
		}
	}
	return buf[0], nil
}

func termios(fd uintptr, request uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}
