package maint

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/paths"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/version"
)

type harness struct {
	t                          *testing.T
	root, state, bin, versions string

	mu     sync.Mutex
	latest string
	files  map[string][]byte // "<tag>/<name>"
	hits   int
}

// newHarness isolates every julienning location in temp dirs, serves fake
// GitHub release pages, and pretends version cur is installed and running.
func newHarness(t *testing.T, cur string) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:        t,
		root:     root,
		state:    filepath.Join(root, "home", ".julienning"),
		bin:      filepath.Join(root, "bin"),
		versions: filepath.Join(root, "versions"),
		files:    map[string][]byte{},
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("JULIENNING_HOME", h.state)
	t.Setenv(paths.EnvBinDir, h.bin)
	t.Setenv(paths.EnvVersionsDir, h.versions)
	t.Setenv(selfupdate.EnvRepo, "")
	// paths.FindLink scans PATH; keep the developer's real install out of it.
	t.Setenv("PATH", filepath.Join(root, "empty-path"))

	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	t.Setenv(selfupdate.EnvReleasesBase, srv.URL+"/o/r")

	oldVersion, oldExe := version.Version, paths.Executable
	t.Cleanup(func() { version.Version, paths.Executable = oldVersion, oldExe })
	version.Version = cur
	running := filepath.Join(h.versions, selfupdate.DisplayVersion(cur))
	paths.Executable = func() (string, error) { return running, nil }

	h.writeFile(running, "running "+cur)
	if err := os.MkdirAll(h.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(running, h.link()); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
	p := strings.TrimPrefix(r.URL.Path, "/o/r")
	switch {
	case p == "/releases/latest":
		http.Redirect(w, r, "https://github.com/o/r/releases/tag/"+h.latest, http.StatusFound)
	case strings.HasPrefix(p, "/releases/download/"):
		if b, ok := h.files[strings.TrimPrefix(p, "/releases/download/")]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// publish adds a release whose binary is a script that runs `version`
// successfully. corrupt publishes a checksum that does not match.
func (h *harness) publish(tag string, corrupt bool) {
	h.t.Helper()
	h.publishBody(tag, runnable(tag), corrupt)
}

func runnable(tag string) string {
	return "#!/bin/sh\necho julienning " + strings.TrimPrefix(tag, "v") + "\n"
}

func (h *harness) publishBody(tag, body string, corrupt bool) {
	h.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "julienning", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		h.t.Fatal(err)
	}
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	hexSum := hex.EncodeToString(sum[:])
	if corrupt {
		hexSum = strings.Repeat("0", 64)
	}
	asset := selfupdate.AssetName(strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[tag+"/"+asset] = archive
	h.files[tag+"/checksums.txt"] = []byte(fmt.Sprintf("%s  %s\n", hexSum, asset))
}

func (h *harness) setLatest(tag string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = tag
}

func (h *harness) hitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

func (h *harness) link() string { return filepath.Join(h.bin, "julienning") }

func (h *harness) writeFile(p, body string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = cli.Main(append([]string{"update"}, args...), strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

// active returns the version the link points to.
func (h *harness) active() string {
	h.t.Helper()
	target, err := os.Readlink(h.link())
	if err != nil {
		h.t.Fatal(err)
	}
	if filepath.Dir(target) != h.versions {
		h.t.Fatalf("link points outside the versions dir: %s", target)
	}
	return filepath.Base(target)
}

func (h *harness) cachedLatest() string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.state, selfupdate.CheckFile))
	if err != nil {
		h.t.Fatal(err)
	}
	var c struct {
		Latest string `json:"latest"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		h.t.Fatal(err)
	}
	return c.Latest
}

func TestUpdateToLatest(t *testing.T) {
	h := newHarness(t, "0.2.1")
	h.publish("v0.3.0", false)
	h.setLatest("v0.3.0")

	code, out, errOut := h.run()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "Updated 0.2.1 → 0.3.0.\n" {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(errOut, "Downloading julienning 0.3.0") {
		t.Fatalf("stderr = %q", errOut)
	}
	if got := h.active(); got != "0.3.0" {
		t.Fatalf("active = %q", got)
	}
	if b, _ := os.ReadFile(h.link()); string(b) != runnable("v0.3.0") {
		t.Fatalf("installed binary = %q", b)
	}
	if _, err := os.Stat(filepath.Join(h.versions, "0.2.1")); err != nil {
		t.Fatalf("previous version was removed: %v", err)
	}
	if got := h.cachedLatest(); got != "0.3.0" {
		t.Fatalf("update-check latest = %q", got)
	}
}

func TestUpdateUpToDate(t *testing.T) {
	h := newHarness(t, "0.3.0")
	h.setLatest("v0.3.0")

	code, out, errOut := h.run()
	if code != 0 || out != "julienning 0.3.0 is up to date.\n" || errOut != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := h.active(); got != "0.3.0" {
		t.Fatalf("active = %q", got)
	}
	if got := h.cachedLatest(); got != "0.3.0" {
		t.Fatalf("update-check latest = %q", got)
	}
}

func TestUpdateNewerThanLatest(t *testing.T) {
	h := newHarness(t, "0.4.0")
	h.setLatest("v0.3.0")
	code, out, _ := h.run()
	if code != 0 || !strings.Contains(out, "0.4.0 is up to date") || h.active() != "0.4.0" {
		t.Fatalf("exit %d, stdout %q, active %q", code, out, h.active())
	}
}

func TestUpdateDevAlwaysMovesToLatest(t *testing.T) {
	h := newHarness(t, "dev")
	h.publish("v0.3.0", false)
	h.setLatest("v0.3.0")

	code, out, errOut := h.run()
	if code != 0 || out != "Updated dev → 0.3.0.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := h.active(); got != "0.3.0" {
		t.Fatalf("active = %q", got)
	}
}

func TestUpdateExplicitVersion(t *testing.T) {
	h := newHarness(t, "0.3.0")
	h.publish("v0.2.5", false)
	h.setLatest("v0.3.0")

	code, out, errOut := h.run("--version", "0.2.5")
	if code != 0 || out != "Updated 0.3.0 → 0.2.5.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := h.active(); got != "0.2.5" {
		t.Fatalf("active = %q", got)
	}
	// The cache holds the real latest, so the downgrade gets a hint.
	if got := h.cachedLatest(); got != "0.3.0" {
		t.Fatalf("update-check latest = %q", got)
	}
}

func TestUpdatePrunesOldVersions(t *testing.T) {
	h := newHarness(t, "0.2.1")
	for i, v := range []string{"0.1.0", "0.1.1", "0.2.0"} {
		p := filepath.Join(h.versions, v)
		h.writeFile(p, v)
		old := time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	h.publish("v0.3.0", false)
	h.setLatest("v0.3.0")
	if code, _, errOut := h.run(); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	entries, err := os.ReadDir(h.versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != selfupdate.KeepVersions {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("versions left = %v", names)
	}
	for _, keep := range []string{"0.3.0", "0.2.1"} { // active, running
		if _, err := os.Stat(filepath.Join(h.versions, keep)); err != nil {
			t.Fatalf("%s removed: %v", keep, err)
		}
	}
}

func TestUpdateChecksumMismatchLeavesInstallAlone(t *testing.T) {
	h := newHarness(t, "0.2.1")
	h.publish("v0.3.0", true)
	h.setLatest("v0.3.0")

	code, out, errOut := h.run()
	if code != cli.ExitError || out != "" || !strings.Contains(errOut, "checksum mismatch") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := h.active(); got != "0.2.1" {
		t.Fatalf("active = %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.versions, "0.3.0")); !os.IsNotExist(err) {
		t.Fatalf("unverified binary installed: %v", err)
	}
}

func TestUpdateMissingRelease(t *testing.T) {
	h := newHarness(t, "0.2.1")
	code, _, errOut := h.run("--version", "v9.9.9")
	if code != cli.ExitError || !strings.Contains(errOut, "release v9.9.9 not found") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if got := h.active(); got != "0.2.1" {
		t.Fatalf("active = %q", got)
	}
}

// Regression: update switched the link before checking that the new build
// runs; install.sh refuses such a build, and so must update.
func TestUpdateRefusesBinaryThatDoesNotRun(t *testing.T) {
	for name, body := range map[string]string{
		"exits non-zero": "#!/bin/sh\necho 'Killed: 9' >&2\nexit 137\n",
		"not a program":  "ELF for another platform",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "0.2.1")
			h.publishBody("v0.3.0", body, false)
			h.setLatest("v0.3.0")

			code, out, errOut := h.run()
			if code != cli.ExitError || out != "" || !strings.Contains(errOut, "julienning 0.3.0 does not run on this machine") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
			if got := h.active(); got != "0.2.1" {
				t.Fatalf("active = %q", got)
			}
			if _, err := os.Stat(filepath.Join(h.versions, "0.3.0")); !os.IsNotExist(err) {
				t.Fatalf("broken binary installed: %v", err)
			}
		})
	}
}

// Regression: an install made with JULIENNING_BIN_DIR=X refused to update
// once that env var was gone. The link is found through PATH and switched
// in place; no second link appears in ~/.local/bin.
func TestUpdateFindsLinkOutsideDefaultBinDir(t *testing.T) {
	h := newHarness(t, "0.2.1")
	h.publish("v0.3.0", false)
	h.setLatest("v0.3.0")
	t.Setenv(paths.EnvBinDir, "")
	t.Setenv("PATH", h.bin)

	code, out, errOut := h.run()
	if code != 0 || out != "Updated 0.2.1 → 0.3.0.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := h.active(); got != "0.3.0" {
		t.Fatalf("active = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(h.root, "home", ".local", "bin", "julienning")); !os.IsNotExist(err) {
		t.Fatalf("created a second link in ~/.local/bin (%v)", err)
	}
}

func TestUpdateRefusesUnmanagedInstall(t *testing.T) {
	cases := map[string]func(h *harness){
		"no link": func(h *harness) { os.Remove(h.link()) },
		"regular file": func(h *harness) {
			os.Remove(h.link())
			h.writeFile(h.link(), "copied")
		},
		"link elsewhere": func(h *harness) {
			other := filepath.Join(h.root, "go", "bin", "julienning")
			h.writeFile(other, "go install")
			os.Remove(h.link())
			if err := os.Symlink(other, h.link()); err != nil {
				h.t.Fatal(err)
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "0.2.1")
			h.publish("v0.3.0", false)
			h.setLatest("v0.3.0")
			breakIt(h)
			code, out, errOut := h.run()
			if code != cli.ExitError || out != "" {
				t.Fatalf("exit %d, stdout %q", code, out)
			}
			for _, want := range []string{h.link(), "cannot manage this installation", "curl -fsSL https://raw.githubusercontent.com/muratgozel/julienning/main/scripts/install.sh | bash"} {
				if !strings.Contains(errOut, want) {
					t.Fatalf("stderr %q lacks %q", errOut, want)
				}
			}
			if n := h.hitCount(); n != 0 {
				t.Fatalf("contacted the release server %d times", n)
			}
			if _, err := os.Stat(filepath.Join(h.versions, "0.3.0")); !os.IsNotExist(err) {
				t.Fatal("installed despite an unmanaged layout")
			}
		})
	}
}

func TestUpdateUsageErrors(t *testing.T) {
	h := newHarness(t, "0.2.1")
	for _, args := range [][]string{{"--version", "latest"}, {"extra"}, {"--bogus"}} {
		code, _, errOut := h.run(args...)
		if code != cli.ExitUsage || !strings.Contains(errOut, "usage: julienning update") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	if n := h.hitCount(); n != 0 {
		t.Fatalf("usage errors contacted the server %d times", n)
	}
}
