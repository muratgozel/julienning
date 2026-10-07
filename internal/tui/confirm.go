package tui

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ConfirmOptions configures Confirm.
type ConfirmOptions struct {
	// Question is the first line, e.g. `Move "Fix flaky clock tests" from
	// beta to alpha?`.
	Question string
	// Detail explains what continuing does; it is wrapped to the terminal
	// width and indented under the question.
	Detail string
	// In and Out carry the [Y/n] line fallback (normally stdin and stdout);
	// the raw-mode prompt talks to the terminal directly.
	In  io.Reader
	Out io.Writer
	// TTY is the terminal device; empty means /dev/tty.
	TTY string
	// Getenv reads TERM and NO_COLOR; nil means os.Getenv.
	Getenv func(string) string
}

// confirmHint is the raw-mode prompt's key line.
const confirmHint = "Enter to continue · n or Esc to go back"

// Confirm asks a yes/no question with single keys on the terminal: Enter, y
// or Y continue (true); n, N, Esc, Ctrl-C or Ctrl-D go back (false). Other
// keys are ignored. Without a usable terminal it falls back to a [Y/n] line
// prompt on In/Out (ConfirmFallback). Like Pick it draws on the alternate
// screen, so nothing is left behind either way, and always restores the
// terminal (see openRaw).
func Confirm(opts ConfirmOptions) (bool, error) {
	getenv := envOr(opts.Getenv)
	t, err := openRaw(opts.TTY, getenv)
	if errors.Is(err, errNoRawTerminal) {
		return ConfirmFallback(opts)
	}
	if err != nil {
		return false, err
	}
	defer t.close()
	return confirmLoop(newConfirmModel(opts, getenv), t.tty, t.tty, t.size)
}

// newConfirmModel is the raw-mode prompt's model; NO_COLOR set to any
// non-empty value turns every SGR sequence off (see newModel).
func newConfirmModel(opts ConfirmOptions, getenv func(string) string) ConfirmModel {
	m := NewConfirm(opts.Question, opts.Detail)
	m.NoColor = getenv("NO_COLOR") != ""
	return m
}

// ConfirmModel is the confirm prompt's state: a question, its explanation,
// and the answer once one of the answer keys was pressed.
type ConfirmModel struct {
	Question string
	Detail   string
	NoColor  bool

	Done bool // an answer key was pressed
	Yes  bool // the answer, valid when Done: true continues
}

// NewConfirm returns an unanswered prompt.
func NewConfirm(question, detail string) ConfirmModel {
	return ConfirmModel{Question: question, Detail: detail}
}

// Update applies one key press; after an answer it ignores further keys.
func (m ConfirmModel) Update(k Key) ConfirmModel {
	if m.Done {
		return m
	}
	switch k.Kind {
	case KeyEnter:
		m.Done, m.Yes = true, true
	case KeyEsc, KeyInterrupt:
		m.Done, m.Yes = true, false
	case KeyRune:
		switch k.Rune {
		case 'y', 'Y':
			m.Done, m.Yes = true, true
		case 'n', 'N':
			m.Done, m.Yes = true, false
		}
	}
	return m
}

// confirmIndent prefixes the explanation and the key line.
const confirmIndent = "  "

// View renders the prompt as lines separated by "\n", none wider than
// width-1 columns. Text wraps at spaces rather than being cut: the account
// names come last in the question and the way back last in the key line.
// When the lines do not fit in height, the explanation gives way first and
// then the question; the key line always shows.
func (m ConfirmModel) View(width, height int) string {
	return strings.Join(m.lines(width, height), "\n")
}

func (m ConfirmModel) lines(width, height int) []string {
	w := max(width, 20) - 1
	indented := func(s string) []string {
		var out []string
		for _, l := range wrap(sanitize(s), w-Width(confirmIndent)) {
			out = append(out, confirmIndent+l)
		}
		return out
	}
	body := wrap(sanitize(m.Question), w)
	if m.Detail != "" {
		body = append(body, indented(m.Detail)...)
	}
	// The key line is chrome, dim like the picker's help line; the question
	// and its explanation stay normal. paint drops the SGR under NoColor.
	hint := indented(confirmHint)
	for i, l := range hint {
		hint[i] = paint(l, toneDim, m.NoColor)
	}
	if room := max(height-len(hint), 1); len(body) > room {
		body = body[:room]
	}
	return append(body, hint...)
}

// wrap breaks s into lines of at most width columns at whitespace (runs of
// it collapse); a word wider than width is cut into width-column pieces.
func wrap(s string, width int) []string {
	width = max(width, 1)
	var lines []string
	var cur strings.Builder
	curW := 0
	flush := func() {
		lines = append(lines, cur.String())
		cur.Reset()
		curW = 0
	}
	for _, word := range strings.Fields(s) {
		for Width(word) > width {
			if curW > 0 {
				flush()
			}
			n := clusterPrefix(word, width)
			lines = append(lines, word[:n])
			word = word[n:]
		}
		ww := Width(word)
		switch {
		case ww == 0:
		case curW == 0:
			cur.WriteString(word)
			curW = ww
		case curW+1+ww <= width:
			cur.WriteByte(' ')
			cur.WriteString(word)
			curW += 1 + ww
		default:
			flush()
			cur.WriteString(word)
			curW = ww
		}
	}
	if curW > 0 || len(lines) == 0 {
		flush()
	}
	return lines
}

// clusterPrefix is the byte length of the longest prefix of s, in whole
// characters (see nextCluster), that fits in width columns; at least one
// character, so wrapping always makes progress.
func clusterPrefix(s string, width int) int {
	n, w := 0, 0
	for n < len(s) {
		size, cw := nextCluster(s[n:])
		if n > 0 && w+cw > width {
			break
		}
		n += size
		w += cw
	}
	return n
}

// confirmLoop is Confirm's key loop, separated from terminal setup so tests
// can drive it with readers.
//
// Unlike run it reads synchronously and never waits for the rest of an
// escape sequence: a lone ESC at the end of a read is the Esc key at once.
// A timed wait would leave a read in flight when the prompt returns, and on
// macOS that read cannot be abandoned (see run): it would steal the first
// key typed into the picker that reopens after "go back". The cost is that
// an arrow key split across reads (ssh, tmux) counts as Esc, which only goes
// back to the picker. EOF answers no.
func confirmLoop(m ConfirmModel, in io.Reader, out io.Writer, size func() (int, int)) (bool, error) {
	var pending []byte
	buf := make([]byte, 256)
	// An accept (Enter/y) arriving within acceptGrace of the prompt opening
	// is ignored: it is almost always the Enter that selected the row in the
	// picker, repeated or auto-repeated, not a decision to move the session.
	// Declines are always honoured.
	start := confirmClock()
	for {
		w, h := size()
		if err := drawLines(out, m.lines(w, h), -1); err != nil {
			return false, err
		}
		n, err := in.Read(buf)
		if n > 0 {
			var keys []Key
			keys, pending = DecodeKeys(append(pending, buf[:n]...))
			if len(pending) == 1 && pending[0] == 0x1b {
				keys = append(keys, Key{Kind: KeyEsc})
				pending = nil
			}
			for _, k := range keys {
				next := m.Update(k)
				if next.Done && next.Yes && confirmClock().Sub(start) < acceptGrace {
					continue
				}
				if m = next; m.Done {
					return m.Yes, nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("read from terminal: %w", err)
		}
	}
}

// acceptGrace is how long after opening the prompt an accept key is ignored;
// confirmClock is a seam for tests.
var (
	acceptGrace  = 300 * time.Millisecond
	confirmClock = time.Now
)

// maxAnswer bounds how much of one fallback answer line is kept.
const maxAnswer = 256

// ConfirmFallback is the line-based confirm used without a raw-mode
// terminal, on opts.In and opts.Out: it prints the question and the
// explanation, then asks "Continue? [Y/n] ". An empty answer, y or yes
// continue; n or no go back; end of input goes back; anything else asks
// again. It reads one byte at a time so nothing after the answer's newline
// is consumed: the picker's fallback reads the same input next.
func ConfirmFallback(opts ConfirmOptions) (bool, error) {
	in, out := opts.In, opts.Out
	if in == nil || out == nil {
		return false, errors.New("no terminal available for the confirmation")
	}
	fmt.Fprintln(out, sanitize(opts.Question))
	if opts.Detail != "" {
		fmt.Fprintln(out, confirmIndent+sanitize(opts.Detail))
	}
	for {
		fmt.Fprint(out, "Continue? [Y/n] ")
		ans, err := readLine(in)
		if err != nil {
			fmt.Fprintln(out)
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("read answer: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(out, "Answer y or n.")
	}
}

// readLine reads up to and including the next '\n' a byte at a time and
// returns the line without its line ending (at most maxAnswer bytes are
// kept). A final line without '\n' is returned; io.EOF only when nothing was
// read.
func readLine(r io.Reader) (string, error) {
	var line []byte
	var one [1]byte
	read := false
	for {
		n, err := r.Read(one[:])
		if n > 0 {
			read = true
			if one[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			if len(line) < maxAnswer {
				line = append(line, one[0])
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && read {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			return "", err
		}
	}
}
