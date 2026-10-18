package sshx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
)

func TestArgs(t *testing.T) {
	tg := Target{Name: "box", SSH: "devbox.example.com", Julienning: "/home/dev/.local/bin/julienning"}
	got := Args("/home/me/.julienning", tg, true, "exec '/home/dev/.local/bin/julienning' agent hello")
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ControlMaster=auto",
		"-o", "ControlPath=/home/me/.julienning/ssh/%C", "-o", "ControlPersist=60", "-t", "--",
		"devbox.example.com", "exec '/home/dev/.local/bin/julienning' agent hello"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := Args("/s", tg, false, "x"); !slices.Contains(got, "-T") || slices.Contains(got, "-t") {
		t.Fatalf("no terminal: %q", got)
	}
	// The destination always follows "--", even one that looks like an
	// option (hosts.ValidSSH refuses those anyway).
	if got := Args("/s", Target{SSH: "-oProxyCommand=x"}, false, "c"); got[len(got)-3] != "--" {
		t.Fatalf("-- missing: %q", got)
	}
	// A socket path that would not fit (with ssh's temporary suffix) runs
	// without a control master.
	long := "/" + strings.Repeat("d", 60)
	if got := Args(long, tg, false, "x"); slices.Contains(got, "ControlMaster=auto") {
		t.Fatalf("long socket path kept the control master: %q", got)
	}
	edge := "/" + strings.Repeat("d", maxSocketPath-hashLen-tempSuffix-len("/ssh/")-1)
	if got := Args(edge, tg, false, "x"); !slices.Contains(got, "ControlMaster=auto") {
		t.Fatalf("a path of exactly %d bytes lost the control master", maxSocketPath)
	}
}

func TestAgentCommand(t *testing.T) {
	if got := AgentCommand("/home/dev/.local/bin/julienning", "run", "eyJ4IjoxfQ"); got != "exec '/home/dev/.local/bin/julienning' agent run eyJ4IjoxfQ" {
		t.Fatal(got)
	}
}

// fakeSSH installs an ssh that logs its argv and environment, then answers
// as $FAKE_SSH_SCRIPT says (it is run with sh, its stdin is ssh's).
func fakeSSH(t *testing.T, script string) (log string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.EnvHome, filepath.Join(dir, "state"))
	log = filepath.Join(dir, "log")
	bin := filepath.Join(dir, "ssh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + log + "'\nenv | grep '^JULIENNING_' >> '" + log + ".env'\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := Binary
	Binary = bin
	t.Cleanup(func() { Binary = prev })
	return log
}

var box = Target{Name: "box", SSH: "devbox.example.com", Julienning: "/home/dev/.local/bin/julienning"}

func TestAgentDecodesThroughNoise(t *testing.T) {
	log := fakeSSH(t, `cat > /dev/null; echo "Last login: today"; echo 'JULIENNING-AGENT/1'; echo '{"proto":1,"version":"0.10.0","os":"linux"}'`)
	t.Setenv("JULIENNING_TOKEN", "must-not-reach-ssh")
	var h hosts.Hello
	if err := Agent(context.Background(), box, "hello", map[string]string{"a": "b"}, &h, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if h.Version != "0.10.0" || h.OS != "linux" {
		t.Fatalf("%+v", h)
	}
	raw, _ := os.ReadFile(log)
	if !strings.HasSuffix(string(raw), "-T\n--\ndevbox.example.com\nexec '/home/dev/.local/bin/julienning' agent hello\n") {
		t.Fatalf("argv:\n%s", raw)
	}
	if env, _ := os.ReadFile(log + ".env"); len(env) != 0 {
		t.Fatalf("JULIENNING_* reached ssh: %s", env)
	}
	if fi, err := os.Stat(filepath.Join(os.Getenv(config.EnvHome), SocketDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir: %v %v", fi, err)
	}
}

func TestAgentFailures(t *testing.T) {
	cases := []struct {
		script string
		check  func(error) bool
		msg    string
	}{
		{`echo 'ssh: connect to host devbox.example.com port 22: Connection refused' >&2; exit 255`,
			func(err error) bool { var e *UnreachableError; return errors.As(err, &e) },
			"cannot reach box (ssh: connect to host devbox.example.com port 22: Connection refused); check with: ssh devbox.example.com"},
		{`echo 'julienning: unknown command "agent" (run julienning help)' >&2; exit 2`,
			func(err error) bool { var e *NoAgentError; return errors.As(err, &e) },
			"julienning on box is too old to answer this machine (no agent command); update it with: julienning host add box"},
		{`echo "sh: 1: exec: /home/dev/.local/bin/julienning: not found" >&2; exit 127`,
			func(err error) bool { var e *NoAnswerError; return errors.As(err, &e) },
			"box: no answer from julienning at /home/dev/.local/bin/julienning (run: julienning host add box to repair the entry): sh: 1: exec: /home/dev/.local/bin/julienning: not found"},
		{`echo 'JULIENNING-AGENT/1'; echo '{"error":{"code":"NOT_SET_UP","message":"julienning is not set up"}}'; exit 1`,
			func(err error) bool {
				var e *hosts.AgentError
				return errors.As(err, &e) && e.Code == hosts.CodeNotSetUp
			},
			"julienning is not set up"},
		{`sleep 5`,
			func(err error) bool { return err != nil },
			"box did not answer in time (context deadline exceeded); check with: ssh devbox.example.com"},
	}
	for _, c := range cases {
		fakeSSH(t, c.script)
		var h hosts.Hello
		err := Agent(context.Background(), box, "hello", nil, &h, 500*time.Millisecond)
		if !c.check(err) || err.Error() != c.msg {
			t.Errorf("%s:\n got %v\nwant %s", c.script, err, c.msg)
		}
	}
}

func TestScriptAndExitCodes(t *testing.T) {
	fakeSSH(t, `exec /bin/sh -s`)
	out, code, err := Script(context.Background(), box, "echo hi; exit 3\n", 5*time.Second, nil)
	if err != nil || code != 3 || string(out) != "hi\n" {
		t.Fatalf("%q %d %v", out, code, err)
	}
}
