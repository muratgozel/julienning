package switching

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/tui"
	"github.com/muratgozel/julienning/internal/usage"
)

// finish prints the result (non-launch) or runs the session hand-off
// (launch mode). `current` is already set when it runs. cache (may be nil)
// supplies the nicknames that label the other dirs in the picker.
func finish(env cli.Env, cfg *config.Config, cache *sharedcache.Cache, sel selection, o options, now time.Time) error {
	loc := location()
	if o.json {
		doc, err := jsonDoc(sel)
		if err != nil {
			return err
		}
		if err := printJSON(env, doc); err != nil {
			return err
		}
		selfupdate.Hint(env.Stderr)
		return nil
	}
	line := summaryLine(sel, now, loc)
	fmt.Fprintln(env.Stdout, line)
	if !launchMode(env, o) {
		selfupdate.Hint(env.Stderr)
		return nil
	}
	return handOff(env, cfg, cache, sel, o, now, loc, line)
}

// handOff lists sessions, lets the user pick one, moves it into the target
// when it lives elsewhere, and replaces this process with claude.
func handOff(env cli.Env, cfg *config.Config, cache *sharedcache.Cache, sel selection, o options, now time.Time, loc *time.Location, header string) error {
	target := sel.cd
	if sel.email == "" || !sel.shared {
		// Never move team sessions into a dir that is not a shared account;
		// the warning was printed when the target was resolved.
		return execClaude(target.Dir, nil, "")
	}

	cwd, err := getwd()
	if err != nil {
		warnf(env, "cannot determine the current directory (%v); listing every project", err)
		o.all = true
	}
	list, err := sessions.List(sessions.ListOptions{Dirs: cfg.Configs, Cwd: cwd, All: o.all, Limit: o.limit, Now: now})
	if err != nil {
		warnf(env, "some sessions could not be read: %v", err)
	}
	if len(list) == 0 {
		return execClaude(target.Dir, nil, "")
	}

	labels := dirLabels(cfg, cache)
	labels[target.Name] = sel.label()
	idx, err := pickSession(env, rows(list, target, labels, o.all, now, loc), header, scopeLine(len(list), o.limit, o.all, cwd))
	if errors.Is(err, tui.ErrCanceled) {
		fmt.Fprintf(env.Stdout, "Not starting claude; %s stays selected.\n", sel.label())
		selfupdate.Hint(env.Stderr)
		return nil
	}
	if err != nil {
		return err
	}
	if idx <= 0 || idx > len(list) {
		return execClaude(target.Dir, nil, "")
	}
	s := list[idx-1]
	if s.Live {
		// The picker does not offer live rows; guard anyway.
		return fmt.Errorf("session %q is open in another terminal (%s); exit it there first", s.Label(), labelOf(labels, s.LiveIn))
	}
	if filepath.Clean(s.ConfigDir) != filepath.Clean(target.Dir) {
		rep, err := sessions.Move(s, target)
		if err != nil {
			return err
		}
		rep.From, rep.To = moveLabels(labels, s, target)
		fmt.Fprintln(env.Stdout, rep.String())
		for _, c := range rep.Memory.Conflicts {
			warnf(env, "project memory file %s differs between %s and %s; kept %s's version", c, rep.From, rep.To, rep.To)
		}
		for _, w := range rep.Warnings {
			warnf(env, "%s", w)
		}
	}
	if s.Cwd != "" {
		if fi, err := os.Stat(s.Cwd); err != nil || !fi.IsDir() {
			warnf(env, "the session's directory %s no longer exists; claude may not find the session from here", s.Cwd)
		}
	}
	return execClaude(target.Dir, []string{"--resume", s.ID}, s.Cwd)
}

// labelOf is labels[name], falling back to the config name.
func labelOf(labels map[string]string, name string) string {
	if l := labels[name]; l != "" {
		return l
	}
	return name
}

// moveLabels names the two ends of a move in the report. Two dirs logged
// into the same account share a nickname ("from alpha to alpha" says
// nothing), so those are shown by their shortened paths instead.
func moveLabels(labels map[string]string, s sessions.Session, target config.ConfigDir) (from, to string) {
	from, to = labelOf(labels, s.ConfigName), labelOf(labels, target.Name)
	if from == to {
		return shortenHome(s.ConfigDir), shortenHome(target.Dir)
	}
	return from, to
}

// rows builds the picker: "New session" first (pinned, default), then one
// row per session: title (or first prompt), account label (labels: config
// name → nickname, see dirLabels), relative last activity, short id, and the
// first prompt as the second line (see promptPreview).
func rows(list []sessions.Session, target config.ConfigDir, labels map[string]string, all bool, now time.Time, loc *time.Location) []tui.Item {
	items := []tui.Item{{Title: "New session", Meta: "in " + labelOf(labels, target.Name), Pinned: true}}
	for _, s := range list {
		prompt := promptPreview(s.FirstPrompt)
		title, detail := s.Title, prompt
		if title == "" {
			title, detail = prompt, ""
		}
		meta := []string{labelOf(labels, s.ConfigName)}
		if all && s.Cwd != "" {
			meta = append(meta, shortenHome(s.Cwd))
		}
		meta = append(meta, relTime(s.LastActive, now, loc), s.ShortID())
		it := tui.Item{
			Title:  title,
			Meta:   strings.Join(meta, " · "),
			Detail: detail,
			Search: s.ID + " " + s.Cwd,
		}
		switch {
		case s.Live:
			it.Disabled = true
			it.Note = "(open in another terminal)"
		case s.MaybeOpen:
			it.Note = "(may be open)"
			it.Muted = true
		}
		items = append(items, it)
	}
	return items
}

// scopeLine says what the picker lists, for its second header line:
//
//	100 most recent sessions in ~/Code/shop   (the limit was reached)
//	37 sessions in ~/Code/shop                (everything found)
//	1 session                                 (--all: every project)
//
// The picker appends " · type to filter".
func scopeLine(n, limit int, all bool, cwd string) string {
	noun := "sessions"
	if n == 1 {
		noun = "session"
	}
	s := fmt.Sprintf("%d %s", n, noun)
	if n >= limit {
		s = fmt.Sprintf("%d most recent %s", n, noun)
	}
	if !all && cwd != "" {
		s += " in " + shortenHome(cwd)
	}
	return s
}

// promptPreview makes a first prompt readable on one line: whitespace
// collapses, leading ">" quote markers go, and markdown code fences (a run
// of 3+ backticks or tildes with its info string, e.g. "```go") are dropped
// wherever they are. sessions has already folded newlines into spaces, so a
// quote marker is only recognisable at the very start ("a > b" mid-prompt is
// prose). When nothing but markup is left, the collapsed prompt is kept.
func promptPreview(p string) string {
	words := strings.Fields(p)
	var out []string
	lead := true
	for _, w := range words {
		if isFence(w) {
			continue
		}
		if lead {
			if w = strings.TrimLeft(w, ">"); w == "" {
				continue
			}
			lead = false
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return strings.Join(words, " ")
	}
	return strings.Join(out, " ")
}

// isFence reports whether w opens or closes a fenced code block: 3+ backticks
// or tildes, optionally followed by an info string without that character
// (so inline "```x```" is kept).
func isFence(w string) bool {
	for _, c := range []string{"`", "~"} {
		rest := strings.TrimLeft(w, c)
		if len(w)-len(rest) >= 3 && !strings.Contains(rest, c) {
			return true
		}
	}
	return false
}

// relTime is "2h ago" up to six days, then the local date and time.
func relTime(t, now time.Time, loc *time.Location) string {
	d := now.Sub(t)
	if d > 6*24*time.Hour {
		return t.In(loc).Format("2006-01-02 15:04")
	}
	return usage.Ago(d)
}

func shortenHome(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if p == h {
		return "~"
	}
	if strings.HasPrefix(p, h+string(filepath.Separator)) {
		return "~" + p[len(h):]
	}
	return p
}
