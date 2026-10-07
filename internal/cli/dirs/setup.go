package dirs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/discover"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/shell"
)

// rcMarker ends the line setup appends to the rc file. Re-runs look for it, so
// a user who rewrites the line keeps the marker to stay idempotent.
//
// legacyRCMarker is what builds before v0.4 wrote. It is too generic (an
// unrelated `# julienning is great` comment matched it), so a line still
// carrying it is rewritten in place rather than duplicated.
const (
	rcMarker       = "# julienning-shell-hook"
	legacyRCMarker = "# julienning"
)

// indent aligns continuation lines under the "Label:   " column.
const indent = "          "

func init() {
	cli.Register(&cli.Command{
		Name:    "setup",
		Summary: "set up this machine: identity, Worker, config dirs, settings.json, shell hook",
		Usage:   "setup [--dev NAME] [--remote-url URL] [--token TOKEN] [--name-prefix NAME] [--shell zsh|bash] [--no-rc] [--yes] [--share EMAIL]... [--nick EMAIL=NAME]...",
		Run:     runSetup,
	})
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runSetup(env cli.Env) error {
	fs := cli.NewFlagSet("setup", env)
	dev := fs.String("dev", "", "developer short name (default: prompt, or $USER)")
	remoteURL := fs.String("remote-url", "", "Worker base URL")
	token := fs.String("token", "", "team token for the Worker")
	shellName := fs.String("shell", "", "shell to install the hook for (zsh or bash)")
	noRC := fs.Bool("no-rc", false, "do not touch the shell rc file")
	yes := fs.Bool("yes", false, "never prompt; take the non-interactive defaults")
	namePrefix := fs.String("name-prefix", "", "name dirs registered from now on <NAME><N> (default julienning: julienning1, …)")
	var shareFlags, nickFlags stringList
	fs.Var(&shareFlags, "share", "share this account with the team (repeatable)")
	fs.Var(&nickFlags, "nick", "team nickname for an account named by this run, as EMAIL=NAME (repeatable; default: the email's local part)")
	if err := parseFlags(fs, env.Args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cli.Usagef("unexpected argument %q", fs.Arg(0))
	}
	if *shellName != "" && !shell.Supported(*shellName) {
		return cli.Usagef("unsupported shell %q (want zsh or bash)", *shellName)
	}
	if *namePrefix != "" && !config.ValidNamePrefix(*namePrefix) {
		return cli.Usagef("invalid --name-prefix %q: %s (e.g. --name-prefix team)", *namePrefix, config.NamePrefixRule)
	}
	if *remoteURL != "" {
		u, err := validateRemoteURL(*remoteURL)
		if err != nil {
			return cli.Usagef("%v", err)
		}
		*remoteURL = u
	}
	toShare := map[string]bool{}
	for _, s := range shareFlags {
		e, err := normalizeEmail(s)
		if err != nil {
			return err
		}
		toShare[e] = true
	}
	nicks, err := parseNickFlags(nickFlags)
	if err != nil {
		return err
	}

	in := bufio.NewReader(env.Stdin)
	interactive := !*yes && isInteractive(env)

	// 1. identity
	cfg, fresh, err := loadOrCreate(env, in, interactive, *dev)
	if err != nil {
		return err
	}
	state := "unchanged"
	if fresh {
		state = "created"
	}
	fmt.Fprintf(env.Stdout, "Identity: dev=%s machine=%s (%s)\n", cfg.Dev, cfg.MachineID, state)
	if *namePrefix != "" {
		setNamePrefix(env, cfg, *namePrefix)
	}

	// 2. remote (never print the token)
	listing, err := setupRemote(env, cfg, in, interactive, *remoteURL, *token)
	if err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	// 3. allowlist
	cache := setupAllowlist(env, listing)

	// 4. discover and classify
	var client remote.Client
	if listing != nil {
		client = newClient(cfg)
	}
	sh := &sharer{env: env, cfg: cfg, cache: cache, client: client, in: in, interactive: interactive, nicks: nicks, used: map[string]bool{}}
	shareFailures, err := classifyDirs(sh, listing, toShare)
	if err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	// 5. settings.json of every registered dir
	patchFailures := patchAll(env, cfg)

	// 6. shell hook
	if err := shellHook(env, cfg, cache, *shellName, *noRC); err != nil {
		return err
	}

	// 7. next steps
	fmt.Fprintln(env.Stdout, "")
	fmt.Fprintln(env.Stdout, "Next steps:")
	fmt.Fprintf(env.Stdout, "  open a new terminal, or run: %s\n", sourceHint(*shellName))
	if len(cfg.Configs) == 0 {
		fmt.Fprintln(env.Stdout, "  julienning new-config --login   # create a config dir for a team account")
	}
	fmt.Fprintln(env.Stdout, "  julienning accounts   # who is free right now")
	fmt.Fprintln(env.Stdout, "  julienning next       # switch to the best account")
	selfupdate.Hint(env.Stderr)
	// Every step runs before failing, so one broken dir or share never hides
	// the rest of the report; the exit status still says something is wrong.
	var failed []string
	if shareFailures > 0 {
		failed = append(failed, fmt.Sprintf("could not share %d account(s) with the team", shareFailures))
	}
	if patchFailures > 0 {
		failed = append(failed, fmt.Sprintf("could not update settings.json of %d config dir(s)", patchFailures))
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s (see above); fix and re-run setup to retry", strings.Join(failed, "; "))
	}
	return nil
}

// loadOrCreate returns the existing config or a brand new one. The machine id
// is generated exactly once, on the first run, and never regenerated.
func loadOrCreate(env cli.Env, in *bufio.Reader, interactive bool, devFlag string) (*config.Config, bool, error) {
	cfg, err := config.Load()
	switch {
	case err == nil:
		if devFlag != "" {
			name, err := normalizeDev(devFlag)
			if err != nil {
				return nil, false, err
			}
			cfg.Dev = name
		}
		return cfg, false, nil
	case errors.Is(err, config.ErrNotSetup):
	default:
		return nil, false, err
	}

	name, err := chooseDev(env, in, interactive, devFlag)
	if err != nil {
		return nil, false, err
	}
	cfg, err = config.New(name)
	if err != nil {
		return nil, false, err
	}
	return cfg, true, nil
}

func chooseDev(env cli.Env, in *bufio.Reader, interactive bool, devFlag string) (string, error) {
	if devFlag != "" {
		return normalizeDev(devFlag)
	}
	fallback := strings.ToLower(strings.TrimSpace(os.Getenv("USER")))
	if interactive {
		label := fallback
		if label == "" {
			label = "<required>"
		}
		if answer := prompt(env.Stdout, in, fmt.Sprintf("Your dev name [%s]: ", label)); answer != "" {
			return normalizeDev(answer)
		}
	}
	if fallback == "" {
		return "", errors.New("cannot determine a dev name ($USER is empty); pass --dev NAME")
	}
	return normalizeDev(fallback)
}

func normalizeDev(s string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(s))
	if !config.ValidDevName(name) {
		return "", fmt.Errorf("invalid dev name %q: use lowercase letters, digits, dot, underscore or dash, max 32 chars (e.g. --dev murat)", s)
	}
	return name, nil
}

// setupRemote settles the Worker URL and token (flags, else config, else
// prompt), verifies them, and returns the allowlist fetched on the way. A nil
// listing means the Worker is not usable this run. New values that fail to
// verify are an error so a typo is never saved; a previously saved Worker
// that is merely unreachable only degrades to the cached allowlist.
func setupRemote(env cli.Env, cfg *config.Config, in *bufio.Reader, interactive bool, urlFlag, tokenFlag string) (*remote.Listing, error) {
	prevURL, prevToken := cfg.Remote.URL, cfg.Remote.Token
	u, tok := prevURL, prevToken
	if urlFlag != "" {
		u = urlFlag
	}
	if tokenFlag != "" {
		tok = tokenFlag
	}
	if u == "" && interactive {
		answer := prompt(env.Stdout, in, "Worker URL (ask your team lead; empty to skip): ")
		if answer != "" {
			v, err := validateRemoteURL(answer)
			if err != nil {
				return nil, err
			}
			u = v
		}
	}
	for attempt := 0; ; attempt++ {
		if u != "" && tok == "" && interactive {
			t, err := readSecret(env, "Team token (input hidden): ")
			if err != nil {
				return nil, err
			}
			tok = t
		}
		changed := u != prevURL || tok != prevToken
		cfg.Remote.URL, cfg.Remote.Token = u, tok
		state := "unchanged"
		if changed {
			state = "updated"
		}
		switch {
		case u == "":
			fmt.Fprintln(env.Stdout, "Remote:   not configured (pass --remote-url URL --token TOKEN, or run setup in a terminal)")
			return nil, nil
		case tok == "":
			fmt.Fprintf(env.Stdout, "Remote:   %s (%s, no token: pass --token TOKEN)\n", u, state)
			return nil, nil
		}
		listing, err := verifyRemote(cfg)
		switch {
		case err == nil:
			fmt.Fprintf(env.Stdout, "Remote:   %s (%s, verified)\n", u, state)
			return listing, nil
		case isAuthError(err) && interactive && tokenFlag == "" && attempt == 0:
			fmt.Fprintln(env.Stdout, "The Worker rejected the team token; enter the current one.")
			tok = ""
			continue
		case isAuthError(err):
			return nil, errTokenRejected
		case changed:
			return nil, fmt.Errorf("could not verify the Worker, nothing was saved: %v (check --remote-url and your network)", err)
		default:
			fmt.Fprintf(env.Stdout, "Remote:   %s (unchanged, unreachable: %v)\n", u, err)
			return nil, nil
		}
	}
}

// setNamePrefix applies --name-prefix. It is saved with the rest of the
// config; configs registered before keep their names.
func setNamePrefix(env cli.Env, cfg *config.Config, prefix string) {
	state := "unchanged"
	if prefix != cfg.Prefix() {
		state = "updated"
		if len(cfg.Configs) > 0 {
			state += "; registered configs keep their names, see `julienning rename`"
		}
	}
	cfg.NamePrefix = prefix
	fmt.Fprintf(env.Stdout, "Names:    %s1, %s2, … for newly registered dirs (%s)\n", prefix, prefix, state)
}

// verifyRemote checks GET /healthz, then an authenticated GET /accounts that
// proves the token and yields the allowlist.
func verifyRemote(cfg *config.Config) (*remote.Listing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*remoteTimeout)
	defer cancel()
	if err := checkHealth(ctx, cfg.Remote.URL); err != nil {
		return nil, err
	}
	return newClient(cfg).ListAccounts(ctx, cfg.Dev)
}

// setupAllowlist saves the fetched allowlist, or falls back to the cache.
// An unusable cache means "nothing is shared" (sharedcache's rule).
func setupAllowlist(env cli.Env, listing *remote.Listing) *sharedcache.Cache {
	if listing != nil {
		cache, err := sharedcache.FromListing(listing, now())
		if err == nil {
			fmt.Fprintf(env.Stdout, "Shared:   %d account(s) on the team allowlist\n", len(cache.Emails))
			return cache
		}
		warnf(env, "could not save the allowlist: %v", err)
		c := &sharedcache.Cache{FetchedAt: now().UTC(), Nicknames: map[string]string{}}
		for _, a := range listing.Accounts {
			e := strings.ToLower(a.Email)
			c.Emails = append(c.Emails, e)
			if a.Nickname != "" {
				c.Nicknames[e] = strings.ToLower(a.Nickname)
			}
		}
		c.Emails = sortedUnique(c.Emails)
		return c
	}
	cache, err := sharedcache.Load()
	if err != nil {
		warnf(env, "ignoring the cached allowlist: %v", err)
		cache = &sharedcache.Cache{}
	}
	if cache.FetchedAt.IsZero() {
		fmt.Fprintln(env.Stdout, "Shared:   unknown (no Worker yet; nothing is treated as shared)")
	} else {
		age := now().Sub(cache.FetchedAt).Round(time.Minute)
		fmt.Fprintf(env.Stdout, "Shared:   %d account(s) (cached %s ago)\n", len(cache.Emails), age)
	}
	return cache
}

// dirRow is one line of the setup report.
type dirRow struct {
	dir, nick, email, status string
}

// parseNickFlags validates --nick EMAIL=NAME values at the boundary.
func parseNickFlags(vals []string) (map[string]string, error) {
	out := map[string]string{}
	owner := map[string]string{}
	for _, v := range vals {
		i := strings.LastIndex(v, "=")
		if i <= 0 {
			return nil, cli.Usagef("invalid --nick %q: want EMAIL=NAME", v)
		}
		email, err := normalizeEmail(v[:i])
		if err != nil {
			return nil, err
		}
		nick, err := normalizeNickname(v[i+1:])
		if err != nil {
			return nil, cli.Usagef("invalid --nick %q: %v", v, err)
		}
		if prev, ok := out[email]; ok && prev != nick {
			return nil, cli.Usagef("--nick gives %s two nicknames (%s, %s)", email, prev, nick)
		}
		if o, ok := owner[nick]; ok && o != email {
			return nil, cli.Usagef("--nick gives the nickname %s to both %s and %s", nick, o, email)
		}
		out[email], owner[nick] = nick, email
	}
	return out, nil
}

// sharer shares and names accounts during setup step 4.
type sharer struct {
	env         cli.Env
	cfg         *config.Config
	cache       *sharedcache.Cache // the allowlist from step 3; shares and names are applied to it
	client      remote.Client      // nil when the Worker is not usable this run
	in          *bufio.Reader
	interactive bool
	nicks       map[string]string // --nick EMAIL=NAME
	used        map[string]bool   // emails whose --nick was applied
	cacheWarned bool
}

// share adds email to the allowlist. The nickname is --nick, else asked for
// when prompted (the user just answered yes in a terminal), else the
// default; prompted shares ask again after a taken nickname.
func (s *sharer) share(email string, prompted bool) error {
	if s.client == nil {
		return errors.New("Worker not reachable")
	}
	nick, given := s.nicks[email]
	if given {
		s.used[email] = true
	} else if !prompted {
		if nick = defaultNickname(email); nick == "" {
			return fmt.Errorf("no nickname can be derived from %s; pass --nick %s=NAME", email, email)
		}
	}
	id := remote.Identity{Dev: s.cfg.Dev, MachineID: s.cfg.MachineID}
	_, err := s.withNickname(email, nick, prompted, "share", "pass --nick "+email+"=NAME", func(ctx context.Context, n string) error {
		return s.client.Share(ctx, email, n, id)
	})
	return err
}

// nameLegacy gives a nickname to shared accounts this machine is logged
// into whose Worker record predates nicknames: in a terminal it asks (the
// default offered), otherwise it assigns --nick or the default. Each
// assignment is printed. Failures are warnings, not setup failures: the
// account stays shared and usable by email or config name.
func (s *sharer) nameLegacy(order []string, listing *remote.Listing) {
	if listing == nil || s.client == nil {
		return
	}
	legacy := map[string]bool{}
	for _, a := range listing.Accounts {
		if a.Nickname == "" {
			legacy[strings.ToLower(a.Email)] = true
		}
	}
	for _, e := range order {
		if !legacy[e] || s.cache.Nickname(e) != "" {
			continue
		}
		nick, given := s.nicks[e]
		if given {
			s.used[e] = true
		} else if !s.interactive {
			if nick = defaultNickname(e); nick == "" {
				warnf(s.env, "%s has no nickname and none can be derived from it; name it with: julienning nick %s NAME", e, e)
				continue
			}
		}
		got, err := s.withNickname(e, nick, s.interactive, "set nickname", "", func(ctx context.Context, n string) error {
			return s.client.SetNickname(ctx, e, n)
		})
		if err != nil {
			warnf(s.env, "%s has no nickname yet: %v; name it with: julienning nick %s NAME", e, err, e)
			continue
		}
		fmt.Fprintf(s.env.Stdout, "Nickname: %s is now %s (rename with: julienning nick %s NEW)\n", e, got, got)
	}
}

// withNickname runs call (Share or SetNickname) with nick, asking for one
// first when nick is empty. With ask, a taken nickname is reported and asked
// for again; without, it is an error, followed by hint when one is given.
// Success is recorded in the allowlist cache.
func (s *sharer) withNickname(email, nick string, ask bool, action, hint string, call func(context.Context, string) error) (string, error) {
	for {
		if nick == "" {
			n, ok := s.askNickname(email)
			if !ok {
				return "", errors.New("no nickname entered")
			}
			nick = n
		}
		ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
		err := call(ctx, nick)
		cancel()
		switch {
		case err == nil:
			s.remember(email, nick)
			return nick, nil
		case remote.IsConflict(err) && ask:
			fmt.Fprintf(s.env.Stdout, "%s%v; choose another.\n", indent, nicknameTaken(nick, holderOf(s.cache, nick, email), err))
			nick = ""
		case remote.IsConflict(err) && hint != "":
			return "", fmt.Errorf("%w; %s", nicknameTaken(nick, holderOf(s.cache, nick, email), err), hint)
		case remote.IsConflict(err):
			return "", nicknameTaken(nick, holderOf(s.cache, nick, email), err)
		default:
			return "", remoteErr(action, err)
		}
	}
}

// askNickname prompts until it gets a valid nickname; Enter takes the
// default. ok is false when input ends.
func (s *sharer) askNickname(email string) (string, bool) {
	def := defaultNickname(email)
	q := fmt.Sprintf("Nickname for %s [%s]: ", email, def)
	if def == "" {
		q = fmt.Sprintf("Nickname for %s: ", email)
	}
	for {
		answer, ok := ask(s.env.Stdout, s.in, q)
		switch {
		case !ok:
			return "", false
		case answer == "" && def != "":
			return def, true
		case answer == "":
			fmt.Fprintf(s.env.Stdout, "%sa nickname is required (%s)\n", indent, shell.NicknameRule)
			continue
		}
		n, err := normalizeNickname(answer)
		if err != nil {
			fmt.Fprintf(s.env.Stdout, "%s%v\n", indent, err)
			continue
		}
		return n, true
	}
}

// remember applies a share or naming to the in-memory allowlist and to
// shared.json. The step-3 listing predates it and a refresh would not show
// it yet either (KV listings lag writes); AddWithNickname keeps fetched_at,
// because the rest of the list is no fresher than before.
func (s *sharer) remember(email, nick string) {
	s.cache.Emails = sortedUnique(append(s.cache.Emails, email))
	if s.cache.Nicknames == nil {
		s.cache.Nicknames = map[string]string{}
	}
	s.cache.Nicknames[email] = nick
	if _, err := sharedcache.AddWithNickname(email, nick); err != nil && !s.cacheWarned {
		s.cacheWarned = true
		warnf(s.env, "could not update the allowlist cache: %v", err)
	}
}

// classifyDirs implements setup step 4: discover dirs, decide per email
// whether it is shared (sharing it under a nickname when asked to), name
// shared accounts that have no nickname yet, register shared dirs, and
// print one line per dir. It returns how many shares failed.
func classifyDirs(s *sharer, listing *remote.Listing, toShare map[string]bool) (int, error) {
	env, cfg, cache := s.env, s.cfg, s.cache
	cands, err := discover.Find(cfg.Configs, cfg.Prefix())
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, c := range cands {
		seen[c.Dir] = true
	}
	// Registered dirs outside discovery's reach (or deleted) are reported too.
	for _, cd := range cfg.Configs {
		if seen[filepath.Clean(cd.Dir)] {
			continue
		}
		c := discover.Candidate{Dir: cd.Dir, Name: cd.Name, Registered: true, Source: "registered"}
		c.Email, c.LoggedIn, c.Err = dirEmail(cd.Dir)
		cands = append(cands, c)
	}

	// Decide every email once, in listing order, before printing the table.
	firstDir := map[string]string{}
	var order []string
	for _, c := range cands {
		if c.LoggedIn {
			if _, ok := firstDir[c.Email]; !ok {
				firstDir[c.Email] = c.Dir
				order = append(order, c.Email)
			}
		}
	}
	for _, e := range sortedKeys(toShare) {
		if _, found := firstDir[e]; !found {
			fmt.Fprintf(env.Stdout, "Note:     --share %s: no config dir here is logged in as it (use `julienning share %s` to share it anyway)\n", e, e)
		}
	}
	shareErr := map[string]error{}
	failures := 0
	for _, e := range order {
		share, prompted := false, false
		switch {
		case cache.Contains(e):
		case toShare[e]:
			share = true
		case cfg.Declined(e):
		case s.interactive && s.client != nil:
			q := fmt.Sprintf("Share %s (found in %s) with the team? [y/N] ", e, shortenHome(firstDir[e]))
			if confirm(env.Stdout, s.in, q) {
				share, prompted = true, true
			} else {
				cfg.Decline(e)
			}
		}
		if !share {
			continue
		}
		if err := s.share(e, prompted); err != nil {
			shareErr[e] = err
			failures++
			continue
		}
		cfg.Undecline(e)
	}
	s.nameLegacy(order, listing)
	for _, e := range sortedKeys(s.nicks) {
		if s.used[e] {
			continue
		}
		n := s.nicks[e]
		switch _, found := firstDir[e]; {
		case !found:
			fmt.Fprintf(env.Stdout, "Note:     --nick %s=%s: no config dir here is logged in as it (use `julienning share %s --nick %s` to share it anyway)\n", e, n, e, n)
		case s.client == nil:
			fmt.Fprintf(env.Stdout, "Note:     --nick %s=%s: not applied (Worker not reachable; re-run setup later)\n", e, n)
		case cache.Nickname(e) != "" && cache.Nickname(e) != n:
			fmt.Fprintf(env.Stdout, "Note:     --nick %s=%s: it is already called %s (rename with: julienning nick %s %s)\n", e, n, cache.Nickname(e), cache.Nickname(e), n)
		case !cache.Contains(e):
			fmt.Fprintf(env.Stdout, "Note:     --nick %s=%s: not shared in this run (add --share %s)\n", e, n, e)
		}
	}

	// Re-name now that the shares are settled: the dirs registered below get
	// the low numbers.
	discover.AssignNames(cands, cfg.Configs, cfg.Prefix(), func(c discover.Candidate) bool {
		return c.LoggedIn && cache.Contains(c.Email)
	})
	rows := make([]dirRow, 0, len(cands))
	for _, c := range cands {
		// Config names are internal labels: the NICK column shows what people
		// type; the name appears only in the status of registered dirs.
		row := dirRow{dir: shortenHome(c.Dir), nick: "-", email: "-"}
		if c.LoggedIn {
			row.email = c.Email
			if n := cache.Nickname(c.Email); n != "" && cache.Contains(c.Email) {
				row.nick = n
			}
		}
		_, statErr := os.Stat(c.Dir)
		switch {
		case c.Registered && os.IsNotExist(statErr):
			row.status = fmt.Sprintf("missing (registered as %s; run `julienning forget %s`)", c.Name, c.Name)
		case c.Err != nil:
			row.status = "unreadable account file: " + c.Err.Error()
		case !c.LoggedIn && c.Registered:
			row.status = fmt.Sprintf("not logged in (registered as %s)", c.Name)
		case !c.LoggedIn:
			row.status = "not logged in"
		case cache.Contains(c.Email):
			if !c.Registered {
				if err := cfg.Add(config.ConfigDir{Name: c.Name, Dir: c.Dir}); err != nil {
					return failures, err
				}
			}
			row.status = fmt.Sprintf("shared (registered as %s)", c.Name)
		case shareErr[c.Email] != nil:
			row.status = "not shared (share failed: " + shareErr[c.Email].Error() + ")"
			fmt.Fprintf(env.Stderr, "julienning: share %s: %v\n", c.Email, shareErr[c.Email])
			delete(shareErr, c.Email) // report once per email
		case c.Registered && cfg.Declined(c.Email):
			row.status = fmt.Sprintf("now logged in as %s (personal, declined; registered as %s)", c.Email, c.Name)
		case cfg.Declined(c.Email):
			row.status = "personal (declined)"
		case c.Registered:
			row.status = fmt.Sprintf("now logged in as %s (not shared; registered as %s)", c.Email, c.Name)
		case s.client == nil:
			row.status = "not shared (Worker not reachable; re-run setup later)"
		default:
			row.status = "not shared (re-run setup in a terminal or pass --share EMAIL)"
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "Dirs:     no Claude config dirs found")
		return failures, nil
	}
	fmt.Fprintf(env.Stdout, "Dirs:     %d found\n", len(rows))
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\n", indent, r.dir, r.nick, r.email, r.status)
	}
	return failures, w.Flush()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// patchAll wires julienning into settings.json of every registered dir and
// returns how many dirs failed. Failures (e.g. a non-object file) are
// reported per dir and skipped so the other dirs are still patched.
func patchAll(env cli.Env, cfg *config.Config) int {
	if len(cfg.Configs) == 0 {
		fmt.Fprintln(env.Stdout, "Settings: no config dirs registered yet")
		return 0
	}
	exe, err := patchCommand(env)
	if err != nil {
		fmt.Fprintf(env.Stdout, "Settings: skipped (%v)\n", err)
		return len(cfg.Configs)
	}
	failures := 0
	parts := make([]string, 0, len(cfg.Configs))
	var notes []string
	for _, cd := range cfg.Configs {
		if fi, err := os.Stat(cd.Dir); err != nil || !fi.IsDir() {
			parts = append(parts, cd.Name+" missing")
			continue
		}
		res, err := claudecfg.Patch(cd.Dir, exe)
		if err != nil {
			fmt.Fprintf(env.Stderr, "julienning: %v\n", err)
			parts = append(parts, cd.Name+" failed")
			failures++
			continue
		}
		parts = append(parts, cd.Name+" "+res.String())
		if res.ReplacedStatusLine != "" {
			notes = append(notes, fmt.Sprintf("%snote: %s: replaced statusLine %q (saved; `julienning uninstall` or `forget` restores it)", indent, cd.Name, res.ReplacedStatusLine))
		}
	}
	fmt.Fprintf(env.Stdout, "Settings: %s\n", strings.Join(parts, ", "))
	for _, n := range notes {
		fmt.Fprintln(env.Stdout, n)
	}
	return failures
}

// rcPath returns the rc file for a shell. bash reads ~/.bash_profile for login
// shells on macOS, which is what Terminal.app starts.
func rcPath(home, shellName string) string {
	if shellName == shell.Bash {
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	}
	return filepath.Join(home, ".zshrc")
}

// sourceHint is the command that loads julienning's shell functions into
// the current terminal: sourcing the rc file setup edited.
func sourceHint(shellFlag string) string {
	shellName, _ := resolveShell(shellFlag)
	home, err := homeDir()
	if err != nil {
		return "source your shell rc file"
	}
	return "source " + shortenHome(rcPath(home, shellName))
}

func resolveShell(flagValue string) (string, string) {
	if flagValue != "" {
		return flagValue, ""
	}
	base := filepath.Base(strings.TrimSpace(os.Getenv("SHELL")))
	if shell.Supported(base) {
		return base, ""
	}
	if base != "" && base != "." && base != string(filepath.Separator) {
		return shell.Zsh, fmt.Sprintf("$SHELL is %s, which julienning cannot configure; using zsh", base)
	}
	return shell.Zsh, ""
}

func shellHook(env cli.Env, cfg *config.Config, cache *sharedcache.Cache, shellFlag string, noRC bool) error {
	if noRC {
		fmt.Fprintln(env.Stdout, "Shell:    skipped (--no-rc)")
		return nil
	}
	shellName, note := resolveShell(shellFlag)
	home, err := homeDir()
	if err != nil {
		return err
	}
	rc := rcPath(home, shellName)

	raw, err := os.ReadFile(rc)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", rc, err)
	}
	existing := string(raw)
	line := hookLine(shellName)

	switch state, at := hookState(existing); state {
	case hookPresent:
		fmt.Fprintf(env.Stdout, "Shell:    %s unchanged (hook already present)\n", shortenHome(rc))
	case hookLegacy:
		lines := strings.Split(existing, "\n")
		lines[at] = line
		if err := rewriteRC(rc, strings.Join(lines, "\n")); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Shell:    %s updated marker\n", shortenHome(rc))
	default:
		var buf strings.Builder
		buf.WriteString(existing)
		if existing != "" && !strings.HasSuffix(existing, "\n") {
			buf.WriteString("\n")
		}
		buf.WriteString(line + "\n")
		if err := rewriteRC(rc, buf.String()); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "Shell:    hook added to %s\n", shortenHome(rc))
	}
	if note != "" {
		fmt.Fprintf(env.Stdout, "%snote: %s\n", indent, note)
	}
	// A hand-written alias named like a generated function wins when typed
	// (aliases take precedence over functions), hiding the function.
	var nicks []string
	for _, en := range cache.Entries() {
		if en.Nickname != "" {
			nicks = append(nicks, en.Nickname)
		}
	}
	seen := map[int]bool{}
	for _, h := range handWrittenAliases(existing, nicks) {
		seen[h.line] = true
		fmt.Fprintf(env.Stdout, "%snote: %s:%d has a hand-written `alias claude-%s`; it hides julienning's claude-%s function, so remove it\n", indent, shortenHome(rc), h.line, h.name, h.name)
	}
	// Aliases that select a registered dir by path keep working (they export
	// CLAUDE_CONFIG_DIR, which the wrapper honours) but bypass the selection
	// and go stale when the login moves; the account's claude-<nickname>
	// function replaces each one.
	dirs := make([]string, 0, len(cfg.Configs))
	for _, cd := range cfg.Configs {
		dirs = append(dirs, cd.Dir)
	}
	hits, err := discover.RCAliases(rc, home, dirs)
	if err != nil {
		warnf(env, "could not check %s for hand-written aliases: %v", shortenHome(rc), err)
	}
	for _, h := range hits {
		if seen[h.Line] {
			continue
		}
		repl := "its account's `claude-<nickname>` function replaces it"
		if email, ok, _ := dirEmail(h.Dir); ok && cache.Contains(email) && cache.Nickname(email) != "" {
			repl = "julienning's `claude-" + cache.Nickname(email) + "` function replaces it"
		}
		fmt.Fprintf(env.Stdout, "%snote: %s:%d has a hand-written `alias %s` for %s; %s, so you can remove that line\n", indent, shortenHome(rc), h.Line, h.Alias, shortenHome(h.Dir), repl)
	}
	for _, n := range claudeAliasLines(existing) {
		warnf(env, "%s:%d defines `alias claude=…`; if it runs a path rather than `claude`, it bypasses julienning's claude wrapper and the selected config dir is ignored (make it call `claude`, or remove it)", shortenHome(rc), n)
	}
	return nil
}

var claudeAliasRe = regexp.MustCompile(`^\s*alias\s+(-[a-zA-Z]+\s+)*claude=`)

// claudeAliasLines returns the 1-based lines defining `alias claude=` (as
// Claude's own installer does). The wrapper is defined with `function claude`
// so such an alias cannot break the eval, but an alias to a binary path
// still skips the wrapper.
func claudeAliasLines(content string) []int {
	var out []int
	for i, line := range strings.Split(content, "\n") {
		if claudeAliasRe.MatchString(line) {
			out = append(out, i+1)
		}
	}
	return out
}

// hookState classifies an rc file. A hook counts as present only on a line
// that is not commented out and *ends* with the marker, so neither an
// unrelated `# julienning is great` comment nor a commented-out hook is
// mistaken for an installed hook.
type rcHookState int

const (
	hookAbsent rcHookState = iota
	hookPresent
	hookLegacy
)

// hookState returns the state and, for hookLegacy, the 0-based index of the
// line to rewrite. A new-marker line anywhere wins over a legacy one, so a
// file carrying both is never rewritten.
func hookState(content string) (rcHookState, int) {
	legacyAt := -1
	for i, line := range strings.Split(content, "\n") {
		switch hookLineKind(line) {
		case hookPresent:
			return hookPresent, i
		case hookLegacy:
			if legacyAt < 0 {
				legacyAt = i
			}
		}
	}
	if legacyAt >= 0 {
		return hookLegacy, legacyAt
	}
	return hookAbsent, -1
}

// hookLineKind classifies one rc line. Legacy lines must also mention
// `julienning shell-init`, so uninstall never deletes an unrelated line that
// merely ends with "# julienning".
func hookLineKind(line string) rcHookState {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		return hookAbsent
	case strings.HasSuffix(trimmed, rcMarker):
		return hookPresent
	case strings.HasSuffix(trimmed, legacyRCMarker) && strings.Contains(trimmed, "julienning shell-init"):
		return hookLegacy
	default:
		return hookAbsent
	}
}

func hookLine(shellName string) string {
	return fmt.Sprintf("command -v julienning >/dev/null 2>&1 && eval \"$(julienning shell-init %s)\"  %s", shellName, rcMarker)
}

// rewriteRC replaces an rc file's content atomically: a temp file in the
// real file's directory is written, fsynced and renamed over it, so a crash
// or full disk never leaves a truncated rc file. rc files are often symlinks
// into a dotfiles repo, so the link is resolved first and the file it points
// to is replaced (renaming onto the link would turn it into a plain file).
// The file's permission bits are kept; a new file gets 0644.
func rewriteRC(path, content string) error {
	target, err := resolveRCTarget(path)
	if err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(target); err == nil {
		perm = fi.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".julienning-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(name, target); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// resolveRCTarget follows symlinks to the file rewriteRC must replace. A
// missing file is written in place; a dangling symlink is written where it
// points, so the link becomes valid.
func resolveRCTarget(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	switch {
	case err == nil:
		return resolved, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	fi, lerr := os.Lstat(path)
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	dest, lerr := os.Readlink(path)
	if lerr != nil {
		return "", fmt.Errorf("resolve %s: %w", path, lerr)
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(path), dest)
	}
	return dest, nil
}

type aliasHit struct {
	line int // 1-based
	name string
}

// handWrittenAliases finds pre-existing `alias claude-<nick>` lines for
// team nicknames. They hide the generated functions, so they are reported;
// they are never edited.
func handWrittenAliases(content string, names []string) []aliasHit {
	if len(names) == 0 {
		return nil
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	re := regexp.MustCompile(`^\s*alias\s+claude-(` + strings.Join(quoted, "|") + `)=`)
	var out []aliasHit
	for i, line := range strings.Split(content, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			out = append(out, aliasHit{line: i + 1, name: m[1]})
		}
	}
	return out
}
