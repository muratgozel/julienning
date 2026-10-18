package hosts

import (
	"context"
	"errors"
	"fmt"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sshx"
	"github.com/muratgozel/julienning/internal/version"
)

// localVersion is this machine's julienning version (a var for tests).
var localVersion = func() string { return version.Version }

// updateHere installs tag on this machine and activates it (a var for
// tests); the running binary stays the old one.
var updateHere = func(ctx context.Context, tag string) error {
	link, _, err := selfupdate.ActiveLink()
	if err != nil {
		var nm *selfupdate.NotManagedError
		if errors.As(err, &nm) {
			return fmt.Errorf("%v, so this machine cannot be updated in place; install with: %s", nm, selfupdate.InstallerCommand())
		}
		return err
	}
	if _, err := selfupdate.Install(ctx, tag); err != nil {
		return err
	}
	if err := selfupdate.Activate(link, selfupdate.DisplayVersion(tag)); err != nil {
		return err
	}
	_, err = selfupdate.Prune(selfupdate.KeepVersions, link)
	return err
}

// errUpdatedHere stops a command after this machine updated itself: the
// running binary is the old one, so the command must be run again.
var errUpdatedHere = errors.New("updated this machine")

// versionMode is what a version mismatch does.
type versionMode int

const (
	// versionsStrict: forwarded commands and transfer need the same
	// major.minor; a terminal is offered the update, else it is an error.
	versionsStrict versionMode = iota
	// versionsLenient: host list, host remove, update --host and uninstall
	// --host are never refused, since they are how a host gets fixed.
	versionsLenient
)

// checkVersions applies SPEC "Versions" to a host that answered hello: the
// same major.minor on both machines (patch and pre-release may differ),
// then the agent protocol. A development build on either side skips the
// number check with a warning. It returns the hello to go on with, which
// is a new one after the host was updated; errUpdatedHere when this machine
// updated itself instead.
func checkVersions(ctx context.Context, env cli.Env, a *asker, h hosts.Host, hello *hosts.Hello, mode versionMode) (*hosts.Hello, error) {
	local, remoteV := localVersion(), hello.Version
	protoErr := func() error {
		if hello.Proto == hosts.Proto {
			return nil
		}
		return fmt.Errorf("%s speaks agent protocol %d (julienning %s) and this machine %d (julienning %s); update the older one", h.Name, hello.Proto, remoteV, hosts.Proto, local)
	}
	switch {
	case selfupdate.IsDevBuild(local):
		warnf(env, "this machine runs a development build (%s); only the agent protocol is checked against %s", local, h.Name)
		return hello, protoErrUnlessLenient(protoErr(), mode)
	case selfupdate.IsDevBuild(remoteV):
		warnf(env, "%s runs a development build (%s); only the agent protocol is checked against it", h.Name, remoteV)
		return hello, protoErrUnlessLenient(protoErr(), mode)
	}
	same, err := selfupdate.SameMinor(local, remoteV)
	if err != nil {
		return nil, fmt.Errorf("%s reports julienning version %q: %v", h.Name, remoteV, err)
	}
	if same {
		return hello, protoErrUnlessLenient(protoErr(), mode)
	}
	if mode == versionsLenient {
		return hello, nil
	}
	cmp, _ := selfupdate.CompareVersions(remoteV, local)
	if cmp < 0 {
		return updateHost(ctx, env, a, h, hello)
	}
	q := fmt.Sprintf("%s runs julienning %s and this machine %s; cross-machine commands need the same %s. Update this machine to %s now?",
		h.Name, remoteV, local, selfupdate.MajorMinor(remoteV), selfupdate.DisplayVersion(remoteV))
	refusal := fmt.Errorf("%s runs julienning %s and this machine %s (cross-machine commands need the same major.minor); update this machine: julienning update --version v%s",
		h.Name, remoteV, local, selfupdate.DisplayVersion(remoteV))
	if err := a.ask(q, true, ""); err != nil {
		return nil, refusal
	}
	tag := "v" + selfupdate.DisplayVersion(remoteV)
	if err := updateHere(ctx, tag); err != nil {
		return nil, err
	}
	fmt.Fprintf(env.Stdout, "Updated %s → %s; run the command again.\n", local, selfupdate.DisplayVersion(remoteV))
	return nil, errUpdatedHere
}

func protoErrUnlessLenient(err error, mode versionMode) error {
	if mode == versionsLenient {
		return nil
	}
	return err
}

// updateHost offers to bring an older host to this machine's version,
// through the installer there (host add step 3's install).
func updateHost(ctx context.Context, env cli.Env, a *asker, h hosts.Host, hello *hosts.Hello) (*hosts.Hello, error) {
	local := localVersion()
	target := selfupdate.DisplayVersion(local)
	if !hello.Managed {
		return nil, fmt.Errorf("%s runs julienning %s at %s, which the installer did not put there; update it by hand to %s (cross-machine commands need the same major.minor)",
			h.Name, hello.Version, hello.Paths.Julienning, selfupdate.MajorMinor(local))
	}
	q := fmt.Sprintf("%s runs julienning %s and this machine %s; cross-machine commands need the same %s. Update %s to %s now?",
		h.Name, hello.Version, local, selfupdate.MajorMinor(local), h.Name, target)
	refusal := fmt.Errorf("%s runs julienning %s and this machine %s (cross-machine commands need the same major.minor); update %s: julienning update --host %s --version v%s",
		h.Name, hello.Version, local, h.Name, h.Name, target)
	if err := a.ask(q, true, ""); err != nil {
		return nil, refusal
	}
	t := sshx.TargetOf(h)
	p, err := probe(ctx, t)
	if err != nil {
		return nil, err
	}
	tag, err := installTag(ctx, local)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(env.Stdout, "Updating julienning on %s to %s…\n", h.Name, selfupdate.DisplayVersion(tag))
	path, err := installOnHost(ctx, env, t, p, hello.OS, hello.Arch, tag)
	if err != nil {
		return nil, err
	}
	t.Julienning = path
	var again hosts.Hello
	if err := agentCall(ctx, t, "hello", nil, &again); err != nil {
		return nil, hostErr(h.Name, err)
	}
	fmt.Fprintf(env.Stdout, "Updated julienning on %s to %s.\n", h.Name, again.Version)
	return &again, nil
}
