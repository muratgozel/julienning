package accounts

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/usage"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "accounts",
		Summary: "list the shared accounts, ranked, with usage and claims",
		Usage:   "accounts [--json]",
		Run:     runAccounts,
	})
}

func runAccounts(env cli.Env) error {
	fs := cli.NewFlagSet("accounts", env)
	asJSON := fs.Bool("json", false, "print the Worker JSON with local_config and local_configs added per account, plus the syncing accounts")
	if err := fs.Parse(env.Args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return cli.Usagef("accounts takes no arguments")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if w := claudecfg.SetupWarning(cfg.Configs); w != "" {
		warnf(env, "%s", w)
	}
	if err := cfg.RequireRemote(); err != nil {
		return err
	}
	n, err := now()
	if err != nil {
		return err
	}

	// Claims first (SPEC "accounts"): this also cleans up after sessions that
	// crashed without their SessionEnd hook. A failure only costs accuracy.
	rctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	_, err = claims.Sync(rctx, cfg, newClient(cfg, reconcileTimeout), n)
	cancel()
	if err != nil {
		warnf(env, "could not sync this machine's claims (%v)", err)
	}

	listing, err := newClient(cfg, interactiveTimeout).ListAccounts(context.Background(), cfg.Dev)
	if err != nil {
		return err
	}
	// The listing is the allowlist: keep the status line's gate current.
	cache, err := sharedcache.FromListing(listing, n)
	if err != nil {
		warnf(env, "could not update the shared account cache (%v)", err)
	}
	syncRows := syncing(cache, listing)

	local, unreadable := localConfigs(cfg)
	for _, name := range slices.Sorted(maps.Keys(unreadable)) {
		warnf(env, "cannot read the account of config %s (%v)", name, unreadable[name])
	}
	cur, curOK, err := cfg.Current()
	if err != nil {
		return err
	}
	isCurrent := func(cd config.ConfigDir) bool { return curOK && cur.Dir == cd.Dir }

	if *asJSON {
		err = accountsJSON(env, listing, syncRows, local, isCurrent)
	} else {
		err = accountsTable(env, cfg, listing, syncRows, local, isCurrent, n)
	}
	if err != nil {
		return err
	}
	updateHint(env.Stderr)
	return nil
}

// syncingAccount is an account this machine just shared that the Worker's
// listing does not show yet.
type syncingAccount struct {
	email    string // lowercased
	nickname string // "" when unknown
}

// syncingState is the STATE of a syncing row.
const syncingState = "syncing (just shared)"

// syncing returns, sorted by email, the cache's LocalAdds that the listing
// lacks: KV listings lag writes by up to a minute, so an account shared here
// a moment ago would otherwise look unshared. cache must be the one
// sharedcache.FromListing returned: it already dropped adds older than
// sharedcache.LocalAddGrace, measured on the wall clock the adds were stamped
// with (not JULIENNING_NOW_EPOCH), so they are not filtered again here. nil
// (the cache could not be saved, which was warned about) yields none.
func syncing(cache *sharedcache.Cache, l *remote.Listing) []syncingAccount {
	if cache == nil || len(cache.LocalAdds) == 0 {
		return nil
	}
	listed := make(map[string]bool, len(l.Accounts))
	for _, a := range l.Accounts {
		listed[strings.ToLower(a.Email)] = true
	}
	var out []syncingAccount
	for _, e := range slices.Sorted(maps.Keys(cache.LocalAdds)) {
		e = strings.ToLower(e)
		if !listed[e] {
			out = append(out, syncingAccount{email: e, nickname: cache.Nickname(e)})
		}
	}
	return out
}

func accountsTable(env cli.Env, cfg *config.Config, listing *remote.Listing, syncRows []syncingAccount, local map[string][]config.ConfigDir,
	isCurrent func(config.ConfigDir) bool, n time.Time) error {
	if len(listing.Accounts) == 0 && len(syncRows) == 0 {
		fmt.Fprintln(env.Stdout, "no shared accounts yet (share one: julienning share EMAIL)")
		return nil
	}
	loc := location()
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tNICK\tACCOUNT\tSESSION\tWEEK\tSTATE\tLOCAL\tUPDATED")
	for i := range listing.Accounts {
		a := &listing.Accounts[i]
		rank := a.Rank
		if rank == 0 {
			rank = i + 1
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			rank, orDash(a.Nickname), a.Email, column(a.Session, n, loc), column(a.Week, n, loc),
			state(a, cfg.Dev, n, loc), localColumn(local[strings.ToLower(a.Email)], isCurrent), updated(a, n))
	}
	// Unranked: the Worker has not listed them yet.
	for _, s := range syncRows {
		fmt.Fprintf(tw, "-\t%s\t%s\t-\t-\t%s\t%s\t-\n",
			orDash(s.nickname), s.email, syncingState, localColumn(local[s.email], isCurrent))
	}
	return tw.Flush()
}

// localColumn lists every registered dir logged into the account as a
// home-shortened path, the selected one marked with `*`; "-" when none. Paths,
// not config names: users never type dir names, but they do need to know
// which directory holds the login.
func localColumn(dirs []config.ConfigDir, isCurrent func(config.ConfigDir) bool) string {
	if len(dirs) == 0 {
		return "-"
	}
	paths := make([]string, len(dirs))
	for i, cd := range dirs {
		paths[i] = shortenHome(cd.Dir)
		if isCurrent(cd) {
			paths[i] = "*" + paths[i]
		}
	}
	return strings.Join(paths, ",")
}

// orDash renders an empty cell as "-" (records shared before nicknames
// existed have none until setup assigns one).
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// column renders one window as "50% → 17:30"; an absent window is "-".
func column(w *remote.Window, n time.Time, loc *time.Location) string {
	if w == nil {
		return "-"
	}
	return window(w, n, loc)
}

// state renders the STATE column. busy_by is the Worker's verdict from this
// dev's perspective (every other dev holding or using it); the reporter and
// the claim holders are only a fallback for a Worker that left it empty.
// Exhausted wins over every other state, as on the Worker.
// ownActivityTTL mirrors the Worker's ACTIVITY_TTL_MIN: a usage report
// younger than this counts as "in use".
const ownActivityTTL = 15 * time.Minute

// mine reports whether the querying dev holds a claim on the account or
// reported usage recently. The Worker leaves one's own activity out of
// `state`/`busy_by` on purpose (so `next` never avoids your own account), so
// the table has to add "you" back for the human reading it.
func mine(a *remote.Account, self string, n time.Time) bool {
	if self == "" {
		return false
	}
	for _, c := range a.Claims {
		if c.Dev == self {
			return true
		}
	}
	return a.Reporter != nil && a.Reporter.Dev == self && a.CollectedAt != nil && n.Sub(*a.CollectedAt) < ownActivityTTL
}

func state(a *remote.Account, self string, n time.Time, loc *time.Location) string {
	if a.IsExhausted() {
		s := exhaustedState(a, n, loc)
		if mine(a, self, n) {
			if len(a.BusyBy) > 0 {
				return s + " and you"
			}
			return s + ", in use by you"
		}
		return s
	}
	you := mine(a, self, n)
	switch a.State {
	case "in_use":
		who := a.BusyBy
		if len(who) == 0 && a.Reporter != nil {
			who = []string{a.Reporter.Dev}
		}
		s := busy("in use", who, a.CollectedAt, n)
		if you {
			s += " and you"
		}
		return s
	case "claimed":
		who := a.BusyBy
		if len(who) == 0 {
			who = claimDevs(a.Claims, self)
		}
		s := busy("claimed", who, earliestClaim(a.Claims, who), n)
		if you {
			s += " and you"
		}
		return s
	default:
		if you {
			return "in use by you"
		}
		return "free"
	}
}

// exhaustedState renders `exhausted (week resets Fri 10:00)`: the window the
// account waits on and its reset in loc. Either part may be unknown:
// `exhausted (week)` (a refusal reported without its reset), `exhausted
// (resets …)` (no window matches exhausted_until), bare `exhausted` without
// both. Other devs on it follow as `, in use by ali, can` (no age: in this
// state busy_by does not say whether they hold a claim or reported usage).
func exhaustedState(a *remote.Account, n time.Time, loc *time.Location) string {
	var parts []string
	if w := a.ExhaustedWindow(); w != "" {
		parts = append(parts, w)
	}
	if until := a.ExhaustedUntil; until != nil {
		if until.After(n) {
			parts = append(parts, "resets "+usage.FormatReset(*until, n, loc))
		} else {
			// Our clock is past the Worker's exhausted_until (clock skew):
			// "resets reset" would be nonsense.
			parts = append(parts, "has reset")
		}
	}
	var b strings.Builder
	b.WriteString("exhausted")
	if len(parts) > 0 {
		b.WriteString(" (" + strings.Join(parts, " ") + ")")
	}
	if len(a.BusyBy) > 0 {
		b.WriteString(", in use by " + strings.Join(a.BusyBy, ", "))
	}
	return b.String()
}

// claimDevs returns the sorted, unique devs other than self holding a claim.
func claimDevs(cs []remote.Claim, self string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if c.Dev != self && c.Dev != "" && !seen[c.Dev] {
			seen[c.Dev] = true
			out = append(out, c.Dev)
		}
	}
	sort.Strings(out)
	return out
}

// earliestClaim is when the first of the given devs claimed the account, so
// "(3m)" reads as "busy for 3 minutes".
func earliestClaim(cs []remote.Claim, devs []string) *time.Time {
	var first *time.Time
	for i := range cs {
		c := &cs[i]
		if !slices.Contains(devs, c.Dev) || c.At.IsZero() {
			continue
		}
		if first == nil || c.At.Before(*first) {
			first = &c.At
		}
	}
	return first
}

func busy(what string, who []string, since *time.Time, n time.Time) string {
	var b strings.Builder
	b.WriteString(what)
	if len(who) > 0 {
		b.WriteString(" by ")
		b.WriteString(strings.Join(who, ", "))
	}
	if since != nil {
		b.WriteString(" (")
		b.WriteString(usage.ShortDuration(n.Sub(*since)))
		b.WriteString(")")
	}
	return b.String()
}

func updated(a *remote.Account, n time.Time) string {
	if a.CollectedAt == nil {
		return "never"
	}
	return usage.Ago(n.Sub(*a.CollectedAt))
}

// accountsJSON passes the Worker document through (nickname included, null or
// absent for legacy records), adding local_config (the preferred local dir's
// config name: the selected one, else the first) and local_configs (all of
// them) to every account, so unknown fields keep working after a Worker
// upgrade. Syncing accounts go in a top-level "syncing" array (always
// present), never inside "accounts": they are not ranked and have no Worker
// record yet.
func accountsJSON(env cli.Env, l *remote.Listing, syncRows []syncingAccount, local map[string][]config.ConfigDir, isCurrent func(config.ConfigDir) bool) error {
	doc, err := l.Object()
	if err != nil {
		return err
	}
	list, _ := doc["accounts"].([]any)
	for _, item := range list {
		a, ok := item.(map[string]any)
		if !ok {
			continue
		}
		email, _ := a["email"].(string)
		names, preferred := configNames(local[strings.ToLower(email)], isCurrent)
		a["local_config"] = preferred
		a["local_configs"] = names
	}
	rows := make([]any, 0, len(syncRows))
	for _, s := range syncRows {
		var nick any
		if s.nickname != "" {
			nick = s.nickname
		}
		names, _ := configNames(local[s.email], isCurrent)
		rows = append(rows, map[string]any{"email": s.email, "nickname": nick, "local_configs": names})
	}
	doc["syncing"] = rows
	return printJSON(env, doc)
}

// configNames returns the config names of dirs (never nil, so JSON shows [])
// and the preferred one: the selected dir, else the first, else "".
func configNames(dirs []config.ConfigDir, isCurrent func(config.ConfigDir) bool) (names []string, preferred string) {
	names = make([]string, 0, len(dirs))
	for _, cd := range dirs {
		names = append(names, cd.Name)
		if isCurrent(cd) {
			preferred = cd.Name
		}
	}
	if preferred == "" && len(names) > 0 {
		preferred = names[0]
	}
	return names, preferred
}
