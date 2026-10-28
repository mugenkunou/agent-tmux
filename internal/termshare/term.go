package termshare

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// Terminal mode helpers, standard library only (Linux termios ioctls).

func getTermios(f *os.File) (syscall.Termios, error) {
	var t syscall.Termios
	err := termiosIoctl(f.Fd(), syscall.TCGETS, &t)
	return t, err
}

func setTermios(f *os.File, t syscall.Termios) error {
	return termiosIoctl(f.Fd(), syscall.TCSETS, &t)
}

func termiosIoctl(fd uintptr, request uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}

// keyMode is raw input (every byte arrives at once, unechoed, with signal
// keys delivered as plain bytes) but leaves output processing alone, so
// "\n" still starts a new line. It suits prompts and menus.
func keyMode(t syscall.Termios) syscall.Termios {
	t.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR | syscall.IXON | syscall.ISTRIP
	t.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG | syscall.IEXTEN
	t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 1, 0
	return t
}

// passthroughMode is keyMode with output processing off too, so bytes from
// the pane reach the terminal exactly as the program wrote them (a bare
// "\n" there means "cursor down", not "new line").
func passthroughMode(t syscall.Termios) syscall.Termios {
	t = keyMode(t)
	t.Oflag &^= syscall.OPOST
	return t
}

// ReadKey reads one keypress from a terminal without waiting for Enter or
// echoing it. Signal keys arrive as plain bytes (Ctrl-C is 3). Bytes left
// over from a multi-byte key, such as an arrow, are discarded so they do not
// leak into whatever reads the terminal next.
func ReadKey(f *os.File) (byte, error) {
	saved, err := getTermios(f)
	if err != nil {
		return 0, err
	}
	defer setTermios(f, saved)
	raw := keyMode(saved)
	if err := setTermios(f, raw); err != nil {
		return 0, err
	}
	buf := make([]byte, 1)
	if _, err := f.Read(buf); err != nil {
		return 0, err
	}
	raw.Cc[syscall.VMIN] = 0
	if setTermios(f, raw) == nil {
		rest := make([]byte, 64)
		for {
			if n, _ := f.Read(rest); n == 0 {
				break
			}
		}
	}
	return buf[0], nil
}

// windowSize returns a terminal's size in columns and rows.
func windowSize(f *os.File) (cols, rows int, err error) {
	var size struct{ rows, cols, x, y uint16 }
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size))); errno != 0 {
		return 0, 0, errno
	}
	if size.cols == 0 || size.rows == 0 {
		return 0, 0, errors.New("terminal reports no size")
	}
	return int(size.cols), int(size.rows), nil
}
