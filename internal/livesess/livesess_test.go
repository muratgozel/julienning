package livesess

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListSkipsKeysMalformedAndDead(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "sessions")
	os.MkdirAll(reg, 0o700)
	os.WriteFile(filepath.Join(reg, "100.json"), []byte(`{"pid":100,"sessionId":"a","cwd":"/x","name":"n","status":"idle"}`), 0o644)
	os.WriteFile(filepath.Join(reg, "200.json"), []byte(`{"pid":200,"sessionId":"b"}`), 0o644)
	os.WriteFile(filepath.Join(reg, "300.json"), []byte(`not json`), 0o644)
	os.WriteFile(filepath.Join(reg, "400.json"), []byte(`{"pid":999,"sessionId":"mismatch"}`), 0o644)
	os.WriteFile(filepath.Join(reg, "100.abc.key"), []byte(`{"peerToken":"secret"}`), 0o600)

	old := Alive
	t.Cleanup(func() { Alive = old })
	Alive = func(s Session) bool { return s.PID == 100 }

	got, err := List(dir)
	if err != nil || len(got) != 1 || got[0].SessionID != "a" || got[0].ConfigDir != dir {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if !RegistryExists(dir) || RegistryExists(t.TempDir()) {
		t.Fatal("RegistryExists")
	}
	if s, err := List(t.TempDir()); s != nil || err != nil {
		t.Fatalf("missing registry: %v %v", s, err)
	}
}

func TestAliveRealProcess(t *testing.T) {
	if !Alive(Session{PID: os.Getpid()}) {
		t.Fatal("own pid should be alive")
	}
	start, ok := psStart(os.Getpid())
	if !ok {
		t.Skip("ps unavailable")
	}
	s := Session{PID: os.Getpid(), ProcStart: start.Format("Mon Jan 2 15:04:05 2006")}
	if !Alive(s) {
		t.Fatal("matching procStart should be alive")
	}
	s.ProcStart = start.Add(-time.Hour).Format("Mon Jan 2 15:04:05 2006")
	if Alive(s) {
		t.Fatal("different procStart means pid reuse")
	}
}

// fakeProc points procRoot at a temp tree holding a stat file per pid.
func fakeProc(t *testing.T, stats map[int]string) {
	t.Helper()
	root := t.TempDir()
	old := procRoot
	t.Cleanup(func() { procRoot = old })
	procRoot = root
	for pid, stat := range stats {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeDomain makes local the pid namespace Alive compares pidDomain with.
func fakeDomain(t *testing.T, local string) {
	t.Helper()
	old := localDomain
	t.Cleanup(func() { localDomain = old })
	localDomain = func() string { return local }
}

// statLine is a /proc/<pid>/stat line in state with starttime (field 22)
// start, with a command name that holds spaces and ")".
func statLine(pid int, state, start string) string {
	return strconv.Itoa(pid) + " (claude (x) y) " + state + " 1 2 3 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 " + start + " 1234 56\n"
}

// liveChild starts a process that stays alive for the test, standing in for
// a running claude.
func liveChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd.Process.Pid
}

// deadPID returns the pid of a process that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// Linux registry entries carry procStart as /proc clock ticks; a reused pid
// was taken for the session's own process because those never parsed.
func TestAliveLinuxTicks(t *testing.T) {
	// This process started after the session's, as julienning starts after
	// the claude it inspects.
	pid, self := liveChild(t), os.Getpid()
	selfStat := statLine(self, "R", "12500000")
	fakeProc(t, map[int]string{pid: statLine(pid, "S", "12098273"), self: selfStat})
	if !Alive(Session{PID: pid, ProcStart: "12098273"}) {
		t.Fatal("matching starttime should be alive")
	}
	if Alive(Session{PID: pid, ProcStart: "11967812"}) {
		t.Fatal("different starttime means pid reuse")
	}
	if !Alive(Session{PID: pid, ProcStart: "1792441862347"}) {
		t.Fatal("a number past our own start is no tick count; it must keep the entry alive")
	}
	for _, state := range []string{"Z", "X"} {
		fakeProc(t, map[int]string{pid: statLine(pid, state, "12098273"), self: selfStat})
		if Alive(Session{PID: pid, ProcStart: "12098273"}) {
			t.Errorf("state %s: an exited, unreaped process is not alive", state)
		}
	}
	for name, stat := range map[string]string{
		"no stat file":     "",
		"no parenthesis":   "123 claude S 1 2 3",
		"too few fields":   strconv.Itoa(pid) + " (claude) S 1 2 3\n",
		"non-numeric tick": statLine(pid, "S", "soon"),
	} {
		stats := map[int]string{self: selfStat}
		if stat != "" {
			stats[pid] = stat
		}
		fakeProc(t, stats)
		if !Alive(Session{PID: pid, ProcStart: "12098273"}) {
			t.Errorf("%s: an unreadable starttime must keep the entry alive", name)
		}
	}
	fakeProc(t, map[int]string{pid: statLine(pid, "S", "12098273")})
	if !Alive(Session{PID: pid, ProcStart: "11967812"}) {
		t.Fatal("without our own start a mismatch cannot be judged; it must keep the entry alive")
	}
}

// A pid from another pid namespace says nothing about this one's processes;
// Claude Code treats such an entry as unproven, and so does Alive.
func TestAlivePIDDomain(t *testing.T) {
	pid, self := liveChild(t), os.Getpid()
	fakeProc(t, map[int]string{pid: statLine(pid, "S", "12098273"), self: statLine(self, "R", "12500000")})
	for _, c := range []struct {
		local, entry string
		alive        bool
	}{
		{":abc:pid:[42]", "", false}, // older versions: judged here
		{":abc:pid:[42]", "linux:abc:pid:[42]", false},
		{":abc:pid:[42]", "wsl:abc:pid:[42]", false},
		{":abc:pid:[42]", "linux:abc:pid:[7]", true},
		{":abc:pid:[42]", "linux:def:pid:[42]", true},
		{":abc:pid:[42]", "darwin", true},
		{"darwin", "darwin", false},
		{"darwin", "linux:abc:pid:[42]", true},
	} {
		fakeDomain(t, c.local)
		// A reused pid by this namespace's /proc: dead unless the domain
		// keeps it.
		s := Session{PID: pid, ProcStart: "11967812", PIDDomain: c.entry}
		if got := Alive(s); got != c.alive {
			t.Errorf("local %q, entry %q: alive=%v, want %v", c.local, c.entry, got, c.alive)
		}
	}
	fakeDomain(t, ":abc:pid:[42]")
	if !Alive(Session{PID: deadPID(t), PIDDomain: "linux:abc:pid:[7]"}) {
		t.Fatal("a foreign entry must stay alive even when its pid is free here")
	}
}

// TestProcStatReal ties field 22 of the real /proc to ps's start time, so a
// wrong field index cannot pass on the fake tree alone.
func TestProcStatReal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux only")
	}
	pid := os.Getpid()
	st, ok := readStat(pid)
	if !ok {
		t.Fatal("cannot read own starttime from /proc")
	}
	if !Alive(Session{PID: pid, ProcStart: strconv.FormatUint(st.start, 10)}) || Alive(Session{PID: pid, ProcStart: strconv.FormatUint(st.start-1, 10)}) {
		t.Fatal("Alive must compare /proc starttime")
	}
	out, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		t.Skip("getconf unavailable")
	}
	hz, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || hz == 0 {
		t.Skipf("CLK_TCK %q", out)
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		t.Skip("no /proc/stat")
	}
	var btime int64
	for _, line := range strings.Split(string(stat), "\n") {
		if v, found := strings.CutPrefix(line, "btime "); found {
			if btime, err = strconv.ParseInt(v, 10, 64); err != nil {
				t.Fatalf("btime: %v", err)
			}
		}
	}
	ps, ok := psStart(pid)
	if btime == 0 || !ok {
		t.Skip("btime or ps unavailable")
	}
	got := time.Unix(btime+int64(st.start/hz), 0)
	if d := got.Sub(ps); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("starttime %d ticks gives %v, ps says %v", st.start, got.UTC(), ps)
	}
}

// A real exited child stays a zombie until reaped; kill(pid, 0) still
// succeeds on it, so only the /proc state tells it apart.
func TestAliveRealZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux only")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd.Wait(); err != nil {
			t.Error(err)
		}
	})
	pid := cmd.Process.Pid
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		st, ok := readStat(pid)
		if ok && st.state == "Z" {
			if Alive(Session{PID: pid, ProcStart: strconv.FormatUint(st.start, 10)}) {
				t.Fatal("a zombie is not alive")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never became a zombie: %+v %v", st, ok)
		}
	}
}

func TestCountsAsSession(t *testing.T) {
	for kind, want := range map[string]bool{"": true, "interactive": true, "bg": true, "daemon": false, "daemon-worker": false} {
		if got := CountsAsSession(Session{Kind: kind}); got != want {
			t.Errorf("kind %q: got %v", kind, got)
		}
	}
	if CountsAsSession(Session{Kind: "interactive", Spare: true}) {
		t.Error("spare must not count")
	}
	// Claude Code 2.1.303 registers `claude -p` as kind "interactive"; the
	// usage probe is told apart by the name it passes with --name.
	if CountsAsSession(Session{Kind: "interactive", Name: ProbeSessionName}) {
		t.Error("julienning's usage probe must not count")
	}
	if !CountsAsSession(Session{Kind: "interactive", Name: "fix the build"}) {
		t.Error("a named session must still count")
	}
}

func TestParseStartBothOrders(t *testing.T) {
	a, ok1 := parseStart("Tue Sep 29 12:21:04 2026")
	b, ok2 := parseStart("Tue 29 Sep  12:21:04 2026   ")
	if !ok1 || !ok2 || !a.Equal(b) {
		t.Fatalf("%v %v %v %v", a, ok1, b, ok2)
	}
}
