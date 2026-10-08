package switching

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/tui"
	"github.com/muratgozel/julienning/internal/usage"

	"golang.org/x/term"
)

const (
	listTimeout      = 10 * time.Second
	reconcileTimeout = 3 * time.Second
	maxLimit         = 500
)

// Seams for tests: commands run through cli.Main without a TTY, a Worker or
// a claude binary.
var (
	newClient = func(cfg *config.Config, timeout time.Duration) remote.Client {
		return remote.NewHTTP(cfg.Remote.URL, cfg.Remote.Token, timeout)
	}
	// interactive reports whether both stdin and stdout are terminals.
	interactive = func(env cli.Env) bool { return isTerminal(env.Stdin) && isTerminal(env.Stdout) }
	// pickSession shows the picker and returns the chosen row. scope says
	// what is listed (see scopeLine); initial is the row it opens on.
	pickSession = func(env cli.Env, items []tui.Item, header, scope string, initial int) (int, error) {
		return tui.Pick(items, tui.Options{Header: header, Scope: scope, In: env.Stdin, Out: env.Stdout, Initial: initial})
	}
	// confirmMove asks before a session moves between dirs (see
	// moveQuestion); false means go back to the picker.
	confirmMove = func(env cli.Env, question, detail string) (bool, error) {
		return tui.Confirm(tui.ConfirmOptions{Question: question, Detail: detail, In: env.Stdin, Out: env.Stdout})
	}
	// execClaude replaces the process with claude (see launch.Exec).
	execClaude = func(dir string, args []string, chdir string) error { return launch.Exec(dir, args, chdir) }
	getwd      = os.Getwd
)

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// options are the flags shared by next and use.
type options struct {
	all      bool
	limit    int
	noLaunch bool
	json     bool
	clear    bool // use only
}

// parseFlags parses flags anywhere on the command line (`use NAME --json`
// as well as `use --json NAME`) and returns the positional arguments.
func parseFlags(name string, env cli.Env, withClear bool) (options, []string, map[string]bool, error) {
	var o options
	fs := cli.NewFlagSet(name, env)
	fs.BoolVar(&o.all, "all", false, "list sessions of every project, not just the current directory's")
	fs.IntVar(&o.limit, "limit", sessions.DefaultLimit, "how many recent sessions to offer")
	fs.BoolVar(&o.noLaunch, "no-launch", false, "switch only; do not start claude")
	fs.BoolVar(&o.json, "json", false, "print one JSON document (implies --no-launch)")
	if withClear {
		fs.BoolVar(&o.clear, "clear", false, "remove the selection so plain claude uses your default config")
	}
	var pos []string
	args := env.Args
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return o, nil, nil, err
			}
			return o, nil, nil, cli.Usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if o.limit < 1 || o.limit > maxLimit {
		return o, nil, nil, cli.Usagef("--limit must be between 1 and %d", maxLimit)
	}
	return o, pos, set, nil
}

// launchMode is SPEC step 3's condition.
func launchMode(env cli.Env, o options) bool {
	return !o.noLaunch && !o.json && interactive(env)
}

func warnf(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: warning: %s\n", fmt.Sprintf(format, a...))
}

// warnSetup prints one warning when a registered dir's settings.json lacks
// an entry this julienning adds (a newer version wiring a new hook), so the
// user knows to re-run setup.
func warnSetup(env cli.Env, cfg *config.Config) {
	if w := claudecfg.SetupWarning(cfg.Configs); w != "" {
		warnf(env, "%s", w)
	}
}

// clock honours JULIENNING_NOW_EPOCH so tests and bug reports can freeze it.
func clock() (time.Time, error) { return usage.Now(os.Getenv) }

// location resolves $TZ at call time; time.Local is fixed at process start,
// which would make t.Setenv("TZ", …) a silent no-op in tests.
func location() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// reconcile syncs claims with live sessions before ranking. It goes through
// claims.Sync (lock + reconciled marker), never claims.Reconcile directly:
// racing the detached claim-sync would lose claims.json records. Advisory:
// a failure is a warning, and a busy lock means another process is doing it.
func reconcile(env cli.Env, cfg *config.Config, c remote.Client, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	if _, err := claims.Sync(ctx, cfg, c, now); err != nil {
		warnf(env, "could not sync claims with live sessions (%v)", err)
	}
}

// localByEmail maps each logged-in email to the registered dirs holding it,
// sorted by name. Dirs whose account file cannot be read are reported and
// skipped; dirs without a login are skipped silently (normal for new dirs).
func localByEmail(env cli.Env, cfg *config.Config) map[string][]config.ConfigDir {
	out := map[string][]config.ConfigDir{}
	for _, cd := range cfg.Configs {
		email, err := claudecfg.ReadEmail(cd.Dir)
		if errors.Is(err, claudecfg.ErrNotLoggedIn) {
			continue
		}
		if err != nil {
			warnf(env, "skipping %s: %v", cd.Name, err)
			continue
		}
		out[email] = append(out[email], cd)
	}
	for e := range out {
		sort.Slice(out[e], func(i, j int) bool { return out[e][i].Name < out[e][j].Name })
	}
	return out
}

// selection is the switch target and what we know about its account.
type selection struct {
	cd       config.ConfigDir
	email    string          // "" when not logged in
	nickname string          // team nickname of email; "" when unknown
	account  *remote.Account // nil when the Worker did not list it
	shared   bool            // email on the team allowlist
	stay     bool            // next: already the current dir
}

// label is how users see the target (SPEC "Nicknames"): the account
// nickname, else the config name.
func (s selection) label() string {
	if s.nickname != "" {
		return s.nickname
	}
	return s.cd.Name
}

// summaryLine is the one-liner printed (and used as the picker header):
//
//	Now using alpha (claude1@x.io) in ~/.claude-x — session 12% → 17:30, week 28% → Tue 21:00.
//	Staying on alpha (claude1@x.io) in ~/.claude-x (best available) — …
//
// Without a nickname it keeps the v1 form, naming the config dir:
//
//	Now using julienning3 (claude3@x.io) — …
//	Staying on julienning3 (best available) — …
func summaryLine(sel selection, now time.Time, loc *time.Location) string {
	var b strings.Builder
	if sel.stay {
		b.WriteString("Staying on " + sel.label())
	} else {
		b.WriteString("Now using " + sel.label())
	}
	switch {
	case sel.nickname != "":
		b.WriteString(" (" + sel.email + ") in " + shortenHome(sel.cd.Dir))
	case sel.email != "" && !sel.stay:
		b.WriteString(" (" + sel.email + ")")
	}
	if sel.stay {
		b.WriteString(" (best available)")
	}
	if sel.account != nil {
		b.WriteString(" — " + usageSummary(sel.account, now, loc))
	}
	b.WriteString(".")
	return b.String()
}

// usageSummary renders "session 12% → 17:30, week 28% → Tue 21:00".
func usageSummary(a *remote.Account, now time.Time, loc *time.Location) string {
	if a.Session == nil && a.Week == nil {
		return "no usage reported yet"
	}
	return "session " + window(a.Session, now, loc) + ", week " + window(a.Week, now, loc)
}

func window(w *remote.Window, now time.Time, loc *time.Location) string {
	if w == nil {
		return "unknown"
	}
	reset := "?"
	if w.ResetsAt != nil {
		reset = usage.FormatReset(*w.ResetsAt, now, loc)
	}
	return usage.FormatPercent(w.Percent()) + "% → " + reset
}

// jsonDoc is the --json output: the Worker's account object (unknown fields
// preserved) plus "config" and "nickname"; without an account, just config,
// email and nickname. Unknown values are null.
func jsonDoc(sel selection) (map[string]any, error) {
	var nick any
	if sel.nickname != "" {
		nick = sel.nickname
	}
	if sel.account != nil {
		obj, err := sel.account.Object()
		if err != nil {
			return nil, err
		}
		obj["config"] = sel.cd.Name
		obj["nickname"] = nick
		return obj, nil
	}
	doc := map[string]any{"config": sel.cd.Name, "email": nil, "nickname": nick}
	if sel.email != "" {
		doc["email"] = sel.email
	}
	return doc, nil
}

func printJSON(env cli.Env, doc any) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}
	_, err = fmt.Fprintf(env.Stdout, "%s\n", raw)
	return err
}

func findAccount(l *remote.Listing, email string) *remote.Account {
	if l == nil || email == "" {
		return nil
	}
	for i := range l.Accounts {
		if strings.EqualFold(l.Accounts[i].Email, email) {
			return &l.Accounts[i]
		}
	}
	return nil
}

// accountName is "alpha (claude1@x.io)", or just the email without a nickname.
func accountName(nick, email string) string {
	if nick == "" {
		return email
	}
	return nick + " (" + email + ")"
}

// fetchListing syncs claims with live sessions, then lists the team's
// accounts. A failure is a warning and returns nil: `use` must work offline.
func fetchListing(env cli.Env, cfg *config.Config, now time.Time) *remote.Listing {
	client := newClient(cfg, listTimeout)
	reconcile(env, cfg, client, now)
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	l, err := client.ListAccounts(ctx, cfg.Dev)
	if err != nil {
		warnf(env, "could not reach the Worker (%v); using the cached shared-account list", err)
		return nil
	}
	return l
}

// refreshCache saves the listing's allowlist and nicknames and returns the
// cache to resolve and label with. When saving fails the listing still
// knows the nicknames, so an in-memory copy is returned instead of nil.
func refreshCache(env cli.Env, l *remote.Listing, now time.Time) *sharedcache.Cache {
	c, err := sharedcache.FromListing(l, now)
	if err == nil {
		return c
	}
	warnf(env, "could not update the shared-account cache (%v)", err)
	mem := &sharedcache.Cache{FetchedAt: now.UTC(), Nicknames: map[string]string{}}
	for _, a := range l.Accounts {
		e := strings.ToLower(a.Email)
		mem.Emails = append(mem.Emails, e)
		if n := strings.ToLower(strings.TrimSpace(a.Nickname)); n != "" {
			mem.Nicknames[e] = n
		}
	}
	sort.Strings(mem.Emails)
	return mem
}

// loadCache reads the cached allowlist; nil (after a warning) when it is
// unreadable, which resolve and the labels treat as "no nicknames known".
func loadCache(env cli.Env) *sharedcache.Cache {
	c, err := sharedcache.Load()
	if err != nil {
		warnf(env, "cannot read the shared-account cache (%v)", err)
		return nil
	}
	return c
}

// dirLabels maps every registered config name to how users see it: the
// nickname of the account it is logged into right now, else its name.
func dirLabels(cfg *config.Config, cache *sharedcache.Cache) map[string]string {
	out := make(map[string]string, len(cfg.Configs))
	for _, cd := range cfg.Configs {
		// An unreadable or missing login only changes the label to the
		// config name; the commands warn about unreadable logins elsewhere.
		email, err := claudecfg.ReadEmail(cd.Dir)
		if err != nil {
			email = ""
		}
		out[cd.Name] = resolve.Label(cache, cd, email)
	}
	return out
}
