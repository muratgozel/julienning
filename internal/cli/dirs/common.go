package dirs

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/paths"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/shell"
)

// remoteTimeout bounds each Worker call made by these interactive commands.
const remoteTimeout = 10 * time.Second

// Injection points. Tests replace them so no command touches the network, a
// terminal, or the real binary location.
var (
	newClient = func(cfg *config.Config) remote.Client {
		return remote.NewHTTP(cfg.Remote.URL, cfg.Remote.Token, remoteTimeout)
	}
	checkHealth   = httpHealthz
	isInteractive = func(env cli.Env) bool { return isTerminal(env.Stdin) && isTerminal(env.Stdout) }
	readSecret    = readSecretFromTerminal
	stableCommand = paths.StableCommand
	now           = time.Now
)

// homeDir wraps os.UserHomeDir with an actionable error.
func homeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory (set $HOME): %w", err)
	}
	return h, nil
}

// shortenHome replaces the home prefix with "~" for display only.
func shortenHome(path string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return path
	}
	if path == h {
		return "~"
	}
	if strings.HasPrefix(path, h+string(filepath.Separator)) {
		return "~" + path[len(h):]
	}
	return path
}

// expandPath expands a leading "~/" and returns a cleaned absolute path.
func expandPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, err := homeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(h, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	return filepath.Clean(abs), nil
}

// dirEmail reads a dir's login live. ok is false when the dir is not logged
// in; err is set only for unreadable or invalid account files.
func dirEmail(dir string) (email string, ok bool, err error) {
	email, err = claudecfg.ReadEmail(dir)
	switch {
	case err == nil:
		return email, true, nil
	case errors.Is(err, claudecfg.ErrNotLoggedIn):
		return "", false, nil
	default:
		return "", false, err
	}
}

// emailLabel renders a config dir's account for human output.
func emailLabel(dir string) string {
	email, ok, err := dirEmail(dir)
	switch {
	case ok:
		return email
	case err != nil:
		return "(unreadable)"
	default:
		return "(not logged in)"
	}
}

// isTerminal reports whether v is an interactive terminal, so commands only
// prompt when somebody is there to answer. term.IsTerminal, unlike a
// char-device check, is false for /dev/null.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// prompt writes a question and reads one line. A read failure or EOF yields
// the empty string, which callers treat as "take the default".
func prompt(w io.Writer, r *bufio.Reader, question string) string {
	fmt.Fprint(w, question)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimSpace(line)
}

// ask is prompt that also reports the end of input (ok false), so a loop
// that re-asks stops when stdin runs dry instead of spinning on "".
func ask(w io.Writer, r *bufio.Reader, question string) (answer string, ok bool) {
	fmt.Fprint(w, question)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// confirm asks a y/N question; anything but y/yes is a no.
func confirm(w io.Writer, r *bufio.Reader, question string) bool {
	answer := strings.ToLower(prompt(w, r, question))
	return answer == "y" || answer == "yes"
}

// readSecretFromTerminal reads one line without echo. The value is never
// printed or logged.
func readSecretFromTerminal(env cli.Env, question string) (string, error) {
	f, ok := env.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("cannot read the token without a terminal; pass --token TOKEN")
	}
	fmt.Fprint(env.Stdout, question)
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(env.Stdout)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// warnf prints a non-fatal problem; the command carries on.
func warnf(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: warning: %s\n", fmt.Sprintf(format, a...))
}

// loadConfig loads config.json; ErrNotSetup already says "run julienning setup".
func loadConfig() (*config.Config, error) {
	return config.Load()
}

// parseFlags parses args, converting bad invocation into a cli.UsageError.
// The flag set's own output is discarded so the message is printed once, by
// cli.Main, together with the command's synopsis.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return cli.Usagef("%v", err)
	}
	return nil
}

// parseFlagsPermute parses args where flags may appear before or after the
// positional arguments (`adopt ~/.claude-x --name x` must work like
// `adopt --name x ~/.claude-x`). Go's flag package stops at the first
// non-flag, so we re-parse what is left after each positional.
func parseFlagsPermute(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := parseFlags(fs, args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// invalidName is the error for a config name that fails
// config.ValidConfigName.
func invalidName(name string) error {
	return fmt.Errorf("invalid config name %q: use letters, digits, underscore or dash, starting with a letter or digit", name)
}

func unknownConfig(cfg *config.Config, name string) error {
	if len(cfg.Configs) == 0 {
		return fmt.Errorf("unknown config %q (none registered; run `julienning setup` or `julienning new-config`)", name)
	}
	known := make([]string, 0, len(cfg.Configs))
	for _, cd := range cfg.Configs {
		known = append(known, cd.Name)
	}
	return fmt.Errorf("unknown config %q (known: %s)", name, strings.Join(known, ", "))
}

// normalizeEmail validates an email argument at the boundary.
func normalizeEmail(s string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(s))
	if !claudecfg.ValidEmail(e) {
		return "", cli.Usagef("invalid email %q", s)
	}
	return e, nil
}

// patchCommand returns the executable settings.json should run, warning once
// per command when it is not the installed, update-proof path.
func patchCommand(env cli.Env) (string, error) {
	exe, stable, err := stableCommand()
	if err != nil {
		return "", err
	}
	if !stable {
		link, _ := paths.BinLink()
		warnf(env, "settings.json will run %s, not the installed %s; install julienning (scripts/install.sh or `make install`) and re-run `julienning setup` so updates keep working", exe, shortenHome(link))
	}
	return exe, nil
}

// --- Worker helpers ---

// validateRemoteURL rejects anything but a plain http(s) base URL. Userinfo is
// refused because the URL is printed in status lines.
func validateRemoteURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("invalid Worker URL %q (want https://<name>.<account>.workers.dev)", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid Worker URL %q: no credentials, query or fragment (pass the token with --token)", s)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// httpHealthz checks GET <base>/healthz answers 200. No token is sent.
func httpHealthz(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("cannot reach the Worker at %s: %v", base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain for keep-alive; content is irrelevant
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the Worker at %s answered %d on /healthz (is the URL the Worker's base URL?)", base, resp.StatusCode)
	}
	return nil
}

// isAuthError reports a Worker 401/403: the team token is wrong.
func isAuthError(err error) bool {
	var re *remote.Error
	return errors.As(err, &re) && (re.Status == http.StatusUnauthorized || re.Status == http.StatusForbidden)
}

// errTokenRejected is the actionable message for isAuthError.
var errTokenRejected = errors.New("the team token was rejected by the Worker (re-run `julienning setup` in a terminal, or pass the current one with --token)")

// remoteErr maps a Worker failure to an actionable message.
func remoteErr(action string, err error) error {
	if isAuthError(err) {
		return errTokenRejected
	}
	return fmt.Errorf("%s: %w", action, err)
}

// --- nicknames and targets ---

// normalizeNickname lowercases and trims a nickname a user typed and
// validates it at the boundary.
func normalizeNickname(s string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(s))
	if !shell.ValidNickname(n) {
		return "", fmt.Errorf("invalid nickname %q: %s", s, shell.NicknameRule)
	}
	return n, nil
}

// defaultNickname is the suggested nickname for an email: its local part
// with invalid characters turned into dashes (config.DefaultNickname, the
// rule the pending share on login uses too); "" when nothing valid is left.
func defaultNickname(email string) string { return config.DefaultNickname(email) }

// accountLabel renders an account as "nick (email)", or the bare email when
// it has no nickname.
func accountLabel(nick, email string) string {
	if nick == "" {
		return email
	}
	return nick + " (" + email + ")"
}

// loadCache reads shared.json. An unreadable cache is reported and treated
// as empty, sharedcache's "nothing is shared" rule.
func loadCache(env cli.Env) *sharedcache.Cache {
	cache, err := sharedcache.Load()
	if err != nil {
		warnf(env, "ignoring the cached allowlist: %v", err)
		return &sharedcache.Cache{}
	}
	return cache
}

// resolveTarget maps a nickname, email or config name to a registered dir
// (resolve.Target), preferring the current selection on ties.
func resolveTarget(env cli.Env, cfg *config.Config, arg string) (resolve.Match, error) {
	current, ok, err := cfg.Current()
	if err != nil {
		return resolve.Match{}, err
	}
	var cur *config.ConfigDir
	if ok {
		cur = &current
	}
	return resolve.Target(cfg, loadCache(env), arg, cur)
}

// accountByTarget maps a nickname, an email or a config name (the dir's
// current login) to a team account, for the commands that act on the
// account rather than on a dir (nick, unshare). A nickname the cache does
// not know triggers one allowlist refresh, because a teammate may have
// shared or renamed it after this machine's last fetch.
func accountByTarget(ctx context.Context, env cli.Env, cfg *config.Config, client remote.Client, arg string) (email, nick string, err error) {
	cache := loadCache(env)
	arg = strings.TrimSpace(arg)
	if strings.Contains(arg, "@") {
		if email, err = normalizeEmail(arg); err != nil {
			return "", "", err
		}
		return email, cache.Nickname(email), nil
	}
	lower := strings.ToLower(arg)
	if email, ok := cache.ByNickname(lower); ok {
		return email, lower, nil
	}
	if cd, ok := cfg.Find(arg); ok {
		email, loggedIn, err := dirEmail(cd.Dir)
		switch {
		case err != nil:
			return "", "", fmt.Errorf("config %q: %w", cd.Name, err)
		case !loggedIn:
			return "", "", fmt.Errorf("config %q is not logged in, so it names no account (use the account's nickname or email)", cd.Name)
		}
		return email, cache.Nickname(email), nil
	}
	if !shell.ValidNickname(lower) {
		return "", "", cli.Usagef("%q is not a nickname, an email or a config name", arg)
	}
	fresh, _, err := sharedcache.Refresh(ctx, client, cfg.Dev, now())
	if err != nil {
		return "", "", fmt.Errorf("nickname %q is not in the cached allowlist and refreshing it failed: %w", lower, remoteErr("list accounts", err))
	}
	if email, ok := fresh.ByNickname(lower); ok {
		return email, lower, nil
	}
	return "", "", fmt.Errorf("unknown nickname %q (`julienning accounts` lists the team's accounts; an email works too)", lower)
}

// nicknameTaken is the message for a 409 from the Worker. holder is the
// account the allowlist says owns nick ("" when unknown; the Worker's own
// message is shown then, in case it names the holder).
func nicknameTaken(nick, holder string, err error) error {
	if holder != "" {
		return fmt.Errorf("nickname %q is already used by %s", nick, holder)
	}
	var re *remote.Error
	if errors.As(err, &re) && re.Message != "" && re.Message != "nickname is taken" {
		return fmt.Errorf("nickname %q is already used by another team account (%s)", nick, re.Message)
	}
	return fmt.Errorf("nickname %q is already used by another team account", nick)
}

// holderOf returns who holds nick according to cache, unless it is self.
func holderOf(cache *sharedcache.Cache, nick, self string) string {
	if cache == nil {
		return ""
	}
	if e, ok := cache.ByNickname(nick); ok && e != self {
		return e
	}
	return ""
}

// takenBy names the holder of nick after a conflict. The conflict usually
// means the cached allowlist is behind, so it is refreshed first; the
// cached copy is the fallback.
func takenBy(ctx context.Context, env cli.Env, cfg *config.Config, client remote.Client, nick, self string) string {
	fresh, _, err := sharedcache.Refresh(ctx, client, cfg.Dev, now())
	if err != nil {
		warnf(env, "could not refresh the allowlist to see who uses %q: %v", nick, remoteErr("list accounts", err))
		fresh = loadCache(env)
	}
	return holderOf(fresh, nick, self)
}
