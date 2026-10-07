package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncodeCwd(t *testing.T) {
	cases := map[string]string{
		projCwd:                      projEnc,
		"/tmp/a b/c.d_e":             "-tmp-a-b-c-d-e",
		"/Users/jürgen/x":            "-Users-j-rgen-x",
		"/p/🍕":                       "-p---", // astral rune = two UTF-16 units
		"/p/" + string([]byte{0xff}): "-p--",
	}
	for in, want := range cases {
		if got := EncodeCwd(in); got != want {
			t.Errorf("EncodeCwd(%q) = %q, want %q", in, got, want)
		}
	}
}

func metaOf(t *testing.T, lines ...string) meta {
	t.Helper()
	p := writeSession(t, t.TempDir(), projEnc, sid1, time.Time{}, lines...)
	m, err := readMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFirstPromptRules(t *testing.T) {
	m := metaOf(t,
		mustJSON(t, map[string]any{"type": "permission-mode", "permissionMode": "default", "sessionId": sid1}),
		mustJSON(t, map[string]any{"type": "file-history-snapshot", "messageId": "m", "snapshot": map[string]any{}}),
		userLine(t, "sidechain prompt", 1*time.Second, map[string]any{"isSidechain": true}),
		userLine(t, "Caveat: meta", 2*time.Second, map[string]any{"isMeta": true}),
		userLine(t, "peer message", 3*time.Second, map[string]any{"origin": map[string]any{"kind": "peer", "from": "x"}}),
		userLine(t, "<command-name>/clear</command-name>", 4*time.Second, nil),
		userLine(t, []any{map[string]any{"type": "tool_result", "tool_use_id": "t", "content": "ok"}}, 5*time.Second, nil),
		userLine(t, "continued summary", 6*time.Second, map[string]any{"isCompactSummary": true}),
		userLine(t, []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "AAAA"}},
			map[string]any{"type": "text", "text": "  Fix the\n\tcheckout   flow  "},
		}, 7*time.Second, nil),
		userLine(t, "second prompt", 8*time.Second, nil),
		assistantLine(t, "ok", 9*time.Second),
	)
	if m.firstPrompt != "Fix the checkout flow" {
		t.Errorf("firstPrompt = %q", m.firstPrompt)
	}
	if m.cwd != projCwd {
		t.Errorf("cwd = %q", m.cwd)
	}
	if want := baseTime.Add(9 * time.Second); !m.lastActive.Equal(want) {
		t.Errorf("lastActive = %v, want %v", m.lastActive, want)
	}
	if m.title != "" {
		t.Errorf("title = %q", m.title)
	}
}

func TestTitlePrecedence(t *testing.T) {
	prompt := userLine(t, "hello", 0, nil)
	cases := []struct {
		name  string
		lines []string
		file  string
		want  string
	}{
		{"summary only", []string{metaLine(t, "summary", "summary", "Legacy summary"), prompt}, "", "Legacy summary"},
		{"ai beats summary", []string{metaLine(t, "summary", "summary", "Legacy"), prompt, metaLine(t, "ai-title", "aiTitle", "AI title")}, "", "AI title"},
		{"last ai wins", []string{prompt, metaLine(t, "ai-title", "aiTitle", "old"), metaLine(t, "ai-title", "aiTitle", "new")}, "", "new"},
		{"custom beats ai", []string{prompt, metaLine(t, "custom-title", "customTitle", "julienning"), metaLine(t, "ai-title", "aiTitle", "AI")}, "", "julienning"},
		{"file beats entries", []string{prompt, metaLine(t, "custom-title", "customTitle", "entry")}, `{"customTitle":"from file"}`, "from file"},
		{"malformed file ignored", []string{prompt, metaLine(t, "ai-title", "aiTitle", "AI")}, `{nope`, "AI"},
		{"escape sequences neutralised", []string{prompt, metaLine(t, "ai-title", "aiTitle", "red\x1b[31m title\u202E")}, "", "red [31m title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := writeSession(t, dir, projEnc, sid1, time.Time{}, tc.lines...)
			if tc.file != "" {
				writeFile(t, filepath.Join(dir, "projects", projEnc, sid1, "custom-title.json"), tc.file, 0o600, time.Time{})
			}
			m, err := readMeta(p)
			if err != nil {
				t.Fatal(err)
			}
			if m.title != tc.want {
				t.Errorf("title = %q, want %q", m.title, tc.want)
			}
		})
	}
}

// A large file is only read at both ends: metadata in the middle is not
// seen, the tail's title and timestamp are, and a half-written last line is
// ignored.
func TestLargeFileHeadAndTail(t *testing.T) {
	var lines []string
	lines = append(lines, userLine(t, "first question", 0, nil))
	lines = append(lines, metaLine(t, "ai-title", "aiTitle", "early title"))
	filler := strings.Repeat("x", 4000)
	for i := 0; i < 300; i++ { // ~1.2 MB
		lines = append(lines, assistantLine(t, filler, time.Duration(i+1)*time.Second))
	}
	lines = append(lines, metaLine(t, "custom-title", "customTitle", "middle title"))
	for i := 0; i < 100; i++ {
		lines = append(lines, assistantLine(t, filler, time.Duration(400+i)*time.Second))
	}
	lines = append(lines, metaLine(t, "ai-title", "aiTitle", "late title"))
	lines = append(lines, assistantLine(t, "last", 600*time.Second))
	dir := t.TempDir()
	p := writeSession(t, dir, projEnc, sid1, time.Time{}, lines...)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cut := assistantLine(t, "being written", 700*time.Second)
	if _, err := f.WriteString(cut[:len(cut)/2]); err != nil {
		t.Fatal(err)
	}
	f.Close()

	m, err := readMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.firstPrompt != "first question" {
		t.Errorf("firstPrompt = %q", m.firstPrompt)
	}
	if m.title != "late title" {
		t.Errorf("title = %q (the middle custom title is out of reach by design)", m.title)
	}
	if want := baseTime.Add(600 * time.Second); !m.lastActive.Equal(want) {
		t.Errorf("lastActive = %v, want %v", m.lastActive, want)
	}
}

// A first prompt with a pasted screenshot is longer than the head window:
// its text block is still recovered from the cut line.
func TestFirstPromptFromCutLine(t *testing.T) {
	big := strings.Repeat("A", chunkSize+1000)
	// Claude Code's own key order (type before message, role before
	// content); Go's map encoding would sort them alphabetically.
	m := metaOf(t,
		`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"`+projCwd+`","sessionId":"`+sid1+
			`","version":"2.1.283","gitBranch":"main","type":"user","message":{"role":"user","content":[`+
			`{"type":"text","text":"Why does this screen look broken?"},`+
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+big+`"}}]},`+
			`"uuid":"u1","timestamp":"`+tsAt(0)+`","origin":null}`,
		assistantLine(t, "because", time.Second),
	)
	if m.firstPrompt != "Why does this screen look broken?" {
		t.Errorf("firstPrompt = %q", m.firstPrompt)
	}

	// Same shape but injected by a peer: the origin comes after the message,
	// beyond the cut, so we cannot see it... but when it is before, reject.
	m = metaOf(t,
		`{"type":"user","origin":{"kind":"peer"},"message":{"role":"user","content":[{"type":"text","text":"peer says"},{"type":"image","source":{"data":"`+big+`"}}]}}`,
	)
	if m.firstPrompt != "" {
		t.Errorf("peer prompt leaked: %q", m.firstPrompt)
	}
}

// When nothing usable is in the head, Claude Code's last-prompt record still
// identifies the session.
func TestLastPromptFallback(t *testing.T) {
	huge := strings.Repeat("y", chunkSize+10)
	m := metaOf(t,
		userLine(t, huge, 0, nil),
		assistantLine(t, "ok", time.Second),
		mustJSON(t, map[string]any{"type": "last-prompt", "lastPrompt": "deploy it", "leafUuid": "l", "sessionId": sid1}),
	)
	if m.firstPrompt != "deploy it" {
		t.Errorf("firstPrompt = %q", m.firstPrompt)
	}
}

func TestMalformedInput(t *testing.T) {
	m := metaOf(t,
		"not json at all",
		`{"type":"user","isMeta":"yes"}`, // wrong type for a known field
		`{"type":"user","message":{"role":"user","content":"caf`+string([]byte{0xe9})+` au lait"},"timestamp":"garbage"}`,
		"",
		`{"type":"assistant","timestamp":"`+tsAt(time.Minute)+`"}`,
		`{"type":"assistant","timest`,
	)
	if m.firstPrompt != "caf\uFFFD au lait" {
		t.Errorf("firstPrompt = %q", m.firstPrompt)
	}
	if !m.lastActive.Equal(baseTime.Add(time.Minute)) {
		t.Errorf("lastActive = %v", m.lastActive)
	}
}

func TestCleanTextCaps(t *testing.T) {
	long := strings.Repeat("word ", 200)
	got := cleanText(long, 20)
	if len([]rune(got)) > 21 || !strings.HasSuffix(got, "…") {
		t.Errorf("cleanText cap = %q", got)
	}
	if got := cleanText("a\x00b\u2028c", 50); got != "a b c" {
		t.Errorf("cleanText controls = %q", got)
	}
}
