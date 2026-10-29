// Package transcript turns raw pane output (the bytes a terminal received,
// exactly as recorded) into readable lines, and searches those lines.
//
// Render has two modes: plain, which strips every escape sequence, and color,
// which keeps SGR (color and style) sequences and strips the rest. In both, a
// line is resolved the way a terminal would show it once the line is done:
// carriage return and backspace overwrite cells, erase-in-line (CSI K) clears
// cells, and cursor-left, cursor-right and column moves (CSI D, C, G) move
// within the line. "\r\n" is one newline. In color mode every line starts
// unstyled and ends with a reset, so each line can be printed on its own.
//
// Search finds lines by their plain text and numbers them exactly like
// Render, so a match's line number is the same line in a plain rendering.
//
// Known limits:
//   - A line is only what one terminal row would hold. Cursor moves across
//     rows (CSI A, B, H, J, scroll regions, the alternate screen) are ignored,
//     so full-screen programs such as vim, htop or less come out as redraw
//     noise: every repaint appears as more lines.
//   - Cells are runes. Wide (CJK) and combining characters take one cell each,
//     so overwrites on such lines can be off by a column.
//   - Tab is kept as a tab character, not expanded to a column.
//   - A line longer than MaxLine cells is split into several lines (that is,
//     it wraps, as it would on a terminal MaxLine columns wide).
//   - Invalid UTF-8 (including stray 8-bit C1 bytes) becomes U+FFFD, one per
//     bad byte; a sequence cut off at the end of input also becomes U+FFFD.
//   - Other control characters (BEL, NUL, ...) are dropped.
//   - A truncated escape sequence is dropped; garbage inside one ends it early.
package transcript

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxLine is the most cells one line may hold before it is flushed as a line
// of its own. It bounds memory whatever the input.
const MaxLine = 64 * 1024

const (
	maxStyle  = 256 // longest accumulated SGR text kept for one cell
	maxParams = 64  // longest CSI parameter text kept; the rest is ignored
	reset     = "\x1b[0m"
)

// Match is one line found by Search.
type Match struct {
	Line int    // 1-based line number, the same as in a plain rendering
	Text string // the line as plain text
}

// Render reads raw bytes from src and writes the rendered lines to dst, each
// ending in "\n". With color, SGR sequences are kept; otherwise the output is
// plain text. Memory use does not depend on the size of src.
func Render(dst io.Writer, src io.Reader, color bool) error {
	bw := bufio.NewWriter(dst)
	d := newDecoder(color, func(line string) bool {
		bw.WriteString(line)
		bw.WriteByte('\n')
		return true
	})
	if err := feed(d, src); err != nil {
		return err
	}
	d.finish()
	return bw.Flush()
}

// Search reads raw bytes from src and returns the last limit lines whose plain
// text contains needle, ignoring case, newest first. more reports whether
// further, older matches exist. It keeps only limit matches in memory. limit
// must be at least 1.
func Search(src io.Reader, needle string, limit int) (matches []Match, more bool, err error) {
	if limit < 1 {
		return nil, false, errLimit
	}
	needle = strings.ToLower(needle)
	ring := make([]Match, limit)
	total, n := 0, 0
	d := newDecoder(false, func(line string) bool {
		n++
		if strings.Contains(strings.ToLower(line), needle) {
			ring[total%limit] = Match{Line: n, Text: line}
			total++
		}
		return true
	})
	if err := feed(d, src); err != nil {
		return nil, false, err
	}
	d.finish()

	kept := total
	if kept > limit {
		kept = limit
		more = true
	}
	for i := 0; i < kept; i++ {
		matches = append(matches, ring[(total-1-i)%limit])
	}
	return matches, more, nil
}

type limitError string

func (e limitError) Error() string { return string(e) }

const errLimit = limitError("transcript: limit must be at least 1")

// feed copies src into the decoder in blocks.
func feed(d *decoder, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		d.write(buf[:n])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type state uint8

const (
	stGround    state = iota
	stEsc             // after ESC
	stEscInter        // ESC then intermediate bytes (charset designation): one final byte left
	stCSI             // inside CSI
	stString          // inside OSC, DCS, APC, PM or SOS: skipped to BEL or ST
	stStringEsc       // ESC seen inside a string: '\' ends it
)

// cell is one character on the line and the SGR text in effect when it was
// written (always "" in plain mode).
type cell struct {
	r     rune
	style string
}

// decoder is the byte-at-a-time state machine behind Render and Search. Its
// state survives between write calls, so input may be split anywhere.
type decoder struct {
	color bool
	emit  func(line string) bool

	st     state
	osc    bool // the current string is an OSC (BEL ends it too)
	params []byte
	inter  bool // CSI had an intermediate byte

	u  [4]byte // UTF-8 sequence being collected
	un int     // bytes held in u
	ul int     // bytes the sequence needs

	cells []cell
	col   int    // cursor column; may be beyond len(cells), padded on write
	style string // current SGR text (color mode)

	sb strings.Builder
}

func newDecoder(color bool, emit func(string) bool) *decoder {
	return &decoder{color: color, emit: emit}
}

func (d *decoder) write(p []byte) {
	for _, b := range p {
		d.step(b)
	}
}

// finish ends the input: a pending UTF-8 fragment becomes U+FFFD, and a last
// line without a newline is flushed if it has any content.
func (d *decoder) finish() {
	d.flushUTF8()
	d.st = stGround
	if len(d.cells) > 0 {
		d.flushLine()
	}
}

func (d *decoder) step(b byte) {
	switch d.st {
	case stString:
		switch {
		case b == 0x1b:
			d.st = stStringEsc
		case b == 0x07 && d.osc, b == 0x18, b == 0x1a:
			d.st = stGround
		}
		return
	case stStringEsc:
		if b == '\\' {
			d.st = stGround
			return
		}
		// ESC inside a string cancels it and starts a new sequence.
		d.st = stEsc
		d.escByte(b)
		return
	case stEsc:
		d.escByte(b)
		return
	case stEscInter:
		switch {
		case b == 0x1b:
			d.st = stEsc
		case b == 0x18, b == 0x1a:
			d.st = stGround
		case b >= 0x20 && b <= 0x2f:
			// more intermediates
		default:
			d.st = stGround // the final byte of a charset designation etc.
		}
		return
	case stCSI:
		d.csiByte(b)
		return
	}
	d.ground(b)
}

// escByte handles the byte after ESC.
func (d *decoder) escByte(b byte) {
	switch {
	case b == 0x1b:
		// ESC ESC: stay
	case b == 0x18 || b == 0x1a:
		d.st = stGround
	case b == '[':
		d.st = stCSI
		d.params = d.params[:0]
		d.inter = false
	case b == ']':
		d.st, d.osc = stString, true
	case b == 'P' || b == 'X' || b == '^' || b == '_':
		d.st, d.osc = stString, false
	case b >= 0x20 && b <= 0x2f:
		d.st = stEscInter
	case b < 0x20:
		d.control(b) // a control inside ESC runs, the ESC stays pending
	case b >= 0x80:
		d.st = stGround
		d.ground(b)
	default:
		d.st = stGround // two-byte sequence: ESC 7, ESC =, ESC c, ...
	}
}

// csiByte handles a byte inside CSI.
func (d *decoder) csiByte(b byte) {
	switch {
	case b == 0x1b:
		d.st = stEsc
	case b == 0x18 || b == 0x1a:
		d.st = stGround
	case b < 0x20:
		d.control(b)
	case b >= 0x20 && b <= 0x2f:
		d.inter = true
	case b >= 0x30 && b <= 0x3f:
		if len(d.params) < maxParams {
			d.params = append(d.params, b)
		}
	case b >= 0x40 && b <= 0x7e:
		d.st = stGround
		d.csi(b)
	case b == 0x7f:
		// ignored
	default:
		d.st = stGround // not valid in CSI: end it, treat as text
		d.ground(b)
	}
}

// csi runs a finished CSI sequence.
func (d *decoder) csi(final byte) {
	if d.inter {
		return
	}
	p := d.params
	if len(p) > 0 && p[0] >= '<' && p[0] <= '?' {
		return // private sequences: mode switches, bracketed paste, ...
	}
	switch final {
	case 'm':
		if d.color {
			d.sgr(string(p))
		}
	case 'K':
		d.eraseLine(num(p, 0, 0))
	case 'D':
		d.col -= num(p, 0, 1)
		if d.col < 0 {
			d.col = 0
		}
	case 'C':
		d.col += num(p, 0, 1)
		if d.col > MaxLine-1 {
			d.col = MaxLine - 1
		}
	case 'G':
		d.col = num(p, 0, 1) - 1
		if d.col > MaxLine-1 {
			d.col = MaxLine - 1
		}
	case 'P': // delete characters
		n := num(p, 0, 1)
		if d.col < len(d.cells) {
			end := d.col + n
			if end > len(d.cells) {
				end = len(d.cells)
			}
			d.cells = append(d.cells[:d.col], d.cells[end:]...)
		}
	case 'X': // erase characters
		n := num(p, 0, 1)
		for i := d.col; i < d.col+n && i < len(d.cells); i++ {
			d.cells[i] = cell{r: ' '}
		}
	}
}

// sgr records a style change. The current style is kept as the text of the
// SGR sequences since the last reset.
func (d *decoder) sgr(params string) {
	seq := "\x1b[" + params + "m"
	first := params
	if i := strings.IndexAny(params, ";:"); i >= 0 {
		first = params[:i]
	}
	switch {
	case first == "" || first == "0" || first == "00":
		if rest := strings.TrimLeft(params, "0"); rest == "" || rest[0] != ';' {
			d.style = "" // plain reset
			return
		}
		d.style = seq
	case len(d.style)+len(seq) > maxStyle:
		d.style = reset + seq
	default:
		d.style += seq
	}
}

// num reads parameter idx of p (separated by ';'), or def when it is missing
// or zero. The result is capped at MaxLine.
func num(p []byte, idx, def int) int {
	s := string(p)
	for i := 0; i < idx; i++ {
		j := strings.IndexByte(s, ';')
		if j < 0 {
			return def
		}
		s = s[j+1:]
	}
	if j := strings.IndexByte(s, ';'); j >= 0 {
		s = s[:j]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > MaxLine {
		n = MaxLine
	}
	return n
}

// eraseLine implements CSI K: 0 cursor to end, 1 start to cursor, 2 whole line.
func (d *decoder) eraseLine(mode int) {
	switch mode {
	case 0:
		if d.col < len(d.cells) {
			d.cells = d.cells[:d.col]
		}
	case 1:
		for i := 0; i <= d.col && i < len(d.cells); i++ {
			d.cells[i] = cell{r: ' '}
		}
	case 2:
		d.cells = d.cells[:0]
	}
}

// ground handles a byte outside any escape sequence.
func (d *decoder) ground(b byte) {
	if d.un > 0 {
		if b&0xc0 == 0x80 {
			d.u[d.un] = b
			d.un++
			if d.un == d.ul {
				r, _ := utf8.DecodeRune(d.u[:d.un])
				d.un = 0
				d.put(r)
			}
			return
		}
		d.flushUTF8() // cut short: the byte is processed below
	}
	switch {
	case b == 0x1b:
		d.st = stEsc
	case b < 0x20:
		d.control(b)
	case b < 0x7f:
		d.put(rune(b))
	case b == 0x7f:
		// DEL is dropped
	case b >= 0xc2 && b <= 0xdf:
		d.u[0], d.un, d.ul = b, 1, 2
	case b >= 0xe0 && b <= 0xef:
		d.u[0], d.un, d.ul = b, 1, 3
	case b >= 0xf0 && b <= 0xf4:
		d.u[0], d.un, d.ul = b, 1, 4
	default:
		d.put(utf8.RuneError)
	}
}

func (d *decoder) flushUTF8() {
	if d.un > 0 {
		d.un = 0
		d.put(utf8.RuneError)
	}
}

// control runs a C0 control character.
func (d *decoder) control(b byte) {
	switch b {
	case '\n':
		d.flushLine()
	case '\r':
		d.col = 0
	case '\b':
		if d.col > 0 {
			d.col--
		}
	case '\t':
		d.put('\t')
	}
}

// put writes r at the cursor, overwriting whatever is there.
func (d *decoder) put(r rune) {
	if r >= 0x80 && r < 0xa0 {
		return // C1 controls encoded in UTF-8 are dropped
	}
	if d.col >= MaxLine {
		d.flushLine()
	}
	for len(d.cells) < d.col {
		d.cells = append(d.cells, cell{r: ' '})
	}
	c := cell{r: r, style: d.style}
	if d.col < len(d.cells) {
		d.cells[d.col] = c
	} else {
		d.cells = append(d.cells, c)
	}
	d.col++
}

// flushLine hands the finished line to emit and starts a new one. The style
// in effect carries over, as on a terminal; each line still resets at its end.
func (d *decoder) flushLine() {
	d.sb.Reset()
	prev := ""
	for _, c := range d.cells {
		if c.style != prev {
			if prev != "" {
				d.sb.WriteString(reset)
			}
			d.sb.WriteString(c.style)
			prev = c.style
		}
		d.sb.WriteRune(c.r)
	}
	if prev != "" {
		d.sb.WriteString(reset)
	}
	d.cells = d.cells[:0]
	d.col = 0
	d.emit(d.sb.String())
}
