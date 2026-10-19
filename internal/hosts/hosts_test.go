package hosts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
)

func sampleHost(name, id string) Host {
	claude := "/home/dev/.local/bin/claude"
	return Host{
		Name: name, SSH: name + ".example.com", MachineID: id, Home: "/home/dev", OS: "linux", Arch: "amd64",
		Paths:   Paths{Julienning: "/home/dev/.local/bin/julienning", Claude: &claude},
		AddedAt: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), CheckedAt: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC),
	}
}

func TestHostsRoundTripSortedAndPrivate(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	f, err := Load()
	if err != nil || len(f.Hosts) != 0 {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	f.Put(sampleHost("gpu", "77aa01c3e9f0"))
	f.Put(sampleHost("box", "9be1c0a4d2f3"))
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	p, _ := Path()
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("hosts.json mode: %v %v", fi.Mode(), err)
	}
	g, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Hosts) != 2 || g.Hosts[0].Name != "box" || g.Hosts[1].Name != "gpu" {
		t.Fatalf("not sorted by name: %+v", g.Hosts)
	}
	if h, ok := g.ByMachineID("77aa01c3e9f0"); !ok || h.Name != "gpu" || h.Paths.Tmux != nil || *h.Paths.Claude != "/home/dev/.local/bin/claude" {
		t.Fatalf("ByMachineID: %+v", h)
	}
	if !g.Remove("gpu") || g.Remove("gpu") {
		t.Fatal("Remove")
	}
}

// An older julienning must never drop what a newer one added.
func TestHostsKeepsUnknownFields(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	p, _ := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"version": 1, "future": {"x": 1}, "hosts": [{"name": "box", "ssh": "box", "machine_id": "9be1c0a4d2f3", "home": "/home/dev",
"os": "linux", "arch": "arm64", "paths": {"julienning": "/home/dev/.local/bin/julienning", "claude": null, "tmux": null, "git": "/usr/bin/git"},
"added_at": "2026-10-18T09:00:00Z", "checked_at": "2026-10-18T09:00:00Z", "labels": ["gpu"]}]}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := f.Find("box")
	h.CheckedAt = h.CheckedAt.Add(time.Hour)
	f.Put(h)
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	for _, want := range []string{`"future"`, `"labels"`, `"git": "/usr/bin/git"`, `"checked_at": "2026-10-18T10:00:00Z"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("lost %s:\n%s", want, got)
		}
	}
}

func TestHostValidation(t *testing.T) {
	ok := sampleHost("box", "9be1c0a4d2f3")
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Host){
		"local name":      func(h *Host) { h.Name = "local" },
		"upper name":      func(h *Host) { h.Name = "Box" },
		"dash dest":       func(h *Host) { h.SSH = "-oProxyCommand=evil" },
		"space dest":      func(h *Host) { h.SSH = "dev box" },
		"control dest":    func(h *Host) { h.SSH = "box\x1b" },
		"empty dest":      func(h *Host) { h.SSH = "" },
		"machine id":      func(h *Host) { h.MachineID = "XYZ" },
		"relative home":   func(h *Host) { h.Home = "home" },
		"windows":         func(h *Host) { h.OS = "windows" },
		"armv7":           func(h *Host) { h.Arch = "arm" },
		"quote in path":   func(h *Host) { h.Paths.Julienning = "/o'neil/julienning" },
		"backslash path":  func(h *Host) { h.Paths.Julienning = `/a\b/julienning` },
		"relative path":   func(h *Host) { h.Paths.Julienning = "bin/julienning" },
		"relative claude": func(h *Host) { c := "claude"; h.Paths.Claude = &c },
	}
	for name, mutate := range bad {
		h := ok
		mutate(&h)
		if err := h.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	f := &Hosts{Hosts: []Host{sampleHost("box", "9be1c0a4d2f3"), sampleHost("gpu", "9be1c0a4d2f3")}}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "same machine id") {
		t.Errorf("duplicate machine id: %v", err)
	}
}

func TestNormalizeRemote(t *testing.T) {
	want := "github.com/muratgozel/julienning"
	for _, in := range []string{
		"git@github.com:muratgozel/julienning.git",
		"https://github.com/muratgozel/julienning",
		"https://github.com/muratgozel/julienning.git/",
		"ssh://git@GitHub.com:22/muratgozel/julienning.git",
		"https://user:pw@github.com:443/muratgozel/julienning",
		"github.com:muratgozel/julienning",
		"git://github.com/muratgozel/julienning.git",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("%q: got %q", in, got)
		}
	}
	if got := NormalizeRemote("/srv/git/shop.git"); got != "/srv/git/shop" {
		t.Errorf("local path: %q", got)
	}
	if got := NormalizeRemote(""); got != "" {
		t.Errorf("empty: %q", got)
	}
}

// git writes a repository at dir by hand: HEAD on branch, remotes in config.
func gitRepo(t *testing.T, dir, branch string, remotes ...string) {
	t.Helper()
	g := filepath.Join(dir, ".git")
	if err := os.MkdirAll(g, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(g, "HEAD"), "ref: refs/heads/"+branch+"\n")
	cfg := "[core]\n\tbare = false\n"
	for i := 0; i+1 < len(remotes); i += 2 {
		cfg += "[remote \"" + remotes[i] + "\"]\n\turl = " + remotes[i+1] + "\n\tfetch = +refs/heads/*:refs/remotes/x/*\n"
	}
	write(t, filepath.Join(g, "config"), cfg)
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectOf(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "Code", "shop")
	gitRepo(t, repo, "feat/x", "upstream", "https://example.com/team/shop-old.git", "origin", "git@github.com:team/shop.git")
	sub := filepath.Join(repo, "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := ProjectOf(sub, home)
	if p.Checkout != repo || p.GitRoot != repo || p.Sub != "web" || p.Folder != "shop" || p.Branch != "feat/x" {
		t.Errorf("checkout fields: %+v", p)
	}
	if strings.Join(p.Remotes, " ") != "github.com/team/shop example.com/team/shop-old" || p.Repo != "shop" || p.CloneURL != "git@github.com:team/shop.git" {
		t.Errorf("remotes, origin first: %+v", p)
	}
	if !p.InHome || p.HomeRel != "Code/shop/web" {
		t.Errorf("home: %+v", p)
	}
	if rel, ok := p.CheckoutHomeRel(); !ok || rel != "Code/shop" {
		t.Errorf("checkout rel: %q %v", rel, ok)
	}

	// A linked worktree reads the shared repository's config.
	wt := filepath.Join(home, "wt", "shop-fix")
	gitdir := filepath.Join(repo, ".git", "worktrees", "shop-fix")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
	write(t, filepath.Join(gitdir, "commondir"), "../..\n")
	write(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/fix\n")
	w := ProjectOf(wt, home)
	if w.Checkout != wt || w.GitRoot != repo || w.Branch != "fix" || w.Remotes[0] != "github.com/team/shop" {
		t.Errorf("linked worktree: %+v", w)
	}

	plain := ProjectOf(filepath.Join(home, "notes"), home)
	if plain.InRepo() || plain.HomeRel != "notes" {
		t.Errorf("outside git: %+v", plain)
	}
	outside := ProjectOf("/tmp/elsewhere", home)
	if outside.InHome {
		t.Errorf("outside home: %+v", outside)
	}
}

// claudeDir writes a registered config dir whose newest session in one
// project folder ran in cwd.
func claudeDir(t *testing.T, dir, cwd string) {
	t.Helper()
	folder := filepath.Join(dir, "projects", strings.ReplaceAll(cwd, "/", "-"))
	write(t, filepath.Join(folder, "s1.jsonl"), `{"type":"user","cwd":"`+cwd+`","message":{"role":"user","content":"hi"}}`+"\n")
}

func TestLocateSearchOrder(t *testing.T) {
	src := t.TempDir()
	repo := filepath.Join(src, "Code", "shop")
	gitRepo(t, repo, "main", "origin", "git@github.com:team/shop.git")
	if err := os.MkdirAll(filepath.Join(repo, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := ProjectOf(filepath.Join(repo, "web"), src)

	home := t.TempDir()
	o := LocateOptions{Home: home, Machine: "box", RanIn: "the session ran in ~/Code/shop/web here"}

	// Nothing there.
	_, err := Locate(p, o)
	var nf *NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("empty home: %v", err)
	}
	if want := "box has no checkout of github.com/team/shop under " + home + " (checked 0 projects Claude used there and folders named shop)"; err.Error() != want {
		t.Errorf("not found:\n got %s\nwant %s", err, want)
	}

	// A folder named like the repository but another repository: names
	// nominate, the remote decides.
	gitRepo(t, filepath.Join(home, "src", "shop"), "main", "origin", "git@github.com:other/shop.git")
	if _, err := Locate(p, o); !errors.As(err, &nf) {
		t.Fatalf("another repository counted: %v", err)
	}

	// The bounded walk finds the right one, by URL in another form.
	found := filepath.Join(home, "work", "a", "shop")
	gitRepo(t, found, "main", "origin", "https://github.com/team/shop")
	if err := os.MkdirAll(filepath.Join(found, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	loc, err := Locate(p, o)
	if err != nil || loc.Cwd != filepath.Join(found, "web") || loc.How != "search" || loc.Checkout != found {
		t.Fatalf("walk: %+v %v", loc, err)
	}

	// Too deep for the walk (depth 4).
	deep := filepath.Join(home, "a", "b", "c", "d", "shop")
	gitRepo(t, deep, "main", "origin", "https://github.com/team/shop")
	if loc, _ := Locate(p, o); loc.Checkout == deep {
		t.Fatal("the walk went deeper than 4")
	}

	// The same path relative to home wins over the walk.
	same := filepath.Join(home, "Code", "shop")
	gitRepo(t, same, "main", "origin", "git@github.com:team/shop.git")
	if err := os.MkdirAll(filepath.Join(same, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if loc, err := Locate(p, o); err != nil || loc.How != "same-path" || loc.Checkout != same {
		t.Fatalf("same path: %+v %v", loc, err)
	}

	// A project Claude used there wins over both, and stops the search.
	used := filepath.Join(home, "projects", "shop-checkout")
	gitRepo(t, used, "main", "origin", "git@github.com:team/shop.git")
	if err := os.MkdirAll(filepath.Join(used, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(home, ".claude-x")
	claudeDir(t, cdir, filepath.Join(used, "web"))
	o.ConfigDirs = []string{cdir}
	if loc, err := Locate(p, o); err != nil || loc.How != "claude" || loc.Checkout != used {
		t.Fatalf("claude project: %+v %v", loc, err)
	}

	// The sub folder must exist.
	if err := os.RemoveAll(filepath.Join(used, "web")); err != nil {
		t.Fatal(err)
	}
	var pm *PathMissingError
	if _, err := Locate(p, o); !errors.As(err, &pm) || !strings.Contains(err.Error(), "box has ~/projects/shop-checkout but not its web folder (the session ran in ~/Code/shop/web here)") {
		t.Fatalf("missing sub folder: %v", err)
	}
}

func TestLocateChoosesAmongSeveral(t *testing.T) {
	src := t.TempDir()
	repo := filepath.Join(src, "Code", "shop")
	gitRepo(t, repo, "main", "origin", "git@github.com:team/shop.git")
	p := ProjectOf(repo, src)
	home := t.TempDir()
	a, b := filepath.Join(home, "x", "shop"), filepath.Join(home, "y", "shop")
	gitRepo(t, a, "main", "origin", "git@github.com:team/shop.git")
	gitRepo(t, b, "feat/x", "origin", "git@github.com:team/shop.git")

	o := LocateOptions{Home: home, Machine: "box"}
	var ae *AmbiguousError
	if _, err := Locate(p, o); !errors.As(err, &ae) || len(ae.Checkouts) != 2 || ae.Checkouts[0] != "~/x/shop" {
		t.Fatalf("ambiguous: %v", err)
	}
	o.Branch = "feat/x"
	if loc, err := Locate(p, o); err != nil || loc.Checkout != b || loc.Branch != "feat/x" {
		t.Fatalf("branch: %+v %v", loc, err)
	}
	o.Branch = ""
	o.Pick = func(c []string) (int, error) { return 1, nil }
	if loc, err := Locate(p, o); err != nil || loc.Checkout != b {
		t.Fatalf("picker: %+v %v", loc, err)
	}
}

func TestLocateWalkBounds(t *testing.T) {
	src := t.TempDir()
	repo := filepath.Join(src, "shop")
	gitRepo(t, repo, "main", "origin", "git@github.com:team/shop.git")
	p := ProjectOf(repo, src)
	home := t.TempDir()
	for _, skip := range []string{".hidden", "node_modules", "vendor", "target"} {
		gitRepo(t, filepath.Join(home, skip, "shop"), "main", "origin", "git@github.com:team/shop.git")
	}
	// A symlink to a checkout is never followed.
	target := filepath.Join(t.TempDir(), "shop")
	gitRepo(t, target, "main", "origin", "git@github.com:team/shop.git")
	if err := os.Symlink(filepath.Dir(target), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	var nf *NotFound
	if _, err := Locate(p, LocateOptions{Home: home, Machine: "box"}); !errors.As(err, &nf) {
		t.Fatalf("skipped folders were searched: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "plain", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := walkFolders
	walkFolders = 1
	t.Cleanup(func() { walkFolders = prev })
	if _, err := Locate(p, LocateOptions{Home: home, Machine: "box"}); !errors.As(err, &nf) || !nf.Stopped || !strings.Contains(err.Error(), "the search stopped after 5s") {
		t.Fatalf("bound: %v", err)
	}
}

func TestLocateWithoutARepository(t *testing.T) {
	src := t.TempDir()
	p := ProjectOf(filepath.Join(src, "scratch", "notes"), src)
	home := t.TempDir()
	var nf *NotFound
	_, err := Locate(p, LocateOptions{Home: home, Machine: "box"})
	if !errors.As(err, &nf) || !strings.Contains(err.Error(), "box has no ~/scratch/notes") {
		t.Fatalf("missing: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "scratch", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if loc, err := Locate(p, LocateOptions{Home: home, Machine: "box"}); err != nil || loc.How != "path" || loc.Cwd != filepath.Join(home, "scratch", "notes") {
		t.Fatalf("same path: %+v %v", loc, err)
	}
	out := ProjectOf("/var/tmp/x", src)
	if _, err := Locate(out, LocateOptions{Home: home, Machine: "box"}); !errors.As(err, &nf) {
		t.Fatalf("outside home maps nowhere: %v", err)
	}
}

func TestProbeParsing(t *testing.T) {
	out := []byte("Welcome to box!\nJULIENNING-PROBE/1\nuname=Linux x86_64\nuser=dev\nhome=/home/dev\nshell=/bin/bash\nos_name=Debian GNU/Linux 13 (trixie)\nserver=100.64.0.7\npath_julienning=\npath_tmux=/usr/bin/tmux\npath_claude=claude\n")
	p, err := ParseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	if p.User != "dev" || p.Home != "/home/dev" || p.OSName != "Debian GNU/Linux 13 (trixie)" || p.Server != "100.64.0.7" || p.Paths["tmux"] != "/usr/bin/tmux" {
		t.Fatalf("%+v", p)
	}
	if _, ok := p.Paths["claude"]; ok {
		t.Error("a relative path was kept")
	}
	if os, arch, err := p.Platform("box"); err != nil || os != "linux" || arch != "amd64" {
		t.Fatalf("platform: %s %s %v", os, arch, err)
	}
	p.Uname = "Linux armv7l"
	if _, _, err := p.Platform("box"); err == nil || err.Error() != "box is linux/armv7l; julienning releases exist for amd64 and arm64 only" {
		t.Fatalf("armv7: %v", err)
	}
	p.Uname = "Darwin arm64"
	if os, arch, _ := p.Platform("box"); os != "darwin" || arch != "arm64" {
		t.Fatal("darwin")
	}
	if _, err := ParseProbe([]byte("no marker\n")); err == nil {
		t.Fatal("no marker accepted")
	}
	login := ParseLoginPaths([]byte("motd\nJULIENNING-PROBE/1\nT=julienning\nT=claude\n/home/dev/.local/bin/claude\nT=tmux\n"))
	if len(login) != 1 || login["claude"] != "/home/dev/.local/bin/claude" {
		t.Fatalf("login paths: %v", login)
	}
}

// The probe script runs in /bin/sh and finds tools outside PATH.
func TestProbeScriptRuns(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".local", "bin", "claude"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(home, ".local", "bin", "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runSh(t, home, ProbeScript())
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p, err := ParseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	if p.Home != home || p.Paths["claude"] != filepath.Join(home, ".local", "bin", "claude") || p.Paths["julienning"] != "" {
		t.Fatalf("%+v", p)
	}
}

func TestDisplayZone(t *testing.T) {
	t.Setenv("TZ", "Europe/Istanbul")
	if z := DisplayZone(); z != "Europe/Istanbul" {
		t.Errorf("TZ: %q", z)
	}
	t.Setenv("TZ", "EST5EDT,M3.2.0,M11.1.0x")
	if z := DisplayZone(); z != "" {
		t.Errorf("POSIX rule: %q", z)
	}
	t.Setenv("TZ", "")
	if z := DisplayZone(); z != "UTC" {
		t.Errorf("empty TZ: %q", z)
	}
	os.Unsetenv("TZ")
	dir := t.TempDir()
	zoneinfo := filepath.Join(dir, "usr", "share", "zoneinfo", "America")
	write(t, filepath.Join(zoneinfo, "New_York"), "TZif")
	link := filepath.Join(dir, "localtime")
	if err := os.Symlink(filepath.Join(zoneinfo, "New_York"), link); err != nil {
		t.Fatal(err)
	}
	prev := localtimeLink
	localtimeLink = link
	t.Cleanup(func() { localtimeLink = prev })
	if z := DisplayZone(); z != "America/New_York" {
		t.Errorf("link: %q", z)
	}
	localtimeLink = filepath.Join(dir, "missing")
	if z := DisplayZone(); z != "" {
		t.Errorf("no link: %q", z)
	}
}

func TestFrames(t *testing.T) {
	out := append([]byte("motd from .bashrc\n"), EncodeAnswer(Hello{Proto: 1, Version: "0.10.0"})...)
	var h Hello
	if proto, err := DecodeAnswer(out, &h); err != nil || proto != 1 || h.Version != "0.10.0" {
		t.Fatalf("hello: %+v %d %v", h, proto, err)
	}
	out = EncodeAnswer(Errorf(CodeNotSetUp, "julienning is not set up"))
	var ae *AgentError
	if _, err := DecodeAnswer(out, &h); !errors.As(err, &ae) || ae.Code != CodeNotSetUp {
		t.Fatalf("error body: %v", err)
	}
	if _, err := DecodeAnswer([]byte("bash: julienning: command not found\n"), &h); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("no frame: %v", err)
	}
	// A newer agent's hello still decodes: it is frozen across protos.
	newer := []byte("JULIENNING-AGENT/2\n{\"proto\":2,\"version\":\"0.11.0\",\"extra\":true}\n")
	if proto, err := DecodeAnswer(newer, &h); err != nil || proto != 2 || h.Proto != 2 {
		t.Fatalf("newer hello: %d %v", proto, err)
	}
	arg, err := EncodeArg(RunRequest{MachineID: "9be1c0a4d2f3", Argv: []string{"use", "it's; rm -rf ~"}})
	if err != nil || strings.Trim(arg, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		t.Fatalf("argument must be inert in any shell: %q %v", arg, err)
	}
	var back RunRequest
	if err := DecodeArg(arg, &back); err != nil || back.Argv[1] != "it's; rm -rf ~" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

// An error that is not an *AgentError keeps its message in the answer.
func TestEncodeAnswerKeepsPlainErrors(t *testing.T) {
	out := EncodeAnswer(errors.New("several project folders could hold the memory"))
	var h Hello
	_, err := DecodeAnswer(out, &h)
	var ae *AgentError
	if !errors.As(err, &ae) || ae.Code != CodeInvalid || ae.Message != "several project folders could hold the memory" {
		t.Fatalf("got %v", err)
	}
}
