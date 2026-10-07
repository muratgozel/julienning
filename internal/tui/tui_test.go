package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fixture() []Item {
	return []Item{
		{Title: "New session", Meta: "in julienning3", Pinned: true},
		{Title: "Checkout fix", Meta: "sixtynine · 2h ago · 0b7f6c1e", Detail: "fix the checkout flow", Search: "0b7f6c1e"},
		{Title: "Open elsewhere", Meta: "julienning2 · 5m ago · 1c8a7d2f", Note: "(open in another terminal)", Detail: "busy", Disabled: true},
		{Title: "Jenkins pipeline", Meta: "julienning3 · 1d ago · 2d9b8e30", Detail: "make the kotlin build green", Search: "2d9b8e30"},
	}
}

func press(m Model, keys ...Key) Model {
	for _, k := range keys {
		m = m.Update(k)
	}
	return m
}

func runes(s string) []Key {
	var out []Key
	for _, r := range s {
		out = append(out, Key{Kind: KeyRune, Rune: r})
	}
	return out
}

var (
	up    = Key{Kind: KeyUp}
	down  = Key{Kind: KeyDown}
	enter = Key{Kind: KeyEnter}
)

func selected(t *testing.T, m Model) int {
	t.Helper()
	i, ok := m.Selected()
	if !ok {
		t.Fatalf("nothing selectable under cursor %d", m.Cursor)
	}
	return i
}

func TestNavigationSkipsDisabledRows(t *testing.T) {
	m := New(fixture(), "Pick")
	if selected(t, m) != 0 {
		t.Fatal("New session must be the default")
	}
	m = press(m, down)
	if selected(t, m) != 1 {
		t.Fatalf("down → %d", selected(t, m))
	}
	m = press(m, down) // skips the live session
	if selected(t, m) != 3 {
		t.Fatalf("down past disabled → %d", selected(t, m))
	}
	m = press(m, down, down) // stops at the end
	if selected(t, m) != 3 {
		t.Fatalf("end → %d", selected(t, m))
	}
	m = press(m, up)
	if selected(t, m) != 1 {
		t.Fatalf("up → %d", selected(t, m))
	}
	m = press(m, Key{Kind: KeyEnd})
	if selected(t, m) != 3 {
		t.Fatal("End")
	}
	m = press(m, Key{Kind: KeyHome})
	if selected(t, m) != 0 {
		t.Fatal("Home")
	}
	m = press(m, Key{Kind: KeyPageDown})
	if selected(t, m) != 3 {
		t.Fatal("PageDown")
	}
	// j/k navigate while no filter is being typed.
	m = press(m, runes("kk")...)
	if selected(t, m) != 0 || m.Filter != "" {
		t.Fatalf("k: %d filter %q", selected(t, m), m.Filter)
	}
	m = press(m, runes("j")...)
	if selected(t, m) != 1 {
		t.Fatal("j")
	}
	m = press(m, enter)
	if !m.Done || selected(t, m) != 1 {
		t.Fatal("Enter")
	}
}

func TestTypingFilters(t *testing.T) {
	m := press(New(fixture(), "Pick"), runes("CHECK")...)
	if m.Filter != "CHECK" {
		t.Fatalf("filter = %q", m.Filter)
	}
	if got := fmt.Sprint(m.Visible()); got != "[0 1]" {
		t.Fatalf("visible = %s (New session stays pinned)", got)
	}
	if selected(t, m) != 1 {
		t.Fatal("cursor must jump to the first match, not the pinned row")
	}
	// Words match independently, against meta and ids too.
	m = press(New(fixture(), "Pick"), runes("2d9b julienning3")...)
	if got := fmt.Sprint(m.Visible()); got != "[0 3]" {
		t.Fatalf("visible = %s", got)
	}
	// Once typing, j and k are letters.
	m = press(New(fixture(), "Pick"), runes("/jenk")...)
	if m.Filter != "jenk" || selected(t, m) != 3 {
		t.Fatalf("filter %q sel %d", m.Filter, selected(t, m))
	}
	m = press(m, Key{Kind: KeyBackspace}, Key{Kind: KeyBackspace})
	if m.Filter != "je" {
		t.Fatalf("backspace → %q", m.Filter)
	}
	m = press(m, Key{Kind: KeyClearFilter})
	if m.Filter != "" || selected(t, m) != 0 {
		t.Fatal("Ctrl-U clears")
	}
	m = press(m, runes("zzz")...)
	if got := fmt.Sprint(m.Visible()); got != "[0]" || selected(t, m) != 0 {
		t.Fatalf("no match: %s", got)
	}
	if !strings.Contains(m.View(80, 20), "(no matches)") {
		t.Error("no-match hint missing")
	}
}

func TestCancelKeys(t *testing.T) {
	for _, k := range []Key{{Kind: KeyEsc}, {Kind: KeyInterrupt}} {
		m := press(New(fixture(), "Pick"), runes("chec")...)
		m = press(m, k)
		if !m.Canceled || m.Done {
			t.Errorf("%v: %+v", k, m)
		}
		if press(m, enter).Done {
			t.Error("a canceled model ignores further keys")
		}
	}
}

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestViewMarksSelectionWithoutRelyingOnColour(t *testing.T) {
	m := press(New(fixture(), "Pick a session for julienning3"), down)
	out := m.View(60, 20)
	lines := strings.Split(out, "\n")
	var marked []string
	for _, l := range lines {
		plain := sgr.ReplaceAllString(l, "")
		if strings.HasPrefix(plain, "> ") {
			marked = append(marked, plain)
		}
		if w := Width(plain); w > 59 {
			t.Errorf("line wider than 59 (%d): %q", w, plain)
		}
	}
	if len(marked) != 1 || !strings.HasPrefix(marked[0], "> Checkout fix") {
		t.Fatalf("marked = %q\n%s", marked, out)
	}
	if !strings.Contains(out, "\x1b[7m> Checkout fix") {
		t.Errorf("selected row must be in reverse video:\n%q", out)
	}
	if !strings.Contains(out, "\x1b[2m  Open elsewh") || !strings.Contains(out, "(open in another terminal)") {
		t.Errorf("disabled row must be dimmed and labelled:\n%q", out)
	}
	if !strings.Contains(out, "    fix the checkout flow") {
		t.Errorf("detail line missing:\n%s", out)
	}
	if at := m.CursorLine(60, 20); !strings.HasPrefix(sgr.ReplaceAllString(lines[at], ""), "> ") {
		t.Errorf("CursorLine %d points at %q", at, lines[at])
	}

	m.NoColor = true
	out = m.View(60, 20)
	if strings.Contains(out, "\x1b") {
		t.Errorf("NO_COLOR output has escapes: %q", out)
	}
	if !strings.Contains(out, "> Checkout fix") {
		t.Error("marker must survive NO_COLOR")
	}
}

func TestViewScrollsAndTruncates(t *testing.T) {
	var items []Item
	items = append(items, Item{Title: "New session", Pinned: true})
	for i := 1; i <= 30; i++ {
		items = append(items, Item{
			Title:  fmt.Sprintf("Session %02d with a rather long title that will not fit", i),
			Meta:   "julienning3 · 3d ago · abcdef12",
			Detail: strings.Repeat("prompt text ", 20),
		})
	}
	m := New(items, "header")
	m.NoColor = true
	for i := 0; i < 25; i++ {
		m = m.Update(down)
	}
	m = m.Layout(12)
	out := m.View(40, 12)
	lines := strings.Split(out, "\n")
	if len(lines) > 12 {
		t.Fatalf("%d lines for height 12:\n%s", len(lines), out)
	}
	found := false
	for _, l := range lines {
		if Width(l) > 39 {
			t.Errorf("too wide: %q", l)
		}
		if strings.HasPrefix(l, "> Session 25") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cursor row not visible:\n%s", out)
	}
	// Moving back up scrolls back.
	for i := 0; i < 25; i++ {
		m = m.Update(up).Layout(12)
	}
	if out := m.View(40, 12); !strings.Contains(out, "> New session") {
		t.Fatalf("scrolled back:\n%s", out)
	}
}

func TestViewNeutralisesControlCharacters(t *testing.T) {
	m := New([]Item{{Title: "evil\x1b[2Jtitle", Detail: "a\rb"}}, "h\x07")
	m.NoColor = true
	if out := m.View(80, 10); strings.ContainsAny(out, "\x1b\r\x07") {
		t.Errorf("control characters leaked: %q", out)
	}
}

func TestTruncateWide(t *testing.T) {
	if got := Truncate("日本語のタイトル", 7); got != "日本語…" || Width(got) > 7 {
		t.Errorf("Truncate wide = %q (%d)", got, Width(got))
	}
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("%q", got)
	}
}

func TestWidthOfEmojiAndCJK(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"✅", 2}, {"⚡", 2}, {"⭐", 2}, {"🫠", 2}, {"❤️", 2}, {"❤", 2},
		{"☺️", 2},  // text-default symbol forced to emoji by VS16
		{"#️⃣", 2}, // keycap: base + VS16 + combining keycap
		{"日本語", 6}, {"한국어", 6}, {"ｆｕｌｌ", 8},
		{"a\u200Db", 2}, // zero-width joiner
		{"e\u0301", 1},  // combining accent
		{"a\uFE0E", 1},  // text presentation selector alone adds nothing
		{"fix ✅ now ⚡", 13},
	}
	for _, tc := range cases {
		if got := Width(tc.s); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestTruncateKeepsClustersWhole(t *testing.T) {
	cases := []struct {
		s    string
		max  int
		want string
	}{
		{"ab❤️cd", 4, "ab…"},   // ❤️ needs 2 columns, only 1 is left before "…"
		{"ab❤️cd", 5, "ab❤️…"}, // never "ab❤…" with the selector cut off
		{"✅✅✅", 5, "✅✅…"},
		{"🫠 melt", 3, "🫠…"},
		{"e\u0301e\u0301e\u0301", 3, "e\u0301e\u0301e\u0301"},
		{"e\u0301e\u0301e\u0301x", 3, "e\u0301e\u0301…"},
	}
	for _, tc := range cases {
		got := Truncate(tc.s, tc.max)
		if got != tc.want || Width(got) > tc.max {
			t.Errorf("Truncate(%q, %d) = %q (width %d), want %q", tc.s, tc.max, got, Width(got), tc.want)
		}
	}
	// A picker row with emoji never exceeds the terminal width.
	m := New([]Item{{Title: strings.Repeat("⚡✅⭐🫠❤️", 30), Meta: "sixtynine · 2h ago · 0b7f6c1e"}}, "h")
	m.NoColor = true
	for _, line := range strings.Split(m.View(40, 10), "\n") {
		if Width(line) > 39 {
			t.Errorf("line wider than 39 columns (%d): %q", Width(line), line)
		}
	}
}

func TestDecodeKeys(t *testing.T) {
	cases := []struct {
		in   string
		want []KeyKind
	}{
		{"\x1b[A\x1b[B", []KeyKind{KeyUp, KeyDown}},
		{"\x1bOA\x1bOB", []KeyKind{KeyUp, KeyDown}},
		{"\x1b[5~\x1b[6~\x1b[H\x1b[F\x1b[1~\x1b[4~", []KeyKind{KeyPageUp, KeyPageDown, KeyHome, KeyEnd, KeyHome, KeyEnd}},
		{"\x1bx", []KeyKind{KeyEsc, KeyRune}},
		{"\x1b\x1b[A", []KeyKind{KeyEsc, KeyUp}},
		{"\x1b[1;5C", []KeyKind{KeyNone}},
		{"\r\n\x7f\x08\x03\x04\x15\x0e\x10\x01", []KeyKind{KeyEnter, KeyEnter, KeyBackspace, KeyBackspace, KeyInterrupt, KeyInterrupt, KeyClearFilter, KeyDown, KeyUp, KeyNone}},
		{"aé", []KeyKind{KeyRune, KeyRune}},
	}
	for _, tc := range cases {
		keys, rest := DecodeKeys([]byte(tc.in))
		var got []KeyKind
		for _, k := range keys {
			got = append(got, k.Kind)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) || rest != nil {
			t.Errorf("DecodeKeys(%q) = %v rest %q, want %v", tc.in, got, rest, tc.want)
		}
	}
	// A multi-byte rune split across two reads.
	b := []byte("ü")
	keys, rest := DecodeKeys(b[:1])
	if len(keys) != 0 || len(rest) != 1 {
		t.Fatalf("split: %v %q", keys, rest)
	}
	keys, rest = DecodeKeys(append(rest, b[1:]...))
	if len(keys) != 1 || keys[0].Rune != 'ü' || rest != nil {
		t.Fatalf("joined: %v %q", keys, rest)
	}
}

// A trailing ESC or unfinished CSI is held back, not decoded: it may be the
// first half of an arrow key split across reads.
func TestDecodeKeysHoldsBackUnfinishedEscapes(t *testing.T) {
	for _, in := range []string{"\x1b", "\x1b[", "\x1b[1;", "\x1b[5", "\x1bO"} {
		keys, rest := DecodeKeys([]byte("a" + in))
		if len(keys) != 1 || keys[0].Rune != 'a' || string(rest) != in {
			t.Errorf("DecodeKeys(a%q) = %v rest %q", in, keys, rest)
		}
		if !PendingEscape([]byte(in)) {
			t.Errorf("PendingEscape(%q) = false", in)
		}
	}
	for _, in := range []string{"", "a", "\x1b[A", "\x1bx", "\x1b[" + strings.Repeat("1", 20)} {
		if PendingEscape([]byte(in)) {
			t.Errorf("PendingEscape(%q) = true", in)
		}
	}
	// Garbage that never finishes is decoded instead of buffered forever.
	if _, rest := DecodeKeys([]byte("\x1b[" + strings.Repeat("9", 40))); rest != nil {
		t.Errorf("overlong sequence kept: %q", rest)
	}
	// Once no more input will come: a lone ESC is Esc, a cut sequence is dropped.
	if got := FlushKeys([]byte("\x1b")); len(got) != 1 || got[0].Kind != KeyEsc {
		t.Errorf("FlushKeys(ESC) = %v", got)
	}
	if got := FlushKeys([]byte("\x1b[1;")); len(got) != 0 {
		t.Errorf("FlushKeys(ESC[1;) = %v", got)
	}
	if got := FlushKeys([]byte("x\xc3")); len(got) != 1 || got[0].Rune != 'x' {
		t.Errorf("FlushKeys(cut UTF-8) = %v", got)
	}
}

func TestRunLoop(t *testing.T) {
	var out strings.Builder
	i, err := run(New(fixture(), "h"), strings.NewReader("\x1b[B\x1b[B\r"), &out, func() (int, int) { return 80, 24 })
	if err != nil || i != 3 {
		t.Fatalf("run = %d %v", i, err)
	}
	if !strings.Contains(out.String(), "\x1b[H") || !strings.Contains(out.String(), "\r\n") {
		t.Errorf("frames must repaint in place with CRLF: %q", out.String()[:40])
	}
	if _, err := run(New(fixture(), "h"), strings.NewReader("che\x1b"), &out, func() (int, int) { return 80, 24 }); !errors.Is(err, ErrCanceled) {
		t.Errorf("Esc: %v", err)
	}
	if _, err := run(New(fixture(), "h"), strings.NewReader("ch"), &out, func() (int, int) { return 80, 24 }); !errors.Is(err, ErrCanceled) {
		t.Errorf("EOF: %v", err)
	}
}

// chunked delivers a fixed sequence of reads.
type chunked struct{ reads [][]byte }

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.reads) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.reads[0])
	c.reads = c.reads[1:]
	return n, nil
}

// An arrow key cut by a full read buffer is reassembled, not taken as Esc.
func TestRunLoopReassemblesCutEscape(t *testing.T) {
	first := append([]byte(strings.Repeat("x", 1023)), 0x1b)
	in := &chunked{reads: [][]byte{first, []byte("[B"), {0x15}, []byte("\x1b[B\r")}}
	var out strings.Builder
	i, err := run(New(fixture(), "h"), in, &out, func() (int, int) { return 80, 24 })
	if err != nil || i != 1 {
		t.Fatalf("run = %d %v", i, err)
	}
}

// Over ssh or tmux an arrow key can arrive as "ESC" then "[B" in two reads;
// that must move the cursor, not cancel the picker.
func TestRunLoopJoinsEscapeSplitAcrossReads(t *testing.T) {
	cases := []struct {
		name  string
		reads []string
		want  int
	}{
		{"ESC | [B", []string{"\x1b", "[B", "\r"}, 1},
		{"ESC [ | B", []string{"\x1b[", "B", "\r"}, 1},
		{"ESC | [ | B, twice", []string{"\x1b", "[", "B", "\x1b", "[B", "\r"}, 3},
		{"PgDn cut in its parameters", []string{"\x1b[6", "~\r"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &chunked{}
			for _, r := range tc.reads {
				in.reads = append(in.reads, []byte(r))
			}
			var out strings.Builder
			i, err := run(New(fixture(), "h"), in, &out, func() (int, int) { return 80, 24 })
			if err != nil || i != tc.want {
				t.Fatalf("run = %d %v, want %d", i, err, tc.want)
			}
		})
	}
}

// A lone ESC with nothing after it still cancels, once escWait has passed.
func TestRunLoopLoneEscCancelsAfterTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = pw.Write([]byte("ch\x1b")) }()
	type result struct {
		i   int
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		i, err := run(New(fixture(), "h"), pr, io.Discard, func() (int, int) { return 80, 24 })
		done <- result{i, err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, ErrCanceled) {
			t.Fatalf("run = %d %v, want ErrCanceled", r.i, r.err)
		}
		if el := time.Since(start); el < escWait {
			t.Errorf("canceled after %v, before the %v wait", el, escWait)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a lone ESC never canceled")
	}
	// An unfinished CSI that never completes is dropped, not taken as Esc.
	pr2, pw2 := io.Pipe()
	defer pw2.Close()
	go func() {
		_, _ = pw2.Write([]byte("\x1b["))
		time.Sleep(3 * escWait)
		_, _ = pw2.Write([]byte("\r"))
	}()
	i, err := run(New(fixture(), "h"), pr2, io.Discard, func() (int, int) { return 80, 24 })
	if err != nil || i != 0 {
		t.Fatalf("cut CSI then Enter: %d %v", i, err)
	}
}

func TestFallbackPrompt(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
		err   error
		shows []string
	}{
		{"enter picks new session", "\n", 0, nil, []string{"1) New session", "2) Checkout fix", "3) Jenkins pipeline"}},
		{"number skips disabled rows", "3\n", 3, nil, []string{"   Open elsewhere  julienning2 · 5m ago · 1c8a7d2f (open in another terminal)"}},
		{"out of range then valid", "9\n2\n", 1, nil, []string{"Enter a number from 1 to 3."}},
		{"text filters", "kotlin\n3\n", 3, nil, []string{"make the kotlin build green"}},
		{"filter without match", "zzz\nq\n", 0, ErrCanceled, []string{`No sessions match "zzz".`}},
		{"quit", "q\n", 0, ErrCanceled, nil},
		{"end of input", "", 0, ErrCanceled, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			got, err := Fallback(fixture(), Options{Header: "Pick a session for julienning3", In: strings.NewReader(tc.input), Out: &out})
			if !errors.Is(err, tc.err) || (tc.err == nil && got != tc.want) {
				t.Fatalf("got %d, %v; want %d, %v\n%s", got, err, tc.want, tc.err, out.String())
			}
			if !strings.HasPrefix(out.String(), "Pick a session for julienning3\n") {
				t.Errorf("header missing:\n%s", out.String())
			}
			for _, s := range tc.shows {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output lacks %q:\n%s", s, out.String())
				}
			}
			if strings.Contains(out.String(), "\x1b") {
				t.Error("fallback must be plain text")
			}
		})
	}
}

func TestPickFallsBackWithoutARawTerminal(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	var out strings.Builder
	i, err := Pick(fixture(), Options{Header: "h", In: strings.NewReader("2\n"), Out: &out, Getenv: env(map[string]string{"TERM": "dumb"})})
	if err != nil || i != 1 || !strings.Contains(out.String(), "Choose 1-3") {
		t.Fatalf("TERM=dumb: %d %v %q", i, err, out.String())
	}

	notTTY := filepath.Join(t.TempDir(), "not-a-tty")
	if err := os.WriteFile(notTTY, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	i, err = Pick(fixture(), Options{Header: "h", In: strings.NewReader("\n"), Out: &out, TTY: notTTY, Getenv: env(map[string]string{"TERM": "xterm"})})
	if err != nil || i != 0 || !strings.Contains(out.String(), "1) New session") {
		t.Fatalf("no tty: %d %v %q", i, err, out.String())
	}

	out.Reset()
	_, err = Pick(fixture(), Options{Header: "h", In: strings.NewReader("\n"), Out: &out, TTY: filepath.Join(t.TempDir(), "missing"), Getenv: env(nil)})
	if err != nil {
		t.Fatalf("missing tty: %v", err)
	}
}

// hierarchyFixture adds a selectable row that is dimmed: "(may be open)".
func hierarchyFixture() []Item {
	return append(fixture(), Item{
		Title: "Maybe open", Meta: "julienning1 · 1m ago · 3e0c9f41", Note: "(may be open)",
		Detail: "still running?", Muted: true,
	})
}

func assertLines(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// Titles stay normal; metadata, prompts, chrome and unavailable rows are
// dim; the selected row is reverse video on its first line only.
func TestViewDimsSecondaryText(t *testing.T) {
	m := press(New(hierarchyFixture(), "Pick a session"), down)
	assertLines(t, strings.Split(m.View(100, 30), "\n"), []string{
		"Pick a session",
		"\x1b[2mFilter: (type to search)\x1b[0m",
		"  New session\x1b[2m  in julienning3\x1b[0m",
		"\x1b[7m> Checkout fix  sixtynine · 2h ago · 0b7f6c1e\x1b[0m",
		"\x1b[2m    fix the checkout flow\x1b[0m",
		"\x1b[2m  Open elsewhere  julienning2 · 5m ago · 1c8a7d2f (open in another terminal)\x1b[0m",
		"\x1b[2m    busy\x1b[0m",
		"  Jenkins pipeline\x1b[2m  julienning3 · 1d ago · 2d9b8e30\x1b[0m",
		"\x1b[2m    make the kotlin build green\x1b[0m",
		"\x1b[2m  Maybe open  julienning1 · 1m ago · 3e0c9f41 (may be open)\x1b[0m",
		"\x1b[2m    still running?\x1b[0m",
		"\x1b[2m↑/↓ move · type to filter · Enter select · Esc cancel\x1b[0m",
	})

	// A muted row can still be chosen: selected, its first line is reverse
	// video and its prompt stays dim.
	m = press(m, Key{Kind: KeyEnd})
	if selected(t, m) != 4 {
		t.Fatalf("End → %d, want the muted row", selected(t, m))
	}
	lines := strings.Split(m.View(100, 30), "\n")
	if lines[9] != "\x1b[7m> Maybe open  julienning1 · 1m ago · 3e0c9f41 (may be open)\x1b[0m" || lines[10] != "\x1b[2m    still running?\x1b[0m" {
		t.Errorf("selected muted row:\n%q\n%q", lines[9], lines[10])
	}
	if at := m.CursorLine(100, 30); at != 9 {
		t.Errorf("CursorLine = %d, want 9", at)
	}

	// The filter label is dim; typed text stays readable.
	m = press(New(hierarchyFixture(), "h"), runes("check")...)
	if got := strings.Split(m.View(100, 30), "\n")[1]; got != "\x1b[2mFilter: \x1b[0mcheck\x1b[2m · 1 of 4\x1b[0m" {
		t.Errorf("filter line = %q", got)
	}
	m = press(m, runes("zzz")...)
	if out := m.View(100, 30); !strings.Contains(out, "\x1b[2m  (no matches)\x1b[0m") {
		t.Errorf("no-match hint must be dim:\n%q", out)
	}
}

// Without colour the marker and indentation still carry the hierarchy.
func TestViewNoColorKeepsMarkerAndIndent(t *testing.T) {
	m := press(New(hierarchyFixture(), "Pick a session"), down)
	m.NoColor = true
	out := m.View(100, 30)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("NO_COLOR output has escapes: %q", out)
	}
	assertLines(t, strings.Split(out, "\n"), []string{
		"Pick a session",
		"Filter: (type to search)",
		"  New session  in julienning3",
		"> Checkout fix  sixtynine · 2h ago · 0b7f6c1e",
		"    fix the checkout flow",
		"  Open elsewhere  julienning2 · 5m ago · 1c8a7d2f (open in another terminal)",
		"    busy",
		"  Jenkins pipeline  julienning3 · 1d ago · 2d9b8e30",
		"    make the kotlin build green",
		"  Maybe open  julienning1 · 1m ago · 3e0c9f41 (may be open)",
		"    still running?",
		"↑/↓ move · type to filter · Enter select · Esc cancel",
	})
}

// NO_COLOR from the environment reaches the frames the raw-mode loop draws.
func TestRunHonoursNoColorEnv(t *testing.T) {
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
		if _, err := run(newModel(fixture(), "h", getenv), strings.NewReader("\r"), &out, func() (int, int) { return 80, 24 }); err != nil {
			t.Fatal(err)
		}
		if got := sgr.MatchString(out.String()); got != tc.wantSGR {
			t.Errorf("NO_COLOR=%q: SGR present = %v, want %v\n%q", tc.noColor, got, tc.wantSGR, out.String())
		}
		if !strings.Contains(out.String(), "> New session") {
			t.Errorf("NO_COLOR=%q: marker missing:\n%q", tc.noColor, out.String())
		}
	}
}

// A long title gives way so the metadata stays whole; only when the title is
// down to minTitle columns does the metadata get cut, and it is dropped when
// almost nothing of it would show.
func TestRowTruncatesTitleBeforeMetadata(t *testing.T) {
	const meta = "julienning3 · 3d ago · a3656686"
	long := "Refactor the billing module so that invoices come from the new pricing API"
	row := func(title string, width int) string {
		m := New([]Item{{Title: "New session", Pinned: true}, {Title: title, Meta: meta, Detail: long + " " + long}}, "h")
		m.NoColor = true
		lines := strings.Split(m.View(width, 20), "\n")
		for _, l := range lines {
			if Width(l) > width-1 {
				t.Errorf("width %d: line of %d columns: %q", width, Width(l), l)
			}
		}
		if d := lines[4]; !strings.HasPrefix(d, detailIndent+"Refactor") || !strings.HasSuffix(d, "…") {
			t.Errorf("width %d: detail line = %q", width, d)
		}
		return lines[3]
	}
	for _, width := range []int{50, 60, 80, 100} {
		got := row(long, width)
		// Truncate drops a space before its "…", so a line may end a column short.
		if !strings.HasSuffix(got, "…  "+meta) || Width(got) < width-2 {
			t.Errorf("width %d: %q (%d columns) must end in the whole meta after a cut title", width, got, Width(got))
		}
	}
	if got := row("Short", 50); got != "  Short  "+meta {
		t.Errorf("short title: %q", got)
	}
	// 39 columns: the title keeps minTitle columns, the meta is cut.
	if got := row(long, 40); got != "  "+Truncate(long, minTitle)+"  julienning3 · 3d ago ·…" {
		t.Errorf("narrow: %q", got)
	}
	// 19 columns (the minimum): no useful room for the meta at all.
	if got := row(long, 20); got != "  "+Truncate(long, 17) {
		t.Errorf("narrowest: %q", got)
	}
}

// Emoji, ZWJ sequences, keycaps and CJK never push a line past width-1,
// with or without colour, whatever the terminal width.
func TestViewNeverExceedsWidthWithWideText(t *testing.T) {
	items := []Item{
		{Title: "New session", Meta: "in julienning3", Pinned: true},
		{Title: strings.Repeat("⚡✅⭐🫠❤️", 30), Meta: "sixtynine · 2h ago · 0b7f6c1e", Detail: strings.Repeat("日本語のプロンプト 🫠 ", 20)},
		{Title: "e\u0301mojis 👩\u200d💻 at work", Meta: "julienning2 · 5m ago · 1c8a7d2f", Note: "(open in another terminal)", Detail: "#️⃣ ☺️ " + strings.Repeat("x", 300), Disabled: true},
		{Title: "한국어 제목 " + strings.Repeat("🎉", 50), Meta: "julienning3 · 3d ago · 日本語ab", Note: "(may be open)", Muted: true, Detail: "ｆｕｌｌ" + strings.Repeat("🫠", 80)},
	}
	for _, noColor := range []bool{false, true} {
		for _, filter := range []string{"", "🫠"} {
			m := press(New(items, strings.Repeat("Now using 🫠 ", 20)), down)
			m = press(m, runes(filter)...)
			m.NoColor = noColor
			for width := 20; width <= 120; width++ {
				for _, line := range strings.Split(m.View(width, 30), "\n") {
					plain := sgr.ReplaceAllString(line, "")
					if noColor && plain != line {
						t.Fatalf("NO_COLOR line has SGR: %q", line)
					}
					if w := Width(plain); w > width-1 {
						t.Errorf("width %d, color %v, filter %q: %d columns: %q", width, !noColor, filter, w, plain)
					}
				}
			}
		}
	}
}

// manyItems is a picker for n sessions: every third has a prompt line, and
// rows 5, 15, 25, … are open elsewhere (disabled).
func manyItems(n int) []Item {
	items := []Item{{Title: "New session", Meta: "in julienning3", Pinned: true}}
	for i := 1; i <= n; i++ {
		it := Item{
			Title:  fmt.Sprintf("Session %03d", i),
			Meta:   fmt.Sprintf("julienning3 · ~/Code/p%d · 3d ago · %08x", i%7, i),
			Search: fmt.Sprintf("%08x", i),
		}
		if i%3 == 0 {
			it.Detail = fmt.Sprintf("prompt %03d", i)
		}
		if i%10 == 5 {
			it.Disabled, it.Note = true, "(open in another terminal)"
		}
		items = append(items, it)
	}
	return items
}

// assertCursorShown checks that the selected row is drawn where CursorLine
// says, and that the frame fits the terminal.
func assertCursorShown(t *testing.T, m Model, w, h int) []string {
	t.Helper()
	lines := strings.Split(m.View(w, h), "\n")
	if len(lines) > h {
		t.Fatalf("%d lines for height %d", len(lines), h)
	}
	i, ok := m.Selected()
	if !ok {
		t.Fatalf("nothing selectable under cursor %d", m.Cursor)
	}
	at := m.CursorLine(w, h)
	if at < 0 || at >= len(lines) || !strings.HasPrefix(lines[at], "> "+m.Items[i].Title) {
		t.Fatalf("selected %q not drawn (CursorLine %d):\n%s", m.Items[i].Title, at, strings.Join(lines, "\n"))
	}
	return lines
}

// PgDn/PgUp move a screenful at a time, the previous cursor row stays in
// view, disabled rows are skipped, and the cursor is always on screen.
func TestManyRowsPaging(t *testing.T) {
	const w, h = 80, 24
	pgDn, pgUp := Key{Kind: KeyPageDown}, Key{Kind: KeyPageUp}
	m := New(manyItems(150), "Now using alpha")
	m.NoColor = true
	m = m.Layout(h)
	lines := assertCursorShown(t, m, w, h)
	if help := lines[len(lines)-1]; !strings.HasPrefix(help, "↑/↓ PgUp/PgDn move") {
		t.Errorf("help must mention paging when rows do not fit: %q", help)
	}

	presses := 0
	for {
		prev := selected(t, m)
		m = m.Update(pgDn).Layout(h)
		cur := selected(t, m)
		if cur == prev {
			break
		}
		presses++
		if cur-prev < 10 && cur != 150 { // the last page may be short
			t.Errorf("PgDn moved %d → %d: less than a screenful", prev, cur)
		}
		lines := assertCursorShown(t, m, w, h)
		if !strings.Contains(strings.Join(lines, "\n"), m.Items[prev].Title) {
			t.Errorf("PgDn %d → %d scrolled the previous row out of view", prev, cur)
		}
		if m.Items[cur].Disabled {
			t.Fatalf("landed on disabled row %d", cur)
		}
	}
	if selected(t, m) != 150 || presses < 5 || presses > 15 {
		t.Fatalf("PgDn reached %d after %d presses", selected(t, m), presses)
	}
	for presses = 0; selected(t, m) != 0 && presses < 20; presses++ {
		m = m.Update(pgUp).Layout(h)
		assertCursorShown(t, m, w, h)
	}
	if selected(t, m) != 0 {
		t.Fatalf("PgUp stuck at %d", selected(t, m))
	}

	// End shows the last row at the bottom with no blank lines; Home goes back.
	m = m.Update(Key{Kind: KeyEnd}).Layout(h)
	lines = assertCursorShown(t, m, w, h)
	if !strings.HasPrefix(lines[len(lines)-3], "> Session 150") || lines[len(lines)-2] != "    prompt 150" || len(lines) < h-1 {
		t.Errorf("End:\n%s", strings.Join(lines, "\n"))
	}
	m = m.Update(Key{Kind: KeyHome}).Layout(h)
	if lines = assertCursorShown(t, m, w, h); lines[2] != "> New session  in julienning3" {
		t.Errorf("Home:\n%s", strings.Join(lines, "\n"))
	}
	// Arrow keys one row at a time all the way down keep the cursor visible.
	for i := 0; i < 160; i++ {
		m = m.Update(down).Layout(h)
		assertCursorShown(t, m, w, h)
	}
}

// A page that would land on a disabled row stops at the nearest selectable
// row on the same screen, and only crosses a screenful of disabled rows.
func TestPageSkipsDisabledRows(t *testing.T) {
	rows := func(disabled func(i int) bool) []Item {
		var items []Item
		for i := 0; i < 12; i++ {
			items = append(items, Item{Title: fmt.Sprintf("row %02d", i), Disabled: disabled(i)})
		}
		return items
	}
	h := chromeLines + 5 // five one-line rows per screen
	m := New(rows(func(i int) bool { return i == 4 || i >= 9 }), "h").Layout(h)
	for _, step := range []struct {
		key  KeyKind
		want int
	}{
		{KeyPageDown, 3}, // row 4 is disabled
		{KeyPageDown, 7},
		{KeyPageDown, 8}, // rows 9-11 are disabled
		{KeyPageDown, 8},
		{KeyPageUp, 5}, // row 4 is disabled
		{KeyPageUp, 1},
		{KeyPageUp, 0},
	} {
		prev := selected(t, m)
		m = m.Update(Key{Kind: step.key}).Layout(h)
		if got := selected(t, m); got != step.want {
			t.Fatalf("key %v from %d → %d, want %d", step.key, prev, got, step.want)
		}
	}
	m = New(rows(func(i int) bool { return i >= 1 && i <= 6 }), "h").Layout(h)
	if m = m.Update(Key{Kind: KeyPageDown}).Layout(h); selected(t, m) != 7 {
		t.Fatalf("PgDn across a screenful of disabled rows → %d, want 7", selected(t, m))
	}
}

// A terminal that grows (or shrinks) re-flows the window around the cursor.
func TestLayoutFollowsResize(t *testing.T) {
	m := New(manyItems(30), "h")
	m.NoColor = true
	m = m.Update(Key{Kind: KeyEnd}).Layout(12)
	assertCursorShown(t, m, 80, 12)
	m = m.Layout(60) // everything fits now: no rows hidden above blank space
	lines := assertCursorShown(t, m, 80, 60)
	if lines[2] != "  New session  in julienning3" || !strings.HasPrefix(lines[len(lines)-1], "↑/↓ move") {
		t.Errorf("grown terminal:\n%s", strings.Join(lines, "\n"))
	}
	m = m.Layout(8)
	assertCursorShown(t, m, 80, 8)
}

// The scope line says what is listed; a typed filter shows how many match.
func TestScopeAndFilterCount(t *testing.T) {
	m := New(fixture(), "Now using alpha")
	m.Scope = "3 sessions in ~/Code/shop"
	if got := strings.Split(m.View(80, 20), "\n")[1]; got != "\x1b[2m3 sessions in ~/Code/shop · type to filter\x1b[0m" {
		t.Errorf("scope line = %q", got)
	}
	m.NoColor = true
	m = press(m, runes("/jenk")...)
	if got := strings.Split(m.View(80, 20), "\n")[1]; got != "Filter: jenk · 1 of 3" {
		t.Errorf("filter line = %q", got)
	}
	// Too narrow for both: the typed text wins.
	if got := strings.Split(m.View(20, 20), "\n")[1]; got != "Filter: jenk" {
		t.Errorf("narrow filter line = %q", got)
	}

	// 150 rows: an id narrows to one row, Enter picks it.
	m = press(New(manyItems(150), "h"), runes("00000096")...)
	if got := strings.Split(m.View(80, 24), "\n")[1]; got != "\x1b[2mFilter: \x1b[0m00000096\x1b[2m · 1 of 150\x1b[0m" {
		t.Errorf("filter line = %q", got)
	}
	if m = press(m, enter); !m.Done || selected(t, m) != 150 {
		t.Errorf("Enter after filtering selected %d", selected(t, m))
	}

	var out strings.Builder
	_, err := Fallback(fixture(), Options{Header: "Now using alpha", Scope: "3 sessions in ~/Code/shop", In: strings.NewReader("\n"), Out: &out})
	if err != nil || !strings.HasPrefix(out.String(), "Now using alpha\n3 sessions in ~/Code/shop\n1) New session") {
		t.Errorf("fallback: %v\n%s", err, out.String())
	}
}

// One keystroke on 500 rows: filter, layout and a full frame, no I/O.
func BenchmarkKeystroke500Rows(b *testing.B) {
	m := New(manyItems(500), "Now using alpha").Layout(50)
	m.Scope = "500 most recent sessions"
	keys := runes("session 4")
	for b.Loop() {
		x := m
		for _, k := range keys {
			x = x.Update(k).Layout(50)
			_ = x.View(120, 50)
		}
		x = x.Update(Key{Kind: KeyClearFilter}).Update(Key{Kind: KeyEnd}).Layout(50)
		_ = x.View(120, 50)
	}
}
