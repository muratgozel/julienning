package claudecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookGroups counts the matcher groups per hook event.
func hookGroups(t *testing.T, body string) map[string]int {
	t.Helper()
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, body)
	}
	out := map[string]int{}
	for k, v := range doc.Hooks {
		out[k] = len(v)
	}
	return out
}

// recordedStatusLine returns the statusLine patches.json keeps for dir.
func recordedStatusLine(t *testing.T, dir string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("JULIENNING_HOME"), PatchesFile))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Files map[string]struct {
			StatusLine *string `json:"status_line"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	key, err := ResolvedSettingsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := f.Files[key]
	if !ok || rec.StatusLine == nil {
		return "", false
	}
	return *rec.StatusLine, true
}

// A dev build (or a machine without the install symlink) writes the versions
// file path. Re-running must recognise those entries as julienning's: no
// duplicate hook groups, the user's statusLine stays the one recorded, and
// Unpatch restores the original bytes.
func TestPatchVersionsDirPathIsOwned(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	const original = "{\n  \"statusLine\": {\"type\": \"command\", \"command\": \"~/bin/mine.sh\"},\n  \"hooks\": {\n    \"SessionStart\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"echo hi\"}]}\n    ]\n  }\n}\n"
	write(t, dir, original)
	v1 := "/Users/x/.local/share/julienning/versions/0.3.0"
	v2 := "/Users/x/.local/share/julienning/versions/dev"

	res, err := Patch(dir, v1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Added || res.ReplacedStatusLine != "~/bin/mine.sh" {
		t.Fatalf("first patch = %+v", res)
	}
	res, err = Patch(dir, v1)
	if err != nil || res.Action != Unchanged || res.ReplacedStatusLine != "" {
		t.Fatalf("second patch = %+v, %v", res, err)
	}
	res, err = Patch(dir, v2)
	if err != nil || res.Action != Updated || res.ReplacedStatusLine != "" {
		t.Fatalf("patch with another version = %+v, %v", res, err)
	}
	body := readFile(t, filepath.Join(dir, SettingsFile))
	if g := hookGroups(t, body); g["SessionStart"] != 2 || g["SessionEnd"] != 1 {
		t.Fatalf("hook groups = %v, want SessionStart 2 (user + julienning), SessionEnd 1:\n%s", g, body)
	}
	if strings.Contains(body, v1) {
		t.Fatalf("old version path left behind:\n%s", body)
	}
	if got, ok := recordedStatusLine(t, dir); !ok || got != `{"type": "command", "command": "~/bin/mine.sh"}` {
		t.Fatalf("recorded statusLine = %q, %v; want the user's original", got, ok)
	}

	if _, err := Unpatch(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != original {
		t.Fatalf("unpatch did not restore the original:\n%s", got)
	}
}

// Builds before the fix recorded their own versions-path statusLine as the
// user's. Such a record must never be restored.
func TestUnpatchIgnoresRecordedOwnStatusLine(t *testing.T) {
	root := isolate(t)
	dir := t.TempDir()
	vexe := "/Users/x/.local/share/julienning/versions/0.2.0"
	write(t, dir, `{"statusLine":{"type":"command","command":"`+vexe+` statusline"},"model":"opus"}`)
	key, err := ResolvedSettingsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	own, _ := json.Marshal(`{"type":"command","command":"` + vexe + ` statusline"}`)
	keyJSON, _ := json.Marshal(key)
	patches := `{"version":1,"files":{` + string(keyJSON) + `:{"status_line":` + string(own) + `}}}`
	if err := os.MkdirAll(filepath.Join(root, "jl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jl", PatchesFile), []byte(patches), 0o600); err != nil {
		t.Fatal(err)
	}

	// Re-patching does not treat it as the user's either.
	res, err := Patch(dir, exe)
	if err != nil || res.ReplacedStatusLine != "" {
		t.Fatalf("patch = %+v, %v", res, err)
	}
	res, err = Unpatch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Changes, ", ") != "removed statusLine, removed SessionStart hook, removed SessionEnd hook, removed empty hooks" {
		t.Fatalf("changes = %q", res.Changes)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != `{"model":"opus"}` {
		t.Fatalf("got %s", got)
	}
}

func TestIsJulienningCommandVersionFiles(t *testing.T) {
	cases := map[string]bool{
		"/Users/x/.local/share/julienning/versions/0.3.0 statusline":               true,
		"/Users/x/.local/share/julienning/versions/dev hook session-start":         true,
		"'/Users/John Doe/.local/share/julienning/versions/1.0.0-rc.1' statusline": true,
		"/opt/julienning/versions/0.3.0 hook session-end":                          true,
		"/opt/other/versions/0.3.0 statusline":                                     false,
		"/opt/julienning/releases/0.3.0 statusline":                                false,
		"/Users/x/.local/share/julienning/versions/0.3.0 status":                   false,
		"/Users/x/.local/share/julienning/versions/0.3.0":                          false,
	}
	for cmd, want := range cases {
		if got := IsJulienningCommand(cmd); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}

// A copied settings.json loses julienning's entries (restoring the statusLine
// they replaced) so the copy is patched like a fresh file.
func TestSettingsWithoutJulienningRestoresTheUsersFile(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, userSettings)
	if _, err := Patch(dir, exe); err != nil {
		t.Fatal(err)
	}
	patched := readFile(t, filepath.Join(dir, SettingsFile))
	got, err := SettingsWithoutJulienning(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userSettings {
		t.Fatalf("got\n%s\nwant\n%s", got, userSettings)
	}
	if readFile(t, filepath.Join(dir, SettingsFile)) != patched {
		t.Fatal("the source was modified")
	}
}

// Without a patches.json record (an older build, or a file patched on
// another machine) containers holding only julienning entries still go, so
// the copy's Unpatch leaves no empty arrays behind.
func TestSettingsWithoutJulienningDropsEmptiedContainers(t *testing.T) {
	isolate(t)
	src := t.TempDir()
	write(t, src, `{
  "model": "opus",
  "statusLine": {"type": "command", "command": "/a/julienning statusline"},
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "/a/julienning hook session-start"}]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "/a/julienning hook session-end"}]}],
    "Stop": []
  }
}`)
	got, err := SettingsWithoutJulienning(src)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"model\": \"opus\",\n  \"hooks\": {\n    \"Stop\": []\n  }\n}"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	// Only julienning in hooks: the container goes too; the copy round-trips.
	write(t, src, `{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/a/julienning hook session-start"}]}]}}`)
	got, err = SettingsWithoutJulienning(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"model":"opus"}` {
		t.Fatalf("got %s", got)
	}
	dst := t.TempDir()
	if err := WriteSettings(dst, got); err != nil {
		t.Fatal(err)
	}
	if _, err := Patch(dst, exe); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpatch(dst); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dst, SettingsFile)); got != "{\"model\":\"opus\"}\n" {
		t.Fatalf("copy after patch+unpatch = %q", got)
	}
}

func TestSettingsWithoutJulienningNothingToCopy(t *testing.T) {
	isolate(t)
	missing := t.TempDir()
	if got, err := SettingsWithoutJulienning(missing); err != nil || got != nil {
		t.Fatalf("missing file: %q, %v", got, err)
	}
	created := t.TempDir()
	if _, err := Patch(created, exe); err != nil {
		t.Fatal(err)
	}
	if got, err := SettingsWithoutJulienning(created); err != nil || got != nil {
		t.Fatalf("file julienning created: %q, %v", got, err)
	}
	bad := t.TempDir()
	write(t, bad, "[1]")
	if _, err := SettingsWithoutJulienning(bad); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("non-object: %v", err)
	}
}

func TestResolvedSettingsPathFollowsSharedFile(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	c := filepath.Join(root, "c")
	for _, d := range []string{a, b, c} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, a, "{}")
	if err := os.Symlink(filepath.Join(a, SettingsFile), filepath.Join(b, SettingsFile)); err != nil {
		t.Fatal(err)
	}
	ka, err1 := ResolvedSettingsPath(a)
	kb, err2 := ResolvedSettingsPath(b)
	kc, err3 := ResolvedSettingsPath(c) // no file yet
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatal(err1, err2, err3)
	}
	if ka != kb {
		t.Fatalf("shared file: %q != %q", ka, kb)
	}
	if ka == kc || filepath.Base(kc) != SettingsFile {
		t.Fatalf("separate file: %q vs %q", ka, kc)
	}
}
