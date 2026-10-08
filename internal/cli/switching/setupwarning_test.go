package switching

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/claudecfg"
)

// olderPatch is settings.json as a julienning without the StopFailure hook
// left it.
const olderPatch = `{
  "statusLine": {"type": "command", "command": "/usr/local/bin/julienning statusline"},
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "/usr/local/bin/julienning hook session-start"}]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "/usr/local/bin/julienning hook session-end"}]}]
  }
}
`

// next and use warn once when a registered dir lacks an entry this
// julienning adds, and stop once setup has patched it.
func TestNextAndUseWarnWhenSetupIsDue(t *testing.T) {
	const warning = "julienning: warning: this julienning adds a rate-limit hook that settings.json of julienning3 and julienning4 does not have yet; run: julienning setup\n"
	const generic = "julienning: warning: settings.json of julienning4 lacks entries this julienning adds (statusLine, StopFailure hook); run: julienning setup\n"
	for _, cmd := range [][]string{{"next", "--no-launch"}, {"use", "alpha", "--no-launch"}, {"use", "--no-launch"}} {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			f := setup(t)
			a := f.addConfig("julienning3", email1)
			b := f.addConfig("julienning4", email2)
			f.save()
			f.fake.Listing = listing(nacct(email1, "alpha", 12, 28), nacct(email2, "beta", 50, 60))
			for _, cd := range []string{a.Dir, b.Dir} {
				if err := os.WriteFile(filepath.Join(cd, "settings.json"), []byte(olderPatch), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got := f.run(cmd...)
			assertCode(t, got, 0)
			if strings.Count(got.stderr, "julienning setup") != 1 || !strings.Contains(got.stderr, warning) {
				t.Errorf("stderr = %q, want once %q", got.stderr, warning)
			}

			// Something besides the new hook missing: named generically.
			if _, err := claudecfg.Patch(a.Dir, "/usr/local/bin/julienning"); err != nil {
				t.Fatal(err)
			}
			body := strings.Replace(olderPatch, `"statusLine": {"type": "command", "command": "/usr/local/bin/julienning statusline"},`, `"statusLine": {"type": "command", "command": "mine.sh"},`, 1)
			if err := os.WriteFile(filepath.Join(b.Dir, "settings.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			got = f.run(cmd...)
			assertCode(t, got, 0)
			if !strings.Contains(got.stderr, generic) {
				t.Errorf("stderr = %q, want %q", got.stderr, generic)
			}

			if _, err := claudecfg.Patch(b.Dir, "/usr/local/bin/julienning"); err != nil {
				t.Fatal(err)
			}
			got = f.run(cmd...)
			assertCode(t, got, 0)
			if strings.Contains(got.stderr, "julienning setup") {
				t.Errorf("after setup: stderr = %q", got.stderr)
			}
		})
	}
}
