package dirs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

const (
	fakeExe     = "/opt/bin/julienning"
	wantCommand = fakeExe + " statusline"
	testURL     = "https://w.example.dev"
	testToken   = "s3cr3t-team-token"
)

var fixedNow = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

type execCall struct {
	dir   string
	args  []string
	chdir string
}

// harness gives each test its own HOME, JULIENNING_HOME and install dirs,
// a fake Worker, and a fake terminal, so nothing ever touches the
// developer's real ~/.julienning, ~/.zshrc, ~/.claude* or the network.
type harness struct {
	t      *testing.T
	home   string
	jl     string
	stdin  io.Reader
	stdout bytes.Buffer
	stderr bytes.Buffer

	fake        *remote.Fake
	healthErr   error
	interactive bool
	secrets     []string // returned by readSecret in order
	secretCalls int
	stable      bool
	execs       []execCall
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	jl := filepath.Join(home, ".julienning")
	t.Setenv("HOME", home)
	t.Setenv("USER", "Murat")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("JULIENNING_HOME", jl)
	t.Setenv("JULIENNING_BIN_DIR", filepath.Join(home, ".local", "bin"))
	t.Setenv("JULIENNING_VERSIONS_DIR", filepath.Join(home, ".local", "share", "julienning", "versions"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("JULIENNING_REMOTE_URL", "")
	t.Setenv("JULIENNING_TOKEN", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	h := &harness{t: t, home: home, jl: jl, stdin: strings.NewReader(""), fake: &remote.Fake{}, stable: true}

	prevClient, prevHealth, prevInteractive := newClient, checkHealth, isInteractive
	prevSecret, prevStable, prevNow, prevExec := readSecret, stableCommand, now, launch.Exec
	newClient = func(*config.Config) remote.Client { return h.fake }
	checkHealth = func(context.Context, string) error { return h.healthErr }
	isInteractive = func(cli.Env) bool { return h.interactive }
	readSecret = func(cli.Env, string) (string, error) {
		h.secretCalls++
		if len(h.secrets) == 0 {
			return "", errors.New("no secret scripted")
		}
		s := h.secrets[0]
		h.secrets = h.secrets[1:]
		return s, nil
	}
	stableCommand = func() (string, bool, error) { return fakeExe, h.stable, nil }
	now = func() time.Time { return fixedNow }
	launch.Exec = func(dir string, args []string, chdir string) error {
		h.execs = append(h.execs, execCall{dir, args, chdir})
		return nil
	}
	t.Cleanup(func() {
		newClient, checkHealth, isInteractive = prevClient, prevHealth, prevInteractive
		readSecret, stableCommand, now, launch.Exec = prevSecret, prevStable, prevNow, prevExec
	})
	return h
}

func (h *harness) run(fn func(cli.Env) error, args ...string) error {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	return fn(cli.Env{Args: args, Stdin: h.stdin, Stdout: &h.stdout, Stderr: &h.stderr})
}

func (h *harness) mustRun(fn func(cli.Env) error, args ...string) string {
	h.t.Helper()
	if err := h.run(fn, args...); err != nil {
		h.t.Fatalf("command failed: %v\nstdout: %s\nstderr: %s", err, h.stdout.String(), h.stderr.String())
	}
	return h.stdout.String()
}

func (h *harness) mkdir(rel string) string {
	h.t.Helper()
	p := filepath.Join(h.home, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) writeFile(path, body string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// loggedIn creates a config dir logged in as email. ".claude" is the default
// dir, whose account file is ~/.claude.json.
func (h *harness) loggedIn(rel, email string) string {
	h.t.Helper()
	dir := h.mkdir(rel)
	account := filepath.Join(dir, ".claude.json")
	if rel == ".claude" {
		account = filepath.Join(h.home, ".claude.json")
	}
	h.writeFile(account, `{"oauthAccount":{"emailAddress":"`+email+`"}}`)
	return dir
}

// allow puts emails on the fake Worker's allowlist, each nicknamed after
// its local part (a@x.io → a).
func (h *harness) allow(emails ...string) {
	h.fake.Listing = &remote.Listing{}
	for _, e := range emails {
		h.fake.Listing.Accounts = append(h.fake.Listing.Accounts, remote.Account{Email: e, Nickname: strings.SplitN(e, "@", 2)[0]})
	}
}

// allowLegacy adds records from before nicknames existed (no nickname).
func (h *harness) allowLegacy(emails ...string) {
	if h.fake.Listing == nil {
		h.fake.Listing = &remote.Listing{}
	}
	for _, e := range emails {
		h.fake.Listing.Accounts = append(h.fake.Listing.Accounts, remote.Account{Email: e})
	}
}

// cacheNicks writes shared.json with nicknames ("email=nick" pairs; a bare
// email has none).
func (h *harness) cacheNicks(pairs ...string) {
	h.t.Helper()
	var entries []sharedcache.Entry
	for _, p := range pairs {
		e, n, _ := strings.Cut(p, "=")
		entries = append(entries, sharedcache.Entry{Email: e, Nickname: n})
	}
	if _, err := sharedcache.SaveEntries(entries, fixedNow); err != nil {
		h.t.Fatal(err)
	}
}

// conflict409 is the Worker's answer for a nickname held by another account.
func conflict409() error { return &remote.Error{Status: 409, Message: "nickname is taken"} }

// initConfig writes a config.json with the given registrations directly.
func (h *harness) initConfig(withRemote bool, dirs ...config.ConfigDir) *config.Config {
	h.t.Helper()
	cfg, err := config.New("murat")
	if err != nil {
		h.t.Fatal(err)
	}
	if withRemote {
		cfg.Remote = config.Remote{URL: testURL, Token: testToken}
	}
	for _, cd := range dirs {
		if err := cfg.Add(cd); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := cfg.Save(); err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

func (h *harness) cacheEmails(emails ...string) {
	h.t.Helper()
	if _, err := sharedcache.Save(emails, fixedNow); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) config() *config.Config {
	h.t.Helper()
	c, err := config.Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) read(path string) string {
	h.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

func (h *harness) names() []string {
	var out []string
	for _, cd := range h.config().Configs {
		out = append(out, cd.Name)
	}
	return out
}

func contains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("output missing %q:\n%s", want, got)
	}
}

func notContains(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Errorf("output unexpectedly contains %q:\n%s", want, got)
	}
}

// settingsOf decodes the parts of settings.json julienning writes.
type settingsDoc struct {
	StatusLine *struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	} `json:"statusLine"`
	Hooks map[string][]struct {
		Hooks []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func settingsOf(t *testing.T, dir string) settingsDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc settingsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func statusLineOf(t *testing.T, dir string) string {
	t.Helper()
	doc := settingsOf(t, dir)
	if doc.StatusLine == nil {
		t.Fatalf("%s: no statusLine", dir)
	}
	if doc.StatusLine.Type != "command" {
		t.Errorf("%s: statusLine.type = %q", dir, doc.StatusLine.Type)
	}
	return doc.StatusLine.Command
}

// assertPatched checks statusLine plus exactly one julienning hook per event.
func assertPatched(t *testing.T, dir string) {
	t.Helper()
	if got := statusLineOf(t, dir); got != wantCommand {
		t.Errorf("%s: statusLine.command = %q, want %q", dir, got, wantCommand)
	}
	doc := settingsOf(t, dir)
	for event, sub := range map[string]string{"SessionStart": "hook session-start", "SessionEnd": "hook session-end"} {
		n := 0
		for _, g := range doc.Hooks[event] {
			for _, hk := range g.Hooks {
				if hk.Command == fakeExe+" "+sub {
					n++
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d julienning %s hooks, want 1", dir, n, event)
		}
	}
}

// usageErr/asUsage keep the tests readable when asserting exit-code-2 errors.
type usageErr = cli.UsageError

func asUsage(err error, target **usageErr) bool {
	var ue *cli.UsageError
	if errors.As(err, &ue) {
		*target = ue
		return true
	}
	return false
}
