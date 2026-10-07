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
	asJSON := fs.Bool("json", false, "print the Worker JSON with local_config and local_configs added per account")
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
	if _, err := sharedcache.FromListing(listing, n); err != nil {
		warnf(env, "could not update the shared account cache (%v)", err)
	}

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
		err = accountsJSON(env, listing, local, isCurrent)
	} else {
		err = accountsTable(env, cfg, listing, local, isCurrent, n)
	}
	if err != nil {
		return err
	}
	updateHint(env.Stderr)
	return nil
}

func accountsTable(env cli.Env, cfg *config.Config, listing *remote.Listing, local map[string][]config.ConfigDir,
	isCurrent func(config.ConfigDir) bool, n time.Time) error {
	if len(listing.Accounts) == 0 {
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
			state(a, cfg.Dev, n), localColumn(local[strings.ToLower(a.Email)], isCurrent), updated(a, n))
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
func state(a *remote.Account, self string, n time.Time) string {
	switch a.State {
	case "in_use":
		who := a.BusyBy
		if len(who) == 0 && a.Reporter != nil {
			who = []string{a.Reporter.Dev}
		}
		return busy("in use", who, a.CollectedAt, n)
	case "claimed":
		who := a.BusyBy
		if len(who) == 0 {
			who = claimDevs(a.Claims, self)
		}
		return busy("claimed", who, earliestClaim(a.Claims, who), n)
	default:
		return "free"
	}
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
// upgrade.
func accountsJSON(env cli.Env, l *remote.Listing, local map[string][]config.ConfigDir, isCurrent func(config.ConfigDir) bool) error {
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
		dirs := local[strings.ToLower(email)]
		names := make([]string, 0, len(dirs))
		preferred := ""
		for _, cd := range dirs {
			names = append(names, cd.Name)
			if isCurrent(cd) {
				preferred = cd.Name
			}
		}
		if preferred == "" && len(names) > 0 {
			preferred = names[0]
		}
		a["local_config"] = preferred
		a["local_configs"] = names
	}
	return printJSON(env, doc)
}
