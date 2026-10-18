package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestSplitHost(t *testing.T) {
	cases := []struct {
		args, rest []string
		host, err  string
	}{
		{args: []string{"accounts", "--host", "box"}, rest: []string{"accounts"}, host: "box"},
		{args: []string{"--host=box", "use", "alpha"}, rest: []string{"use", "alpha"}, host: "box"},
		{args: []string{"use", "--host", "local"}, rest: []string{"use"}},
		{args: []string{"login", "alpha", "--", "--host", "x"}, rest: []string{"login", "alpha", "--", "--host", "x"}},
		{args: []string{"login", "--host=box", "alpha", "--", "--host=y"}, rest: []string{"login", "alpha", "--", "--host=y"}, host: "box"},
		{args: []string{"use", "--host", "a", "--host", "b"}, err: "--host is given twice"},
		{args: []string{"use", "--host=a", "--host=a"}, err: "--host is given twice"},
		{args: []string{"use", "--host"}, err: "--host needs a host name"},
		{args: []string{"use", "--host="}, err: "--host needs a host name"},
		{args: []string{"use", "--host", "--"}, err: "--host needs a host name"},
	}
	for _, c := range cases {
		host, rest, err := splitHost(c.args)
		switch {
		case c.err != "":
			if err == nil || err.Error() != c.err {
				t.Errorf("%q: got %v, want %s", c.args, err, c.err)
			}
		case err != nil || host != c.host || !slices.Equal(rest, c.rest):
			t.Errorf("%q: got %q %q %v", c.args, host, rest, err)
		}
	}
}

func TestMainHostModes(t *testing.T) {
	var forwarded []string
	var sourced string
	prevForward := Forward
	Forward = func(env Env, host string, c *Command) int {
		forwarded = append([]string{host, c.Name}, env.Args...)
		return 7
	}
	defer func() { Forward = prevForward }()
	for _, c := range []*Command{
		{Name: "t-forward", Usage: "t-forward", Host: HostForward, Run: func(Env) error { t.Error("forwarded command ran here"); return nil }},
		{Name: "t-source", Usage: "t-source", Host: HostSource, Run: func(env Env) error { sourced = env.Host; return nil }},
		{Name: "t-local", Usage: "t-local add|list", Host: HostLocal, Run: func(Env) error { return nil }},
		{Name: "t-internal", Usage: "t-internal", Hidden: true, Run: func(Env) error { return nil }},
	} {
		Register(c)
		defer delete(registry, c.Name)
	}
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Main(args, strings.NewReader(""), &out, &errOut)
		return code, errOut.String()
	}
	if code, _ := run("t-forward", "x", "--host", "box", "y"); code != 7 || !slices.Equal(forwarded, []string{"box", "t-forward", "x", "y"}) {
		t.Errorf("forward: %d %q", code, forwarded)
	}
	if code, _ := run("t-source", "--host=box"); code != 0 || sourced != "box" {
		t.Errorf("source: %d %q", code, sourced)
	}
	if code, _ := run("t-source", "--host", "local"); code != 0 || sourced != "" {
		t.Errorf("--host local: %d %q", code, sourced)
	}
	if code, msg := run("t-local", "list", "--host", "box"); code != ExitUsage || !strings.Contains(msg, "julienning: t-local list does not take --host") {
		t.Errorf("local: %d %q", code, msg)
	}
	if code, msg := run("t-internal", "--host", "box"); code != ExitUsage || !strings.Contains(msg, "t-internal does not take --host") {
		t.Errorf("internal: %d %q", code, msg)
	}
	if code, msg := run("version", "--host", "box"); code != ExitUsage || !strings.Contains(msg, "version does not take --host") {
		t.Errorf("version: %d %q", code, msg)
	}
	if code, msg := run("t-forward", "--host", "a", "--host", "b"); code != ExitUsage || !strings.Contains(msg, "--host is given twice") {
		t.Errorf("twice: %d %q", code, msg)
	}
}

func TestExitStatusIsSilent(t *testing.T) {
	Register(&Command{Name: "t-exit", Usage: "t-exit", Run: func(Env) error { return Exit(3) }})
	defer delete(registry, "t-exit")
	var out, errOut bytes.Buffer
	if code := Main([]string{"t-exit"}, strings.NewReader(""), &out, &errOut); code != 3 || errOut.Len() != 0 {
		t.Fatalf("%d %q", code, errOut.String())
	}
}
