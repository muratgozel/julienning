package hosts

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/cli/switching"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/tui"
)

func TestClaudeVersionRule(t *testing.T) {
	for v, want := range map[string]string{"2.1.303": "2.1", "v2.0.9 (Claude Code)": "2.0", "2.1.303 (Claude Code)": "2.1", "": "", "next": ""} {
		if got := claudeMinor(v); got != want {
			t.Errorf("%q: got %q, want %q", v, got, want)
		}
	}
	here := side{local: true, claude: "2.1.303"}
	box := side{host: hosts.Host{Name: "box"}, claude: "2.1.290"}
	if err := sameClaude(here, box); err != nil {
		t.Errorf("patch releases may differ: %v", err)
	}
	box.claude = "2.0.14"
	err := sameClaude(here, box)
	want := "This machine runs Claude Code 2.1.303 and box 2.0.14; a transfer needs the same major.minor on both machines (patch releases may differ); update Claude Code on box (claude update) and run the transfer again"
	if err == nil || err.Error() != want {
		t.Errorf("minor mismatch:\n got %v\nwant %s", err, want)
	}
	box.claude = "3.0.0"
	if err := sameClaude(here, box); err == nil || !strings.Contains(err.Error(), "update Claude Code on this machine") {
		t.Errorf("the older side is updated: %v", err)
	}
	box.claude = ""
	if err := sameClaude(here, box); err != nil {
		t.Errorf("an unknown version is not a mismatch: %v", err)
	}
}

func TestResumeCommand(t *testing.T) {
	home := sandbox(t)
	box := side{host: hosts.Host{Name: "box"}, home: "/home/dev"}
	c := switching.Choice{Config: config.ConfigDir{Name: "julienning1", Dir: "/home/dev/.claude-x"}, Nickname: "alpha"}
	if got := resumeCommand(box, c, "id1"); got != "claude-alpha --resume id1" {
		t.Errorf("nickname: %s", got)
	}
	c.Nickname = ""
	if got := resumeCommand(box, c, "id1"); got != "CLAUDE_CONFIG_DIR=~/.claude-x claude --resume id1" {
		t.Errorf("no nickname: %s", got)
	}
	c.Config.Dir = "/home/dev/.claude"
	c.Nickname = "alpha"
	if got := resumeCommand(box, c, "id1"); got != "command claude --resume id1" {
		t.Errorf("default dir on a host: %s", got)
	}
	here := side{local: true, home: home}
	c.Config.Dir = filepath.Join(home, ".claude")
	if got := resumeCommand(here, c, "id1"); got != "command claude --resume id1" {
		t.Errorf("default dir here: %s", got)
	}
}

func TestSplitHeader(t *testing.T) {
	header := `{"proto":1,"id":"x"}` + "\n"
	body := "\x1f\x8b binary"
	h, rest, err := splitHeader(strings.NewReader("Welcome!\n{noise}\n"+hosts.Frame()+"\n"+header+body), true)
	if err != nil || string(h) != header {
		t.Fatalf("framed: %q %v", h, err)
	}
	if b, _ := io.ReadAll(rest); string(b) != body {
		t.Fatalf("body: %q", b)
	}
	h, _, err = splitHeader(strings.NewReader(header+body), false)
	if err != nil || string(h) != header {
		t.Fatalf("in process: %q %v", h, err)
	}
	_, _, err = splitHeader(strings.NewReader(hosts.Frame()+"\n"+`{"error":{"code":"LIVE","message":"session \"x\" is open"}}`+"\n"), true)
	var ae *hosts.AgentError
	if !errors.As(err, &ae) || ae.Code != hosts.CodeLive {
		t.Fatalf("error answer: %v", err)
	}
	if _, _, err := splitHeader(strings.NewReader("bash: julienning: not found\n"), true); err == nil {
		t.Fatal("no frame accepted")
	}
}

func TestPickDestination(t *testing.T) {
	sandbox(t)
	newFakeHost(t)
	env := cli.Env{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if _, err := pickDestination(env, &hosts.Hosts{}, side{local: true}); err == nil || err.Error() != "no other machine to transfer to; add one with: julienning host add NAME [SSH_DEST]" {
		t.Fatalf("none: %v", err)
	}
	var shown []tui.Item
	var header string
	prev := pickRow
	pickRow = func(env cli.Env, items []tui.Item, o tui.Options) (int, error) {
		shown, header = items, o.Header
		return 0, nil
	}
	t.Cleanup(func() { pickRow = prev })
	hf := &hosts.Hosts{Hosts: []hosts.Host{boxEntry("9be1c0a4d2f3"), {Name: "gpu", SSH: "gpu.example.com", OS: "linux", Arch: "arm64"}}}
	got, err := pickDestination(env, hf, side{host: hf.Hosts[0]})
	if err != nil || got != hosts.Local || header != "Transfer a session from box to:" || len(shown) != 2 || shown[0].Title != "this machine" || shown[1].Meta != "gpu.example.com · linux/arm64" {
		t.Fatalf("from box: %q %v %q %+v", got, err, header, shown)
	}
	pickRow = func(cli.Env, []tui.Item, tui.Options) (int, error) { return 0, tui.ErrCanceled }
	if _, err := pickDestination(env, hf, side{local: true}); !errors.Is(err, errCanceled) {
		t.Fatalf("Esc: %v", err)
	}
}

func TestTransferNeedsATerminal(t *testing.T) {
	sandbox(t)
	newFakeHost(t)
	var out, errOut bytes.Buffer
	err := runTransfer(cli.Env{Args: []string{"--to", "box"}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut})
	if err == nil || err.Error() != "transfer asks which session to move and needs a terminal" {
		t.Fatalf("got %v", err)
	}
}

// The agent moves sessions only in and out of registered config dirs.
func TestAgentTransferVerbsStayInRegisteredDirs(t *testing.T) {
	home := sandbox(t)
	setUp(t, "https://w", "t")
	out, _, _ := runAgentVerb(t, `{"id":"7e1f0c2a-5b6d-4e7f-8a9b-0c1d2e3f4a5b","config_dir":"`+filepath.Join(home, ".claude-x")+`"}`, "export")
	var ae *hosts.AgentError
	if err := decodeInto(t, out, &struct{}{}); !errors.As(err, &ae) || ae.Code != hosts.CodeInvalid || !strings.Contains(ae.Message, "is not a registered config dir here") {
		t.Fatalf("export: %v", err)
	}
	out, _, _ = runAgentVerb(t, `{"config_dir":"/etc","email":"x@y","cwd":"/tmp"}`+"\n", "receive")
	if err := decodeInto(t, out, &struct{}{}); !errors.As(err, &ae) || !strings.Contains(ae.Message, "/etc is not a registered config dir here") {
		t.Fatalf("receive: %v", err)
	}
	out, _, _ = runAgentVerb(t, `{"id":"x","config_dir":"/etc","manifest_sha256":"0"}`, "release")
	if err := decodeInto(t, out, &struct{}{}); !errors.As(err, &ae) || ae.Code != hosts.CodeInvalid {
		t.Fatalf("release: %v", err)
	}
	arg, _ := hosts.EncodeArg(hosts.LaunchRequest{MachineID: "000000000000", Dir: "/etc", Args: []string{"--resume", "x"}, Host: "box"})
	_, errOut, err := runAgentVerb(t, "", "launch", arg)
	if err == nil || !strings.Contains(errOut, "julienning: box: /etc is not a registered config dir here") {
		t.Fatalf("launch: %v %q", err, errOut)
	}
}

func TestTransferItemsBlockAnotherClaudeMinor(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ls := hosts.SessionsAnswer{
		Sessions: []sessions.Session{
			{ID: "aaaaaaaa-1", ConfigName: "julienning1", Title: "Same minor", ClaudeVersion: "2.1.290", LastActive: now},
			{ID: "bbbbbbbb-2", ConfigName: "julienning1", Title: "Older minor", ClaudeVersion: "2.0.14", LastActive: now},
			{ID: "cccccccc-3", ConfigName: "julienning1", Title: "Unknown", LastActive: now},
			{ID: "dddddddd-4", ConfigName: "julienning1", Title: "Open", ClaudeVersion: "2.0.1", Live: true, LastActive: now},
		},
		Labels: map[string]string{"julienning1": "alpha"},
	}
	items := transferItems(ls, side{local: true}, side{host: hosts.Host{Name: "box"}, claude: "2.1.303"}, false, now, time.UTC)
	if items[0].Disabled || items[2].Disabled {
		t.Errorf("a patch difference or an unknown version blocks: %+v", items)
	}
	if !items[1].Disabled || items[1].Note != "(written by Claude Code 2.0; box runs 2.1)" {
		t.Errorf("another minor: %+v", items[1])
	}
	if items[3].Note != "(open in another terminal)" {
		t.Errorf("an open session keeps its own note: %+v", items[3])
	}
	if !strings.HasPrefix(items[0].Meta, "alpha · ") {
		t.Errorf("label: %q", items[0].Meta)
	}
}
