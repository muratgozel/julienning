package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/ptytest"
	"github.com/muratgozel/julienning/internal/shell"
)

func TestTmuxNewEnough(t *testing.T) {
	for v, want := range map[string]bool{
		"3.5a": true, "2.1": true, "2.10": true, "3.0": true, "next-3.6": true, "master": true, "openbsd-7.4": true,
		"2.0": false, "1.9a": false, "1.8": false, "0.9": false,
	} {
		if got := tmuxNewEnough(v); got != want {
			t.Errorf("%q: got %v, want %v", v, got, want)
		}
	}
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindTmux(t *testing.T) {
	root := t.TempDir()
	onPath, extra := filepath.Join(root, "bin"), filepath.Join(root, "extra")
	prev := tmuxDirs
	tmuxDirs = func() []string { return []string{filepath.Join(root, "none"), extra} }
	t.Cleanup(func() { tmuxDirs = prev })

	t.Setenv("PATH", onPath)
	if _, err := FindTmux(); !errors.Is(err, ErrTmuxNotFound) {
		t.Fatalf("no tmux: %v", err)
	}
	// Outside PATH: the fallback dirs (non-interactive ssh has a minimal PATH).
	writeExe(t, filepath.Join(extra, "tmux"), "#!/bin/sh\necho 'tmux 3.5a'\n")
	got, err := FindTmux()
	if err != nil || got.Path != filepath.Join(extra, "tmux") || got.Version != "3.5a" {
		t.Fatalf("fallback dir: %+v %v", got, err)
	}
	// PATH first.
	writeExe(t, filepath.Join(onPath, "tmux"), "#!/bin/sh\necho 'tmux 2.0'\n")
	var old *TmuxTooOldError
	if _, err := FindTmux(); !errors.As(err, &old) || old.Version != "2.0" || !strings.Contains(err.Error(), "needs tmux 2.1 or newer") {
		t.Fatalf("old tmux: %v", err)
	}
	writeExe(t, filepath.Join(onPath, "tmux"), "#!/bin/sh\nexit 1\n")
	if _, err := FindTmux(); err == nil || !strings.Contains(err.Error(), "-V") {
		t.Fatalf("broken tmux: %v", err)
	}
}

func TestTmuxApplies(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		getenv    func(string) string
		terminals bool
		want      bool
	}{
		{env(), true, true},
		{env("TERM_PROGRAM", "iTerm.app"), true, true},
		{env(), false, false},
		{env("TMUX", "/tmp/tmux-1/default,123,0"), true, false},
		{env("TERM_PROGRAM", "vscode"), true, false},
	}
	for i, c := range cases {
		if got := TmuxApplies(c.getenv, c.terminals); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}

func TestWindowCommandNamesTheAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := WindowCommand(filepath.Join(home, ".claude-x"), "/opt/claude", []string{"--resume", "a b"})
	want := []string{"/bin/sh", "-c", shell.WindowScript, "julienning", "env", "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude-x"), "/opt/claude", "--resume", "a b"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for _, dir := range []string{"", filepath.Join(home, ".claude"), filepath.Join(home, ".claude") + "/"} {
		got := WindowCommand(dir, "/opt/claude", nil)
		if want := []string{"/bin/sh", "-c", shell.WindowScript, "julienning", "env", "-u", "CLAUDE_CONFIG_DIR", "/opt/claude"}; !slices.Equal(got, want) {
			t.Errorf("%q: got %q", dir, got)
		}
	}
	if got := strings.Join(NewWindowArgs("claude", "/w", []string{"x"}), " "); got != "new-window -t =claude: -c /w x" {
		t.Errorf("new-window: %s", got)
	}
	if got := strings.Join(NewSessionArgs("claude", "/w", []string{"x"}), " "); got != "new-session -s claude -c /w x" {
		t.Errorf("new-session: %s", got)
	}
	if got := strings.Join(HasSessionArgs("claude"), " ") + " / " + strings.Join(AttachArgs("claude"), " "); got != "has-session -t =claude / attach-session -t =claude" {
		t.Errorf("targets: %s", got)
	}
}

func TestTmuxEnvDropsTheAccount(t *testing.T) {
	got := tmuxEnv([]string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/x", "CLAUDE_CONFIG_DIRX=1"})
	if want := []string{"PATH=/bin", "CLAUDE_CONFIG_DIRX=1"}; !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

// The helper process for TestExecRunsClaudeInTmux: it calls Exec as a
// command would, from a pseudo-terminal, and is replaced by tmux or claude.
//
// CRITICAL: the helper searches only the test's own dirs for tmux. With the
// default fallback dirs it finds the developer's real tmux and opens a
// window in their real session named claude (new-window succeeds even when
// the attach then fails on TERM=dumb).
func TestMain(m *testing.M) {
	if dir := os.Getenv("LAUNCH_TEST_EXEC"); dir != "" {
		tmuxDirs = func() []string { return nil }
		if p := os.Getenv("PATH"); !strings.HasPrefix(p, os.Getenv("LAUNCH_TEST_BIN")+":") && p != os.Getenv("LAUNCH_TEST_BIN") {
			fmt.Println("refusing to run: PATH does not start with the test's fake bin dir")
			os.Exit(1)
		}
		if err := Exec(dir, []string{"--resume", "two words"}, os.Getenv("LAUNCH_TEST_CHDIR")); err != nil {
			fmt.Println("exec failed:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExecRunsClaudeInTmux(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	home, bin, log := filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "tmux.log")
	jl := filepath.Join(home, ".julienning")
	project := filepath.Join(home, "Code", "shop")
	for _, d := range []string{jl, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExe(t, filepath.Join(bin, "claude"), "#!/bin/sh\necho \"claude dir=${CLAUDE_CONFIG_DIR-unset} pwd=$(pwd) args=$*\"\n")
	writeExe(t, filepath.Join(bin, "tmux"), `#!/bin/sh
[ "$1" = -V ] && { echo 'tmux 3.5a'; exit 0; }
{ printf '[%s]' "$@"; echo " env=${CLAUDE_CONFIG_DIR-unset}"; } >> "`+log+`"
case "$1" in
has-session) [ -f "`+root+`/session" ]; exit ;;
new-session|new-window) dir=$5; shift 5; cd "$dir" && SHELL=/usr/bin/true exec "$@" ;;
attach-session) echo attached ;;
esac
`)
	writeConfig := func(tmux *config.Tmux) {
		cfg, err := config.New("murat")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Tmux = tmux
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(jl, config.ConfigFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(extraEnv ...string) (string, string) {
		t.Helper()
		_ = os.Remove(log)
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Dir = root
		cmd.Env = append([]string{
			"LAUNCH_TEST_EXEC=" + filepath.Join(home, ".claude-x"),
			"LAUNCH_TEST_CHDIR=" + project, "LAUNCH_TEST_BIN=" + bin,
			"HOME=" + home, "JULIENNING_HOME=" + jl, "PATH=" + bin + ":/usr/bin:/bin",
			"CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude-old"), "TERM=dumb",
		}, extraEnv...)
		out, ok, err := ptytest.Run(cmd, 20*time.Second)
		if !ok {
			t.Skip("no pseudo-terminal available")
		}
		if err != nil {
			t.Fatalf("helper: %v\n%s", err, out)
		}
		calls, _ := os.ReadFile(log)
		return out, string(calls)
	}
	window := "[/bin/sh][-c][" + shell.WindowScript + "][julienning][env][CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude-x") + "][" + filepath.Join(bin, "claude") + "][--resume][two words]"

	writeConfig(&config.Tmux{Enabled: true, Session: "claude"})
	out, calls := run()
	want := "[has-session][-t][=claude] env=unset\n[new-session][-s][claude][-c][" + project + "]" + window + " env=unset\n"
	if calls != want {
		t.Errorf("new session:\n got %q\nwant %q", calls, want)
	}
	if !strings.Contains(out, "claude dir="+filepath.Join(home, ".claude-x")+" pwd="+project+" args=--resume two words") {
		t.Errorf("claude did not run in the window:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(root, "session"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, calls = run()
	want = "[has-session][-t][=claude] env=unset\n[new-window][-t][=claude:][-c][" + project + "]" + window + " env=unset\n[attach-session][-t][=claude] env=unset\n"
	if calls != want || !strings.Contains(out, "attached") {
		t.Errorf("existing session:\n got %q\nwant %q\noutput %s", calls, want, out)
	}

	// In place: inside tmux, tmux mode off, or never decided.
	for name, tc := range map[string]struct {
		tmux *config.Tmux
		env  []string
	}{
		"inside tmux": {&config.Tmux{Enabled: true, Session: "claude"}, []string{"TMUX=/tmp/x,1,0"}},
		"off":         {&config.Tmux{Enabled: false}, nil},
		"undecided":   {nil, nil},
	} {
		writeConfig(tc.tmux)
		out, calls := run(tc.env...)
		if calls != "" || !strings.Contains(out, "claude dir="+filepath.Join(home, ".claude-x")+" pwd="+project) {
			t.Errorf("%s: tmux calls %q, output %q", name, calls, out)
		}
	}

	// tmux gone while the mode is on: a warning, then in place. PATH is the
	// fake bin dir alone, so no real tmux can be found.
	writeConfig(&config.Tmux{Enabled: true, Session: "claude"})
	if err := os.Remove(filepath.Join(bin, "tmux")); err != nil {
		t.Fatal(err)
	}
	out, _ = run("PATH=" + bin)
	if !strings.Contains(out, "tmux mode is on, but tmux is not installed; running claude here") || !strings.Contains(out, "claude dir=") {
		t.Errorf("missing tmux:\n%s", out)
	}
}
