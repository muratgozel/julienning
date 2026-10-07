package dirs

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/selfupdate"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "configs",
		Summary: "list registered config dirs",
		Usage:   "configs [--json]",
		Run:     runConfigs,
	})
}

// configRow is one registered dir as `configs --json` prints it.
type configRow struct {
	Name     string `json:"name"`
	Dir      string `json:"dir"`
	Email    string `json:"email"`    // "" when not logged in
	Nickname string `json:"nickname"` // team nickname of Email; "" when none
	LoggedIn bool   `json:"logged_in"`
	// Shared is null when the allowlist cache is empty (never fetched).
	Shared     *bool  `json:"shared"`
	Current    bool   `json:"current"`
	Default    bool   `json:"default"` // Claude's default dir (~/.claude)
	EmailError string `json:"email_error,omitempty"`
}

func runConfigs(env cli.Env) error {
	fs := cli.NewFlagSet("configs", env)
	asJSON := fs.Bool("json", false, "print one JSON document")
	if err := parseFlags(fs, env.Args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cli.Usagef("unexpected argument %q", fs.Arg(0))
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	current, hasCurrent, err := cfg.Current()
	if err != nil {
		return err
	}
	cache := loadCache(env)
	known := !cache.FetchedAt.IsZero() || len(cache.Emails) > 0

	rows := make([]configRow, 0, len(cfg.Configs))
	for _, cd := range cfg.Configs {
		r := configRow{
			Name:    cd.Name,
			Dir:     cd.Dir,
			Current: hasCurrent && current.Dir == cd.Dir,
			Default: claudecfg.IsDefaultDir(cd.Dir),
		}
		email, ok, err := dirEmail(cd.Dir)
		r.Email, r.LoggedIn = email, ok
		if err != nil {
			r.EmailError = err.Error()
		}
		if known {
			shared := ok && cache.Contains(email)
			r.Shared = &shared
		}
		if ok && cache.Contains(email) {
			r.Nickname = cache.Nickname(email)
		}
		rows = append(rows, r)
	}

	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Configs []configRow `json:"configs"`
		}{rows}); err != nil {
			return fmt.Errorf("encode configs: %w", err)
		}
		selfupdate.Hint(env.Stderr)
		return nil
	}

	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "No config dirs registered. Run `julienning setup` or `julienning new-config`.")
		selfupdate.Hint(env.Stderr)
		return nil
	}
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tNAME\tNICK\tDIR\tEMAIL\tSHARED")
	for _, r := range rows {
		marker := " "
		if r.Current {
			marker = "*"
		}
		email := r.Email
		switch {
		case r.EmailError != "":
			email = "(unreadable)"
		case !r.LoggedIn:
			email = "(not logged in)"
		}
		shared := "?"
		if r.Shared != nil {
			shared = "no"
			if *r.Shared {
				shared = "yes"
			}
		}
		nick := r.Nickname
		if nick == "" {
			nick = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", marker, r.Name, nick, shortenHome(r.Dir), email, shared)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	selfupdate.Hint(env.Stderr)
	return nil
}
