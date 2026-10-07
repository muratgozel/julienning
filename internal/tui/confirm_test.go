package tui

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	moveQ = `Move "Fix flaky clock tests" from beta to alpha?`
	moveD = "Its transcript, checkpoints, task list and session environment move to alpha; beta will no longer have it. Project memory is merged, never overwritten."
)

func TestConfirmKeys(t *testing.T) {
	cases := []struct {
		name string
		key  Key
		done bool
		yes  bool
	}{
		{"Enter continues", enter, true, true},
		{"y continues", Key{Kind: KeyRune, Rune: 'y'}, true, true},
		{"Y continues", Key{Kind: KeyRune, Rune: 'Y'}, true, true},
		{"n goes back", Key{Kind: KeyRune, Rune: 'n'}, true, false},
		{"N goes back", Key{Kind: KeyRune, Rune: 'N'}, true, false},
		{"Esc goes back", Key{Kind: KeyEsc}, true, false},
		{"Ctrl-C goes back", Key{Kind: KeyInterrupt}, true, false},
		{"other letters are ignored", Key{Kind: KeyRune, Rune: 'x'}, false, false},
		{"q is not an answer", Key{Kind: KeyRune, Rune: 'q'}, false, false},
		{"arrows are ignored", up, false, false},
		{"Backspace is ignored", Key{Kind: KeyBackspace}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewConfirm(moveQ, moveD).Update(tc.key)
			if m.Done != tc.done || (tc.done && m.Yes != tc.yes) {
				t.Fatalf("Done %v Yes %v, want Done %v Yes %v", m.Done, m.Yes, tc.done, tc.yes)
			}
		})
	}
	// The first answer sticks.
	m := NewConfirm(moveQ, moveD).Update(Key{Kind: KeyRune, Rune: 'n'}).Update(enter)
	if !m.Done || m.Yes {
		t.Errorf("an answer must not change after it is given: %+v", m)
	}
}

func TestConfirmView(t *testing.T) {
	m := NewConfirm(moveQ, moveD)
	m.NoColor = true
	assertLines(t, strings.Split(m.View(80, 24), "\n"), []string{
		moveQ,
		"  Its transcript, checkpoints, task list and session environment move to alpha;",
		"  beta will no longer have it. Project memory is merged, never overwritten.",
		"  Enter to continue · n or Esc to go back",
	})
	// 79 columns is the most a line may take at width 80.
	if l := "  Its transcript, checkpoints, task list and session environment move to alpha;"; Width(l) != 79 {
		t.Fatalf("fixture drifted: %d columns", Width(l))
	}

	// Narrow: the question wraps instead of losing the account names.
	lines := strings.Split(m.View(30, 24), "\n")
	for _, l := range lines {
		if Width(l) > 29 {
			t.Errorf("line wider than 29 columns: %q", l)
		}
	}
	words := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	if words != moveQ+" "+moveD+" "+confirmHint {
		t.Errorf("narrow view must wrap, not cut:\n%s", strings.Join(lines, "\n"))
	}
	if lines[0] != `Move "Fix flaky clock tests"` || lines[1] != "from beta to alpha?" {
		t.Errorf("question lines: %q", lines[:2])
	}

	// Short: the explanation gives way, the key line stays.
	lines = strings.Split(m.View(80, 3), "\n")
	if len(lines) != 3 || lines[0] != moveQ || lines[2] != "  Enter to continue · n or Esc to go back" {
		t.Errorf("short view:\n%s", strings.Join(lines, "\n"))
	}
}

// The key line is dim; the question and explanation are normal text. Under
// NO_COLOR nothing carries an escape.
func TestConfirmViewColour(t *testing.T) {
	m := NewConfirm(moveQ, moveD)
	lines := strings.Split(m.View(80, 24), "\n")
	if lines[0] != moveQ {
		t.Errorf("question must be plain: %q", lines[0])
	}
	if want := sgrDim + "  Enter to continue · n or Esc to go back" + sgrReset; lines[len(lines)-1] != want {
		t.Errorf("key line = %q, want %q", lines[len(lines)-1], want)
	}
	m.NoColor = true
	if v := m.View(80, 24); strings.Contains(v, "\x1b") {
		t.Errorf("NO_COLOR view carries escapes: %q", v)
	}
}

func TestConfirmViewNeutralisesControlCharacters(t *testing.T) {
	m := NewConfirm("Move \"evil\x1b[2J\" from beta to alpha?", "x\x07y")
	if v := m.View(80, 24); strings.ContainsAny(sgr.ReplaceAllString(v, ""), "\x1b\x07") {
		t.Errorf("control characters reached the view: %q", v)
	}
}

func TestWrap(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  []string
	}{
		{"a bb ccc", 4, []string{"a bb", "ccc"}},
		{"  spaced   out  ", 20, []string{"spaced out"}},
		{"", 10, []string{""}},
		{"abcdefghij k", 4, []string{"abcd", "efgh", "ij k"}},
		{"日本語テキスト", 5, []string{"日本", "語テ", "キス", "ト"}},
		{"日", 1, []string{"日"}}, // a character wider than the line still makes progress
	}
	for _, tc := range cases {
		if got := wrap(tc.s, tc.width); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("wrap(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.want)
		}
	}
}

func TestConfirmLoop(t *testing.T) {
	// Key handling is under test here, not the accept grace period (see
	// TestConfirmIgnoresEnterDuringGracePeriod).
	oldGrace := acceptGrace
	acceptGrace = 0
	t.Cleanup(func() { acceptGrace = oldGrace })
	size := func() (int, int) { return 80, 24 }
	cases := []struct {
		name  string
		reads []string
		yes   bool
	}{
		{"Enter", []string{"\r"}, true},
		{"y", []string{"y"}, true},
		{"n", []string{"n"}, false},
		{"N", []string{"N"}, false},
		{"Ctrl-C", []string{"\x03"}, false},
		{"lone Esc answers at once", []string{"\x1b"}, false},
		{"Esc then more keys", []string{"\x1by"}, false},
		{"ignored keys, then Enter", []string{"x\x1b[A", "\r"}, true},
		{"arrow cut after its CSI start is joined", []string{"\x1b[", "B", "y"}, true},
		{"end of input goes back", []string{"x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &chunked{}
			for _, r := range tc.reads {
				in.reads = append(in.reads, []byte(r))
			}
			var out strings.Builder
			yes, err := confirmLoop(NewConfirm(moveQ, moveD), in, &out, size)
			if err != nil || yes != tc.yes {
				t.Fatalf("confirmLoop = %v, %v; want %v", yes, err, tc.yes)
			}
			if !strings.HasPrefix(out.String(), "\x1b[H"+moveQ+"\x1b[K\r\n") {
				t.Errorf("frame must repaint in place with CRLF: %q", out.String())
			}
		})
	}

	_, err := confirmLoop(NewConfirm(moveQ, moveD), errReader{}, io.Discard, size)
	if err == nil || !strings.Contains(err.Error(), "read from terminal: boom") {
		t.Errorf("read error: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestConfirmLoopHonoursNoColorEnv(t *testing.T) {
	for _, tc := range []struct {
		noColor string
		wantSGR bool
	}{{"", true}, {"1", false}} {
		getenv := func(k string) string {
			if k == "NO_COLOR" {
				return tc.noColor
			}
			return ""
		}
		var out strings.Builder
		m := newConfirmModel(ConfirmOptions{Question: moveQ, Detail: moveD}, getenv)
		if _, err := confirmLoop(m, strings.NewReader("\r"), &out, func() (int, int) { return 80, 24 }); err != nil {
			t.Fatal(err)
		}
		if got := sgr.MatchString(out.String()); got != tc.wantSGR {
			t.Errorf("NO_COLOR=%q: SGR present = %v, want %v\n%q", tc.noColor, got, tc.wantSGR, out.String())
		}
	}
}

func TestConfirmFallback(t *testing.T) {
	cases := []struct {
		name  string
		input string
		yes   bool
		shows string
	}{
		{"Enter continues", "\n", true, ""},
		{"y", "y\n", true, ""},
		{"YES", " YES \r\n", true, ""},
		{"n", "n\n", false, ""},
		{"No", "No\n", false, ""},
		{"unclear, then n", "maybe\nn\n", false, "Answer y or n.\nContinue? [Y/n] "},
		{"end of input goes back", "", false, ""},
		{"unclear, then end of input", "maybe", false, "Answer y or n."},
		{"last line without newline", "y", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			yes, err := ConfirmFallback(ConfirmOptions{Question: moveQ, Detail: moveD, In: strings.NewReader(tc.input), Out: &out})
			if err != nil || yes != tc.yes {
				t.Fatalf("got %v, %v; want %v\n%s", yes, err, tc.yes, out.String())
			}
			if want := moveQ + "\n  " + moveD + "\nContinue? [Y/n] "; !strings.HasPrefix(out.String(), want) {
				t.Errorf("output:\n%s\nwant prefix:\n%s", out.String(), want)
			}
			if !strings.Contains(out.String(), tc.shows) {
				t.Errorf("output lacks %q:\n%s", tc.shows, out.String())
			}
			if strings.Contains(out.String(), "\x1b") {
				t.Error("fallback must be plain text")
			}
		})
	}
}

// The fallback stops reading at the answer's newline: the picker's numbered
// fallback reads the next line from the same input.
func TestConfirmFallbackLeavesTheRestUnread(t *testing.T) {
	in := strings.NewReader("n\n2\n")
	if yes, err := ConfirmFallback(ConfirmOptions{Question: moveQ, In: in, Out: io.Discard}); err != nil || yes {
		t.Fatalf("got %v, %v", yes, err)
	}
	rest, _ := io.ReadAll(in)
	if string(rest) != "2\n" {
		t.Errorf("rest = %q, want %q", rest, "2\n")
	}
}

func TestConfirmFallbackErrors(t *testing.T) {
	if _, err := ConfirmFallback(ConfirmOptions{Question: moveQ}); err == nil {
		t.Error("no In/Out must be an error")
	}
	_, err := ConfirmFallback(ConfirmOptions{Question: moveQ, In: errReader{}, Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "read answer: boom") {
		t.Errorf("read error: %v", err)
	}
}

func TestConfirmFallsBackWithoutARawTerminal(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	var out strings.Builder
	yes, err := Confirm(ConfirmOptions{Question: moveQ, In: strings.NewReader("n\n"), Out: &out, Getenv: env(map[string]string{"TERM": "dumb"})})
	if err != nil || yes || !strings.Contains(out.String(), "[Y/n]") {
		t.Fatalf("TERM=dumb: %v %v %q", yes, err, out.String())
	}

	notTTY := filepath.Join(t.TempDir(), "not-a-tty")
	if err := os.WriteFile(notTTY, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	yes, err = Confirm(ConfirmOptions{Question: moveQ, In: strings.NewReader("\n"), Out: &out, TTY: notTTY, Getenv: env(map[string]string{"TERM": "xterm"})})
	if err != nil || !yes || !strings.Contains(out.String(), "[Y/n]") {
		t.Fatalf("no tty: %v %v %q", yes, err, out.String())
	}

	out.Reset()
	yes, err = Confirm(ConfirmOptions{Question: moveQ, In: strings.NewReader("y\n"), Out: &out, TTY: filepath.Join(t.TempDir(), "missing"), Getenv: env(nil)})
	if err != nil || !yes {
		t.Fatalf("missing tty: %v %v", yes, err)
	}
}

// After a declined move the picker reopens on the same row, scrolled into
// view; a disabled or unknown row leaves the default.
func TestSelectItem(t *testing.T) {
	m := New(fixture(), "h").SelectItem(3)
	if selected(t, m) != 3 {
		t.Errorf("SelectItem(3) selected %d", selected(t, m))
	}
	for _, i := range []int{2, -1, 99} { // 2 is disabled
		if got := selected(t, New(fixture(), "h").SelectItem(i)); got != 0 {
			t.Errorf("SelectItem(%d) selected %d, want the default 0", i, got)
		}
	}
	const w, h = 80, 24
	m = New(manyItems(150), "h").SelectItem(120)
	m.NoColor = true
	if selected(t, m) != 120 {
		t.Fatalf("selected %d", selected(t, m))
	}
	assertCursorShown(t, m.Layout(h), w, h)
}

// stepReader hands out one chunk per Read and advances the fake clock so the
// first Enter lands inside the grace period and the second after it.
type stepReader struct {
	chunks [][]byte
	clock  *time.Time
	step   time.Duration
}

func (r *stepReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	c := r.chunks[0]
	r.chunks = r.chunks[1:]
	*r.clock = r.clock.Add(r.step)
	return copy(p, c), nil
}

func TestConfirmIgnoresEnterDuringGracePeriod(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	oldClock, oldGrace := confirmClock, acceptGrace
	t.Cleanup(func() { confirmClock, acceptGrace = oldClock, oldGrace })
	confirmClock = func() time.Time { return now }
	acceptGrace = 300 * time.Millisecond

	m := NewConfirm("Move?", "d")
	size := func() (int, int) { return 80, 24 }

	// Enter at +100ms is swallowed; Enter at +400ms accepts.
	r := &stepReader{chunks: [][]byte{[]byte("\r"), []byte("\r")}, clock: &now, step: 150 * time.Millisecond}
	yes, err := confirmLoop(m, r, io.Discard, size)
	if err != nil || !yes {
		t.Fatalf("second Enter should accept: yes=%v err=%v", yes, err)
	}

	// A decline inside the grace period is honoured immediately.
	now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r = &stepReader{chunks: [][]byte{[]byte("n")}, clock: &now, step: 10 * time.Millisecond}
	yes, err = confirmLoop(m, r, io.Discard, size)
	if err != nil || yes {
		t.Fatalf("n during grace should decline: yes=%v err=%v", yes, err)
	}

	// Only Enter inside the grace period, then EOF → no.
	now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r = &stepReader{chunks: [][]byte{[]byte("\r")}, clock: &now, step: 10 * time.Millisecond}
	yes, err = confirmLoop(m, r, io.Discard, size)
	if err != nil || yes {
		t.Fatalf("lone early Enter must not accept: yes=%v err=%v", yes, err)
	}
}
