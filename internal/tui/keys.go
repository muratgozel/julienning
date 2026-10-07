package tui

import "unicode/utf8"

// KeyKind is a decoded key press.
type KeyKind int

// Keys the picker understands. Everything else decodes to KeyNone.
const (
	KeyNone KeyKind = iota
	KeyRune
	KeyUp
	KeyDown
	KeyPageUp
	KeyPageDown
	KeyHome
	KeyEnd
	KeyEnter
	KeyBackspace
	KeyClearFilter // Ctrl-U
	KeyEsc
	KeyInterrupt // Ctrl-C / Ctrl-D
)

// Key is one key press; Rune is set for KeyRune.
type Key struct {
	Kind KeyKind
	Rune rune
}

// DecodeKeys turns raw terminal input into keys. rest holds a trailing
// incomplete sequence to prepend to the next read: a UTF-8 prefix, or an
// escape sequence that may still be arriving (a lone ESC, "ESC [", "ESC [1;").
//
// ESC followed by '[' or 'O' is a CSI/SS3 sequence (arrows, Home/End,
// PgUp/PgDn); ESC followed by anything else is the Esc key. A trailing lone
// ESC is ambiguous — the Esc key, or an arrow key split across reads (ssh,
// tmux, slow links) — so it is returned in rest: callers wait briefly for
// more input and then hand it to FlushKeys (see run).
func DecodeKeys(buf []byte) (keys []Key, rest []byte) {
	for i := 0; i < len(buf); {
		b := buf[i]
		switch {
		case b == 0x1b:
			if PendingEscape(buf[i:]) {
				return keys, append([]byte(nil), buf[i:]...)
			}
			if buf[i+1] != '[' && buf[i+1] != 'O' {
				keys = append(keys, Key{Kind: KeyEsc})
				i++
				continue
			}
			k, n := decodeSeq(buf[i+2:])
			keys = append(keys, k)
			i += 2 + n
		case b == '\r' || b == '\n':
			keys = append(keys, Key{Kind: KeyEnter})
			i++
		case b == 0x7f || b == 0x08:
			keys = append(keys, Key{Kind: KeyBackspace})
			i++
		case b == 0x03 || b == 0x04:
			keys = append(keys, Key{Kind: KeyInterrupt})
			i++
		case b == 0x0e: // Ctrl-N
			keys = append(keys, Key{Kind: KeyDown})
			i++
		case b == 0x10: // Ctrl-P
			keys = append(keys, Key{Kind: KeyUp})
			i++
		case b == 0x15:
			keys = append(keys, Key{Kind: KeyClearFilter})
			i++
		case b < 0x20:
			keys = append(keys, Key{Kind: KeyNone})
			i++
		default:
			if !utf8.FullRune(buf[i:]) {
				return keys, append([]byte(nil), buf[i:]...)
			}
			r, n := utf8.DecodeRune(buf[i:])
			if r == utf8.RuneError {
				keys = append(keys, Key{Kind: KeyNone})
			} else {
				keys = append(keys, Key{Kind: KeyRune, Rune: r})
			}
			i += n
		}
	}
	return keys, nil
}

// maxPendingEscape bounds how long an unfinished escape sequence may grow
// while waiting for its final byte; anything longer is decoded (as KeyNone)
// instead of buffered, so garbage input cannot grow the buffer forever.
const maxPendingEscape = 16

// PendingEscape reports whether b is exactly an unfinished escape sequence:
// a lone ESC, or "ESC [" / "ESC O" followed only by parameter bytes.
func PendingEscape(b []byte) bool {
	if len(b) == 0 || b[0] != 0x1b || len(b) > maxPendingEscape {
		return false
	}
	if len(b) == 1 {
		return true
	}
	if b[1] != '[' && b[1] != 'O' {
		return false
	}
	for _, c := range b[2:] {
		if !(c >= '0' && c <= '9' || c == ';') {
			return false
		}
	}
	return true
}

// FlushKeys decodes input that no further bytes will complete (the wait
// for the rest of a sequence timed out, or input ended): a lone ESC is the
// Esc key, an unfinished CSI/SS3 sequence is dropped, and a cut-off UTF-8
// sequence is ignored.
func FlushKeys(buf []byte) []Key {
	keys, rest := DecodeKeys(buf)
	// rest is either an unfinished escape (starts with ESC) or a UTF-8
	// prefix (never contains ESC); only a lone ESC means something.
	if len(rest) == 1 && rest[0] == 0x1b {
		keys = append(keys, Key{Kind: KeyEsc})
	}
	return keys
}

// decodeSeq decodes the part of a CSI/SS3 sequence after "ESC [" or "ESC O"
// and returns how many bytes it used.
func decodeSeq(b []byte) (Key, int) {
	n := 0
	param := 0
	for n < len(b) && (b[n] >= '0' && b[n] <= '9' || b[n] == ';') {
		if b[n] != ';' && param < 1000 {
			param = param*10 + int(b[n]-'0')
		}
		n++
	}
	if n >= len(b) {
		return Key{Kind: KeyNone}, n
	}
	final := b[n]
	n++
	switch final {
	case 'A':
		return Key{Kind: KeyUp}, n
	case 'B':
		return Key{Kind: KeyDown}, n
	case 'H':
		return Key{Kind: KeyHome}, n
	case 'F':
		return Key{Kind: KeyEnd}, n
	case '~':
		switch param {
		case 1, 7:
			return Key{Kind: KeyHome}, n
		case 4, 8:
			return Key{Kind: KeyEnd}, n
		case 5:
			return Key{Kind: KeyPageUp}, n
		case 6:
			return Key{Kind: KeyPageDown}, n
		}
	}
	return Key{Kind: KeyNone}, n
}
