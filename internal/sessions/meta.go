package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// chunkSize bounds each of the head and tail reads. It also caps the length
// of any line we try to parse: a longer line is necessarily cut by a chunk
// boundary.
const chunkSize = 256 << 10

// Stored text caps (runes). The picker truncates further to the terminal.
const (
	maxTitleRunes  = 200
	maxPromptRunes = 400
)

// meta is what one head/tail pass extracts from a session file.
type meta struct {
	cwd         string
	title       string
	firstPrompt string
	lastActive  time.Time // zero when no timestamp was found
}

// entry is the subset of a jsonl line we look at. Unknown fields are ignored;
// a line whose known fields have unexpected types fails to decode and is
// skipped like any other malformed line.
type entry struct {
	Type             string          `json:"type"`
	Timestamp        string          `json:"timestamp"`
	Cwd              string          `json:"cwd"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Origin           json.RawMessage `json:"origin"`
	Message          *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	Summary     string `json:"summary"`
	LastPrompt  string `json:"lastPrompt"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// readMeta extracts listing metadata from projects/<enc>/<id>.jsonl without
// reading more than two chunks of it.
func readMeta(path string) (meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return meta{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return meta{}, err
	}
	size := fi.Size()

	head := make([]byte, min(size, chunkSize))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return meta{}, err
	}
	head = head[:n]
	headIsWhole := int64(n) >= size

	var tail []byte
	if headIsWhole {
		tail = head
	} else {
		// Start one byte early so we can tell whether the first line in the
		// window is complete (preceded by '\n') or cut.
		off := max(size-chunkSize-1, 0)
		buf := make([]byte, size-off)
		m, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return meta{}, err
		}
		buf = buf[:m]
		if off > 0 {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil
			}
		}
		tail = buf
	}

	var m meta
	var headTitles, tailTitles titles
	var lastPrompt string

	// Head: first prompt, cwd, titles written near the start (legacy summary).
	lines := bytes.Split(head, []byte{'\n'})
	var cut []byte
	if !headIsWhole && len(lines) > 0 {
		// The last piece did not end with '\n' inside the window: it is a
		// line cut by the chunk boundary.
		cut = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		e, ok := decode(line)
		if !ok {
			continue
		}
		if m.cwd == "" && e.Cwd != "" {
			m.cwd = e.Cwd
		}
		headTitles.observe(e)
		if m.firstPrompt == "" {
			m.firstPrompt = promptOf(e)
		}
	}
	if m.firstPrompt == "" && len(cut) > 0 {
		// A first prompt with a pasted screenshot easily exceeds the head
		// window; salvage its text from the part we have.
		m.firstPrompt = promptFromPartial(cut)
	}

	// Tail: most recent activity and the latest titles.
	for _, line := range bytes.Split(tail, []byte{'\n'}) {
		e, ok := decode(line)
		if !ok {
			continue
		}
		if e.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
				m.lastActive = t
			}
		}
		if m.cwd == "" && e.Cwd != "" {
			m.cwd = e.Cwd
		}
		tailTitles.observe(e)
		if e.Type == "last-prompt" && e.LastPrompt != "" {
			lastPrompt = e.LastPrompt
		}
	}

	title := readTitleFile(path)
	if title == "" {
		title = pickTitle(tailTitles, headTitles)
	}
	m.title = cleanText(title, maxTitleRunes)
	if m.firstPrompt == "" && lastPrompt != "" && !strings.HasPrefix(strings.TrimSpace(lastPrompt), "<") {
		// Fallback beyond the spec: when the head holds no usable prompt
		// (e.g. it was cut mid-line), Claude Code's own last-prompt record
		// still identifies the conversation better than skipping it.
		m.firstPrompt = lastPrompt
	}
	m.firstPrompt = cleanText(m.firstPrompt, maxPromptRunes)
	return m, nil
}

// titles collects the latest title-like records seen in one window.
type titles struct{ custom, ai, summary string }

func (t *titles) observe(e entry) {
	switch e.Type {
	case "custom-title":
		if e.CustomTitle != "" {
			t.custom = e.CustomTitle
		}
	case "ai-title":
		if e.AITitle != "" {
			t.ai = e.AITitle
		}
	case "summary":
		if e.Summary != "" {
			t.summary = e.Summary
		}
	}
}

// pickTitle applies the spec's precedence (custom → ai → legacy summary),
// preferring the tail's record within each kind because it is newer.
func pickTitle(tail, head titles) string {
	for _, s := range []string{tail.custom, head.custom, tail.ai, head.ai, tail.summary, head.summary} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// readTitleFile reads projects/<enc>/<id>/custom-title.json, the /rename
// title Claude Code keeps next to the transcript. Missing or malformed files
// simply mean "no title here".
func readTitleFile(jsonlPath string) string {
	p := filepath.Join(strings.TrimSuffix(jsonlPath, ".jsonl"), "custom-title.json")
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return ""
	}
	var doc struct {
		CustomTitle string `json:"customTitle"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.CustomTitle
}

func decode(line []byte) (entry, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return entry{}, false
	}
	var e entry
	if json.Unmarshal(line, &e) != nil {
		return entry{}, false
	}
	return e, true
}

// promptOf returns the typed prompt of a user entry per SPEC "first_prompt",
// or "" when e is anything else (tool results, injected peer messages,
// meta/caveat lines, sidechains, slash-command wrappers starting with '<').
func promptOf(e entry) string {
	if e.Type != "user" || e.IsSidechain || e.IsMeta || e.IsCompactSummary || !isNull(e.Origin) || e.Message == nil {
		return ""
	}
	text, ok := contentText(e.Message.Content)
	if !ok {
		return ""
	}
	return usablePrompt(text)
}

func usablePrompt(text string) string {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasPrefix(t, "<") {
		return ""
	}
	return t
}

func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// contentText returns a string content, or the first text block of an array
// content. Any tool_result block makes the whole entry a tool result.
func contentText(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	case '[':
		var blocks []block
		if json.Unmarshal(raw, &blocks) != nil {
			return "", false
		}
		text, found := "", false
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return "", false
			}
			if !found && b.Type == "text" {
				text, found = b.Text, true
			}
		}
		return text, found
	}
	return "", false
}

// promptFromPartial salvages a typed prompt from a user line cut by the head
// window. It walks the JSON tokens as far as the bytes allow; fields that sit
// after the cut are unknown and treated as absent.
func promptFromPartial(line []byte) string {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(line)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	var (
		typ, role, text       string
		reject, haveText      bool
		sidechain, isMeta, cs bool
	)
loop:
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := tok.(string)
		switch key {
		case "type":
			if dec.Decode(&typ) != nil {
				break loop
			}
		case "isSidechain":
			if dec.Decode(&sidechain) != nil {
				break loop
			}
		case "isMeta":
			if dec.Decode(&isMeta) != nil {
				break loop
			}
		case "isCompactSummary":
			if dec.Decode(&cs) != nil {
				break loop
			}
		case "origin":
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				break loop
			}
			if !isNull(raw) {
				reject = true
			}
		case "message":
			role, text, haveText, reject = partialMessage(dec, reject)
			if haveText {
				// Anything after the message may be cut; stop here.
				break loop
			}
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				break loop
			}
		}
	}
	if reject || sidechain || isMeta || cs || !haveText {
		return ""
	}
	if typ != "user" && !(typ == "" && role == "user") {
		return ""
	}
	return usablePrompt(text)
}

// partialMessage walks a "message" object value token by token.
func partialMessage(dec *json.Decoder, reject bool) (role, text string, haveText, rejected bool) {
	rejected = reject
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch key, _ := tok.(string); key {
		case "role":
			if dec.Decode(&role) != nil {
				return
			}
		case "content":
			tok, err := dec.Token()
			if err != nil {
				return
			}
			switch v := tok.(type) {
			case string:
				return role, v, true, rejected
			case json.Delim:
				if v != '[' {
					return
				}
				for dec.More() {
					var b block
					if dec.Decode(&b) != nil {
						// e.g. an image block whose base64 runs past the cut
						return role, text, haveText, rejected
					}
					if b.Type == "tool_result" {
						return role, "", false, true
					}
					if !haveText && b.Type == "text" {
						text, haveText = b.Text, true
					}
				}
				return role, text, haveText, rejected
			}
			return
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return
			}
		}
	}
	return
}

// fmtErr wraps a per-file problem without echoing file contents.
func fmtErr(op, path string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("%s %s: %w", op, path, err)
}
