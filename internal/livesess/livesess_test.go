package livesess

import (
	"os"
	"path/filepath"
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

func TestCountsAsSession(t *testing.T) {
	for kind, want := range map[string]bool{"": true, "interactive": true, "bg": true, "daemon": false, "daemon-worker": false} {
		if got := CountsAsSession(Session{Kind: kind}); got != want {
			t.Errorf("kind %q: got %v", kind, got)
		}
	}
	if CountsAsSession(Session{Kind: "interactive", Spare: true}) {
		t.Error("spare must not count")
	}
}

func TestParseStartBothOrders(t *testing.T) {
	a, ok1 := parseStart("Tue Sep 29 12:21:04 2026")
	b, ok2 := parseStart("Tue 29 Sep  12:21:04 2026   ")
	if !ok1 || !ok2 || !a.Equal(b) {
		t.Fatalf("%v %v %v %v", a, ok1, b, ok2)
	}
}
