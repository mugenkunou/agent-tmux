package termshare

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The send policy keeps scripted input readable in the remote host's audit
// log. Remote hosts log every process they start, and a security team reads
// that log with no context: `echo <blob> | base64 -d | bash` shows up there as
// a decoder feeding an unknown script into a shell, which is exactly what a
// malware dropper looks like, however harmless the script was (`echo` is a
// builtin, so the blob never reaches the log at all). The policy refuses
// input whose meaning a reader of that log cannot see, and warns about input
// that reads as hostile.
//
// It applies to scripted input only (send and key). A human who has claimed
// the terminal types straight into the pane and is never checked, which is
// the only way past it: there is deliberately no override flag, because an
// agent that is refused learns to pass whatever flag the refusal mentions.

// ErrRefused marks input the send policy refused; nothing was typed.
var ErrRefused = errors.New("refused by the send policy")

// Level says what the policy does with a finding.
type Level string

const (
	// LevelRefuse stops the input; nothing reaches the pane.
	LevelRefuse Level = "refuse"
	// LevelWarn lets the input through and reports the finding.
	LevelWarn Level = "warn"
)

// Finding is one rule that matched the input.
type Finding struct {
	Rule  string // stable id, e.g. "pipe-to-shell"
	Level Level
	What  string // what the input does, in a few words
	Fix   string // what to send instead
}

// RefusedError lists the refusing findings. errors.Is(err, ErrRefused) holds.
type RefusedError struct {
	Command  string // "send" or "key"
	Findings []Finding
	// Pending is set when earlier scripted input is still on the line.
	Pending bool
}

func (e *RefusedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s refused: nothing was typed into the session", e.Command)
	for _, f := range e.Findings {
		fmt.Fprintf(&b, "\n  - %s (%s). %s", f.What, f.Rule, f.Fix)
	}
	if e.Pending {
		b.WriteString("\n  Text typed by an earlier send --no-enter or key is still on the input line; clear it with: agent-tmux key <session> C-c")
	}
	b.WriteString("\n  Remote hosts log every process, and the security team reads that log with no context." +
		"\n  Send each command as plain text, one per send (use --stdin when quoting is hard)." +
		"\n  Do not work around this check; if a command truly needs it, ask the human to take control and type it.")
	return b.String()
}

func (e *RefusedError) Unwrap() error { return ErrRefused }

// Rules returns the ids of the findings, for the event log.
func (e *RefusedError) Rules() []string {
	ids := make([]string, len(e.Findings))
	for i, f := range e.Findings {
		ids[i] = f.Rule
	}
	return ids
}

// Path prefixes treated as scratch space: world-writable, and where dropped
// scripts usually land.
const tmpDir = `/(?:tmp|var/tmp|dev/shm)/`

// wrappers are commands that run the command after them (sudo -u app sh),
// with their flags, flag values and VAR=value assignments.
const wrappers = `(?:(?:sudo|xargs|nohup|time|exec|env|doas)(?:\s+-\S+(?:\s+[^-\s]\S*)?)*(?:\s+\S+=\S*)*\s+)*`

// cmdStart matches where a command word can begin: the start of the text, a
// shell separator, `$(`, or the `--` before a command handed to another tool
// (`kubectl exec pod -- sh ...`), then any wrappers. So `eval` and `sh`
// match as commands, not as arguments (`grep eval`).
const cmdStart = `(?:^|[;&|(` + "`" + `]|\$\(|\s--\s)\s*` + wrappers

// shellName matches the common POSIX shells and nothing longer (`sha256sum`
// and `shellcheck` are not shells).
const shellName = `(?:/usr)?(?:/bin/)?(?:ba|z|da|k|c|tc|fi|a)?sh`

// sep is what may follow a command word.
const sep = `(?:\s|$|[;&|)])`

const interpreter = `(?:/usr)?(?:/bin/)?(?:python[0-9.]*|perl|ruby|node|php|lua)`

type rule struct {
	id    string
	level Level
	what  string
	fix   string
	match func(text string) bool
}

func anyRegexp(patterns ...string) func(string) bool {
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		res[i] = regexp.MustCompile(p)
	}
	return func(text string) bool {
		for _, re := range res {
			if re.MatchString(text) {
				return true
			}
		}
		return false
	}
}

var rules = []rule{
	{
		id: "multi-line", level: LevelRefuse,
		what: "more than one line in one send",
		fix:  "Send one command per send; Enter is pressed for you.",
		match: func(text string) bool {
			return strings.ContainsAny(text, "\r\n")
		},
	},
	{
		id: "pipe-to-shell", level: LevelRefuse,
		what: "pipes text into a shell or interpreter",
		fix:  "Run the commands themselves; nothing should feed text into bash, sh or python.",
		match: anyRegexp(
			// ... | bash, ... | sudo -E sh -s, ... | env X=1 bash
			`\|&?\s*`+wrappers+shellName+sep,
			// ... | xargs sh -c
			`\|\s*xargs\b[^|;&]*\s`+shellName+`\s+-c\b`,
			// ... | python3, ... | perl -w -  (code read from stdin; `| python3 -m json.tool` reads data)
			`\|&?\s*`+wrappers+interpreter+`(?:\s+-[A-Za-z]+)*(?:\s+-)?\s*(?:$|[;&|)])`,
		),
	},
	{
		id: "eval", level: LevelRefuse,
		what: "runs generated code (eval, source <(...), sh <<<, sh -c \"$(...)\")",
		fix:  "Write the command out in full.",
		match: anyRegexp(
			cmdStart+`eval`+sep,
			cmdStart+`(?:source|\.)\s+<\(`,
			cmdStart+shellName+`(?:\s+-\S+)*\s*(?:<\(|<<<|<\s*`+tmpDir+`)`,
			cmdStart+shellName+`(?:\s+-\S+)*\s+-c\s+["']?\$\(`,
		),
	},
	{
		id: "encoded-blob", level: LevelRefuse,
		what:  "contains an encoded blob (a run of 80+ base64 or hex characters)",
		fix:   "Send the command as readable text; use --stdin when quoting is hard.",
		match: hasEncodedBlob,
	},
	{
		id: "decode-call", level: LevelRefuse,
		what:  "runs decoded data inside an interpreter",
		fix:   "Send the command itself, not code that decodes and runs it.",
		match: decodeAndRun,
	},
	{
		id: "tmp-exec", level: LevelRefuse,
		what: "runs, or makes executable, a file in a temp directory",
		fix:  "Don't leave scripts on remote hosts; send the commands one at a time.",
		match: anyRegexp(
			cmdStart+shellName+`(?:\s+-\S+)*\s+`+tmpDir,
			cmdStart+interpreter+`(?:\s+-\S+)*\s+`+tmpDir,
			cmdStart+`chmod\s+(?:\S+\s+)*?`+tmpDir,
			cmdStart+tmpDir+`\S`,
		),
	},
	{
		id: "tmp-output", level: LevelWarn,
		what: "hides output in a temp file",
		fix:  "Let output go to the screen and read it with agent-tmux screen.",
		// tee is left alone: it still shows the output.
		match: anyRegexp(`>{1,2}\s*` + tmpDir),
	},
	{
		id: "loaded-words", level: LevelWarn,
		what: "uses attacker vocabulary (recon, payload, exploit, ...)",
		fix:  "Use neutral names for markers and files: checks, survey, status.",
		match: anyRegexp(
			`(?i)\b(?:recon|exploit|payload|pwn(?:ed)?|backdoor|exfil(?:trat\w*)?|implant|dropper|privesc|reverse[ _-]?shell|rev[ _-]?shell)\b`,
		),
	},
}

var (
	decodeCall = regexp.MustCompile(`b64decode|base64\.decode|decode_base64|decodebase64|fromhex|unhexlify|atob\(|FromBase64String|['"]base64['"]\s*\)|pack\(\s*['"]H\*`)
	runCall    = regexp.MustCompile(`\b(?:exec|eval|system|popen|spawn\w*|execSync|Function|subprocess|compile)\b`)
)

// decodeAndRun reports interpreter code that decodes data and runs code:
// `exec(b64decode(...))`. Decoding a field to print it (reading a k8s
// secret) is left alone.
func decodeAndRun(text string) bool {
	return decodeCall.MatchString(text) && runCall.MatchString(text)
}

var blobRun = regexp.MustCompile(`[A-Za-z0-9+/]{80,}`)

// hasEncodedBlob reports a long run of base64 (hex is a subset). A long path
// is a run of the same characters, so runs with a slash every 20 characters
// or more often are taken as paths: base64 has about one in 64.
func hasEncodedBlob(text string) bool {
	for _, run := range blobRun.FindAllString(text, -1) {
		if strings.Count(run, "/")*20 <= len(run) {
			return true
		}
	}
	return false
}

// CheckCommand returns every rule the text matches, refusals first.
func CheckCommand(text string) []Finding {
	var refuse, warn []Finding
	for _, r := range rules {
		if !r.match(text) {
			continue
		}
		f := Finding{Rule: r.id, Level: r.level, What: r.what, Fix: r.fix}
		if r.level == LevelRefuse {
			refuse = append(refuse, f)
		} else {
			warn = append(warn, f)
		}
	}
	return append(refuse, warn...)
}

// refusal returns a RefusedError for the refusing findings, or nil.
func refusal(command, text string) error {
	return refusalAfter(command, "", text)
}

// refusalAfter checks pending+text, the whole line once text is typed.
func refusalAfter(command, pending, text string) error {
	line := pending + text
	var refused []Finding
	for _, f := range CheckCommand(line) {
		if f.Level == LevelRefuse {
			refused = append(refused, f)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return &RefusedError{Command: command, Findings: refused, Pending: pending != ""}
}

// Key names tmux understands. Any other send-keys argument is typed as
// literal text, so `key` could otherwise type a whole command past the
// policy.
var (
	namedKey    = regexp.MustCompile(`(?i)^(?:[CMS]-)*(?:Enter|Escape|Tab|BTab|Space|BSpace|Up|Down|Left|Right|Home|End|IC|DC|Insert|Delete|NPage|PPage|PageUp|PageDown|PgUp|PgDn|F[1-9][0-9]?|KP[A-Za-z0-9*+/.-]*)$`)
	modifiedKey = regexp.MustCompile(`^(?:[CMS]-)+.$`)
)

// keyEffect says what one send-keys argument does to the line being typed:
// the literal text it adds, whether it submits the line, and whether it
// throws the line away.
func keyEffect(key string) (text string, submit, clear bool) {
	switch strings.ToLower(key) {
	case "enter", "c-m", "c-j", "kpenter":
		return "", true, false
	case "c-c", "c-u":
		return "", false, true
	case "space":
		return " ", false, false
	}
	if namedKey.MatchString(key) || modifiedKey.MatchString(key) {
		return "", false, false
	}
	return key, false, false
}

// typedLine replays keys onto the pending line and returns the line left
// afterwards, refusing if any line they submit, or the line left typed,
// breaks the policy.
func typedLine(pending string, keys []string) (string, error) {
	line := pending
	for _, key := range keys {
		text, submit, clear := keyEffect(key)
		line += text
		switch {
		case submit:
			if err := refusalAfter("key", pending, line[len(pending):]); err != nil {
				return pending, err
			}
			line, pending = "", ""
		case clear:
			line, pending = "", ""
		}
	}
	if err := refusalAfter("key", pending, line[len(pending):]); err != nil {
		return pending, err
	}
	return line, nil
}
