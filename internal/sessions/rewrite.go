package sessions

import (
	"bytes"
	"fmt"
	"sort"
	"unicode/utf8"
)

// Pair maps one absolute path to another in a path rewrite.
type Pair struct{ From, To string }

// Rewriter replaces paths in the files of a session moved between machines
// (SPEC "Path rewrite"), so the conversation reads as if it ran on the
// destination: the cwd fields, prompts, tool input and output, and the
// file-history snapshots' realParentDir (rewind restores into it; the
// checkpoint files themselves keep their names, which hash the source path,
// and are found by the name the transcript records).
type Rewriter struct {
	json, text []bytePair
}

type bytePair struct{ from, to []byte }

// NewRewriter keeps the pairs whose sides differ, longest source first (a
// match at one position takes the longest pair that fits there).
func NewRewriter(pairs []Pair) *Rewriter {
	seen := map[string]bool{}
	var kept []Pair
	for _, p := range pairs {
		if p.From == "" || p.To == "" || p.From == p.To || seen[p.From] {
			continue
		}
		seen[p.From] = true
		kept = append(kept, p)
	}
	sort.SliceStable(kept, func(i, j int) bool { return len(kept[i].From) > len(kept[j].From) })
	r := &Rewriter{}
	for _, p := range kept {
		r.json = append(r.json, bytePair{[]byte(jsonEscape(p.From)), []byte(jsonEscape(p.To))})
		r.text = append(r.text, bytePair{[]byte(p.From), []byte(p.To)})
	}
	return r
}

// JSON rewrites a .json or .jsonl file, matching both sides as they appear
// inside JSON strings. Lines that are not valid JSON are rewritten the same
// way, never dropped. It returns the new content and the replacements made.
func (r *Rewriter) JSON(data []byte) ([]byte, int) { return replacePaths(data, r.json) }

// Text rewrites a plain text file (project memory).
func (r *Rewriter) Text(data []byte) ([]byte, int) { return replacePaths(data, r.text) }

// pathByte is a byte that can continue a path component: a match must not
// have one on either side, so /home/a/shop never matches inside
// /home/a/shop2 or /mnt/home/a/shop.
func pathByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
}

// replacePaths makes one left-to-right pass: at the leftmost match, the
// longest pair whose boundaries hold wins; replaced text is never looked at
// again.
func replacePaths(data []byte, pairs []bytePair) ([]byte, int) {
	if len(pairs) == 0 {
		return data, 0
	}
	next := make([]int, len(pairs))
	for i, p := range pairs {
		next[i] = indexFrom(data, p.from, 0)
	}
	var out bytes.Buffer
	pos, n := 0, 0
	for {
		best := -1
		for i, p := range pairs {
			if next[i] >= 0 && next[i] < pos {
				next[i] = indexFrom(data, p.from, pos)
			}
			if next[i] < 0 {
				continue
			}
			if best < 0 || next[i] < next[best] || (next[i] == next[best] && len(p.from) > len(pairs[best].from)) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		at := next[best]
		end := at + len(pairs[best].from)
		if (at > 0 && pathByte(data[at-1])) || (end < len(data) && pathByte(data[end])) {
			next[best] = indexFrom(data, pairs[best].from, at+1)
			continue
		}
		if n == 0 {
			out.Grow(len(data))
		}
		out.Write(data[pos:at])
		out.Write(pairs[best].to)
		pos = end
		n++
	}
	if n == 0 {
		return data, 0
	}
	out.Write(data[pos:])
	return out.Bytes(), n
}

func indexFrom(data, sep []byte, from int) int {
	if from > len(data) {
		return -1
	}
	i := bytes.Index(data[from:], sep)
	if i < 0 {
		return -1
	}
	return from + i
}

// jsonEscape is s as JSON.stringify writes it inside a string, without the
// quotes: `"` and `\` escaped, control characters as \b \f \n \r \t or
// \u00XX, everything else (including < > & and U+2028) as is. Go's encoder
// escapes more than that, so it cannot be used to find Claude's text.
func jsonEscape(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
