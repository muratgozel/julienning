package hosts

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/shell"
	"github.com/muratgozel/julienning/internal/sshx"
)

// Injection points for the parts that reach the network or a host.
var (
	runScript = func(ctx context.Context, t sshx.Target, script string, timeout time.Duration) (scriptResult, error) {
		var stderr bytes.Buffer
		out, code, err := sshx.Script(ctx, t, script, timeout, &stderr)
		return scriptResult{out: out, errOut: stderr.Bytes(), code: code}, err
	}
	fetchBinary = selfupdate.FetchBinary
	latestTag   = selfupdate.LatestTag
)

const installTimeout = 10 * time.Minute

// scriptResult is a script's stdout, stderr and exit code.
type scriptResult struct {
	out, errOut []byte
	code        int
}

// last is the last line of what the script printed, for errors.
func (r scriptResult) last() string { return lastLine(append(append([]byte{}, r.out...), r.errOut...)) }

// shq single-quotes s for a POSIX shell.
var shq = shell.SingleQuote

// probe runs hosts.ProbeScript on t and asks the login shell for the tools
// the plain probe did not find (bounded to 5 s: not every host has a
// timeout command, and a startup file can hang).
func probe(ctx context.Context, t sshx.Target) (hosts.Probe, error) {
	r, err := runScript(ctx, t, hosts.ProbeScript(), callTimeout)
	if err != nil {
		return hosts.Probe{}, err
	}
	if r.code != 0 {
		return hosts.Probe{}, fmt.Errorf("the probe failed on %s (exit %d): %s", t.Name, r.code, r.last())
	}
	p, err := hosts.ParseProbe(r.out)
	if err != nil {
		return hosts.Probe{}, fmt.Errorf("%s: %v", t.Name, err)
	}
	// sha256sum and shasum stand in for each other: only one is needed,
	// and most machines have just one of them.
	var missing []string
	for _, tool := range hosts.ProbeTools {
		if p.Paths[tool] == "" && !(tool == "sha256sum" && p.Paths["shasum"] != "") && !(tool == "shasum" && p.Paths["sha256sum"] != "") {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 && p.Shell != "" {
		if r, err := runScript(ctx, t, hosts.LoginShellScript(missing), 5*time.Second); err == nil {
			for tool, path := range hosts.ParseLoginPaths(r.out) {
				if p.Paths[tool] == "" {
					p.Paths[tool] = path
				}
			}
		}
	}
	return p, nil
}

// installTag is the release a host gets: this machine's version, or the
// latest release when this machine runs a development build.
func installTag(ctx context.Context, local string) (string, error) {
	if selfupdate.IsDevBuild(local) {
		t, err := latestTag(ctx)
		if err != nil {
			return "", fmt.Errorf("this machine runs a development build and the latest release could not be looked up: %w", err)
		}
		return t, nil
	}
	return selfupdate.NormalizeTag(local)
}

// installOnHost installs julienning tag on t (SPEC "host add" step 3): the
// official installer run there, pinned to tag, so the host gets exactly
// what a teammate's curl | bash gets; when the host lacks bash, curl, tar
// or a sha256 tool, or the installer fails, this machine downloads the
// release for the host's platform, verifies its checksum and streams it
// through the probe shell, where it installs itself (agent install-self).
// Either way the host ends up in the installer's layout, so auto-update
// and update --host work there. Returns julienning's path on the host.
func installOnHost(ctx context.Context, env cli.Env, t sshx.Target, p hosts.Probe, goos, goarch, tag string) (string, error) {
	ver := selfupdate.DisplayVersion(tag)
	hasTools := p.Paths["bash"] != "" && p.Paths["curl"] != "" && p.Paths["tar"] != "" && (p.Paths["sha256sum"] != "" || p.Paths["shasum"] != "")
	var why string
	if hasTools {
		r, err := runScript(ctx, t, installerScript(p, tag), installTimeout)
		if err == nil && r.code == 0 {
			// The installer's link, never a PATH lookup: an older julienning
			// earlier on the non-interactive PATH would win that.
			link := filepath.Join(p.Home, ".local", "bin", "julienning")
			t.Julienning = link
			var hello hosts.Hello
			if err := agentCall(ctx, t, "hello", nil, &hello); err == nil {
				return link, nil
			}
			why = "the installer finished, but julienning did not answer at " + link
		} else if err != nil {
			return "", err
		} else {
			why = fmt.Sprintf("the installer failed there: %s", r.last())
		}
	} else {
		why = "it lacks bash, curl, tar or sha256sum/shasum, which the installer needs"
	}
	fmt.Fprintf(env.Stdout, "%s%s on %s; sending julienning %s from this machine instead\n", indent, why, t.Name, ver)
	bin, err := fetchBinary(ctx, tag, goos, goarch)
	if err != nil {
		return "", fmt.Errorf("download julienning %s for %s/%s: %w", ver, goos, goarch, err)
	}
	r, err := runScript(ctx, t, fallbackScript(bin), installTimeout)
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(r.out), "julienning "+ver) {
		return "", fmt.Errorf("the julienning sent to %s does not run there (exit %d): %s", t.Name, r.code, r.last())
	}
	var res hosts.InstallSelfResult
	if _, err := hosts.DecodeAnswer(r.out, &res); err != nil {
		return "", fmt.Errorf("julienning could not install itself on %s: %v", t.Name, err)
	}
	return res.Link, nil
}

// installerScript downloads the installer to a temporary file first, so a
// failed download is a failure and not an empty script bash runs happily.
// The tools' dirs go first on PATH (the installer looks them up there), and
// JULIENNING_REPO and JULIENNING_RELEASES_BASE set here go along.
func installerScript(p hosts.Probe, tag string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	var dirs []string
	for _, tool := range []string{"bash", "curl", "tar", "sha256sum", "shasum"} {
		if path := p.Paths[tool]; path != "" {
			if d := filepath.Dir(path); !containsStr(dirs, d) {
				dirs = append(dirs, d)
			}
		}
	}
	b.WriteString("PATH=" + shq(strings.Join(dirs, ":")) + `:"$PATH"; export PATH` + "\n")
	for _, k := range []string{selfupdate.EnvRepo, selfupdate.EnvReleasesBase} {
		if v := os.Getenv(k); v != "" {
			b.WriteString(k + "=" + shq(v) + "; export " + k + "\n")
		}
	}
	url, err := selfupdate.InstallerURL()
	if err != nil {
		url = "https://raw.githubusercontent.com/" + selfupdate.DefaultRepo + "/main/scripts/install.sh"
	}
	b.WriteString(`tmp=$(mktemp "${TMPDIR:-/tmp}/julienning-install.XXXXXX")` + "\n")
	b.WriteString(`trap 'rm -f "$tmp"' EXIT` + "\n")
	b.WriteString(shq(p.Paths["curl"]) + " -fsSL " + shq(url) + ` -o "$tmp"` + "\n")
	b.WriteString(shq(p.Paths["bash"]) + ` "$tmp" ` + shq(tag) + " 2>&1\n")
	return b.String()
}

// fallbackScript carries the binary as base64 inside the script itself:
// /bin/sh -s reads its script from stdin, and dash reads ahead in blocks,
// so binary data after the script on the same stdin would be swallowed.
func fallbackScript(bin []byte) string {
	var b strings.Builder
	b.WriteString(`set -e
umask 077
tmp=$(mktemp "${TMPDIR:-/tmp}/julienning.XXXXXX")
trap 'rm -f "$tmp" "$tmp.b64"' EXIT
cat > "$tmp.b64" <<'JULIENNING_BINARY_END'
`)
	enc := base64.StdEncoding.EncodeToString(bin)
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteByte('\n')
		enc = enc[76:]
	}
	b.WriteString(enc + "\nJULIENNING_BINARY_END\n")
	b.WriteString(`if base64 -d < "$tmp.b64" > "$tmp" 2>/dev/null; then :; else base64 -D < "$tmp.b64" > "$tmp"; fi
chmod 700 "$tmp"
"$tmp" version
"$tmp" agent install-self
`)
	return b.String()
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return "no output"
}

// indent aligns continuation lines under the "Label:   " column.
const indent = "          "
