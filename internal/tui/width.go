package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// runeWidth approximates the terminal column width of r without a Unicode
// width table dependency: combining marks, variation selectors and
// zero-width characters take 0 columns, East Asian wide/fullwidth
// characters and emoji take 2. When unsure it errs wide: an overestimate
// only truncates a little early, an underestimate wraps the row and breaks
// the picker's layout.
func runeWidth(r rune) int {
	switch {
	case r == 0 || r == '\u200B' || r == '\u200C' || r == '\u200D' || r == '\u2060' || r == '\uFEFF':
		return 0
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF: // variation selectors
		return 0
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115F, // Hangul Jamo
		r >= 0x2600 && r <= 0x27BF, // misc symbols, dingbats (⚡ ✅ ❤)
		r >= 0x2B00 && r <= 0x2BFF, // misc symbols and arrows (⭐)
		r >= 0x2E80 && r <= 0x303E, // CJK radicals .. CJK symbols
		r >= 0x3041 && r <= 0x33FF, // Hiragana .. CJK compatibility
		r >= 0x3400 && r <= 0x4DBF, // CJK ext A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F000 && r <= 0x1FAFF, // emoji, pictographs, flags (🫠)
		r >= 0x20000 && r <= 0x3FFFD: // CJK ext B..
		return 2
	}
	return 1
}

// nextCluster splits off the first character of s together with the
// zero-width runes that follow it (combining marks, variation selectors,
// joiners) and returns its width. A base followed by VS16 (U+FE0F, emoji
// presentation, as in "❤️") is drawn 2 columns wide.
func nextCluster(s string) (n, width int) {
	r, size := utf8.DecodeRuneInString(s)
	width = runeWidth(r)
	n = size
	for n < len(s) {
		next, size := utf8.DecodeRuneInString(s[n:])
		if runeWidth(next) != 0 {
			break
		}
		if next == '\uFE0F' && width == 1 {
			width = 2
		}
		n += size
	}
	return n, width
}

// Width is the display width of s in terminal columns.
func Width(s string) int {
	w := 0
	for i := 0; i < len(s); {
		n, cw := nextCluster(s[i:])
		w += cw
		i += n
	}
	return w
}

// Truncate cuts s to at most max columns, ending with "…" when it cuts. It
// never splits a character from its combining marks or variation selector.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if Width(s) <= max {
		return s
	}
	w := 0
	cut := 0
	for cut < len(s) {
		n, cw := nextCluster(s[cut:])
		if w+cw > max-1 {
			break
		}
		w += cw
		cut += n
	}
	return strings.TrimRight(s[:cut], " \u200D") + "…"
}

// sanitize removes control characters so item text can never move the
// cursor or change colours; callers pass untrusted conversation titles.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == utf8.RuneError }) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "\uFFFD"))
}
