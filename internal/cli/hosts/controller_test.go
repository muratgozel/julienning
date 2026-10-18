package hosts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/sshx"
)

// fakeHost answers agent verbs in process: handlers by verb, each given the
// request and returning the answer (or an error, as the agent would).
type fakeHost struct {
	t        *testing.T
	handlers map[string]func(req json.RawMessage) (any, error)
	calls    []string
	forwards [][]string
	scripts  []string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	f := &fakeHost{t: t, handlers: map[string]func(json.RawMessage) (any, error){}}
	prevCall, prevRun, prevForward, prevInteractive := agentCall, runScript, runForwarded, isInteractive
	prevLocal, prevList, prevClose := localVersion, listCall, closeSocket
	agentCall = func(ctx context.Context, t sshx.Target, verb string, req, resp any) error {
		f.calls = append(f.calls, verb)
		h, ok := f.handlers[verb]
		if !ok {
			return fmt.Errorf("unexpected verb %s", verb)
		}
		raw, _ := json.Marshal(req)
		v, err := h(raw)
		if err != nil {
			return err
		}
		out, _ := json.Marshal(v)
		return json.Unmarshal(out, resp)
	}
	listCall = func(ctx context.Context, t sshx.Target, verb string, resp any) error {
		return agentCall(ctx, t, verb, nil, resp)
	}
	runForwarded = func(ctx context.Context, c sshx.Call) (int, error) {
		arg := strings.TrimPrefix(c.Command, sshx.AgentCommand(c.Target.Julienning, "run")+" ")
		var req hosts.RunRequest
		if err := hosts.DecodeArg(arg, &req); err != nil {
			f.t.Fatalf("forwarded command line: %v", err)
		}
		f.forwards = append(f.forwards, req.Argv)
		return 0, nil
	}
	runScript = func(ctx context.Context, t sshx.Target, script string, timeout time.Duration) (scriptResult, error) {
		f.scripts = append(f.scripts, script)
		if strings.Contains(script, "JULIENNING-PROBE") && !strings.Contains(script, "-lc") {
			return scriptResult{out: []byte("JULIENNING-PROBE/1\nuname=Linux x86_64\nuser=dev\nhome=/home/dev\nshell=/bin/bash\nos_name=Debian GNU/Linux 13 (trixie)\nserver=100.64.0.7\npath_julienning=/home/dev/.local/bin/julienning\npath_claude=/home/dev/.local/bin/claude\npath_tmux=/usr/bin/tmux\npath_bash=/usr/bin/bash\npath_curl=/usr/bin/curl\npath_tar=/usr/bin/tar\npath_sha256sum=/usr/bin/sha256sum\npath_shasum=/usr/bin/shasum\n")}, nil
		}
		return scriptResult{}, nil
	}
	isInteractive = func(cli.Env) bool { return false }
	localVersion = func() string { return "0.10.0" }
	closeSocket = func(sshx.Target) {}
	t.Cleanup(func() {
		agentCall, runScript, runForwarded, isInteractive = prevCall, prevRun, prevForward, prevInteractive
		localVersion, listCall, closeSocket = prevLocal, prevList, prevClose
	})
	return f
}

func str(s string) *string { return &s }

func helloFor(id, ver, dev string) hosts.Hello {
	h := hosts.Hello{Proto: hosts.Proto, Version: ver, OS: "linux", Arch: "amd64", Home: "/home/dev", Managed: true,
		Paths: hosts.HelloPaths{Julienning: "/home/dev/.local/bin/julienning"}}
	if id != "" {
		h.MachineID, h.Dev, h.WorkerURL, h.SetUp = str(id), str(dev), str("https://w.example.dev"), true
		h.Tmux = &hosts.TmuxMode{Enabled: false}
	}
	return h
}

func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runHost(cli.Env{Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut})
	return out.String(), errOut.String(), err
}

func saveHost(t *testing.T, h hosts.Host) {
	t.Helper()
	hf, err := hosts.Load()
	if err != nil {
		t.Fatal(err)
	}
	hf.Put(h)
	if err := hf.Save(); err != nil {
		t.Fatal(err)
	}
}

func boxEntry(id string) hosts.Host {
	return hosts.Host{Name: "box", SSH: "devbox.example.com", MachineID: id, Home: "/home/dev", OS: "linux", Arch: "amd64",
		Paths: hosts.Paths{Julienning: "/home/dev/.local/bin/julienning"}}
}

func TestVersionRule(t *testing.T) {
	sandbox(t)
	newFakeHost(t)
	h := boxEntry("9be1c0a4d2f3")
	env := cli.Env{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	check := func(local, remote string, mode versionMode) error {
		localVersion = func() string { return local }
		hello := helloFor("9be1c0a4d2f3", remote, "murat")
		_, err := checkVersions(context.Background(), env, newAsker(env, false), h, &hello, mode)
		return err
	}
	for _, ok := range [][2]string{{"0.10.0", "0.10.3"}, {"0.10.0", "0.10.1-rc.1"}, {"dev", "0.9.0"}, {"0.10.0", "v0.3.0-4-gabc1234"}} {
		if err := check(ok[0], ok[1], versionsStrict); err != nil {
			t.Errorf("%s / %s: %v", ok[0], ok[1], err)
		}
	}
	if err := check("dev", "0.9.0", versionsStrict); err != nil || !strings.Contains(env.Stderr.(*bytes.Buffer).String(), "this machine runs a development build (dev); only the agent protocol is checked against box") {
		t.Errorf("dev warning: %v", err)
	}
	err := check("0.10.0", "0.9.2", versionsStrict)
	if err == nil || err.Error() != "box runs julienning 0.9.2 and this machine 0.10.0 (cross-machine commands need the same major.minor); update box: julienning update --host box --version v0.10.0" {
		t.Errorf("host older: %v", err)
	}
	err = check("0.10.0", "0.11.1", versionsStrict)
	if err == nil || err.Error() != "box runs julienning 0.11.1 and this machine 0.10.0 (cross-machine commands need the same major.minor); update this machine: julienning update --version v0.11.1" {
		t.Errorf("host newer: %v", err)
	}
	if err := check("0.10.0", "0.9.2", versionsLenient); err != nil {
		t.Errorf("lenient: %v", err)
	}
	// Another protocol in the same minor is still refused.
	localVersion = func() string { return "0.10.0" }
	hello := helloFor("9be1c0a4d2f3", "0.10.1", "murat")
	hello.Proto = 2
	if _, err := checkVersions(context.Background(), env, newAsker(env, false), h, &hello, versionsStrict); err == nil || !strings.Contains(err.Error(), "agent protocol 2") {
		t.Errorf("proto: %v", err)
	}
	// An unmanaged host is never offered the installer.
	hello = helloFor("9be1c0a4d2f3", "0.9.0", "murat")
	hello.Managed, hello.Paths.Julienning = false, "/opt/x/julienning"
	if _, err := checkVersions(context.Background(), env, &asker{env: env, yes: true}, h, &hello, versionsStrict); err == nil || !strings.Contains(err.Error(), "which the installer did not put there; update it by hand to 0.10") {
		t.Errorf("unmanaged: %v", err)
	}
}

// The terminal answers: yes updates this machine, which then asks to run
// the command again.
func TestVersionRuleUpdatesThisMachine(t *testing.T) {
	sandbox(t)
	newFakeHost(t)
	var updated string
	prev := updateHere
	updateHere = func(ctx context.Context, tag string) error { updated = tag; return nil }
	t.Cleanup(func() { updateHere = prev })
	var out bytes.Buffer
	env := cli.Env{Stdin: strings.NewReader("\n"), Stdout: &out, Stderr: &bytes.Buffer{}}
	a := newAsker(env, false)
	a.interactive = true
	hello := helloFor("9be1c0a4d2f3", "0.11.1", "murat")
	_, err := checkVersions(context.Background(), env, a, boxEntry("9be1c0a4d2f3"), &hello, versionsStrict)
	if !errors.Is(err, errUpdatedHere) || updated != "v0.11.1" {
		t.Fatalf("%v %q", err, updated)
	}
	for _, want := range []string{"box runs julienning 0.11.1 and this machine 0.10.0; cross-machine commands need the same 0.11. Update this machine to 0.11.1 now? [Y/n] ", "Updated 0.10.0 → 0.11.1; run the command again."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
}

func TestForwardGuards(t *testing.T) {
	sandbox(t)
	f := newFakeHost(t)
	setUp(t, "https://w.example.dev", "tok")
	c := &cli.Command{Name: "setup", Usage: "setup", Host: cli.HostForward}
	var errOut bytes.Buffer
	env := cli.Env{Args: []string{"--token", "secret"}, Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &errOut}
	if code := forward(env, "box", c); code != cli.ExitUsage || !strings.Contains(errOut.String(), "--token is never sent on a host's command line") {
		t.Errorf("--token: %d %q", code, errOut.String())
	}
	errOut.Reset()
	env.Args = nil
	if code := forward(env, "box", &cli.Command{Name: "accounts"}); code != cli.ExitError || !strings.Contains(errOut.String(), `unknown host "box" (see julienning host list; add it with: julienning host add box SSH_DEST)`) {
		t.Errorf("unknown host: %d %q", code, errOut.String())
	}
	saveHost(t, boxEntry("9be1c0a4d2f3"))
	f.handlers["hello"] = func(json.RawMessage) (any, error) { return helloFor("", "0.10.0", ""), nil }
	errOut.Reset()
	if code := forward(env, "box", &cli.Command{Name: "accounts"}); code != cli.ExitError || !strings.Contains(errOut.String(), "julienning is not set up on box; run: julienning host add box") {
		t.Errorf("not set up: %d %q", code, errOut.String())
	}
	if code := forward(env, "box", &cli.Command{Name: "setup"}); code != 0 || len(f.forwards) != 1 || f.forwards[0][0] != "setup" {
		t.Errorf("setup on a host without team settings: %d %v", code, f.forwards)
	}
	f.handlers["hello"] = func(json.RawMessage) (any, error) { return helloFor("77aa01c3e9f0", "0.10.0", "murat"), nil }
	errOut.Reset()
	if code := forward(env, "box", &cli.Command{Name: "accounts"}); code != cli.ExitError || !strings.Contains(errOut.String(), "box answers as machine 77aa01c3e9f0, not 9be1c0a4d2f3: it was rebuilt, or devbox.example.com reaches another machine now") {
		t.Errorf("machine changed: %d %q", code, errOut.String())
	}
}

func TestNeedsProject(t *testing.T) {
	for argv, want := range map[string]bool{
		"use": true, "use alpha": true, "next --all": true, "login alpha": true, "new-config": true,
		"pin alpha": true, "pin": true, "pin --list": false, "unpin": true, "unpin ~/x": false,
		"accounts": false, "setup --yes": false, "dev murat": false,
	} {
		if got := needsProject(strings.Fields(argv)); got != want {
			t.Errorf("%q: got %v", argv, got)
		}
	}
}

func TestHostAddRefusals(t *testing.T) {
	sandbox(t)
	f := newFakeHost(t)
	setUp(t, "https://w.example.dev", "tok")
	f.handlers["hello"] = func(json.RawMessage) (any, error) { return helloFor("9be1c0a4d2f3", "0.10.0", "murat"), nil }

	var ue *cli.UsageError
	for _, args := range [][]string{{"add"}, {"add", "Box"}, {"add", "local"}, {"add", "box", "-oProxyCommand=x"}, {"add", "box", "a b"}, {"add", "a", "b", "c"}} {
		if _, _, err := run(t, args...); !errors.As(err, &ue) {
			t.Errorf("%q: want a usage error, got %v", args, err)
		}
	}

	// The same machine under another name.
	gpu := boxEntry("9be1c0a4d2f3")
	gpu.Name, gpu.SSH = "gpu", "gpu.example.com"
	saveHost(t, gpu)
	if _, _, err := run(t, "add", "box", "devbox.example.com"); err == nil || err.Error() != "devbox.example.com is the same machine as gpu; remove that entry first: julienning host remove gpu" {
		t.Fatalf("same machine: %v", err)
	}
	hf, _ := hosts.Load()
	hf.Remove("gpu")
	_ = hf.Save()

	// Questions that default to no are refused under --yes, and are errors
	// without a terminal.
	saveHost(t, boxEntry("77aa01c3e9f0"))
	if _, _, err := run(t, "add", "box", "devbox.example.com", "--yes"); err == nil || err.Error() != "kept the entry for box as it was; nothing changed" {
		t.Fatalf("changed machine id under --yes: %v", err)
	}
	if _, _, err := run(t, "add", "box", "devbox.example.com"); err == nil || !strings.Contains(err.Error(), "box now answers as machine 9be1c0a4d2f3 (was 77aa01c3e9f0)") || !strings.Contains(err.Error(), "no terminal to ask") {
		t.Fatalf("changed machine id without a terminal: %v", err)
	}
	if _, _, err := run(t, "add", "box", "other.example.com", "--yes"); err == nil || err.Error() != "kept box pointing at devbox.example.com; nothing changed" {
		t.Fatalf("another destination under --yes: %v", err)
	}
	if h, _ := mustLoad(t).Find("box"); h.MachineID != "77aa01c3e9f0" || h.SSH != "devbox.example.com" {
		t.Fatalf("the entry changed: %+v", h)
	}
	for _, verb := range f.calls {
		if verb == "bootstrap" {
			t.Fatal("the team token was sent to a machine that was refused")
		}
	}
}

func mustLoad(t *testing.T) *hosts.Hosts {
	t.Helper()
	hf, err := hosts.Load()
	if err != nil {
		t.Fatal(err)
	}
	return hf
}

// A set-up host on another Worker is only switched after a yes; the token
// travels in bootstrap's request, and a rotated one is replaced after a yes.
func TestHostAddBootstrapQuestions(t *testing.T) {
	sandbox(t)
	f := newFakeHost(t)
	setUp(t, "https://w.example.dev", "tok")
	hello := helloFor("9be1c0a4d2f3", "0.10.0", "murat")
	hello.WorkerURL = str("https://other.example.dev")
	f.handlers["hello"] = func(json.RawMessage) (any, error) { return hello, nil }
	var reqs []hosts.BootstrapRequest
	f.handlers["bootstrap"] = func(raw json.RawMessage) (any, error) {
		var r hosts.BootstrapRequest
		_ = json.Unmarshal(raw, &r)
		reqs = append(reqs, r)
		if !r.ReplaceToken {
			return hosts.BootstrapResult{TokenRejected: true}, nil
		}
		return hosts.BootstrapResult{TokenUpdated: true}, nil
	}
	if _, _, err := run(t, "add", "box", "devbox.example.com", "--yes"); err == nil || err.Error() != "box keeps its Worker (https://other.example.dev); hosts must use this machine's, so it was not added" {
		t.Fatalf("other Worker: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatal("bootstrap ran before the Worker question was answered")
	}
	hello.WorkerURL = str("https://w.example.dev")
	out, _, err := run(t, "add", "box", "devbox.example.com", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 || reqs[0].Token != "tok" || reqs[0].ReplaceToken || !reqs[1].ReplaceToken {
		t.Fatalf("requests: %+v", reqs)
	}
	for _, want := range []string{"Host:     box → devbox.example.com (100.64.0.7): Debian GNU/Linux 13 (trixie), linux/amd64, user dev, home /home/dev",
		"Julienning: 0.10.0 at ~/.local/bin/julienning", "Team:     sent this machine's team token to box", "Setup:    complete on box", "Added box (devbox.example.com)."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	h, ok := mustLoad(t).Find("box")
	if !ok || h.MachineID != "9be1c0a4d2f3" || h.AddedAt.IsZero() || *h.Paths.Tmux != "/usr/bin/tmux" {
		t.Fatalf("entry: %+v", h)
	}
}

func TestHostListStatuses(t *testing.T) {
	sandbox(t)
	f := newFakeHost(t)
	saveHost(t, boxEntry("9be1c0a4d2f3"))
	f.handlers["hello"] = func(json.RawMessage) (any, error) {
		h := helloFor("9be1c0a4d2f3", "0.9.2", "murat")
		h.TmuxVersion = str("3.5a")
		h.ClaudeVersion = str("2.1.302")
		return h, nil
	}
	f.handlers["inventory"] = func(json.RawMessage) (any, error) {
		return hosts.Inventory{Configs: []hosts.InventoryConfig{
			{Name: "julienning1", Email: "claude1@x.io", Nickname: "alpha", Shared: true, Current: true},
			{Name: "julienning2", Email: "claude2@x.io", Shared: true},
			{Name: "default", Email: "me@x.io"},
			{Name: "julienning3"},
		}}, nil
	}
	out, _, err := run(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOST", "box", "linux/amd64", "0.9.2", "2.1.302", "3.5a (off)", "*alpha, claude2@x.io, +1 personal", "julienning 0.9.2 (this machine 0.10.0)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if h, _ := mustLoad(t).Find("box"); h.CheckedAt.IsZero() {
		t.Error("checked_at not updated")
	}
	f.handlers["hello"] = func(json.RawMessage) (any, error) {
		return nil, &sshx.UnreachableError{Target: sshx.Target{Name: "box"}, Detail: "Connection refused"}
	}
	out, _, _ = run(t, "list", "--json")
	if !strings.Contains(out, `"status": "unreachable (Connection refused)"`) || !strings.Contains(out, `"error": "cannot reach box (Connection refused)`) {
		t.Errorf("unreachable:\n%s", out)
	}
	out, _, _ = run(t, "remove", "box")
	if out != "Removed box from this machine's hosts; julienning on box is unchanged (remove it there first with: julienning uninstall --host box)\n" {
		t.Errorf("remove: %q", out)
	}
	if _, _, err := run(t, "remove", "box"); err == nil || !strings.Contains(err.Error(), `unknown host "box"`) {
		t.Errorf("remove twice: %v", err)
	}
	out, _, _ = run(t, "list")
	if out != "No hosts. Add one with: julienning host add NAME [SSH_DEST]\n" {
		t.Errorf("empty: %q", out)
	}
}

// The fallback carries the binary inside the script; /bin/sh (dash on
// Debian reads its script in blocks) must decode it and run it.
func TestFallbackScriptRuns(t *testing.T) {
	dir := t.TempDir()
	bin := []byte("#!/bin/sh\ncase \"$1\" in version) echo 'julienning 0.10.0' ;; agent) echo 'JULIENNING-AGENT/1'; echo '{\"version\":\"0.10.0\",\"link\":\"/home/dev/.local/bin/julienning\"}' ;; esac\n" +
		strings.Repeat("# padding to span several lines of base64 and more than one read block\n", 200))
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + dir, "HOME=" + dir}
	cmd.Stdin = strings.NewReader(fallbackScript(bin))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var res hosts.InstallSelfResult
	if _, err := hosts.DecodeAnswer(out, &res); err != nil || res.Link != "/home/dev/.local/bin/julienning" || !strings.Contains(string(out), "julienning 0.10.0") {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "julienning.*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestInstallerScript(t *testing.T) {
	t.Setenv("JULIENNING_RELEASES_BASE", "http://127.0.0.1:9999")
	t.Setenv("JULIENNING_REPO", "")
	p := hosts.Probe{Paths: map[string]string{"bash": "/usr/bin/bash", "curl": "/opt/nix/bin/curl", "tar": "/usr/bin/tar", "sha256sum": "/usr/bin/sha256sum"}}
	s := installerScript(p, "v0.10.0")
	for _, want := range []string{
		"PATH='/usr/bin:/opt/nix/bin':\"$PATH\"",
		"JULIENNING_RELEASES_BASE='http://127.0.0.1:9999'; export JULIENNING_RELEASES_BASE",
		"'/opt/nix/bin/curl' -fsSL 'http://127.0.0.1:9999/raw/main/scripts/install.sh' -o \"$tmp\"",
		"'/usr/bin/bash' \"$tmp\" 'v0.10.0'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "JULIENNING_REPO") {
		t.Error("an unset override was passed")
	}
}
