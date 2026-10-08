package claudecfg

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
)

func TestMissingEntries(t *testing.T) {
	isolate(t)
	all := []string{"statusLine", "SessionStart", "SessionEnd", "StopFailure"}
	cases := map[string]struct {
		body string // "" leaves the file missing
		want []string
	}{
		"no settings.json":   {"", all},
		"empty object":       {"{}", all},
		"user statusLine":    {`{"statusLine":{"type":"command","command":"mine.sh"}}`, all},
		"old layout":         {oldLayout, []string{"StopFailure"}},
		"fresh patch":        {patchedFresh, nil},
		"hooks not object":   {`{"statusLine":{"type":"command","command":"/a/julienning statusline"},"hooks":[]}`, all[1:]},
		"event not an array": {`{"hooks":{"StopFailure":{"hooks":[{"type":"command","command":"/a/julienning hook stop-failure"}]}}}`, all},
		// An entry counts for its own event only.
		"wrong event": {`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/a/julienning hook session-end"}]}],` +
			`"SessionEnd":[{"hooks":[{"type":"command","command":"/a/julienning hook session-end"}]}],` +
			`"StopFailure":[{"matcher":"x","hooks":["junk",{"type":"command","command":"/a/julienning hook stop-failure"}]}]}}`,
			[]string{"statusLine", "SessionStart"}},
		"statusLine running a hook": {`{"statusLine":{"type":"command","command":"/a/julienning hook session-start"}}`, all},
		"versions path":             {`{"statusLine":{"type":"command","command":"/u/.local/share/julienning/versions/1.2.0 statusline"}}`, all[1:]},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.body != "" {
				write(t, dir, tc.body)
			}
			got, err := MissingEntries(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("missing = %v, want %v", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"[1]", "{nope", `"x"`} {
		dir := t.TempDir()
		write(t, dir, bad)
		if _, err := MissingEntries(dir); err == nil || !strings.Contains(err.Error(), SettingsPath(dir)) {
			t.Errorf("%s: err = %v, want one naming the file", bad, err)
		}
	}
}

// One line for every dir an upgrade left behind, gone once setup re-patched
// them; dirs setup cannot fix from here (missing dir, broken settings.json)
// are skipped.
func TestSetupWarning(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	mk := func(name, body string) config.ConfigDir {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			write(t, dir, body)
		}
		return config.ConfigDir{Name: name, Dir: dir}
	}
	patched := mk("julienning1", "{}")
	if _, err := Patch(patched.Dir, exe); err != nil {
		t.Fatal(err)
	}
	old3 := mk("julienning3", oldLayout)
	old4 := mk("julienning4", oldLayout)
	broken := mk("broken", "{half")
	gone := config.ConfigDir{Name: "gone", Dir: filepath.Join(root, "gone")}
	dirs := []config.ConfigDir{patched, broken, old3, gone, old4}

	want := "this julienning adds a rate-limit hook that settings.json of julienning3 and julienning4 does not have yet; run: julienning setup"
	if got := SetupWarning(dirs); got != want {
		t.Errorf("warning = %q\nwant      %q", got, want)
	}
	if got := SetupWarning(dirs[:3]); got != strings.Replace(want, "julienning3 and julienning4", "julienning3", 1) {
		t.Errorf("one dir: %q", got)
	}

	// Anything beyond the new hook is named generically.
	bare := mk("julienning5", "")
	got := SetupWarning(append(dirs, bare))
	want = "settings.json of julienning3, julienning4 and julienning5 lacks entries this julienning adds " +
		"(statusLine, SessionStart hook, SessionEnd hook, StopFailure hook); run: julienning setup"
	if got != want {
		t.Errorf("warning = %q\nwant      %q", got, want)
	}

	for _, cd := range []config.ConfigDir{old3, old4, bare} {
		if _, err := Patch(cd.Dir, exe); err != nil {
			t.Fatal(err)
		}
	}
	if got := SetupWarning(append(dirs, bare)); got != "" {
		t.Errorf("after setup: %q", got)
	}
	if got := SetupWarning(nil); got != "" {
		t.Errorf("no dirs: %q", got)
	}
}
