package usageprobe

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
)

// fakeClaude is a stand-in for the claude binary. It logs its argv, working
// directory, stdin and environment to <config dir>/probe.log, answers
// `auth status` from <dir>/auth ("out" = logged out), and for anything else
// prints <dir>/out.jsonl, after sleeping when <dir>/mode says "sleep" or
// exiting 3 when it says "exit3". The config dir is CLAUDE_CONFIG_DIR, or
// $HOME/.claude when unset, as for the real claude.
const fakeClaude = `#!/bin/sh
dir="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
{
  printf 'argv:'; for a in "$@"; do printf ' %s' "$a"; done; printf '\n'
  printf 'pwd: %s\n' "$(pwd -P)"
  if read -r line; then echo "stdin: data"; else echo "stdin: eof"; fi
  env | sort | sed 's/^/env: /'
} >> "$dir/probe.log"
if [ "$1" = auth ]; then
  if [ "$(cat "$dir/auth" 2>/dev/null)" = out ]; then
    echo '{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}'
    exit 1
  fi
  echo '{"loggedIn":true,"authMethod":"claude.ai","email":"someone@x.io"}'
  exit 0
fi
case "$(cat "$dir/mode" 2>/dev/null)" in
  sleep) sleep 30 & wait ;;
  exit3) exit 3 ;;
esac
cat "$dir/out.jsonl" 2>/dev/null
exit 0
`

type harness struct {
	t    *testing.T
	home string
}

// newHarness sets HOME and JULIENNING_HOME to temp dirs, installs the fake
// claude, and saves a config.json for dev murat.
func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, home: filepath.Join(root, "home")}
	bin := filepath.Join(root, "bin", "claude")
	for _, d := range []string{h.home, filepath.Dir(bin)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h.home)
	t.Setenv(config.EnvHome, filepath.Join(root, "julienning"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	prevFind, prevTimeout := findClaude, timeout
	findClaude = func() (string, error) { return bin, nil }
	t.Cleanup(func() { findClaude, timeout = prevFind, prevTimeout })
	cfg := &config.Config{Version: config.SchemaVersion, Dev: "murat", MachineID: "3fa9c2d1e07b"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return h
}

// dir creates a config dir logged into email ("" = no account file) and
// registers it under name.
func (h *harness) dir(name, email string) string {
	h.t.Helper()
	d := filepath.Join(h.home, ".claude-"+name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if email != "" {
		h.login(d, email)
	}
	cfg, err := config.Load()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := cfg.Add(config.ConfigDir{Name: name, Dir: d}); err != nil {
		h.t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) login(d, email string) {
	h.t.Helper()
	raw, _ := json.Marshal(map[string]any{"oauthAccount": map[string]any{"emailAddress": email}})
	if err := os.WriteFile(filepath.Join(d, ".claude.json"), raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) write(d, name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) log(d string) string {
	raw, _ := os.ReadFile(filepath.Join(d, "probe.log"))
	return string(raw)
}

const initLine = `{"type":"system","subtype":"init","claude_code_version":"2.1.303","session_id":"x"}`

// usageOut is claude's stream-json for /usage with the given limits rows
// (JSON array), as 2.1.303 printed it for claude5 on 2026-10-19.
func usageOut(limits string) string {
	limits = strings.Join(strings.Fields(limits), " ") // one message per line, as claude prints them
	return initLine + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Current session: 7% used\nLast 24h · 273 requests\n  Top skills: /secret-project 8%"}]},` +
		`"usage_report":{"session":{"total_cost_usd":0},"rate_limits":{"limits":` + limits + `,"extra_usage":{"is_enabled":false}}}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0,"num_turns":0}` + "\n"
}

const realLimits = `[
 {"kind":"session","group":"session","percent":7,"resets_at":"2026-10-19T13:40:00.345468+00:00","scope":null,"severity":"normal","is_active":false},
 {"kind":"weekly_all","group":"weekly","percent":83,"resets_at":"2026-10-24T14:00:00.345489+00:00","scope":null,"severity":"warning","is_active":true},
 {"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-10-24T14:00:00+00:00","scope":{"model":{"display_name":"Fable"},"surface":null},"severity":"normal","is_active":false}
]`

var probeNow = time.Date(2026, 10, 19, 11, 0, 0, 250000000, time.UTC)

func TestRunReadsTheUsageReport(t *testing.T) {
	h := newHarness(t)
	d := h.dir("julienning5", "claude5@x.io")
	h.write(d, "out.jsonl", usageOut(realLimits))

	r := Run(context.Background(), d, probeNow)
	if r.Outcome != OK || r.Err != nil {
		t.Fatalf("outcome %v err %v", r.Outcome, r.Err)
	}
	wantS := time.Date(2026, 10, 19, 13, 40, 0, 0, time.UTC)
	wantW := time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)
	if r.Session.Used != 7 || !r.Session.ResetsAt.Equal(wantS) || r.Week.Used != 83 || !r.Week.ResetsAt.Equal(wantW) {
		t.Fatalf("windows: %+v %v / %+v %v", r.Session, r.Session.ResetsAt, r.Week, r.Week.ResetsAt)
	}
	if !r.At.Equal(probeNow.Truncate(time.Second)) {
		t.Fatalf("at: %v", r.At)
	}
}

func TestRunIsolatesClaude(t *testing.T) {
	h := newHarness(t)
	d := h.dir("julienning5", "claude5@x.io")
	h.write(d, "out.jsonl", usageOut(realLimits))
	// What a probe started from a hook or the status line inherits.
	for k, v := range map[string]string{
		"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "abc", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-x",
		"ANTHROPIC_API_KEY": "sk-ant-api", "ANTHROPIC_BASE_URL": "https://gateway", "CLAUDE_PID": "1",
		"CLAUDE_CONFIG_DIR": filepath.Join(h.home, ".claude-other"), "HTTPS_PROXY": "http://proxy:3128",
	} {
		t.Setenv(k, v)
	}
	if r := Run(context.Background(), d, probeNow); r.Outcome != OK {
		t.Fatalf("outcome %v: %v", r.Outcome, r.Err)
	}
	log := h.log(d)
	if !strings.Contains(log, "argv: "+strings.Join(Args, " ")+"\n") {
		t.Fatalf("argv:\n%s", log)
	}
	for _, a := range []string{"--safe-mode", "--no-session-persistence", "--name " + livesess.ProbeSessionName} {
		if !strings.Contains(log, a) {
			t.Errorf("argv lacks %s", a)
		}
	}
	if !strings.Contains(log, "stdin: eof") {
		t.Error("stdin must be /dev/null")
	}
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	if !strings.Contains(log, "pwd: "+tmp+"\n") {
		t.Errorf("cwd must be the temp dir (%s):\n%s", tmp, log)
	}
	if !strings.Contains(log, "env: CLAUDE_CONFIG_DIR="+d+"\n") || !strings.Contains(log, "env: HTTPS_PROXY=http://proxy:3128\n") {
		t.Errorf("env must select the dir and keep the rest:\n%s", log)
	}
	for _, k := range []string{"CLAUDECODE=", "CLAUDE_CODE_", "ANTHROPIC_", "CLAUDE_PID="} {
		if strings.Contains(log, "env: "+k) {
			t.Errorf("env leaked %s:\n%s", k, log)
		}
	}
	if strings.Contains(log, ".claude-other") {
		t.Errorf("the inherited CLAUDE_CONFIG_DIR leaked:\n%s", log)
	}
}

func TestEnvUnsetsTheVariableForTheDefaultDir(t *testing.T) {
	h := newHarness(t)
	env := Env([]string{"CLAUDE_CONFIG_DIR=/x", "PATH=/bin"}, filepath.Join(h.home, ".claude"))
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("the default dir needs CLAUDE_CONFIG_DIR unset: %v", env)
		}
	}
}

func TestRunWindowNotRunning(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "out.jsonl", usageOut(`[{"kind":"session","percent":0,"resets_at":null},{"kind":"weekly_all","percent":12,"resets_at":"2026-10-24T14:00:00Z"}]`))
	r := Run(context.Background(), d, probeNow)
	if r.Outcome != OK || r.Session.ResetsAt != nil || r.Session.Used != 0 || r.Week.Used != 12 {
		t.Fatalf("%+v", r)
	}

	h.write(d, "out.jsonl", usageOut(`[{"kind":"session","percent":5,"resets_at":null},{"kind":"weekly_all","percent":12,"resets_at":"2026-10-24T14:00:00Z"}]`))
	r = Run(context.Background(), d, probeNow)
	if r.Outcome != Failed || !strings.Contains(r.Shape, "session={percent:number,resets_at:null}") {
		t.Fatalf("usage without a reset must fail with a signature: %+v", r)
	}
}

func TestRunRoundsResetsToTheNearestSecond(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	// What two machines got for the same reset on 2026-10-19 (Linux, macOS).
	h.write(d, "out.jsonl", usageOut(`[{"kind":"session","percent":9,"resets_at":"2026-10-19T13:39:59.706945+00:00"},{"kind":"weekly_all","percent":83,"resets_at":"2026-10-24T14:00:00.345489+00:00"}]`))
	r := Run(context.Background(), d, probeNow)
	if !r.Session.ResetsAt.Equal(time.Date(2026, 10, 19, 13, 40, 0, 0, time.UTC)) || !r.Week.ResetsAt.Equal(time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("resets: %v %v", r.Session.ResetsAt, r.Week.ResetsAt)
	}
}

func TestRunClampsAndRounds(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "out.jsonl", usageOut(`[{"kind":"session","percent":104.2,"resets_at":"2026-10-19T13:40:00Z"},{"kind":"weekly_all","percent":33.333,"resets_at":"2026-10-24T14:00:00Z"}]`))
	r := Run(context.Background(), d, probeNow)
	if r.Outcome != OK || r.Session.Used != 100 || r.Week.Used != 33.3 {
		t.Fatalf("%+v", r)
	}
}

func TestRunSignedOut(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j2", "claude2@x.io")
	// What 2.1.303 printed for a dir without a login: no usage_report.
	h.write(d, "out.jsonl", initLine+"\n"+`{"type":"assistant","message":{"content":[{"type":"text","text":"Total cost: $0.0000"}]}}`+"\n")
	h.write(d, "auth", "out")
	if r := Run(context.Background(), d, probeNow); r.Outcome != SignedOut || r.Err != nil {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(h.log(d), "argv: auth status --json\n") {
		t.Fatal("signed out must be confirmed by claude auth status")
	}

	h.write(d, "auth", "in")
	r := Run(context.Background(), d, probeNow)
	if r.Outcome != Failed || !strings.Contains(r.Err.Error(), "signed in but reported no plan usage") {
		t.Fatalf("a login without a report is not signed out: %+v", r)
	}
}

func TestRunFailures(t *testing.T) {
	for _, tc := range []struct {
		name, out, mode, want string
	}{
		{"too old", strings.Replace(usageOut(realLimits), "2.1.303", "2.1.200", 1), "", "Claude Code 2.1.200 cannot report usage; julienning needs 2.1.301 or newer (run: claude update)"},
		{"endpoint unreachable", initLine + "\n" + `{"type":"assistant","usage_report":{"rate_limits":null}}` + "\n", "", "could not fetch the usage"},
		{"no rows", usageOut(`null`), "", "could not fetch the usage"},
		{"row missing", usageOut(`[{"kind":"session","percent":1,"resets_at":"2026-10-19T13:40:00Z"}]`), "", "unexpected usage report"},
		{"exit status", "", "exit3", "claude exited with status 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.dir("j1", "a@x.io")
			h.write(d, "out.jsonl", tc.out)
			h.write(d, "mode", tc.mode)
			r := Run(context.Background(), d, probeNow)
			if r.Outcome != Failed || r.Err == nil || !strings.Contains(r.Err.Error(), tc.want) {
				t.Fatalf("got %v %v, want %q", r.Outcome, r.Err, tc.want)
			}
			if strings.Contains(r.Err.Error(), "secret-project") {
				t.Fatal("errors must never quote claude's output")
			}
		})
	}
}

func TestRunRowMissingHasASignature(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "out.jsonl", usageOut(`[{"kind":"session","percent":1,"resets_at":"2026-10-19T13:40:00Z"},{"kind":"weekly_scoped","percent":2,"resets_at":null}]`))
	r := Run(context.Background(), d, probeNow)
	if r.Shape != "kinds=[session weekly_scoped] session={percent:number,resets_at:string}" {
		t.Fatalf("shape: %q", r.Shape)
	}
}

func TestRunTimesOut(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "mode", "sleep")
	timeout = 300 * time.Millisecond
	start := time.Now()
	r := Run(context.Background(), d, probeNow)
	if r.Outcome != Failed || !strings.Contains(r.Err.Error(), "did not answer within 300ms") {
		t.Fatalf("%+v", r)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the probe's process group must be killed at the timeout")
	}
}

func TestRunClaudeMissing(t *testing.T) {
	newHarness(t)
	findClaude = func() (string, error) { return "", os.ErrNotExist }
	if r := Run(context.Background(), "/nowhere", probeNow); r.Outcome != Failed {
		t.Fatalf("%+v", r)
	}
}

func TestRunAllKeepsOrder(t *testing.T) {
	h := newHarness(t)
	var dirs []string
	for i, pct := range []string{"1", "2", "3", "4", "5", "6"} {
		d := h.dir("j"+pct, "a"+pct+"@x.io")
		h.write(d, "out.jsonl", usageOut(`[{"kind":"session","percent":`+pct+`,"resets_at":"2026-10-19T13:40:00Z"},{"kind":"weekly_all","percent":0,"resets_at":null}]`))
		dirs = append(dirs, d)
		_ = i
	}
	for i, r := range RunAll(context.Background(), dirs, probeNow) {
		if r.Dir != dirs[i] || r.Session.Used != float64(i+1) {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
}

func TestNewEnough(t *testing.T) {
	for v, want := range map[string]bool{
		"2.1.301": true, "2.1.303": true, "2.2.0": true, "3.0.0": true, "2.1.300": false, "2.0.999": false, "1.9.9": false,
		"2.1.301-beta": true, "garbage": true,
	} {
		if got := newEnough(v); got != want {
			t.Errorf("%s: got %v", v, got)
		}
	}
}
