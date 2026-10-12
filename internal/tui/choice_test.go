package tui

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func choiceItems() []Item {
	return []Item{
		{Title: "5m", Meta: "alpha, beta", Note: "(default)"},
		{Title: "1h", Meta: "gamma"},
		{Title: "not set (Claude Code's default)", Meta: "delta"},
		{Title: "Don't sync", Meta: "leave every account as it is"},
	}
}

func TestChoiceIgnoresTypingButMovesWithJK(t *testing.T) {
	m := New(choiceItems(), "promptCacheTtl")
	m.Choice = true
	m = press(m, runes("1hxyz")...)
	if m.Filter != "" || len(m.Visible()) != 4 {
		t.Fatalf("typing filtered a choice list: filter %q, %d visible", m.Filter, len(m.Visible()))
	}
	if got := selected(t, m); got != 0 {
		t.Fatalf("letters moved the cursor to %d", got)
	}
	m = press(m, runes("jj")...)
	if got := selected(t, m); got != 2 {
		t.Fatalf("j j selected %d, want 2", got)
	}
	m = press(m, runes("k")...)
	m = press(m, Key{Kind: KeyBackspace}, Key{Kind: KeyClearFilter}, enter)
	if !m.Done || selected(t, m) != 1 {
		t.Fatalf("Enter after k: done %v, selected %d", m.Done, selected(t, m))
	}
}

func TestChoiceViewHasNoFilterHints(t *testing.T) {
	m := New(choiceItems(), "promptCacheTtl")
	m.Choice, m.NoColor = true, true
	m.Scope = "3 shared accounts · Esc cancels the whole sync"
	lines := strings.Split(m.View(80, 20), "\n")
	if lines[1] != "3 shared accounts · Esc cancels the whole sync" {
		t.Fatalf("scope line %q", lines[1])
	}
	if help := lines[len(lines)-1]; help != "↑/↓ move · Enter select · Esc cancel" {
		t.Fatalf("help line %q", help)
	}
	if strings.Contains(m.View(80, 20), "filter") {
		t.Fatalf("choice view mentions filtering:\n%s", m.View(80, 20))
	}
}

func TestChoiceFallbackTakesNumbersOnly(t *testing.T) {
	var out strings.Builder
	got, err := Fallback(choiceItems(), Options{Header: "theme", Choice: true, In: strings.NewReader("vim\n2\n"), Out: &out})
	if err != nil || got != 1 {
		t.Fatalf("got %d, %v", got, err)
	}
	if !strings.Contains(out.String(), "Choose 1-4 (Enter = 1, q = cancel): ") || !strings.Contains(out.String(), "Enter a number from 1 to 4.") {
		t.Fatalf("fallback output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "No sessions match") {
		t.Fatalf("text filtered the choices:\n%s", out.String())
	}
}

func TestConfirmCustomHintAndFallbackPrompt(t *testing.T) {
	m := NewConfirm("Overwrite beta's CLAUDE.md with alpha's?", "detail")
	m.Hint = "Enter or y to overwrite · n or Esc to keep beta's"
	m.NoColor = true
	if v := m.View(100, 10); !strings.HasSuffix(v, "  Enter or y to overwrite · n or Esc to keep beta's") {
		t.Fatalf("view:\n%s", v)
	}
	var out strings.Builder
	yes, err := ConfirmFallback(ConfirmOptions{Question: "q", In: strings.NewReader("n\n"), Out: &out, FallbackPrompt: "Overwrite? [Y/n] "})
	if err != nil || yes || !strings.Contains(out.String(), "Overwrite? [Y/n] ") {
		t.Fatalf("yes %v err %v out %q", yes, err, out.String())
	}
}

// sync asks several fallback questions in a row: answers typed ahead must
// reach the next one.
func TestFallbackLeavesTypedAheadAnswers(t *testing.T) {
	in := strings.NewReader("2\n1\ny\n")
	var out strings.Builder
	first, err := Fallback(choiceItems(), Options{Choice: true, In: in, Out: &out})
	if err != nil || first != 1 {
		t.Fatalf("first = %d, %v", first, err)
	}
	second, err := Fallback(choiceItems(), Options{Choice: true, In: in, Out: &out})
	if err != nil || second != 0 {
		t.Fatalf("second = %d, %v", second, err)
	}
	if yes, err := ConfirmFallback(ConfirmOptions{Question: "q", In: in, Out: &out}); err != nil || !yes {
		t.Fatalf("confirm = %v, %v", yes, err)
	}
}

func TestConfirmInterruptCancels(t *testing.T) {
	size := func() (int, int) { return 80, 24 }
	prev := confirmClock
	confirmClock = func() time.Time { return time.Unix(0, 0) }
	t.Cleanup(func() { confirmClock = prev })
	m := NewConfirm("q", "d")
	m.InterruptCancels = true
	if _, err := confirmLoop(m, strings.NewReader("\x03"), io.Discard, size); !errors.Is(err, ErrCanceled) {
		t.Fatalf("Ctrl-C: %v", err)
	}
	if _, err := confirmLoop(m, strings.NewReader(""), io.Discard, size); !errors.Is(err, ErrCanceled) {
		t.Fatalf("EOF: %v", err)
	}
	if yes, err := confirmLoop(m, strings.NewReader("\x1b"), io.Discard, size); err != nil || yes {
		t.Fatalf("Esc must answer no: %v, %v", yes, err)
	}
	if yes, err := confirmLoop(NewConfirm("q", "d"), strings.NewReader("\x03"), io.Discard, size); err != nil || yes {
		t.Fatalf("without InterruptCancels Ctrl-C is a no: %v, %v", yes, err)
	}
	if _, err := ConfirmFallback(ConfirmOptions{Question: "q", In: strings.NewReader(""), Out: io.Discard, InterruptCancels: true}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("fallback EOF: %v", err)
	}
}
