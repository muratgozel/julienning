package switching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/tui"
)

const (
	nowEpoch  = 1789482657 // 2026-09-15T14:30:57Z = 17:30:57 in Europe/Istanbul (+03:00)
	fiveReset = 1789491600 // 2026-09-15T17:00:00Z = 20:00 local
	weekReset = 1789750800 // 2026-09-18T17:00:00Z = Fri 20:00 local
	email1    = "claude1@x.io"
	email2    = "claude2@x.io"
	personal  = "me@personal.io"
	sidA      = "0b7f6c1e-2a4d-4e8b-9f10-1c2d3e4f5a6b"
	sidB      = "1c8a7d2f-3b5e-4f9c-8a21-2d3e4f5a6b7c"
	sidC      = "2d9b8e30-4c6f-4a0d-9b32-3e4f5a6b7c8d"
)

var nowT = time.Unix(nowEpoch, 0).UTC()

type execCall struct {
	dir   string
	args  []string
	chdir string
}

type fixture struct {
	t         *testing.T
	home      string
	jul       string
	proj      string // the working directory sessions belong to
	fake      *remote.Fake
	cfg       *config.Config
	tty       bool
	pickRows  []tui.Item
	pickHdr   string
	pickScope string
	pick      func(items []tui.Item) (int, error)
	pickAt    []int // the row each picker call opened on
	// confirm answers the move confirmation; nil fails the test when one is
	// shown. confirms records each question and detail shown.
	confirm  func(question, detail string) (bool, error)
	confirms []confirmCall
	execs    []execCall
}

type confirmCall struct{ question, detail string }

// setup builds a hermetic machine: temp HOME and JULIENNING_HOME, frozen
// clock, fixed zone, fake Worker, fake terminal, fake claude.
func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, home: filepath.Join(root, "home"), jul: filepath.Join(root, "julienning")}
	f.proj = filepath.Join(f.home, "Code", "shop")
	if err := os.MkdirAll(f.proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(f.proj); err == nil {
		f.proj = real
	}
	t.Setenv("HOME", f.home)
	t.Setenv("JULIENNING_HOME", f.jul)
	t.Setenv("JULIENNING_NOW_EPOCH", "1789482657")
	t.Setenv("TZ", "Europe/Istanbul")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("JULIENNING_REMOTE_URL", "")
	t.Setenv("JULIENNING_TOKEN", "")
	t.Setenv("NO_COLOR", "")

	f.fake = &remote.Fake{}
	restore := []func(){}
	prevClient, prevInteractive, prevPick, prevConfirm, prevExec, prevWd, prevAlive := newClient, interactive, pickSession, confirmMove, execClaude, getwd, livesess.Alive
	restore = append(restore, func() {
		newClient, interactive, pickSession, confirmMove, execClaude, getwd, livesess.Alive = prevClient, prevInteractive, prevPick, prevConfirm, prevExec, prevWd, prevAlive
	})
	t.Cleanup(func() {
		for _, r := range restore {
			r()
		}
	})
	newClient = func(*config.Config, time.Duration) remote.Client { return f.fake }
	interactive = func(cli.Env) bool { return f.tty }
	pickSession = func(_ cli.Env, items []tui.Item, header, scope string, initial int) (int, error) {
		f.pickRows, f.pickHdr, f.pickScope = items, header, scope
		f.pickAt = append(f.pickAt, initial)
		if f.pick == nil {
			t.Fatal("picker shown unexpectedly")
		}
		return f.pick(items)
	}
	confirmMove = func(_ cli.Env, question, detail string) (bool, error) {
		f.confirms = append(f.confirms, confirmCall{question, detail})
		if f.confirm == nil {
			t.Fatalf("move confirmation shown unexpectedly: %s", question)
		}
		return f.confirm(question, detail)
	}
	execClaude = func(dir string, args []string, chdir string) error {
		f.execs = append(f.execs, execCall{dir, args, chdir})
		return nil
	}
	getwd = func() (string, error) { return f.proj, nil }
	livesess.Alive = func(livesess.Session) bool { return true }

	f.cfg = &config.Config{
		Version:            config.SchemaVersion,
		Dev:                "murat",
		MachineID:          "3fa9c2d1e07b",
		Remote:             config.Remote{URL: "https://worker.test", Token: "tok"},
		SendMinIntervalSec: 300,
	}
	return f
}

// addConfig registers ~/.claude-<name>, logged into email ("" = not logged in).
func (f *fixture) addConfig(name, email string) config.ConfigDir {
	f.t.Helper()
	dir := filepath.Join(f.home, ".claude-"+name)
	if name == "default" {
		dir = filepath.Join(f.home, ".claude")
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	// Wired like setup leaves it, so commands have no setup warning to print.
	if _, err := claudecfg.Patch(dir, "/usr/local/bin/julienning"); err != nil {
		f.t.Fatal(err)
	}
	if email != "" {
		acct := filepath.Join(dir, ".claude.json")
		if name == "default" {
			acct = filepath.Join(f.home, ".claude.json")
		}
		body := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
		if err := os.WriteFile(acct, []byte(body), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	cd := config.ConfigDir{Name: name, Dir: dir}
	if err := f.cfg.Add(cd); err != nil {
		f.t.Fatal(err)
	}
	return cd
}

// acceptMoves answers yes to every move confirmation.
func acceptMoves(string, string) (bool, error) { return true, nil }

func (f *fixture) save() {
	f.t.Helper()
	if err := f.cfg.Save(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) setCurrent(cd config.ConfigDir) {
	f.t.Helper()
	if err := config.SetCurrent(cd); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) current() string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.jul, config.CurrentFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// writeSession puts a session for f.proj into cd, last active `ago` before now.
func (f *fixture) writeSession(cd config.ConfigDir, id, title, prompt string, ago time.Duration) string {
	f.t.Helper()
	at := nowT.Add(-ago)
	user, _ := json.Marshal(map[string]any{
		"type": "user", "isSidechain": false, "cwd": f.proj, "sessionId": id, "origin": nil,
		"message":   map[string]any{"role": "user", "content": prompt},
		"timestamp": at.Format("2006-01-02T15:04:05.000Z"),
	})
	lines := string(user) + "\n"
	if title != "" {
		lines += `{"type":"ai-title","aiTitle":"` + title + `","sessionId":"` + id + `"}` + "\n"
	}
	p := filepath.Join(cd.Dir, "projects", sessions.EncodeCwd(f.proj), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		f.t.Fatal(err)
	}
	// Old mtimes: nothing looks recently written (the move refuses those).
	if err := os.Chtimes(p, at, at); err != nil {
		f.t.Fatal(err)
	}
	return p
}

type result struct {
	code           int
	stdout, stderr string
}

func (f *fixture) run(args ...string) result {
	f.t.Helper()
	var out, errOut strings.Builder
	code := cli.Main(args, strings.NewReader(""), &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

func assertCode(t *testing.T, r result, want int) {
	t.Helper()
	if r.code != want {
		t.Fatalf("exit %d, want %d\nstdout: %q\nstderr: %q", r.code, want, r.stdout, r.stderr)
	}
}

func ts(epoch int64) *time.Time {
	v := time.Unix(epoch, 0).UTC()
	return &v
}

func win(used float64, reset int64) *remote.Window {
	return &remote.Window{Used: used, ResetsAt: ts(reset)}
}

func listing(accts ...remote.Account) *remote.Listing {
	return &remote.Listing{GeneratedAt: nowT, TZ: "Europe/Istanbul", Accounts: accts}
}

func acct(email string, session, week float64) remote.Account {
	return remote.Account{Email: email, Session: win(session, fiveReset), Week: win(week, weekReset), State: "free", Claims: []remote.Claim{}, BusyBy: []string{}}
}
