package hosts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// sandbox gives a test its own HOME and state dir, and a PATH without any
// real claude or tmux.
func sandbox(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(config.EnvHome, filepath.Join(home, ".julienning"))
	t.Setenv("JULIENNING_BIN_DIR", filepath.Join(home, ".local", "bin"))
	t.Setenv("JULIENNING_VERSIONS_DIR", filepath.Join(home, ".local", "share", "julienning", "versions"))
	t.Setenv("JULIENNING_REMOTE_URL", "")
	t.Setenv("JULIENNING_TOKEN", "")
	t.Setenv("PATH", filepath.Join(home, "bin"))
	t.Setenv("TZ", "")
	prev, prevClaude, prevTmux := claudeVersionOf, findClaude, findTmux
	claudeVersionOf = func(string) *string { v := "2.1.302"; return &v }
	// Only the sandbox's own claude, and never a tmux.
	findClaude = func() (string, error) {
		p := filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(p); err != nil {
			return "", launch.ErrClaudeNotFound
		}
		return p, nil
	}
	findTmux = func() (launch.Tmux, error) { return launch.Tmux{}, launch.ErrTmuxNotFound }
	t.Cleanup(func() { claudeVersionOf, findClaude, findTmux = prev, prevClaude, prevTmux })
	return home
}

func setUp(t *testing.T, url, token string) *config.Config {
	t.Helper()
	cfg, err := config.New("murat")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remote = config.Remote{URL: url, Token: token}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runAgentVerb(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runAgent(cli.Env{Args: args, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut})
	return out.String(), errOut.String(), err
}

func decodeInto(t *testing.T, out string, v any) error {
	t.Helper()
	_, err := hosts.DecodeAnswer([]byte(out), v)
	return err
}

func TestAgentHello(t *testing.T) {
	home := sandbox(t)
	out, _, err := runAgentVerb(t, "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	var h hosts.Hello
	if err := decodeInto(t, out, &h); err != nil {
		t.Fatal(err)
	}
	if h.Proto != hosts.Proto || h.SetUp || h.MachineID != nil || h.Dev != nil || h.Home != home || h.Paths.Claude != nil {
		t.Fatalf("not set up: %+v", h)
	}
	cfg := setUp(t, "https://w.example.dev", "tok")
	cfg.Tmux = &config.Tmux{Enabled: true, Session: "claude"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(home, ".local", "bin", "claude"))
	out, _, _ = runAgentVerb(t, "", "hello")
	h = hosts.Hello{}
	if err := decodeInto(t, out, &h); err != nil {
		t.Fatal(err)
	}
	if !h.SetUp || h.ID() != cfg.MachineID || *h.Dev != "murat" || *h.WorkerURL != "https://w.example.dev" || h.Tmux == nil || !h.Tmux.Enabled {
		t.Fatalf("set up: %+v", h)
	}
	if h.Paths.Claude == nil || *h.Paths.Claude != filepath.Join(home, ".local", "bin", "claude") || *h.ClaudeVersion != "2.1.302" {
		t.Fatalf("claude outside PATH must be found: %+v", h.Paths)
	}
	if strings.Contains(out, "tok") {
		t.Fatal("hello leaked the token")
	}
}

func writeExe(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeWorker answers the agent's Worker checks: healthErr for /healthz,
// tokens maps a token to whether the Worker accepts it.
func fakeWorker(t *testing.T, healthErr error, accept ...string) {
	t.Helper()
	prevClient, prevHealth := agentClient, agentHealth
	agentHealth = func(context.Context, string) error { return healthErr }
	agentClient = func(url, token string) remote.Client {
		f := &remote.Fake{Listing: &remote.Listing{Accounts: []remote.Account{{Email: "claude1@x.io", Nickname: "alpha"}}}}
		if !slices.Contains(accept, token) {
			f.ListErr = &remote.Error{Status: 401, Message: "unauthorized"}
		}
		return f
	}
	t.Cleanup(func() { agentClient, agentHealth = prevClient, prevHealth })
}

func bootstrapReq(t *testing.T, req hosts.BootstrapRequest) (hosts.BootstrapResult, error) {
	t.Helper()
	raw, _ := json.Marshal(req)
	out, _, err := runAgentVerb(t, string(raw), "bootstrap")
	var res hosts.BootstrapResult
	derr := decodeInto(t, out, &res)
	if err != nil && derr == nil {
		t.Fatalf("exit %v without an error body: %s", err, out)
	}
	return res, derr
}

func TestAgentBootstrapFresh(t *testing.T) {
	sandbox(t)
	fakeWorker(t, nil, "team-token")
	res, err := bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev/", Token: "team-token"})
	if err != nil || !res.Created {
		t.Fatalf("%+v %v", res, err)
	}
	cfg, err := config.Load()
	if err != nil || cfg.Dev != "murat" || cfg.Remote.URL != "https://w.example.dev" || cfg.Remote.Token != "team-token" {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if c, err := sharedcache.Load(); err != nil || !c.Contains("claude1@x.io") {
		t.Fatalf("allowlist not saved: %v", err)
	}
	// Idempotent: the machine id is never regenerated.
	res, err = bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev", Token: "team-token"})
	if err != nil || res.Created || res.TokenUpdated {
		t.Fatalf("second run: %+v %v", res, err)
	}
	if again, _ := config.Load(); again.MachineID != cfg.MachineID {
		t.Fatal("machine id regenerated")
	}
}

func TestAgentBootstrapUnreachableSavesNothing(t *testing.T) {
	sandbox(t)
	fakeWorker(t, errors.New("cannot reach the Worker at https://w.example.dev: connection refused"))
	_, err := bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev", Token: "t"})
	var ae *hosts.AgentError
	if !errors.As(err, &ae) || ae.Code != hosts.CodeWorkerUnreachable || !strings.Contains(ae.Message, "connection refused") {
		t.Fatalf("got %v", err)
	}
	if _, err := config.Load(); !errors.Is(err, config.ErrNotSetup) {
		t.Fatalf("something was saved: %v", err)
	}
	fakeWorker(t, nil, "good")
	_, err = bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev", Token: "bad"})
	if !errors.As(err, &ae) || ae.Message != "the Worker rejects the team token" {
		t.Fatalf("rejected token: %v", err)
	}
}

func TestAgentBootstrapRotatedToken(t *testing.T) {
	sandbox(t)
	setUp(t, "https://w.example.dev", "old")
	fakeWorker(t, nil, "new")
	res, err := bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev", Token: "new"})
	if err != nil || !res.TokenRejected || res.TokenUpdated {
		t.Fatalf("rejected: %+v %v", res, err)
	}
	if cfg, _ := config.Load(); cfg.Remote.Token != "old" {
		t.Fatal("token replaced without asking")
	}
	res, err = bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev", Token: "new", ReplaceToken: true})
	if err != nil || !res.TokenUpdated {
		t.Fatalf("replace: %+v %v", res, err)
	}
	if cfg, _ := config.Load(); cfg.Remote.Token != "new" {
		t.Fatal("token not replaced")
	}
}

func TestAgentBootstrapAnotherWorker(t *testing.T) {
	sandbox(t)
	before := setUp(t, "https://other.example.dev", "o")
	fakeWorker(t, nil, "t")
	_, err := bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev", Token: "t"})
	var ae *hosts.AgentError
	if !errors.As(err, &ae) || ae.Code != hosts.CodeWorkerMismatch || !strings.Contains(ae.Message, "https://other.example.dev") {
		t.Fatalf("mismatch: %v", err)
	}
	res, err := bootstrapReq(t, hosts.BootstrapRequest{Dev: "murat", WorkerURL: "https://w.example.dev", Token: "t", ReplaceWorker: true})
	if err != nil || !res.WorkerReplaced {
		t.Fatalf("replace: %+v %v", res, err)
	}
	cfg, _ := config.Load()
	if cfg.Remote.URL != "https://w.example.dev" || cfg.MachineID != before.MachineID {
		t.Fatalf("after: %+v", cfg)
	}
}

func TestAgentBootstrapRejectsBadInput(t *testing.T) {
	sandbox(t)
	for _, req := range []string{`{`, `{"dev":"Bad Name","worker_url":"https://w","token":"t"}`, `{"dev":"murat","worker_url":"ftp://w","token":"t"}`, `{"dev":"murat","worker_url":"https://w"}`} {
		out, _, err := runAgentVerb(t, req, "bootstrap")
		var ae *hosts.AgentError
		if derr := decodeInto(t, out, &hosts.BootstrapResult{}); !errors.As(derr, &ae) || ae.Code != hosts.CodeInvalid || err == nil {
			t.Errorf("%s: %v %v", req, derr, err)
		}
	}
}

func TestAgentUnknownVerb(t *testing.T) {
	sandbox(t)
	out, _, err := runAgentVerb(t, "", "teleport")
	var ae *hosts.AgentError
	var es *cli.ExitStatus
	if derr := decodeInto(t, out, &hosts.Hello{}); !errors.As(derr, &ae) || ae.Code != hosts.CodeProto || !errors.As(err, &es) || es.Code != 1 {
		t.Fatalf("%v %v", derr, err)
	}
}

func TestAgentInventory(t *testing.T) {
	home := sandbox(t)
	out, _, _ := runAgentVerb(t, "", "inventory")
	var ae *hosts.AgentError
	if err := decodeInto(t, out, &hosts.Inventory{}); !errors.As(err, &ae) || ae.Code != hosts.CodeNotSetUp {
		t.Fatalf("not set up: %v", err)
	}
	cfg := setUp(t, "https://w", "t")
	team, mine := filepath.Join(home, ".claude-a"), filepath.Join(home, ".claude-b")
	for dir, email := range map[string]string{team: "claude1@x.io", mine: "me@x.io"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"`+email+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = cfg.Add(config.ConfigDir{Name: "julienning1", Dir: team})
	_ = cfg.Add(config.ConfigDir{Name: "julienning2", Dir: mine})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := config.SetCurrent(config.ConfigDir{Name: "julienning1", Dir: team}); err != nil {
		t.Fatal(err)
	}
	if _, err := sharedcache.SaveEntries([]sharedcache.Entry{{Email: "claude1@x.io", Nickname: "alpha"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, _, _ = runAgentVerb(t, "", "inventory")
	var inv hosts.Inventory
	if err := decodeInto(t, out, &inv); err != nil {
		t.Fatal(err)
	}
	want := []hosts.InventoryConfig{
		{Name: "julienning1", Dir: team, Email: "claude1@x.io", Nickname: "alpha", Shared: true, Current: true},
		{Name: "julienning2", Dir: mine, Email: "me@x.io"},
	}
	if !slices.Equal(inv.Configs, want) {
		t.Fatalf("got %+v", inv.Configs)
	}
}

// captureExec replaces execSelf for a test and returns what it was asked.
func captureExec(t *testing.T) *struct {
	argv, env []string
	cwd       string
} {
	t.Helper()
	got := &struct {
		argv, env []string
		cwd       string
	}{}
	prev := execSelf
	execSelf = func(argv, env []string) error {
		got.argv, got.env = argv, env
		got.cwd, _ = os.Getwd()
		return nil
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { execSelf = prev; _ = os.Chdir(wd) })
	return got
}

func runReq(t *testing.T, req hosts.RunRequest) (string, error) {
	t.Helper()
	arg, err := hosts.EncodeArg(req)
	if err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runAgentVerb(t, "", "run", arg)
	return errOut, err
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func TestAgentRun(t *testing.T) {
	home := sandbox(t)
	cfg := setUp(t, "https://w", "t")
	got := captureExec(t)
	writeExe(t, filepath.Join(home, ".local", "bin", "claude"))

	errOut, err := runReq(t, hosts.RunRequest{MachineID: "000000000000", Argv: []string{"accounts"}, Host: "box"})
	var es *cli.ExitStatus
	if !errors.As(err, &es) || !strings.Contains(errOut, `julienning: box: this is machine "`+cfg.MachineID+`", not "000000000000"`) || got.argv != nil {
		t.Fatalf("machine mismatch: %v %q", err, errOut)
	}

	if _, err := runReq(t, hosts.RunRequest{MachineID: cfg.MachineID, Argv: []string{"accounts", "--json"}, TZ: "Europe/Istanbul", NoColor: true, Host: "box"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.argv, []string{"accounts", "--json"}) {
		t.Errorf("argv: %q", got.argv)
	}
	realHome, _ := filepath.EvalSymlinks(home)
	if c, _ := filepath.EvalSymlinks(got.cwd); c != realHome {
		t.Errorf("a command without a project runs in the home: %s", got.cwd)
	}
	if envValue(got.env, "TZ") != "Europe/Istanbul" || envValue(got.env, "NO_COLOR") != "1" {
		t.Errorf("zone and color: %q", got.env)
	}
	if p := envValue(got.env, "PATH"); !strings.Contains(p, ":"+filepath.Join(home, ".local", "bin")+":"+filepath.Join(home, "bin")) {
		t.Errorf("claude's dir must come before the inherited PATH: %s", p)
	}

	// The controller's checkout is found here by its repository.
	src := t.TempDir()
	gitRepo(t, filepath.Join(src, "Code", "shop"), "git@github.com:team/shop.git")
	gitRepo(t, filepath.Join(home, "src", "shop"), "https://github.com/team/shop")
	if err := os.MkdirAll(filepath.Join(home, "src", "shop", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "Code", "shop", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := hosts.ProjectOf(filepath.Join(src, "Code", "shop", "web"), src)
	if _, err := runReq(t, hosts.RunRequest{MachineID: cfg.MachineID, Argv: []string{"use"}, Project: p, Host: "box"}); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(home, "src", "shop", "web"))
	if c, _ := filepath.EvalSymlinks(got.cwd); c != want || envValue(got.env, "PWD") != filepath.Join(home, "src", "shop", "web") {
		t.Errorf("checkout: %s / PWD %s", got.cwd, envValue(got.env, "PWD"))
	}

	// No counterpart: pin fails, use runs in the home after a note.
	none := hosts.ProjectOf(filepath.Join(src, "notes"), src)
	got.argv = nil
	errOut, err = runReq(t, hosts.RunRequest{MachineID: cfg.MachineID, Argv: []string{"pin", "alpha"}, Project: none, Host: "box"})
	if err == nil || !strings.Contains(errOut, "julienning: box: box has no ~/notes") || got.argv != nil {
		t.Errorf("strict: %v %q", err, errOut)
	}
	errOut, err = runReq(t, hosts.RunRequest{MachineID: cfg.MachineID, Argv: []string{"use"}, Project: none, Host: "box"})
	if err != nil || !strings.Contains(errOut, "julienning: note: ~/notes has no checkout on box (it is outside a git repository, so only the same path is looked for); running in ~ there") {
		t.Errorf("note: %v %q", err, errOut)
	}
}

func gitRepo(t *testing.T, dir, origin string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+origin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunEnvKeepsTheHostZoneForAnUnknownOne(t *testing.T) {
	sandbox(t)
	env := runEnv([]string{"TZ=Asia/Tokyo", "PATH=/bin"}, hosts.RunRequest{TZ: "Not/AZone"}, "/tmp")
	if envValue(env, "TZ") != "Asia/Tokyo" {
		t.Fatalf("%q", env)
	}
	env = runEnv([]string{"TZ=Asia/Tokyo", "PATH=/bin"}, hosts.RunRequest{TZ: "Europe/Istanbul"}, "/tmp")
	if envValue(env, "TZ") != "Europe/Istanbul" || strings.Count(strings.Join(env, "\n"), "TZ=") != 1 {
		t.Fatalf("%q", env)
	}
}
