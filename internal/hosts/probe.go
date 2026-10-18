package hosts

import (
	"bufio"
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
)

// probeMarker precedes the probe's answer; startup-file noise before it is
// dropped.
const probeMarker = "JULIENNING-PROBE/1"

// ProbeTools are the commands host add looks for on a new host.
var ProbeTools = []string{"julienning", "claude", "tmux", "bash", "curl", "tar", "sha256sum", "shasum"}

// probeDirs are searched after the non-interactive PATH (Debian gives ssh
// commands /usr/local/bin:/usr/bin:/bin:/usr/games, and ~/.bashrc returns
// early for them): user installs, Homebrew, Nix.
const probeDirs = `"$HOME/.local/bin" /usr/local/bin /opt/homebrew/bin "$HOME/.nix-profile/bin" /run/current-system/sw/bin`

// ProbeScript is the POSIX script `host add` runs as `exec /bin/sh -s` on a
// host before anything is known about it. It changes nothing.
func ProbeScript() string {
	return `echo ` + probeMarker + `
echo "uname=$(uname -sm 2>/dev/null)"
echo "user=$(id -un 2>/dev/null)"
echo "home=$HOME"
echo "shell=${SHELL:-}"
if [ -r /etc/os-release ]; then
  (. /etc/os-release 2>/dev/null; echo "os_name=${PRETTY_NAME:-}")
elif command -v sw_vers >/dev/null 2>&1; then
  echo "os_name=$(sw_vers -productName 2>/dev/null) $(sw_vers -productVersion 2>/dev/null)"
fi
(set -- ${SSH_CONNECTION:-}; echo "server=${3:-}")
jl_find() {
  p=$(command -v "$1" 2>/dev/null) && case $p in /*) echo "$p"; return ;; esac
  for d in ` + probeDirs + `; do
    if [ -x "$d/$1" ] && [ ! -d "$d/$1" ]; then echo "$d/$1"; return; fi
  done
}
for t in ` + strings.Join(ProbeTools, " ") + `; do echo "path_$t=$(jl_find "$t")"; done
`
}

// LoginShellScript asks the host's login shell for tools the probe did not
// find (the login PATH can hold more). Run as `exec /bin/sh -s`; the
// controller stops it after 5 s, since not every host has a timeout command.
// The inner command is plain `;`-separated commands so fish runs it too.
func LoginShellScript(tools []string) string {
	var inner strings.Builder
	inner.WriteString("echo " + probeMarker)
	for _, t := range tools {
		inner.WriteString("; echo T=" + t + "; command -v " + t)
	}
	return `exec "${SHELL:-/bin/sh}" -lc '` + inner.String() + `' </dev/null 2>/dev/null
`
}

// Probe is what the probe learnt about a host.
type Probe struct {
	Uname  string // `uname -sm`
	User   string
	Home   string
	Shell  string
	OSName string // PRETTY_NAME, or sw_vers on macOS
	Server string // the server address from $SSH_CONNECTION
	Paths  map[string]string
}

// ParseProbe reads ProbeScript's output.
func ParseProbe(out []byte) (Probe, error) {
	body, ok := afterMarker(out)
	if !ok {
		return Probe{}, fmt.Errorf("the probe printed no answer")
	}
	p := Probe{Paths: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case k == "uname":
			p.Uname = v
		case k == "user":
			p.User = v
		case k == "home":
			p.Home = v
		case k == "shell":
			p.Shell = v
		case k == "os_name":
			p.OSName = v
		case k == "server":
			p.Server = v
		case strings.HasPrefix(k, "path_"):
			if v != "" && filepath.IsAbs(v) {
				p.Paths[strings.TrimPrefix(k, "path_")] = v
			}
		}
	}
	if p.Home == "" || !filepath.IsAbs(p.Home) {
		return Probe{}, fmt.Errorf("the probe found no home directory")
	}
	return p, nil
}

// ParseLoginPaths reads LoginShellScript's output: "T=<tool>" then, when
// found, its path on the next line.
func ParseLoginPaths(out []byte) map[string]string {
	body, ok := afterMarker(out)
	paths := map[string]string{}
	if !ok {
		return paths
	}
	tool := ""
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if t, ok := strings.CutPrefix(line, "T="); ok {
			tool = t
			continue
		}
		if tool != "" && filepath.IsAbs(line) {
			paths[tool] = line
		}
		tool = ""
	}
	return paths
}

func afterMarker(out []byte) ([]byte, bool) {
	for i := 0; i < len(out); {
		end := bytes.IndexByte(out[i:], '\n')
		line, next := out[i:], len(out)
		if end >= 0 {
			line, next = out[i:i+end], i+end+1
		}
		if string(bytes.TrimSpace(line)) == probeMarker {
			return out[next:], true
		}
		i = next
	}
	return nil, false
}

// Platform maps `uname -sm` to a release platform. A machine releases are
// not built for is an error naming it.
func (p Probe) Platform(name string) (goos, goarch string, err error) {
	f := strings.Fields(p.Uname)
	if len(f) < 2 {
		return "", "", fmt.Errorf("%s did not say what it runs (uname -sm printed %q)", name, p.Uname)
	}
	switch f[0] {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("%s runs %s; julienning releases exist for macOS and Linux only", name, f[0])
	}
	switch f[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "arm64", "aarch64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("%s is %s/%s; julienning releases exist for amd64 and arm64 only", name, goos, f[1])
	}
	return goos, goarch, nil
}
