package dirs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/tui"
)

// syncScript answers sync's questions: answers maps a header prefix (the
// setting key, or "CLAUDE.md") to the title of the row to choose, "ESC" to
// cancel; a question without an answer takes the first row (the default).
// overwrite answers the CLAUDE.md overwrite question per label.
type syncScript struct {
	answers   map[string]string
	overwrite map[string]bool
	headers   []string
	items     map[string][]tui.Item
	asked     []string // overwrite questions
	onAsk     func()
}

func (h *harness) scriptSync(s *syncScript) {
	h.t.Helper()
	s.items = map[string][]tui.Item{}
	prevPick, prevConfirm := pickChoice, confirmOverwrite
	pickChoice = func(_ cli.Env, items []tui.Item, header, _ string) (int, error) {
		s.headers = append(s.headers, header)
		key := strings.Fields(strings.TrimSuffix(strings.Fields(header)[0], ":"))[0]
		s.items[key] = items
		want, ok := s.answers[key]
		if !ok {
			return 0, nil
		}
		if want == "ESC" {
			return 0, tui.ErrCanceled
		}
		for i, it := range items {
			if it.Title == want {
				return i, nil
			}
		}
		h.t.Fatalf("%s: no row %q in %+v", key, want, items)
		return 0, nil
	}
	confirmOverwrite = func(_ cli.Env, question, _, keep string) (bool, error) {
		s.asked = append(s.asked, question)
		if s.onAsk != nil {
			s.onAsk()
		}
		return s.overwrite[keep], nil
	}
	h.t.Cleanup(func() { pickChoice, confirmOverwrite = prevPick, prevConfirm })
}

// syncFixture: alpha, beta and gamma are shared and logged in here, ~/.claude
// holds a personal account.
func syncFixture(t *testing.T) (*harness, map[string]string) {
	h := newHarness(t)
	t.Setenv("TZ", "Europe/Istanbul")
	dirs := map[string]string{
		"alpha":    h.loggedIn(".claude-a", "alpha@x.io"),
		"beta":     h.loggedIn(".claude-b", "beta@x.io"),
		"gamma":    h.loggedIn(".claude-c", "gamma@x.io"),
		"personal": h.loggedIn(".claude", "me@personal.io"),
	}
	h.initConfig(true,
		config.ConfigDir{Name: "julienning1", Dir: dirs["alpha"]},
		config.ConfigDir{Name: "julienning2", Dir: dirs["beta"]},
		config.ConfigDir{Name: "julienning3", Dir: dirs["gamma"]},
		config.ConfigDir{Name: "default", Dir: dirs["personal"]},
	)
	h.allow("alpha@x.io", "beta@x.io", "gamma@x.io")
	h.writeFile(filepath.Join(dirs["alpha"], "settings.json"), "{\n  \"model\": \"opus[1m]\",\n  \"theme\": \"dark\",\n  \"statusLine\": {\"type\": \"command\", \"command\": \"/opt/bin/julienning statusline\"},\n  \"n\": 1.50\n}\n")
	h.writeFile(filepath.Join(dirs["beta"], "settings.json"), "{\n    \"model\": \"sonnet\",\n    \"promptCacheTtl\": \"1h\",\n    \"autoContinueAtUsageLimit\": true\n}\n")
	h.writeFile(filepath.Join(dirs["personal"], "settings.json"), "{\"model\": \"haiku\"}\n")
	h.writeFile(filepath.Join(dirs["alpha"], "CLAUDE.md"), "# Team rules\nBe brief.\n")
	h.writeFile(filepath.Join(dirs["beta"], "CLAUDE.md"), "# Old rules\n")
	h.writeFile(filepath.Join(dirs["personal"], "CLAUDE.md"), "personal\n")
	old := time.Date(2026, 9, 20, 7, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dirs["beta"], "CLAUDE.md"), old, old); err != nil {
		t.Fatal(err)
	}
	newer := time.Date(2026, 9, 23, 12, 24, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dirs["alpha"], "CLAUDE.md"), newer, newer); err != nil {
		t.Fatal(err)
	}
	h.interactive = true
	return h, dirs
}

func TestSyncAsksEverySettingAndWritesTheChoices(t *testing.T) {
	h, dirs := syncFixture(t)
	s := &syncScript{
		answers: map[string]string{
			"model":                    "opus[1m]",
			"promptCacheTtl":           "1h",
			"subagentPromptCacheTtl":   "Don't sync",
			"autoContinueAtUsageLimit": "not set (Claude Code's default)",
			"editorMode":               "vim",
			"CLAUDE.md":                "alpha CLAUDE.md",
		},
		overwrite: map[string]bool{"beta": false},
	}
	h.scriptSync(s)
	out := h.mustRun(runSync)

	if len(s.headers) != 8 {
		t.Fatalf("asked %d questions, want 7 settings + CLAUDE.md:\n%s", len(s.headers), strings.Join(s.headers, "\n"))
	}
	if s.headers[0] != "model (1 of 7): Default model (opus is the latest Opus)" {
		t.Fatalf("first header %q", s.headers[0])
	}
	// Fixed values are offered though nobody has them, the default first;
	// "not set" lists the accounts without the key.
	ttl := s.items["promptCacheTtl"]
	if ttl[0].Title != "5m" || ttl[0].Note != "(default)" || ttl[0].Meta != "no account yet" ||
		ttl[1].Title != "1h" || ttl[1].Meta != "beta" ||
		ttl[2].Title != "not set (Claude Code's default)" || ttl[2].Meta != "alpha, gamma" ||
		ttl[3].Title != "Don't sync" {
		t.Fatalf("promptCacheTtl rows: %+v", ttl)
	}
	md := s.items["CLAUDE.md"]
	if len(md) != 3 || md[0].Title != "alpha CLAUDE.md" || md[0].Meta != "updated 2026-09-23 15:24 · 2 lines" || md[0].Detail != "# Team rules" ||
		md[1].Title != "beta CLAUDE.md" || md[1].Meta != "updated 2026-09-20 10:00 · 1 line" {
		t.Fatalf("CLAUDE.md rows (newest first, local time): %+v", md)
	}
	if len(s.asked) != 1 || s.asked[0] != "Overwrite beta's CLAUDE.md with alpha's?" {
		t.Fatalf("overwrite questions: %q", s.asked)
	}

	wantAlpha := "{\n  \"model\": \"opus[1m]\",\n  \"theme\": \"dark\",\n  \"statusLine\": {\"type\": \"command\", \"command\": \"/opt/bin/julienning statusline\"},\n  \"n\": 1.50,\n  \"effortLevel\": \"xhigh\",\n  \"promptCacheTtl\": \"1h\",\n  \"editorMode\": \"vim\"\n}\n"
	if got := h.read(filepath.Join(dirs["alpha"], "settings.json")); got != wantAlpha {
		t.Fatalf("alpha settings:\n%s\nwant\n%s", got, wantAlpha)
	}
	wantBeta := "{\n    \"model\": \"opus[1m]\",\n    \"promptCacheTtl\": \"1h\",\n    \"effortLevel\": \"xhigh\",\n    \"theme\": \"dark\",\n    \"editorMode\": \"vim\"\n}\n"
	if got := h.read(filepath.Join(dirs["beta"], "settings.json")); got != wantBeta {
		t.Fatalf("beta settings:\n%s\nwant\n%s", got, wantBeta)
	}
	wantGamma := "{\n  \"model\": \"opus[1m]\",\n  \"effortLevel\": \"xhigh\",\n  \"promptCacheTtl\": \"1h\",\n  \"theme\": \"dark\",\n  \"editorMode\": \"vim\"\n}\n"
	if got := h.read(filepath.Join(dirs["gamma"], "settings.json")); got != wantGamma {
		t.Fatalf("gamma settings (created):\n%s\nwant\n%s", got, wantGamma)
	}
	if got := h.read(filepath.Join(dirs["gamma"], "CLAUDE.md")); got != "# Team rules\nBe brief.\n" {
		t.Fatalf("gamma CLAUDE.md = %q", got)
	}
	if got := h.read(filepath.Join(dirs["beta"], "CLAUDE.md")); got != "# Old rules\n" {
		t.Fatalf("declined overwrite changed beta: %q", got)
	}
	// The personal account is never touched.
	if got := h.read(filepath.Join(dirs["personal"], "settings.json")); got != "{\"model\": \"haiku\"}\n" {
		t.Fatalf("personal settings changed: %q", got)
	}
	if got := h.read(filepath.Join(dirs["personal"], "CLAUDE.md")); got != "personal\n" {
		t.Fatalf("personal CLAUDE.md changed: %q", got)
	}
	for _, want := range []string{
		"Shared accounts: alpha, beta, gamma\n",
		"model                      opus[1m]: set in beta, gamma\n",
		"effortLevel                xhigh: set in alpha, beta, gamma\n",
		"subagentPromptCacheTtl     not synced\n",
		"autoContinueAtUsageLimit   not set (Claude Code's default): removed from beta\n",
		"theme                      dark: set in beta, gamma\n",
		"CLAUDE.md                  alpha's: copied to gamma; kept beta's\n",
	} {
		contains(t, out, want)
	}
	notContains(t, out, "personal")
}

func TestSyncEscWritesNothing(t *testing.T) {
	h, dirs := syncFixture(t)
	before := h.read(filepath.Join(dirs["beta"], "settings.json"))
	h.scriptSync(&syncScript{answers: map[string]string{"theme": "ESC"}})
	out := h.mustRun(runSync)
	contains(t, out, "Sync canceled; nothing was changed.")
	if got := h.read(filepath.Join(dirs["beta"], "settings.json")); got != before {
		t.Fatalf("beta changed after a cancel:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dirs["gamma"], "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gamma's settings.json was created after a cancel: %v", err)
	}
}

// A CLAUDE.md edited while sync waits for an answer is kept, not clobbered.
func TestSyncKeepsACLAUDEmdEditedMeanwhile(t *testing.T) {
	h, dirs := syncFixture(t)
	s := &syncScript{answers: map[string]string{"CLAUDE.md": "alpha CLAUDE.md"}, overwrite: map[string]bool{"beta": true}}
	s.onAsk = func() { h.writeFile(filepath.Join(dirs["beta"], "CLAUDE.md"), "edited meanwhile\n") }
	h.scriptSync(s)
	out := h.mustRun(runSync)
	if got := h.read(filepath.Join(dirs["beta"], "CLAUDE.md")); got != "edited meanwhile\n" {
		t.Fatalf("beta CLAUDE.md = %q", got)
	}
	contains(t, h.stderr.String(), "left beta's CLAUDE.md as it is: it changed while sync was running")
	contains(t, out, "CLAUDE.md                  alpha's: copied to gamma; kept beta's\n")
}

func TestSyncOverwritesWhenConfirmed(t *testing.T) {
	h, dirs := syncFixture(t)
	h.scriptSync(&syncScript{answers: map[string]string{"CLAUDE.md": "beta CLAUDE.md"}, overwrite: map[string]bool{"alpha": true}})
	out := h.mustRun(runSync)
	for _, d := range []string{"alpha", "beta", "gamma"} {
		if got := h.read(filepath.Join(dirs[d], "CLAUDE.md")); got != "# Old rules\n" {
			t.Fatalf("%s CLAUDE.md = %q", d, got)
		}
	}
	contains(t, out, "CLAUDE.md                  beta's: copied to alpha, gamma\n")
}

func TestSyncSkipsCLAUDEmdWhenAllAreTheSame(t *testing.T) {
	h, dirs := syncFixture(t)
	for _, d := range []string{"beta", "gamma"} {
		h.writeFile(filepath.Join(dirs[d], "CLAUDE.md"), "# Team rules\nBe brief.\n")
	}
	s := &syncScript{}
	h.scriptSync(s)
	out := h.mustRun(runSync)
	if len(s.headers) != 7 {
		t.Fatalf("asked %d questions", len(s.headers))
	}
	contains(t, out, "CLAUDE.md                  already the same in every shared account\n")
}

func TestSyncFromCopiesOneAccountWithoutAsking(t *testing.T) {
	h, dirs := syncFixture(t)
	h.interactive = false
	h.scriptSync(&syncScript{answers: map[string]string{"model": "ESC"}})
	out := h.mustRun(runSync, "--from", "beta")
	for _, d := range []string{"alpha", "gamma"} {
		raw := h.read(filepath.Join(dirs[d], "settings.json"))
		for _, want := range []string{`"model": "sonnet"`, `"promptCacheTtl": "1h"`, `"autoContinueAtUsageLimit": true`} {
			contains(t, raw, want)
		}
		notContains(t, raw, `"theme"`)
		if got := h.read(filepath.Join(dirs[d], "CLAUDE.md")); got != "# Old rules\n" {
			t.Fatalf("%s CLAUDE.md = %q", d, got)
		}
	}
	contains(t, h.read(filepath.Join(dirs["alpha"], "settings.json")), `"statusLine"`)
	contains(t, out, "theme                      not set (Claude Code's default): removed from alpha\n")
	contains(t, out, "CLAUDE.md                  beta's: copied to alpha, gamma\n")

	err := h.run(runSync, "--from", "me@personal.io")
	if err == nil || !strings.Contains(err.Error(), "not a shared account logged in here; --from takes one of: alpha, beta, gamma") {
		t.Fatalf("--from a personal account: %v", err)
	}
}

func TestSyncCheck(t *testing.T) {
	h, dirs := syncFixture(t)
	h.interactive = false
	err := h.run(runSync, "--check")
	if err == nil || !strings.Contains(err.Error(), "of 8 differ between the shared accounts") {
		t.Fatalf("err = %v", err)
	}
	out := h.stdout.String()
	contains(t, out, "model                      differs: opus[1m] (alpha) · sonnet (beta) · not set (Claude Code's default) (gamma)\n")
	contains(t, out, "editorMode                 same: not set (Claude Code's default)\n")
	contains(t, out, "CLAUDE.md                  differs: alpha (updated 2026-09-23 15:24) · beta (updated 2026-09-20 10:00) · none: gamma\n")

	h.mustRun(runSync, "--from", "alpha")
	out = h.mustRun(runSync, "--check")
	contains(t, out, "Every shared account here has the same settings and CLAUDE.md.")
	_ = dirs
}

func TestSyncRefusals(t *testing.T) {
	h, dirs := syncFixture(t)
	h.interactive = false
	if err := h.run(runSync); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("without a terminal: %v", err)
	}
	var ue *cli.UsageError
	if err := h.run(runSync, "--from", "beta", "--check"); !errors.As(err, &ue) {
		t.Fatalf("--from with --check: %v", err)
	}
	h.allow("alpha@x.io")
	err := h.run(runSync, "--check")
	if err == nil || !strings.Contains(err.Error(), "at least two shared accounts logged in here, and only alpha is") {
		t.Fatalf("one shared account: %v", err)
	}
	_ = dirs
}

// A settings.json that is not JSON leaves that account out of the settings
// with a warning; the rest still sync.
func TestSyncLeavesOutBrokenSettings(t *testing.T) {
	h, dirs := syncFixture(t)
	h.writeFile(filepath.Join(dirs["gamma"], "settings.json"), `{"model": `)
	h.scriptSync(&syncScript{})
	out := h.mustRun(runSync)
	contains(t, h.stderr.String(), "left gamma out of the settings")
	if got := h.read(filepath.Join(dirs["gamma"], "settings.json")); got != `{"model": ` {
		t.Fatalf("broken file changed: %q", got)
	}
	contains(t, out, "model                      opus: set in alpha, beta\n")
}

// When the Worker cannot be reached the cached allowlist decides.
func TestSyncFallsBackToTheCachedAllowlist(t *testing.T) {
	h, _ := syncFixture(t)
	h.interactive = false
	h.fake.ListErr = errors.New("offline")
	h.cacheNicks("alpha@x.io=alpha", "beta@x.io=beta")
	_ = h.run(runSync, "--check")
	contains(t, h.stderr.String(), "using the cached one")
	contains(t, h.stdout.String(), "Shared accounts: alpha, beta\n")
}

func TestRelativeImports(t *testing.T) {
	got := relativeImports([]byte("See @docs/a.md and @~/b.md, @/abs.md.\nmail me@x.io\n```\n@code/x.md\n```\n@rules.md\n"))
	if strings.Join(got, ",") != "@docs/a.md,@rules.md" {
		t.Fatalf("got %q", got)
	}
}
