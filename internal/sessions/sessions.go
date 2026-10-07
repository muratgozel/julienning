// Package sessions lists Claude Code conversations across registered config
// dirs and moves one conversation (with its side files and project memory)
// from one config dir to another, so a teammate can hand a session to a
// fresher account without losing context.
//
// CRITICAL for future agents: everything read here is an undocumented Claude
// Code on-disk format (observed in 2.1.28x). Session files reach 60+ MB and a
// project folder can hold hundreds of them, so listing never reads a whole
// file: it picks candidates by mtime and reads at most a 256 KB head and a
// 256 KB tail of each. Keep it that way.
package sessions

import (
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Session is one conversation file, projects/<ProjectDir>/<ID>.jsonl.
type Session struct {
	ID          string    `json:"id"`
	ConfigName  string    `json:"config"`
	ConfigDir   string    `json:"config_dir"`
	ProjectDir  string    `json:"project_dir"` // encoded folder name under projects/
	Cwd         string    `json:"cwd"`         // from the entries; may be empty
	Title       string    `json:"title"`
	FirstPrompt string    `json:"first_prompt"`
	LastActive  time.Time `json:"last_active"`
	ModTime     time.Time `json:"mod_time"`
	Size        int64     `json:"size"`
	// Live: a running claude process in some registered dir has this session
	// open (livesess registry). LiveIn is that dir's config name.
	Live   bool   `json:"live"`
	LiveIn string `json:"live_in,omitempty"`
	// MaybeOpen: the session's dir has no live registry (older Claude Code)
	// and the file changed in the last RecentWindow, so it may be open.
	MaybeOpen bool `json:"maybe_open,omitempty"`
}

// ShortID is the first 8 characters of the id, as shown in the picker.
func (s Session) ShortID() string {
	if len(s.ID) <= 8 {
		return s.ID
	}
	return s.ID[:8]
}

// Label is the title, else the first prompt, else "session <short id>";
// used for one-line human output.
func (s Session) Label() string {
	switch {
	case s.Title != "":
		return s.Title
	case s.FirstPrompt != "":
		return Truncate(s.FirstPrompt, 60)
	default:
		return "session " + s.ShortID()
	}
}

// RecentWindow is how recently a session file may have changed before a dir
// without a live registry makes us assume it could still be open.
const RecentWindow = 2 * time.Minute

// idRe accepts the file names Claude Code uses for sessions (UUIDs). The
// leading alphanumeric matters: the id ends up as `claude --resume <id>` and
// as a path component, so "-x" or ".." must never get through.
var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidID reports whether id is safe to use as a session id.
func ValidID(id string) bool { return idRe.MatchString(id) && !strings.Contains(id, "..") }

// validProjectDir reports whether name is a single, safe path component.
func validProjectDir(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
}

// maxEncodedLen is where Claude Code starts truncating project folder names
// and appending a hash.
const maxEncodedLen = 200

// EncodeCwd returns the projects/ folder name Claude Code uses for cwd: every
// character outside [A-Za-z0-9] becomes '-'. Claude Code does this on a
// JavaScript string, so a character outside the Basic Multilingual Plane
// (two UTF-16 code units) becomes two dashes; invalid UTF-8 bytes become one
// dash each. Callers must handle the >200-character case (see List).
func EncodeCwd(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for i := 0; i < len(cwd); {
		r, size := utf8.DecodeRuneInString(cwd[i:])
		i += size
		switch {
		case r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// cleanText makes untrusted conversation text safe and compact for a
// terminal: control characters (including ESC, so titles cannot inject
// escape sequences) become spaces, whitespace runs collapse, invalid UTF-8
// becomes U+FFFD, and the result is capped at max runes.
func cleanText(s string, max int) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range strings.ToValidUTF8(s, "\uFFFD") {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || isBidiControl(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		if n >= max {
			return strings.TrimSpace(b.String()) + "…"
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// isBidiControl catches the explicit direction overrides that can make a
// title render differently from its bytes (Trojan Source style).
func isBidiControl(r rune) bool {
	return r >= '\u202A' && r <= '\u202E' || r >= '\u2066' && r <= '\u2069' || r == '\u200E' || r == '\u200F'
}

// Truncate shortens s to at most max runes, ending with "…" when cut.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	rs := []rune(s)
	return strings.TrimSpace(string(rs[:max-1])) + "…"
}
