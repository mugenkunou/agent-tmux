package transcript

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"testing/iotest"
)

func render(t *testing.T, in string, color bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := Render(&out, strings.NewReader(in), color); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestRenderPlain(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"text", "hello\nworld\n", "hello\nworld\n"},
		{"no final newline", "a\nb", "a\nb\n"},
		{"blank lines", "a\n\nb\n", "a\n\nb\n"},
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"progress bar", "\r 10%\r 50%\r100%\n", "100%\n"},
		{"progress shorter", "downloading...\rdone\n", "doneloading...\n"},
		{"progress clear", "downloading...\r\x1b[Kdone\n", "done\n"},
		{"backspace", "abc\b\bX\n", "aXc\n"},
		{"backspace erase", "abc\b \bd\n", "abd\n"},
		{"backspace at start", "\b\bab\n", "ab\n"},
		{"colors", "\x1b[1;31mred\x1b[0m plain\n", "red plain\n"},
		{"256 color", "\x1b[38;5;196mx\x1b[m\n", "x\n"},
		{"osc bel", "\x1b]0;my title\x07shell$ \n", "shell$ \n"},
		{"osc st", "\x1b]2;my title\x1b\\shell$ \n", "shell$ \n"},
		{"osc hyperlink", "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\\n", "link\n"},
		{"bracketed paste", "\x1b[?2004hls\x1b[?2004l\n", "ls\n"},
		{"csi intermediate", "a\x1b[0 qb\x1b[!pc\n", "abc\n"},
		{"csi params", "a\x1b[1;2;3Hb\x1b[2Jc\n", "abc\n"},
		{"charset", "\x1b(Bab\x1b)0c\x1b%Gd\n", "abcd\n"},
		{"two byte", "\x1b7a\x1b8b\x1b=c\x1b>d\n", "abcd\n"},
		{"dcs", "a\x1bP1$r0m\x1b\\b\n", "ab\n"},
		{"apc pm sos", "a\x1b_x\x1b\\b\x1b^y\x1b\\c\x1bXz\x1b\\d\n", "abcd\n"},
		{"erase to end", "hello\b\b\b\x1b[Kp\n", "hep\n"},
		{"erase to start", "hello\b\b\x1b[1Kz\n", "   zo\n"},
		{"erase line", "hello\x1b[2Kx\n", "     x\n"},
		{"cursor left", "abcd\x1b[2DXY\n", "abXY\n"},
		{"cursor right", "ab\x1b[3Cc\n", "ab   c\n"},
		{"column", "abcdef\x1b[2Gz\n", "azcdef\n"},
		{"delete chars", "abcdef\x1b[3G\x1b[2P\n", "abef\n"},
		{"erase chars", "abcdef\x1b[3G\x1b[2X\n", "ab  ef\n"},
		{"utf8", "héllo wörld ✓ 日本\n", "héllo wörld ✓ 日本\n"},
		{"utf8 overwrite", "日本語\r本\n", "本本語\n"},
		{"invalid utf8", "a\xffb\xc3(c\xe2\x82\n", "a�b�(c�\n"},
		{"bell and nul", "a\x07b\x00c\n", "abc\n"},
		{"truncated csi", "ab\x1b[12", "ab\n"},
		{"truncated osc", "ab\x1b]0;title", "ab\n"},
		{"esc in csi", "a\x1b[1\x1b[mb\n", "ab\n"},
		{"cancel", "a\x1b[1\x18b\n", "ab\n"},
		{"newline in csi", "a\x1b[1\nz\n", "a\n\n"},
		{"garbage in csi", "a\x1b[1\xffb\n", "a�b\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := render(t, c.in, false); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestRenderColor(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain text", "abc\n", "abc\n"},
		{"keeps sgr", "\x1b[31mred\x1b[0m ok\n", "\x1b[31mred\x1b[0m ok\n"},
		{"reset at end", "\x1b[1;32mgreen\n", "\x1b[1;32mgreen\x1b[0m\n"},
		{"short reset", "\x1b[31mr\x1b[mx\n", "\x1b[31mr\x1b[0mx\n"},
		{"stacked", "\x1b[1m\x1b[31mb\n", "\x1b[1m\x1b[31mb\x1b[0m\n"},
		{"strips others", "\x1b]0;t\x07\x1b[?2004h\x1b[31mx\x1b[K\n", "\x1b[31mx\x1b[0m\n"},
		{"overwrite takes new style", "\x1b[31mabc\r\x1b[32mX\n", "\x1b[31m\x1b[32mX\x1b[0m\x1b[31mbc\x1b[0m\n"},
		{"overwrite plain over color", "\x1b[31mabc\r\x1b[0mX\n", "X\x1b[31mbc\x1b[0m\n"},
		{"erase keeps rest", "\x1b[31mabc\x1b[0m\r\x1b[2Kx\n", "x\n"},
		{"style carries to next line", "\x1b[34ma\nb\n", "\x1b[34ma\x1b[0m\n\x1b[34mb\x1b[0m\n"},
		{"reset then style", "\x1b[31ma\x1b[0;1mb\n", "\x1b[31ma\x1b[0m\x1b[0;1mb\x1b[0m\n"},
		{"truecolor", "\x1b[38;2;1;2;3mx\n", "\x1b[38;2;1;2;3mx\x1b[0m\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := render(t, c.in, true); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStyleBounded(t *testing.T) {
	in := strings.Repeat("\x1b[1m", 10000) + "x\n"
	got := render(t, in, true)
	if len(got) > 2*maxStyle+32 || !strings.Contains(got, "x") {
		t.Errorf("style grew without bound: %d bytes", len(got))
	}
}

func TestLongLineWraps(t *testing.T) {
	got := render(t, strings.Repeat("a", MaxLine+5)+"\n", false)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 || len(lines[0]) != MaxLine || lines[1] != "aaaaa" {
		t.Errorf("got %d lines (%d, %d)", len(lines), len(lines[0]), len(lines[len(lines)-1]))
	}
}

func TestHugeCursorMoveBounded(t *testing.T) {
	got := render(t, "a\x1b[99999999999999C\x1b[99999999999999Cb\n", false)
	if len(got) > MaxLine+2 {
		t.Errorf("output %d bytes", len(got))
	}
}

// Input split at any point must render the same as the whole input.
func TestSplitInput(t *testing.T) {
	in := "\x1b[1;31mred\x1b[0m\r\n\x1b]0;t\x07 10%\r100%\r\n日本\x1b[2D本\x1b[?2004h\x1b]2;x\x1b\\" +
		"a\x1b(Bb\x1bP1$r\x1b\\c\xff\xc3\nlast"
	for _, color := range []bool{false, true} {
		whole := render(t, in, color)

		var one bytes.Buffer
		if err := Render(&one, iotest.OneByteReader(strings.NewReader(in)), color); err != nil {
			t.Fatal(err)
		}
		if one.String() != whole {
			t.Errorf("color=%v: byte-at-a-time %q != whole %q", color, one.String(), whole)
		}
		for i := 0; i <= len(in); i++ {
			d := newDecoder(color, func(string) bool { return true })
			var sb strings.Builder
			d.emit = func(l string) bool { sb.WriteString(l + "\n"); return true }
			d.write([]byte(in[:i]))
			d.write([]byte(in[i:]))
			d.finish()
			if sb.String() != whole {
				t.Fatalf("color=%v split at %d: %q != %q", color, i, sb.String(), whole)
			}
		}
	}
}

func TestSearch(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		if i%2 == 0 {
			fmt.Fprintf(&sb, "\x1b[32mLine %d ERROR\x1b[0m\r\n", i)
		} else {
			fmt.Fprintf(&sb, "line %d ok\r\n", i)
		}
	}
	in := sb.String()

	got, more, err := Search(strings.NewReader(in), "error", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Match{{10, "Line 10 ERROR"}, {8, "Line 8 ERROR"}, {6, "Line 6 ERROR"}}
	if fmt.Sprint(got) != fmt.Sprint(want) || !more {
		t.Errorf("got %v more=%v, want %v more=true", got, more, want)
	}

	got, more, _ = Search(strings.NewReader(in), "ERROR", 5)
	if len(got) != 5 || more || got[4].Line != 2 {
		t.Errorf("exact fit: %v more=%v", got, more)
	}

	got, more, _ = Search(strings.NewReader(in), "ERROR", 100)
	if len(got) != 5 || more {
		t.Errorf("big limit: %v more=%v", got, more)
	}

	got, more, _ = Search(strings.NewReader(in), "nothing", 5)
	if len(got) != 0 || more {
		t.Errorf("no match: %v more=%v", got, more)
	}

	got, _, _ = Search(strings.NewReader("a\rb ok\n"), "A", 5)
	if len(got) != 0 {
		t.Errorf("overwritten text must not match: %v", got)
	}

	if _, _, err := Search(strings.NewReader(in), "x", 0); err == nil {
		t.Error("limit 0 should fail")
	}
}

// A match's line number must be the same line in the plain rendering.
func TestSearchLineNumbersMatchRender(t *testing.T) {
	in := "one\r\ntwo\n\x1b]0;t\x07\n\r 1%\r100% needle\n" + strings.Repeat("x", MaxLine) + "needle\nend"
	lines := strings.Split(strings.TrimSuffix(render(t, in, false), "\n"), "\n")
	got, _, err := Search(strings.NewReader(in), "needle", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, m := range got {
		if lines[m.Line-1] != m.Text {
			t.Errorf("line %d: render %q, search %q", m.Line, lines[m.Line-1], m.Text)
		}
	}
	// color mode numbers lines the same way
	if n := strings.Count(render(t, in, true), "\n"); n != len(lines) {
		t.Errorf("color has %d lines, plain %d", n, len(lines))
	}
}

func TestSearchSplitReads(t *testing.T) {
	in := "a \xe6\x97\xa5 b\r\nfind me\n"
	got, _, err := Search(iotest.OneByteReader(strings.NewReader(in)), "FIND", 1)
	if err != nil || len(got) != 1 || got[0] != (Match{2, "find me"}) {
		t.Errorf("got %v, %v", got, err)
	}
}

func FuzzRender(f *testing.F) {
	for _, s := range []string{
		"", "plain\n", "\x1b[31mx\x1b[0m", "\x1b]0;t\x07", "\x1b]0;t\x1b\\", "\r 10%\r100%\n",
		"\x1b[", "\x1b", "\xff\xc3\xe2\x82", "a\b\b\bb", "\x1b[999999999999C", "\x1bP\x1b", "\x1b(",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var plain, color bytes.Buffer
		if err := Render(&plain, bytes.NewReader(in), false); err != nil {
			t.Fatal(err)
		}
		if err := Render(&color, bytes.NewReader(in), true); err != nil {
			t.Fatal(err)
		}
		if bytes.IndexByte(plain.Bytes(), 0x1b) >= 0 {
			t.Fatalf("ESC in plain output %q for %q", plain.Bytes(), in)
		}
		if bytes.Count(plain.Bytes(), []byte("\n")) != bytes.Count(color.Bytes(), []byte("\n")) {
			t.Fatalf("line count differs for %q", in)
		}
		if _, _, err := Search(bytes.NewReader(in), "a", 3); err != nil {
			t.Fatal(err)
		}
	})
}
