package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/muratgozel/julienning/internal/paths"
)

// SameMinor reports whether two release versions share major.minor, the
// rule for cross-machine commands (SPEC "Versions"): patch and pre-release
// may differ. In 0.x the minor is the breaking number. Dev builds are not
// versions here; callers check IsDevBuild first.
func SameMinor(a, b string) (bool, error) {
	va, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	return va.major == vb.major && va.minor == vb.minor, nil
}

// MajorMinor renders "0.10" for a release version ("" when it is none).
func MajorMinor(v string) string {
	s, err := parseVersion(v)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d", s.major, s.minor)
}

// FetchBinary downloads release tag's archive for goos/goarch (another
// machine's platform), verifies it against the release's checksums.txt
// (fail closed, as Install) and returns the julienning binary it holds. It
// is not run here: it may not be for this machine.
func FetchBinary(ctx context.Context, tag, goos, goarch string) ([]byte, error) {
	if _, err := parseVersion(tag); err != nil {
		return nil, fmt.Errorf("invalid release tag: %w", err)
	}
	ver := DisplayVersion(tag)
	base, err := releasesBase()
	if err != nil {
		return nil, err
	}
	asset := AssetName(ver, goos, goarch)
	relURL := base + "/releases/download/" + url.PathEscape(tag) + "/"
	sums, err := fetchChecksums(ctx, relURL+"checksums.txt", tag)
	if err != nil {
		return nil, err
	}
	want, ok := sums[asset]
	if !ok {
		return nil, fmt.Errorf("checksums.txt of release %s has no entry for %s; refusing to install an unverified binary", tag, asset)
	}
	var archive bytes.Buffer
	lim := fetchLimits{total: archiveTimeout, stall: archiveStallTimeout, maxBytes: maxArchiveBytes}
	got, err := downloadTo(ctx, relURL+asset, &archive, lim, func(status int) error {
		if status == http.StatusNotFound {
			return fmt.Errorf("release %s has no build for %s/%s (%s not found)", tag, goos, goarch, asset)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("checksum mismatch for %s (expected %s, got %s): the download is corrupt or was tampered with; nothing was installed", asset, want, got)
	}
	var bin bytes.Buffer
	if err := extractBinary(&archive, &bin); err != nil {
		return nil, fmt.Errorf("%s: %w", asset, err)
	}
	return bin.Bytes(), nil
}

// InstallSelf installs the binary at src as version ver into the installer
// layout, as `update` would: VersionsDir/<ver> (it must run `version`
// first), the command symlink switched to it (the managed link, else
// paths.BinLink on a machine without one), older versions pruned. It is what
// `agent install-self` runs from a temporary copy streamed to a host that
// could not run the installer. Returns the link.
func InstallSelf(ctx context.Context, src, ver string) (string, error) {
	if !validVersionName(ver) || ver == DevVersion {
		return "", fmt.Errorf("refusing to install version %q (not a release)", ver)
	}
	dir, err := paths.VersionsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create versions dir: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	dest := filepath.Join(dir, ver)
	tmp, err := os.CreateTemp(dir, "."+ver+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	fail := func(err error) (string, error) {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if _, err := io.Copy(tmp, io.LimitReader(in, maxBinaryBytes+1)); err != nil {
		return fail(fmt.Errorf("copy %s: %w", src, err))
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fail(fmt.Errorf("chmod %s: %w", tmpName, err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("write %s: %w", tmpName, err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := verifyRuns(ctx, tmpName); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("julienning %s does not run on this machine (%v); nothing was installed", ver, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("install %s: %w", dest, err)
	}
	link, _, err := ActiveLink()
	var nm *NotManagedError
	switch {
	case errors.As(err, &nm):
		if link, err = paths.BinLink(); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}
	if err := Activate(link, ver); err != nil {
		return "", err
	}
	if _, err := Prune(KeepVersions, link); err != nil {
		return link, fmt.Errorf("installed %s, but pruning old versions failed: %w", ver, err)
	}
	return link, nil
}

// InstallerURL is the installer script the official one-liner fetches, which
// a host runs during `host add`. With JULIENNING_RELEASES_BASE set (tests,
// mirrors) it is <base>/raw/main/scripts/install.sh, a path GitHub itself
// serves for https://github.com/<repo>, so a mirror of the releases can
// serve the installer too.
func InstallerURL() (string, error) {
	if ReleasesBase != "" || os.Getenv(EnvReleasesBase) != "" {
		base, err := releasesBase()
		if err != nil {
			return "", err
		}
		return base + "/raw/main/scripts/install.sh", nil
	}
	repo, err := Repo()
	if err != nil {
		return "", err
	}
	return "https://raw.githubusercontent.com/" + repo + "/main/scripts/install.sh", nil
}
