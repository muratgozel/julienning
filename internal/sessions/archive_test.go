package sessions

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
)

const moveID = "7e1f0c2a-5b6d-4e7f-8a9b-0c1d2e3f4a5b"

// machine is one side of a cross-machine move: a home with a config dir
// logged into email and a git checkout of the same repository.
type machine struct {
	home, dir, checkout string
}

func put(t *testing.T, p, body string) { writeFile(t, p, body, 0o644, time.Time{}) }

func newMachine(t *testing.T, checkoutRel, dirName, email string) machine {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir()) // physical, as Claude records cwd
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	m := machine{home: home, dir: filepath.Join(home, dirName), checkout: filepath.Join(home, checkoutRel)}
	put(t, filepath.Join(m.dir, ".claude.json"), `{"oauthAccount":{"emailAddress":"`+email+`"}}`)
	if err := os.MkdirAll(filepath.Join(m.dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(m.checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(m.checkout, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	return m
}

// sourceSession writes a session in the source's checkout (its web folder)
// with every kind of item, and returns it as listed.
func sourceSession(t *testing.T, src machine) Session {
	t.Helper()
	cwd := filepath.Join(src.checkout, "web")
	folder := EncodeCwd(cwd)
	mem := EncodeCwd(src.checkout)
	line := func(v map[string]any) string { raw, _ := json.Marshal(v); return string(raw) }
	transcript := strings.Join([]string{
		line(map[string]any{"type": "user", "cwd": cwd, "sessionId": moveID, "version": "2.1.303", "gitBranch": "feat/x", "message": map[string]any{"role": "user", "content": "fix " + cwd + "/app.go"}}),
		line(map[string]any{"type": "file-history-snapshot", "snapshot": map[string]any{"trackedFileBackups": map[string]any{"app.go": map[string]any{"backupFileName": "15eb036d97ac43d1@v1", "realParentDir": cwd}}}}),
		line(map[string]any{"type": "attachment", "path": filepath.Join(src.dir, "projects", mem, "memory") + "/", "cwd": cwd, "sessionId": moveID, "version": "2.1.303"}),
		`not json but ` + src.home + `/notes`,
	}, "\n") + "\n"
	put(t, filepath.Join(src.dir, "projects", folder, moveID+".jsonl"), transcript)
	put(t, filepath.Join(src.dir, "projects", folder, moveID, "subagents", "agent-1.jsonl"), line(map[string]any{"cwd": cwd})+"\n")
	put(t, filepath.Join(src.dir, "file-history", moveID, "15eb036d97ac43d1@v1"), "package main // "+cwd+"\n")
	put(t, filepath.Join(src.dir, "session-env", moveID, "env.sh"), "export X=1\n")
	put(t, filepath.Join(src.dir, "tasks", moveID, "1.json"), `{"subject":"look at `+cwd+`"}`)
	put(t, filepath.Join(src.dir, "todos", moveID+"-agent-"+moveID+".json"), `[]`)
	put(t, filepath.Join(src.dir, "projects", mem, "memory", "MEMORY.md"), "- [Build](build.md) — flags\n")
	put(t, filepath.Join(src.dir, "projects", mem, "memory", "build.md"), "run make in "+src.checkout+"\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(src.dir, "projects", folder, moveID+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	s, err := FindSession(config.ConfigDir{Name: "julienning1", Dir: src.dir}, moveID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// alive makes every registry entry count as a running process.
func alive(t *testing.T) {
	prev := livesess.Alive
	livesess.Alive = func(livesess.Session) bool { return true }
	t.Cleanup(func() { livesess.Alive = prev })
}

func packSource(src machine, s Session) PackSource {
	return PackSource{Home: src.home, Cwd: s.Cwd, Checkout: src.checkout, CheckoutReal: src.checkout, GitRoot: src.checkout}
}

func unpackOpts(dst machine, email string) UnpackOptions {
	return UnpackOptions{Dir: dst.dir, Email: email, Cwd: filepath.Join(dst.checkout, "web"), Checkout: dst.checkout, GitRoot: dst.checkout,
		Home: dst.home, ReceivedDir: filepath.Join(dst.home, ".julienning", "received"), From: "3fa9c2d1e07b", Now: time.Now()}
}

func read(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// tree lists every path under root, for "nothing was left behind" checks.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestPackUnpackRelease(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
	put(t, filepath.Join(dst.dir, "projects", EncodeCwd(dst.checkout), "memory", "MEMORY.md"), "- [Old](old.md) — kept\n")
	s := sourceSession(t, src)
	if s.ClaudeVersion != "2.1.303" || s.GitBranch != "feat/x" {
		t.Fatalf("version and branch: %+v", s)
	}

	var buf bytes.Buffer
	h, msha, err := Pack(s, packSource(src, s), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.Bytes == 0 || h.Source.ProjectFolder != EncodeCwd(s.Cwd) || h.Source.MemoryFolder != EncodeCwd(src.checkout) || h.ClaudeVersion != "2.1.303" {
		t.Fatalf("header: %+v", h)
	}
	if strings.Contains(buf.String(), ".claude.json") {
		t.Fatal("the login travelled")
	}

	res, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io"))
	if err != nil {
		t.Fatal(err)
	}
	dcwd := filepath.Join(dst.checkout, "web")
	folder := EncodeCwd(dcwd)
	if !res.Committed || res.ProjectFolder != folder || res.Rewrites < 8 {
		t.Fatalf("result: %+v", res)
	}
	got := read(t, filepath.Join(dst.dir, "projects", folder, moveID+".jsonl"))
	for _, want := range []string{`"cwd":"` + dcwd + `"`, `"content":"fix ` + dcwd + `/app.go"`, `"realParentDir":"` + dcwd + `"`, `"backupFileName":"15eb036d97ac43d1@v1"`,
		`"path":"` + filepath.Join(dst.dir, "projects", EncodeCwd(dst.checkout), "memory") + `/"`, "not json but " + dst.home + "/notes"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, src.home) {
		t.Errorf("a source path survived:\n%s", got)
	}
	if v := read(t, filepath.Join(dst.dir, "file-history", moveID, "15eb036d97ac43d1@v1")); !strings.Contains(v, s.Cwd) {
		t.Errorf("file-history is a copy of the user's file and must not be rewritten: %s", v)
	}
	if v := read(t, filepath.Join(dst.dir, "tasks", moveID, "1.json")); !strings.Contains(v, dcwd) {
		t.Errorf("task file not rewritten: %s", v)
	}
	if v := read(t, filepath.Join(dst.dir, "projects", folder, moveID, "subagents", "agent-1.jsonl")); !strings.Contains(v, dcwd) {
		t.Errorf("subagent transcript not rewritten: %s", v)
	}
	memDir := filepath.Join(dst.dir, "projects", EncodeCwd(dst.checkout), "memory")
	if v := read(t, filepath.Join(memDir, "build.md")); v != "run make in "+dst.checkout+"\n" {
		t.Errorf("memory file not rewritten: %q", v)
	}
	if v := read(t, filepath.Join(memDir, "MEMORY.md")); v != "- [Old](old.md) — kept\n- [Build](build.md) — flags\n" {
		t.Errorf("MEMORY.md not merged: %q", v)
	}
	if len(res.Memory.Added) != 1 || res.Memory.IndexLines != 1 {
		t.Errorf("memory report: %+v", res.Memory)
	}
	if left, _ := filepath.Glob(filepath.Join(dst.dir, "projects", ".*")); len(left) != 0 {
		t.Errorf("staging left behind: %v", left)
	}
	rc, ok := loadReceipt(filepath.Join(dst.home, ".julienning", "received"), moveID)
	if !ok || rc.ManifestSHA256 != msha || rc.From != "3fa9c2d1e07b" {
		t.Fatalf("receipt: %+v %v", rc, ok)
	}

	// The connection dropped after the commit: the same pack again is
	// "already", and nothing is written twice.
	before := tree(t, dst.dir)
	res, err = Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io"))
	if err != nil || !res.Already || res.Committed {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if after := tree(t, dst.dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatal("the retry wrote something")
	}

	warnings, err := Release(s, msha)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("release: %v %v", warnings, err)
	}
	for _, gone := range []string{filepath.Join("projects", s.ProjectDir, moveID+".jsonl"), filepath.Join("file-history", moveID), filepath.Join("tasks", moveID)} {
		if _, err := os.Lstat(filepath.Join(src.dir, gone)); !os.IsNotExist(err) {
			t.Errorf("release left %s", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(src.dir, "projects", EncodeCwd(src.checkout), "memory", "build.md")); err != nil {
		t.Error("release removed the source's project memory, which is merged, never moved")
	}

	// Once the destination's copy changed, the same pack is a collision.
	put(t, filepath.Join(dst.dir, "projects", folder, moveID+".jsonl"), "resumed\n")
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io")); !errors.Is(err, ErrCollision) {
		t.Fatalf("changed copy: %v", err)
	}
}

func TestReleaseKeepsAChangedSession(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	s := sourceSession(t, src)
	_, msha, err := Pack(s, packSource(src, s), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(src.dir, "projects", s.ProjectDir, moveID+".jsonl")
	if err := os.WriteFile(p, []byte(read(t, p)+`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	if _, err := Release(s, msha); !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "changed on this machine while it was being sent; both copies are kept") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a changed session was deleted")
	}
}

func TestUnpackRefusals(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	s := sourceSession(t, src)
	var buf bytes.Buffer
	if _, _, err := Pack(s, packSource(src, s), &buf); err != nil {
		t.Fatal(err)
	}

	dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "other@x.io")); err == nil || !strings.Contains(err.Error(), "is no longer logged into other@x.io") {
		t.Errorf("another login: %v", err)
	}
	put(t, filepath.Join(dst.dir, "projects", "-elsewhere", moveID+".jsonl"), "{}\n")
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io")); !errors.Is(err, ErrCollision) {
		t.Errorf("same id elsewhere: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(dst.dir, "projects", "-elsewhere"))
	alive(t)
	put(t, filepath.Join(dst.dir, "sessions", "4242.json"), `{"pid":4242,"sessionId":"`+moveID+`","cwd":"/x","kind":"interactive"}`)
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io")); err == nil || !strings.Contains(err.Error(), "is open here") {
		t.Errorf("open here: %v", err)
	}
	_ = os.Remove(filepath.Join(dst.dir, "sessions", "4242.json"))

	// A connection that drops mid-stream leaves nothing.
	before := tree(t, dst.dir)
	cut := buf.Bytes()[:buf.Len()*2/3]
	if _, err := Unpack(bytes.NewReader(cut), unpackOpts(dst, "claude2@x.io")); err == nil || !strings.Contains(err.Error(), "connection may have dropped") {
		t.Errorf("truncated: %v", err)
	}
	if after := tree(t, dst.dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("a truncated archive left files:\n%s", strings.Join(after, "\n"))
	}
}

// craft builds a header line and an archive from raw tar entries.
type rawEntry struct {
	name, link string
	typ        byte
	body       string
}

func craft(t *testing.T, h PackHeader, entries []rawEntry, manifest *Manifest) []byte {
	t.Helper()
	var buf bytes.Buffer
	raw, _ := json.Marshal(h)
	buf.Write(append(raw, '\n'))
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.link, Size: int64(len(e.body)), ModTime: time.Now()}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	if manifest != nil {
		mraw, _ := json.Marshal(manifest)
		_ = tw.WriteHeader(&tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(mraw))})
		_, _ = tw.Write(mraw)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestUnpackRejectsBadArchives(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	folder := EncodeCwd(filepath.Join(src.checkout, "web"))
	mem := EncodeCwd(src.checkout)
	h := PackHeader{Proto: ArchiveFormat, ID: moveID, Bytes: 100, Source: PackSource{Home: src.home, ConfigDir: src.dir, ProjectFolder: folder, MemoryFolder: mem, Cwd: filepath.Join(src.checkout, "web")}}
	jsonl := "projects/" + folder + "/" + moveID + ".jsonl"
	fh := "file-history/" + moveID
	cases := map[string][]rawEntry{
		"parent":       {{name: "../escape", typ: tar.TypeReg, body: "x"}},
		"absolute":     {{name: "/etc/passwd", typ: tar.TypeReg, body: "x"}},
		"dotdot":       {{name: fh + "/../../x", typ: tar.TypeReg, body: "x"}},
		"hard link":    {{name: fh + "/a", typ: tar.TypeLink, link: jsonl}},
		"device":       {{name: fh + "/a", typ: tar.TypeChar}},
		"through link": {{name: fh, typ: tar.TypeDir}, {name: fh + "/l", typ: tar.TypeSymlink, link: "/tmp"}, {name: fh + "/l/x", typ: tar.TypeReg, body: "x"}},
		"credentials":  {{name: ".credentials.json", typ: tar.TypeReg, body: "{}"}},
		"registry":     {{name: "sessions/1.json", typ: tar.TypeReg, body: "{}"}},
		"key":          {{name: fh + "/x.key", typ: tar.TypeReg, body: "k"}},
		"other id":     {{name: "file-history/other/x", typ: tar.TypeReg, body: "x"}},
		"memory link":  {{name: "projects/" + mem + "/memory", typ: tar.TypeDir}, {name: "projects/" + mem + "/memory/x.md", typ: tar.TypeSymlink, link: "/etc/hosts"}},
		"oversize":     {{name: jsonl, typ: tar.TypeReg, body: strings.Repeat("x", 101)}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
			before := tree(t, dst.dir)
			_, err := Unpack(bufio.NewReader(bytes.NewReader(craft(t, h, entries, &Manifest{ID: moveID}))), unpackOpts(dst, "claude2@x.io"))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if after := tree(t, dst.dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("left files:\n%s", strings.Join(after, "\n"))
			}
		})
	}
	// Bytes that do not match the manifest are refused before any rename.
	t.Run("checksum", func(t *testing.T) {
		dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
		before := tree(t, dst.dir)
		m := &Manifest{ID: moveID, Entries: []ManifestEntry{{Path: jsonl, Type: "file", Size: 2, SHA256: strings.Repeat("0", 64)}}}
		_, err := Unpack(bytes.NewReader(craft(t, h, []rawEntry{{name: jsonl, typ: tar.TypeReg, body: "{}"}}, m)), unpackOpts(dst, "claude2@x.io"))
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "does not match the manifest") {
			t.Fatalf("got %v", err)
		}
		if after := tree(t, dst.dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Fatalf("left files:\n%s", strings.Join(after, "\n"))
		}
	})
	t.Run("format", func(t *testing.T) {
		dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
		bad := h
		bad.Proto = 99
		if _, err := Unpack(bytes.NewReader(craft(t, bad, nil, &Manifest{ID: moveID})), unpackOpts(dst, "claude2@x.io")); err == nil || !strings.Contains(err.Error(), "archive format 99") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestPackRefusesAnOpenSession(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	s := sourceSession(t, src)
	alive(t)
	put(t, filepath.Join(src.dir, "sessions", "4242.json"), `{"pid":4242,"sessionId":"`+moveID+`","cwd":"/x","kind":"interactive"}`)
	if _, _, err := Pack(s, packSource(src, s), &bytes.Buffer{}); !errors.Is(err, ErrLive) {
		t.Fatalf("got %v", err)
	}
}

// Another session of the project may write the shared memory while one
// travels: that is not the travelling session changing.
func TestReleaseIgnoresSharedMemoryChanges(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
	s := sourceSession(t, src)
	var buf bytes.Buffer
	if _, _, err := Pack(s, packSource(src, s), &buf); err != nil {
		t.Fatal(err)
	}
	res, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(src.dir, "projects", EncodeCwd(src.checkout), "memory", "new.md"), "written by another session\n")
	if _, err := Release(s, res.ManifestSHA256); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// A transfer whose answer was lost may be retried into another account;
// the receipt finds the first copy, so the session is not received twice.
func TestRetryIntoAnotherAccountFindsTheFirstCopy(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
	other := filepath.Join(dst.home, ".claude-y")
	put(t, filepath.Join(other, ".claude.json"), `{"oauthAccount":{"emailAddress":"claude3@x.io"}}`)
	s := sourceSession(t, src)
	var buf bytes.Buffer
	if _, _, err := Pack(s, packSource(src, s), &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io")); err != nil {
		t.Fatal(err)
	}
	o := unpackOpts(dst, "claude3@x.io")
	o.Dir = other
	res, err := Unpack(bytes.NewReader(buf.Bytes()), o)
	if err != nil || !res.Already || res.Dir != dst.dir {
		t.Fatalf("retry into another account: %+v %v", res, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(other, "projects", "*", moveID+".jsonl")); len(matches) != 0 {
		t.Fatal("the session was received a second time")
	}
}

// A checkout reached through a symlink is stored under its physical path,
// the one Claude Code records and keys folders by.
func TestUnpackNamesTheRealCheckout(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	dst := newMachine(t, "data/shop", ".claude-x", "claude2@x.io")
	link := filepath.Join(dst.home, "code")
	if err := os.Symlink(filepath.Join(dst.home, "data"), link); err != nil {
		t.Fatal(err)
	}
	s := sourceSession(t, src)
	var buf bytes.Buffer
	if _, _, err := Pack(s, packSource(src, s), &buf); err != nil {
		t.Fatal(err)
	}
	o := unpackOpts(dst, "claude2@x.io")
	o.Cwd, o.Checkout, o.GitRoot = filepath.Join(link, "shop", "web"), filepath.Join(link, "shop"), filepath.Join(link, "shop")
	res, err := Unpack(bytes.NewReader(buf.Bytes()), o)
	real := filepath.Join(dst.checkout, "web")
	if err != nil || res.ProjectFolder != EncodeCwd(real) {
		t.Fatalf("folder: %+v %v", res, err)
	}
	if got := read(t, filepath.Join(dst.dir, "projects", EncodeCwd(real), moveID+".jsonl")); !strings.Contains(got, `"cwd":"`+real+`"`) {
		t.Fatalf("cwd not the physical path:\n%s", got)
	}
}

// The first copy went into another account and the source changed since:
// a collision, naming where the first copy is.
func TestRetryIntoAnotherAccountAfterAChange(t *testing.T) {
	src := newMachine(t, "Code/shop", ".claude-a", "claude1@x.io")
	dst := newMachine(t, "src/shop", ".claude-x", "claude2@x.io")
	other := filepath.Join(dst.home, ".claude-y")
	put(t, filepath.Join(other, ".claude.json"), `{"oauthAccount":{"emailAddress":"claude3@x.io"}}`)
	s := sourceSession(t, src)
	var buf bytes.Buffer
	if _, _, err := Pack(s, packSource(src, s), &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(bytes.NewReader(buf.Bytes()), unpackOpts(dst, "claude2@x.io")); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(src.dir, "projects", s.ProjectDir, moveID+".jsonl")
	f, err := os.OpenFile(jsonl, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"user","message":{"content":"one more"}}` + "\n")
	f.Close()
	buf.Reset()
	if _, _, err := Pack(s, packSource(src, s), &buf); err != nil {
		t.Fatal(err)
	}
	o := unpackOpts(dst, "claude3@x.io")
	o.Dir = other
	_, err = Unpack(bytes.NewReader(buf.Bytes()), o)
	if !errors.Is(err, ErrCollision) || !strings.Contains(err.Error(), dst.dir) {
		t.Fatalf("got %v", err)
	}
}
