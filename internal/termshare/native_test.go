package termshare

import (
	"bytes"
	"testing"
)

func TestUnescapeOutput(t *testing.T) {
	in := []byte(`a\134b\015\012\033[1mX\033[0m`)
	want := []byte("a\\b\r\n\x1b[1mX\x1b[0m")
	if got := unescapeOutput(in); !bytes.Equal(got, want) {
		t.Errorf("unescapeOutput = %q, want %q", got, want)
	}
	if got := unescapeOutput([]byte(`\13`)); !bytes.Equal(got, []byte(`\13`)) {
		t.Errorf("a truncated escape must pass through, got %q", got)
	}
}

func TestInputFilter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		action int
		keys   string
	}{
		{"plain text", []string{"ls\r"}, exitDetached, "ls\r"},
		{"arrow key", []string{"\x1b[A"}, exitDetached, "\x1b[A"},
		{"alt-x", []string{"\x1bx"}, exitDetached, "\x1bx"},
		{"lone escape", []string{"\x1b"}, exitDetached, "\x1b"},
		{"toggle mid-typing", []string{"ab\x14cd"}, exitToggle, "ab"},
		{"quit", []string{"\x11"}, exitQuit, ""},
		{"menu", []string{"\x1d"}, exitMenu, ""},
		{"ctrl-c passes", []string{"\x03"}, exitDetached, "\x03"},
		{"device attributes reply", []string{"\x1b[?62;22c"}, exitDetached, ""},
		{"cursor position reply", []string{"\x1b[12;40R"}, exitDetached, ""},
		{"modified F3 is a key", []string{"\x1b[1;5R"}, exitDetached, "\x1b[1;5R"},
		{"colour query reply", []string{"x\x1b]11;rgb:0000/0000/0000\x1b\\y"}, exitDetached, "xy"},
		{"reply split across reads", []string{"\x1b[?6", "2c"}, exitDetached, ""},
		{"focus event passes", []string{"\x1b[I"}, exitDetached, "\x1b[I"},
	} {
		var f inputFilter
		action, keys := exitDetached, []byte{}
		for _, chunk := range tc.chunks {
			a, k := f.filter([]byte(chunk))
			keys = append(keys, k...)
			if a != exitDetached {
				action = a
			}
		}
		if action != tc.action || string(keys) != tc.keys {
			t.Errorf("%s: got action %d keys %q, want %d %q", tc.name, action, keys, tc.action, tc.keys)
		}
	}
}
