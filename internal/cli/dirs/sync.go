package dirs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/teamsync"
	"github.com/muratgozel/julienning/internal/tui"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "sync",
		Summary: "give every shared account here the same Claude Code settings and CLAUDE.md, asking which to keep",
		Usage:   "sync [--from NICKNAME|EMAIL|CONFIG | --check]",
		Run:     runSync,
	})
}

// Seams for tests: the questions sync asks.
var (
	// pickChoice shows one sync question (tui.Pick in choice mode) and
	// returns the chosen row; tui.ErrCanceled cancels the whole sync.
	pickChoice = func(env cli.Env, items []tui.Item, header, scope string) (int, error) {
		return tui.Pick(items, tui.Options{Header: header, Scope: scope, In: env.Stdin, Out: env.Stdout, Choice: true})
	}
	// confirmOverwrite asks before an existing CLAUDE.md is replaced.
	confirmOverwrite = func(env cli.Env, question, detail, keep string) (bool, error) {
		return tui.Confirm(tui.ConfirmOptions{
			Question: question, Detail: detail, In: env.Stdin, Out: env.Stdout,
			Hint:           "Enter or y to overwrite · n or Esc to keep " + keep + "'s",
			FallbackPrompt: "Overwrite? [Y/n] ",
		})
	}
)

// syncLabelWidth aligns the key column of sync's report.
const syncLabelWidth = 26

// syncTarget is one registered dir logged into a shared account.
type syncTarget struct {
	cd          config.ConfigDir
	label       string
	values      map[string]string // canonical values of teamsync.Settings keys
	settingsErr error             // settings.json unreadable: left out of the settings part
	md          claudecfg.ClaudeMD
	mdErr       error  // CLAUDE.md unreadable: left out of the CLAUDE.md part
	mdKey       string // CLAUDE.md with symlinks resolved, so dirs sharing one file count once
}

func (t *syncTarget) account() teamsync.Account {
	return teamsync.Account{Label: t.label, Values: t.values}
}

func runSync(env cli.Env) error {
	fs := cli.NewFlagSet("sync", env)
	from := fs.String("from", "", "make every shared account match this one, without asking")
	check := fs.Bool("check", false, "only list what differs; exit 1 when the shared accounts are out of sync")
	if err := parseFlags(fs, env.Args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cli.Usagef("unexpected argument %q", fs.Arg(0))
	}
	if *from != "" && *check {
		return cli.Usagef("--from and --check cannot be combined")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !*check && *from == "" && !isInteractive(env) {
		return errors.New("sync asks which value every shared account should get and needs a terminal; run it in one, or use `julienning sync --from NICKNAME` or `julienning sync --check`")
	}
	cache := syncCache(env, cfg)
	targets, err := syncTargets(env, cfg, cache)
	if err != nil {
		return err
	}
	labels := make([]string, len(targets))
	for i, t := range targets {
		labels[i] = t.label
	}
	fmt.Fprintf(env.Stdout, "Shared accounts: %s\n", strings.Join(labels, ", "))

	switch {
	case *check:
		return syncCheck(env, targets)
	case *from != "":
		src, err := syncSource(env, cfg, targets, *from)
		if err != nil {
			return err
		}
		return syncFrom(env, targets, src)
	}
	return syncAsk(env, targets)
}

// syncCache refreshes the allowlist when the Worker answers (a teammate may
// have shared an account since), else uses the cached one.
func syncCache(env cli.Env, cfg *config.Config) *sharedcache.Cache {
	if cfg.RequireRemote() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
		defer cancel()
		c, _, err := sharedcache.Refresh(ctx, newClient(cfg), cfg.Dev, now())
		if err == nil {
			return c
		}
		warnf(env, "could not refresh the shared-account list (%v); using the cached one", remoteErr("list accounts", err))
	}
	return loadCache(env)
}

// syncTargets reads every registered dir logged into a shared account, by
// label. Personal accounts, dirs not logged in and unreadable logins are
// left out (the last with a warning); sync never touches them.
func syncTargets(env cli.Env, cfg *config.Config, cache *sharedcache.Cache) ([]*syncTarget, error) {
	var targets []*syncTarget
	for _, cd := range cfg.Configs {
		email, ok, err := dirEmail(cd.Dir)
		if err != nil {
			warnf(env, "left out %s: its login could not be read (%v)", shortenHome(cd.Dir), err)
			continue
		}
		if !ok || !cache.Contains(email) {
			continue
		}
		t := &syncTarget{cd: cd, label: resolve.Label(cache, cd, email)}
		if raw, err := claudecfg.SettingsValues(cd.Dir, teamsync.Keys()); err != nil {
			t.settingsErr = err
		} else {
			t.values = map[string]string{}
			for k, v := range raw {
				t.values[k] = teamsync.Canonical(v)
			}
		}
		t.md, t.mdErr = claudecfg.ReadClaudeMD(cd.Dir)
		t.mdKey = t.md.Path
		if r, err := filepath.EvalSymlinks(t.md.Path); err == nil {
			t.mdKey = r
		}
		targets = append(targets, t)
	}
	// Two dirs logged into one account share its nickname; their paths
	// tell them apart.
	count := map[string]int{}
	for _, t := range targets {
		count[t.label]++
	}
	for _, t := range targets {
		if count[t.label] > 1 {
			t.label += " (" + shortenHome(t.cd.Dir) + ")"
		}
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].label < targets[j].label })
	if len(targets) < 2 {
		what := "none is"
		if len(targets) == 1 {
			what = "only " + targets[0].label + " is"
		}
		return nil, fmt.Errorf("sync needs at least two shared accounts logged in here, and %s (`julienning configs` lists the registered dirs; sign one in with `julienning login <config>` or `julienning new-config`)", what)
	}
	for _, t := range targets {
		if t.settingsErr != nil {
			warnf(env, "left %s out of the settings: %v", t.label, t.settingsErr)
		}
		if t.mdErr != nil {
			warnf(env, "left %s out of CLAUDE.md: %v", t.label, t.mdErr)
		}
	}
	return targets, nil
}

// settingsTargets are the targets whose settings.json could be read.
func settingsTargets(targets []*syncTarget) ([]*syncTarget, []teamsync.Account) {
	var ts []*syncTarget
	var accs []teamsync.Account
	for _, t := range targets {
		if t.settingsErr == nil {
			ts = append(ts, t)
			accs = append(accs, t.account())
		}
	}
	return ts, accs
}

// mdTargets are the targets whose CLAUDE.md could be read, one per file.
func mdTargets(targets []*syncTarget) []*syncTarget {
	var out []*syncTarget
	seen := map[string]bool{}
	for _, t := range targets {
		if t.mdErr == nil && !seen[t.mdKey] {
			seen[t.mdKey] = true
			out = append(out, t)
		}
	}
	return out
}

// syncSource resolves --from to one of the targets.
func syncSource(env cli.Env, cfg *config.Config, targets []*syncTarget, arg string) (*syncTarget, error) {
	m, err := resolveTarget(env, cfg, arg)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if filepath.Clean(t.cd.Dir) == filepath.Clean(m.Dir.Dir) {
			return t, nil
		}
	}
	labels := make([]string, len(targets))
	for i, t := range targets {
		labels[i] = t.label
	}
	return nil, fmt.Errorf("%s is not a shared account logged in here; --from takes one of: %s", arg, strings.Join(labels, ", "))
}

// decision is the value chosen for one setting; sync false means leave it.
type decision struct {
	setting teamsync.Setting
	value   string
	sync    bool
}

// mdPlan is what happens to CLAUDE.md: copy src's content into the targets
// in write, keep the ones in keep. src nil means CLAUDE.md is not synced;
// note then says why.
type mdPlan struct {
	src   *syncTarget
	write []*syncTarget
	keep  []*syncTarget
	same  []*syncTarget
	note  string
}

func syncAsk(env cli.Env, targets []*syncTarget) error {
	sts, accs := settingsTargets(targets)
	var decisions []decision
	if len(accs) > 0 {
		names := make([]string, len(accs))
		for i, a := range accs {
			names[i] = a.Label
		}
		scope := fmt.Sprintf("Every shared account gets the value you pick (%s) · Esc cancels the sync, nothing is written", strings.Join(names, ", "))
		for i, s := range teamsync.Settings {
			choices := teamsync.Choices(s, accs)
			items := make([]tui.Item, 0, len(choices)+1)
			for _, c := range choices {
				it := tui.Item{Title: teamsync.Display(c.Value), Meta: strings.Join(c.Accounts, ", ")}
				if it.Meta == "" {
					it.Meta = "no account yet"
				}
				if c.Default {
					it.Note = "(default)"
				}
				items = append(items, it)
			}
			items = append(items, tui.Item{Title: "Don't sync", Meta: "leave every account as it is"})
			header := fmt.Sprintf("%s (%d of %d): %s", s.Key, i+1, len(teamsync.Settings), s.Summary)
			idx, err := pickChoice(env, items, header, scope)
			if errors.Is(err, tui.ErrCanceled) {
				fmt.Fprintln(env.Stdout, "Sync canceled; nothing was changed.")
				return nil
			}
			if err != nil {
				return err
			}
			d := decision{setting: s}
			if idx >= 0 && idx < len(choices) {
				d.value, d.sync = choices[idx].Value, true
			}
			decisions = append(decisions, d)
		}
	}

	plan, err := askClaudeMD(env, mdTargets(targets))
	if errors.Is(err, tui.ErrCanceled) {
		fmt.Fprintln(env.Stdout, "Sync canceled; nothing was changed.")
		return nil
	}
	if err != nil {
		return err
	}
	return applySync(env, sts, decisions, plan)
}

// askClaudeMD asks which CLAUDE.md every account should get (newest first)
// and, for each account holding a different one, whether to overwrite it.
func askClaudeMD(env cli.Env, ts []*syncTarget) (mdPlan, error) {
	var with []*syncTarget
	for _, t := range ts {
		if t.md.Exists {
			with = append(with, t)
		}
	}
	switch {
	case len(ts) == 0:
		return mdPlan{note: "could not be read in any shared account"}, nil
	case len(with) == 0:
		return mdPlan{note: "no shared account has one"}, nil
	case len(with) == len(ts) && allSameMD(with):
		return mdPlan{note: "already the same in every shared account"}, nil
	}
	sort.SliceStable(with, func(i, j int) bool { return with[i].md.ModTime.After(with[j].md.ModTime) })
	items := make([]tui.Item, 0, len(with)+1)
	for _, t := range with {
		var same []string
		for _, o := range with {
			if o != t && bytes.Equal(o.md.Content, t.md.Content) {
				same = append(same, o.label)
			}
		}
		it := tui.Item{
			Title:  t.label + " CLAUDE.md",
			Meta:   "updated " + mdTime(t.md) + " · " + lineCount(t.md.Content),
			Detail: firstLine(t.md.Content),
		}
		if len(same) > 0 {
			it.Note = "(same as " + strings.Join(same, ", ") + ")"
		}
		items = append(items, it)
	}
	items = append(items, tui.Item{Title: "Don't sync CLAUDE.md", Meta: "leave every account's as it is"})
	idx, err := pickChoice(env, items,
		"CLAUDE.md: which one should every shared account get?",
		"Newest first · accounts without one get it, and you are asked before one is overwritten · Esc cancels the sync")
	if err != nil {
		return mdPlan{}, err
	}
	if idx < 0 || idx >= len(with) {
		return mdPlan{note: "not synced"}, nil
	}
	plan := mdPlan{src: with[idx]}
	for _, t := range ts {
		switch {
		case t == plan.src:
		case !t.md.Exists:
			plan.write = append(plan.write, t)
		case bytes.Equal(t.md.Content, plan.src.md.Content):
			plan.same = append(plan.same, t)
		default:
			yes, err := confirmOverwrite(env,
				fmt.Sprintf("Overwrite %s's CLAUDE.md with %s's?", t.label, plan.src.label),
				fmt.Sprintf("%s's was updated %s (%s); %s's %s (%s).", t.label, mdTime(t.md), lineCount(t.md.Content), plan.src.label, mdTime(plan.src.md), lineCount(plan.src.md.Content)),
				t.label)
			if err != nil {
				return mdPlan{}, err
			}
			if yes {
				plan.write = append(plan.write, t)
			} else {
				plan.keep = append(plan.keep, t)
			}
		}
	}
	return plan, nil
}

// syncFrom gives every target src's values and CLAUDE.md, without asking.
func syncFrom(env cli.Env, targets []*syncTarget, src *syncTarget) error {
	if src.settingsErr != nil {
		return fmt.Errorf("cannot read %s's settings: %w", src.label, src.settingsErr)
	}
	if src.mdErr != nil {
		return fmt.Errorf("cannot read %s's CLAUDE.md: %w", src.label, src.mdErr)
	}
	sts, _ := settingsTargets(targets)
	var decisions []decision
	for _, s := range teamsync.Settings {
		decisions = append(decisions, decision{setting: s, value: src.values[s.Key], sync: true})
	}
	plan := mdPlan{note: src.label + " has none; every account keeps its own"}
	if src.md.Exists {
		plan = mdPlan{src: src}
		for _, t := range mdTargets(targets) {
			switch {
			case t.mdKey == src.mdKey:
			case t.md.Exists && bytes.Equal(t.md.Content, src.md.Content):
				plan.same = append(plan.same, t)
			default:
				plan.write = append(plan.write, t)
			}
		}
	}
	return applySync(env, sts, decisions, plan)
}

// applySync writes the decisions and the CLAUDE.md plan, then reports one
// line per setting and one for CLAUDE.md. A file that cannot be written is
// warned about and the rest still go; the command then fails.
func applySync(env cli.Env, ts []*syncTarget, decisions []decision, plan mdPlan) error {
	failed := 0
	ok := map[*syncTarget]bool{}
	for _, t := range ts {
		var edits []claudecfg.SettingEdit
		for _, d := range decisions {
			if !d.sync {
				continue
			}
			if e := teamsync.Edit(d.setting.Key, d.value, t.account()); e != nil {
				edits = append(edits, *e)
			}
		}
		if len(edits) == 0 {
			ok[t] = true
			continue
		}
		if _, err := claudecfg.EditSettings(t.cd.Dir, edits); err != nil {
			warnf(env, "could not update %s's settings: %v", t.label, err)
			failed++
			continue
		}
		ok[t] = true
	}
	for _, d := range decisions {
		if !d.sync {
			syncLine(env, d.setting.Key, "not synced")
			continue
		}
		var changed []string
		for _, t := range ts {
			if ok[t] && teamsync.Edit(d.setting.Key, d.value, t.account()) != nil {
				changed = append(changed, t.label)
			}
		}
		v := teamsync.Display(d.value)
		switch {
		case len(changed) == 0:
			syncLine(env, d.setting.Key, v+": already in every shared account")
		case d.value == teamsync.Unset:
			syncLine(env, d.setting.Key, v+": removed from "+strings.Join(changed, ", "))
		default:
			syncLine(env, d.setting.Key, v+": set in "+strings.Join(changed, ", "))
		}
	}
	if plan.src == nil {
		syncLine(env, "CLAUDE.md", plan.note)
	} else {
		if imp := relativeImports(plan.src.md.Content); len(imp) > 0 {
			warnf(env, "%s's CLAUDE.md imports %s relative to its own dir; those files are not copied", plan.src.label, strings.Join(imp, ", "))
		}
		var copied, kept []string
		for _, t := range plan.write {
			err := claudecfg.WriteClaudeMD(t.cd.Dir, plan.src.md.Content, t.md)
			switch {
			case errors.Is(err, claudecfg.ErrClaudeMDChanged):
				warnf(env, "left %s's CLAUDE.md as it is: it changed while sync was running", t.label)
				kept = append(kept, t.label)
			case err != nil:
				warnf(env, "could not write %s's CLAUDE.md: %v", t.label, err)
				failed++
			default:
				copied = append(copied, t.label)
			}
		}
		for _, t := range plan.keep {
			kept = append(kept, t.label)
		}
		parts := []string{}
		if len(copied) > 0 {
			parts = append(parts, "copied to "+strings.Join(copied, ", "))
		}
		if len(kept) > 0 {
			parts = append(parts, "kept "+strings.Join(kept, "'s, ")+"'s")
		}
		if len(parts) == 0 {
			parts = append(parts, "already in every shared account")
		}
		syncLine(env, "CLAUDE.md", plan.src.label+"'s: "+strings.Join(parts, "; "))
	}
	if failed > 0 {
		return fmt.Errorf("could not update %d file(s) (see above); fix them and run `julienning sync` again", failed)
	}
	return nil
}

// syncCheck lists each setting and CLAUDE.md as same or differing, and
// fails when anything differs.
func syncCheck(env cli.Env, targets []*syncTarget) error {
	_, accs := settingsTargets(targets)
	differ := 0
	for _, s := range teamsync.Settings {
		if v, same := teamsync.Same(s.Key, accs); same {
			syncLine(env, s.Key, "same: "+teamsync.Display(v))
			continue
		}
		differ++
		var parts []string
		for _, c := range teamsync.Choices(s, accs) {
			if len(c.Accounts) > 0 {
				parts = append(parts, teamsync.Display(c.Value)+" ("+strings.Join(c.Accounts, ", ")+")")
			}
		}
		syncLine(env, s.Key, "differs: "+strings.Join(parts, " · "))
	}
	ts := mdTargets(targets)
	var with, without []string
	groups := []*syncTarget{}
	members := map[*syncTarget][]string{}
	for _, t := range ts {
		if !t.md.Exists {
			without = append(without, t.label)
			continue
		}
		with = append(with, t.label)
		found := false
		for _, g := range groups {
			if bytes.Equal(g.md.Content, t.md.Content) {
				members[g] = append(members[g], t.label)
				found = true
				break
			}
		}
		if !found {
			groups = append(groups, t)
			members[t] = []string{t.label}
		}
	}
	switch {
	case len(with) == 0:
		syncLine(env, "CLAUDE.md", "same: none in any shared account")
	case len(groups) == 1 && len(without) == 0:
		syncLine(env, "CLAUDE.md", "same in every shared account")
	default:
		differ++
		var parts []string
		for _, g := range groups {
			parts = append(parts, strings.Join(members[g], ", ")+" (updated "+mdTime(g.md)+")")
		}
		if len(without) > 0 {
			parts = append(parts, "none: "+strings.Join(without, ", "))
		}
		syncLine(env, "CLAUDE.md", "differs: "+strings.Join(parts, " · "))
	}
	if differ > 0 {
		return fmt.Errorf("%d of %d differ between the shared accounts; `julienning sync` asks which to keep, `julienning sync --from NICKNAME` copies one account's", differ, len(teamsync.Settings)+1)
	}
	fmt.Fprintln(env.Stdout, "Every shared account here has the same settings and CLAUDE.md.")
	return nil
}

func syncLine(env cli.Env, key, text string) {
	fmt.Fprintf(env.Stdout, "%-*s %s\n", syncLabelWidth, key, text)
}

func allSameMD(ts []*syncTarget) bool {
	for _, t := range ts[1:] {
		if !bytes.Equal(t.md.Content, ts[0].md.Content) {
			return false
		}
	}
	return true
}

// mdTime is a CLAUDE.md's modification time in local time.
func mdTime(md claudecfg.ClaudeMD) string { return md.ModTime.Local().Format("2006-01-02 15:04") }

func lineCount(b []byte) string {
	n := bytes.Count(b, []byte("\n"))
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return "(empty)"
}

// importRe finds Claude Code's @path imports outside code spans.
var importRe = regexp.MustCompile("(?:^|\\s)@([^\\s`]+)")

// relativeImports lists the @imports of a CLAUDE.md that resolve relative
// to its own directory (not ~/… or /…), which a copy into another config
// dir would break.
func relativeImports(content []byte) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range importRe.FindAllStringSubmatch(line, -1) {
			p := m[1]
			if strings.HasPrefix(p, "~") || strings.HasPrefix(p, "/") || !strings.ContainsAny(p, "/.") || strings.Contains(p, "@") {
				continue
			}
			out = append(out, "@"+p)
		}
	}
	return out
}
