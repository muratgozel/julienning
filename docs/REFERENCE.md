# julienning reference

Every julienning command, flag, output, file and error in one page, with output from real runs; for the quick start, see the [README](../README.md).

## Contents

- [Overview](#overview)
- [Policy note](#policy-note)
- [Install](#install)
- [Setup](#setup)
  - [Nicknames](#nicknames)
  - [Discovery](#discovery)
  - [Config names](#config-names)
  - [The allowlist](#the-allowlist)
  - [What it changes](#what-it-changes)
  - [Default dir](#default-dir)
- [Daily use](#daily-use)
  - [Session picker](#session-picker)
  - [Other commands](#other-commands)
- [How claims work](#how-claims-work)
- [How usage reaches KV](#how-usage-reaches-kv)
- [Undo](#undo)
- [Web view](#web-view)
- [Deploying the Worker](#deploying-the-worker)
- [Development](#development)
- [Troubleshooting](#troubleshooting)
  - [Files in `~/.julienning`](#files-in-julienning)

## Overview

julienning lets a small team share a pool of Claude.ai subscriptions from the
shell: each shared account lives in its own Claude Code config dir and has a
team-wide nickname (`alpha`), `julienning next` switches you to the best free
one and can carry your current conversation over to it, and `claude-alpha`
starts Claude on a given one. Every machine reports each shared account's
rate-limit usage and "in use" claims to a small Cloudflare Worker (KV), so the
whole team sees the same ranked list. macOS and Linux, zsh and bash; Claude
Code must already be installed.

Nothing team-specific is compiled in. The Worker URL and the team token are
entered at `julienning setup` and shared out of band.

## Policy note

- **No auth tokens.** julienning never reads, copies or sends Claude
  credentials, and never opens the `<dir>/sessions/*.key` files. Signing in is
  Claude Code's own flow; `julienning login TARGET` only starts `claude` with
  the right `CLAUDE_CONFIG_DIR`.
- **What it touches:** it sets `CLAUDE_CONFIG_DIR` (and unsets it for
  `~/.claude`, see [Default dir](#default-dir)). It reads the account email
  (`oauthAccount.emailAddress` in `~/.claude.json` for `~/.claude`,
  `<dir>/.claude.json` otherwise) and the JSON Claude Code pipes to status line
  scripts (model, context %, `rate_limits.five_hour` / `seven_day` used % and
  reset time). For the session picker it reads Claude Code's live-session
  registry (`<dir>/sessions/<pid>.json`) and the first and last 256 KB of recent
  transcripts; a session you pick from another dir is moved between your
  config dirs, and the `.git` entry of its working directory (or the nearest
  parent that has one) is read to find the repository whose project memory
  goes with it. All of this stays local.
- **What leaves the machine**, and only for accounts on the team allowlist: the
  email and its team nickname, session and week used % with their reset times,
  when they were read, your dev name and a random machine id. An email reaches
  the Worker only when you share it (the `setup` prompt, `setup --share`, or
  `julienning share`).
- **Personal accounts never report.** A dir logged into an email that is not on
  the allowlist sends no usage and no claims, and `julienning current` does not
  ask the Worker about it.
- `~/.julienning/errors.log` never contains email addresses, and no file name
  under `~/.julienning` does either (per-account files are named by a short
  hash of the email).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/muratgozel/julienning/main/scripts/install.sh | bash
```

It needs only bash, curl, tar and `sha256sum` or `shasum`. It looks up the latest
release through GitHub's `/releases/latest` redirect (no API token), downloads
`julienning_<version>_<os>_<arch>.tar.gz` and `checksums.txt`, refuses to
install if the checksum entry is missing or does not match, and checks that the
new binary runs `version` before switching to it. It creates:

| Path | What |
|------|------|
| `${XDG_DATA_HOME:-~/.local/share}/julienning/versions/<version>` | one binary per version (`<version>` without the `v`) |
| `~/.local/bin/julienning` | symlink to the active version, swapped atomically |

It keeps the 3 most recent versions (by mtime) plus the active one and deletes
older ones. Re-running it upgrades.

```
Downloading julienning 0.3.0 (darwin/arm64)...
Updated 0.2.1 → 0.3.0.
Installed julienning 0.3.0: /Users/you/.local/bin/julienning -> /Users/you/.local/share/julienning/versions/0.3.0

Next: julienning setup
```

If `~/.local/bin` is not on your `PATH`, it prints the line to add for zsh and
bash.

**Pin a version** (an argument wins over the env var; the leading `v` is
optional). Pre-release tags (`vX.Y.Z-rc.N`) are never "latest"; pin them this
way:

```sh
curl -fsSL https://raw.githubusercontent.com/muratgozel/julienning/main/scripts/install.sh | bash -s -- v0.3.0
curl -fsSL https://raw.githubusercontent.com/muratgozel/julienning/main/scripts/install.sh | JULIENNING_VERSION=v0.3.0 bash
```

**Update** from the installed binary:

```sh
julienning update                    # latest release
julienning update --version v0.2.9   # a given release; downgrades allowed
```

It prints `Updated 0.2.1 → 0.3.0.`, or `julienning 0.3.0 is up to date.` (or
`julienning 0.3.1 is up to date (newer than the latest release 0.3.0).`). Local
builds (`dev`, git-describe versions) always move to the latest release.
Like the installer it checks the checksum and runs the new binary's `version`
before switching; if either fails, the active version stays. A slow download is
not cut off, only one that receives no data for 30 s (or runs past 10
minutes). It switches the `julienning` symlink that runs it, looked up in
`$JULIENNING_BIN_DIR`, `~/.local/bin`, then `PATH`, so an install made with
`JULIENNING_BIN_DIR` updates without it. `update` only manages the layout
above; on any other install it refuses and prints the curl one-liner.
`--version` with a release older than the latest prints a reminder when
auto-update is on, since the next daily check would move back:

```
julienning: note: auto-update will move to 0.3.0 again within a day; to stay on 0.2.9, set "auto_update": false in /Users/you/.julienning/config.json (or export JULIENNING_AUTO_UPDATE=0)
```

**Auto-update.** Once a day, the background processes that report usage and
sync claims (never an interactive command) look up the latest release and,
when it is newer than the active version, install it the same way `update`
does: download, checksum, run its `version`, switch the symlink, prune. A
lock in `~/.julienning/update.lock` keeps concurrent processes from
installing twice. Commands that are already running keep their binary; the
next command runs the new one and prints once, on stderr:

```
julienning updated to 0.3.0
```

A failure (no network, a broken release, a full disk) goes to
`~/.julienning/errors.log` as `UPDATE_CHECK_FAILED` and leaves the active
version as it was; a failed lookup is retried on the next background run, a
failed install at the next daily check. Auto-update never touches local
builds (`make install`, git-describe versions, or a `dev` version behind the
symlink) or installs not made by the installer. To turn it off, set
`"auto_update": false` in `~/.julienning/config.json`, or export
`JULIENNING_AUTO_UPDATE=0` (or `false`) where Claude Code runs; the variable
can only turn it off, and a value that is not a boolean keeps it off and is
logged.

**Update hint.** When a newer release is known that auto-update did not
install (it is off, the install is not managed, or the last attempt failed),
`accounts`, `current`, `configs`, `setup`, and `next` / `use` when they do
not start claude, print one line on stderr:

```
julienning 0.3.0 is available (you have 0.2.1): julienning update
```

Both lines come from `~/.julienning/update-check.json`, which the background
check refreshes at most once per 24 h. The status line and hooks never
print them, and dev builds never see them.

**Local builds:** `make install` builds version `dev` into
`<versions dir>/dev` and points the same symlink at it (`julienning update`
later moves it to a release; auto-update leaves it alone).

**Install overrides:**

| Variable | Read by | Default |
|----------|---------|---------|
| `JULIENNING_VERSION` | `install.sh` | latest release |
| `JULIENNING_BIN_DIR` | `install.sh`, `make install`, `update`, `uninstall --purge`, the settings.json command path | `~/.local/bin` |
| `JULIENNING_VERSIONS_DIR` | same | `${XDG_DATA_HOME:-~/.local/share}/julienning/versions` |
| `JULIENNING_REPO` | `install.sh`, `update`, auto-update | `muratgozel/julienning` |
| `JULIENNING_RELEASES_BASE` | `install.sh`, `update`, auto-update | `https://github.com/<repo>` (tests, mirrors) |
| `JULIENNING_AUTO_UPDATE` | auto-update | on; `0` or `false` turns it off |

**Coming from an older julienning:** run the installer (it replaces the old
binary with the symlink), then `julienning setup`, which also names the shared
accounts you are logged into that have no nickname yet (see
[Nicknames](#nicknames)). `release` is gone; use `julienning use --clear`. The
`claude-<config name>` aliases are gone too; use `claude-<nickname>`.

## Setup

```sh
julienning setup
```

```
julienning setup [--dev NAME] [--remote-url URL] [--token TOKEN] [--name-prefix NAME] [--shell zsh|bash] [--no-rc] [--yes] [--share EMAIL]... [--nick EMAIL=NAME]...
```

On the first run in a terminal it asks for (`--dev`, `--remote-url` and
`--token` answer up front):

- **dev name**, default `$USER` (lowercase letters, digits, `.`, `_`, `-`, at
  most 32). It goes with every report and claim. A 12-hex machine id is
  generated once.
- **Worker URL** and **team token** (typed without echo), from whoever runs the
  Worker. Both are checked with `GET /healthz` and an authenticated
  `GET /accounts` before anything is saved; a rejected token is asked for once
  more.

A first run on a machine with a personal `~/.claude`, one team account (a
teammate already shared it as `alpha`) and one account nobody has shared yet:

```
Your dev name [murat]:
Identity: dev=murat machine=d523d292db2e (created)
Worker URL (ask your team lead; empty to skip): https://julienning.example.workers.dev
Team token (input hidden):
Remote:   https://julienning.example.workers.dev (updated, verified)
Shared:   3 account(s) on the team allowlist
Share me@personal.com (found in ~/.claude) with the team? [y/N] n
Share claude4@example.com (found in ~/.claude-work) with the team? [y/N] y
Nickname for claude4@example.com [claude4]: delta
Dirs:     4 found
          ~/.claude              -      me@personal.com      personal (declined)
          ~/.claude-julienning1  alpha  claude1@example.com  shared (registered as julienning1)
          ~/.claude-old          -      -                    not logged in
          ~/.claude-work         delta  claude4@example.com  shared (registered as julienning2)
Settings: julienning1 added, julienning2 added
          note: julienning1: replaced statusLine "~/bin/my-status.sh" (saved; `julienning uninstall` or `forget` restores it)
Shell:    hook added to ~/.zshrc

Next steps:
  open a new terminal, or run: source ~/.zshrc
  julienning accounts   # who is free right now
  julienning next       # switch to the best account
```

The columns are the dir, the account's team nickname (`-` when it has none or
is not shared), the email and what setup decided.

### Nicknames

You know a shared account by its **nickname**: you type `julienning use alpha`
or `claude-alpha`, and `accounts`, `current`, `next` and the session picker
show `alpha`, never a dir name. A nickname is team-wide: the Worker stores it
with the allowlist entry, and whoever shares the account picks it once for
everyone. Setup asks `Nickname for claude4@example.com [claude4]:`; Enter takes
the email's local part, lowercased, with any other character turned into `-`
(`john+team@x.io` → `john-team`). `new-config` asks
`Nickname for this account [the email's name]:` before you sign in, so Enter
there means the same default, taken once the email is known.

- 1–32 characters of `a-z`, `0-9`, `.`, `_`, `-`, starting with a letter or
  digit, and unique in the team. A taken one is refused with the account that
  holds it (`nickname "alpha" is already used by claude1@example.com`), and
  setup asks again.
- `julienning share EMAIL --nick NAME` picks it from the command line, and
  `setup --nick EMAIL=NAME` (repeatable) without a terminal; otherwise the
  default is used.
- `julienning nick TARGET NEW` renames it for the whole team. Teammates see the
  new name once their cached allowlist refreshes (hourly in the background, or
  right away with `julienning accounts`), and the new `claude-<nickname>`
  function in their next terminal.
- Accounts shared before nicknames existed have none (`-`). Setup names the
  ones logged in on this machine: it asks in a terminal, otherwise it takes
  `--nick` or the default, and prints
  `Nickname: claude5@example.com is now claude5 (rename with: julienning nick claude5 NEW)`.
  Until then such an account is shown by its config name.

Wherever a command takes an account (`use`, `login`, `nick`, `unshare`) you can
give a nickname, an email or a config name, and a nickname wins over a config
name spelled the same. A nickname or email is resolved when the command runs,
to the registered dir that is logged into that account right now (the selected
one first), so moving a login to another dir needs no re-run of setup. When no
dir here is logged into it, `use`, `login` and `claude-<nickname>` say how to
sign in:

```
julienning: beta (claude2@example.com) is shared but not logged in on this machine; sign in with `julienning login <config>` or `julienning new-config`
```

`nick` and `unshare` act on the team account, so it need not be logged in
here; a config name stands for its dir's current login.

### Discovery

Setup looks for Claude config dirs in:

1. `~/.claude`, when it exists;
2. top-level entries of `$HOME` (hidden or not, symlinks followed) that contain
   `.claude.json`, or `projects/` together with `settings.json`,
   `history.jsonl` or `sessions/`. On macOS, `Desktop`, `Documents`,
   `Downloads`, `Library`, `Movies`, `Music`, `Pictures` and `Applications` are
   skipped;
3. paths assigned to `CLAUDE_CONFIG_DIR=` in `~/.zshrc`, `~/.zprofile`,
   `~/.zshenv`, `~/.bashrc`, `~/.bash_profile`, `~/.profile`. Lines are split
   into shell words: only a real assignment counts (at the start of a
   command, after `export`, `env` and the like, or inside an `alias` body),
   never quoted text such as `echo "CLAUDE_CONFIG_DIR=/tmp"` or a comment.
   Values may use quotes, `~`, `$HOME` or `${HOME}`; any other expansion, a
   relative path, `/`, `$HOME` itself or a parent of it is skipped;
4. the current `$CLAUDE_CONFIG_DIR` (skipped when it is `/`, `$HOME` or a
   parent of `$HOME`; `adopt` refuses those too);
5. dirs already registered (a deleted one shows as
   `` missing (registered as NAME; run `julienning forget NAME`) ``).

### Config names

Each registered dir also has a config name, an internal label: you see it in
`configs`, in the setup listing (`registered as julienning1`) and in the
commands that manage dirs (`adopt`, `forget`, `rename`, `new-config`, and
`login` / `use` for a fresh dir nobody is signed into yet). It never becomes a
shell name. Names never reuse the directory name, which often names the team
or the person; julienning picks `<prefix><N>`:

- `~/.claude` is `default`;
- a dir whose name ends in a number keeps that number when it is free:
  `~/.claude-acme1` → `julienning1`, `~/.claude-acme2` → `julienning2`;
- any other dir gets the smallest free number: `~/.claude-acme` →
  `julienning3`, `~/.claude-work` → `julienning4`;
- dirs setup registers are numbered before dirs it only lists, so a personal
  dir never takes a team dir's number.

The prefix is `julienning` unless you pick another with
`julienning setup --name-prefix team` (letters, digits, `_`, `-`, not ending
in a digit); it applies to dirs registered from then on. Names never change
on their own: re-runs keep them, and you choose your own with
`julienning rename julienning2 work`, `adopt --name` or `new-config --name`.

### The allowlist

The Worker holds the team allowlist: every account it knows is shared. Setup
decides per email:

- **on the allowlist** → the dir is registered automatically (`shared (registered as julienning1)`);
- **declined before** on this machine → skipped (`personal (declined)`);
- **anything else** → in a terminal, `Share <email> (found in <dir>) with the team? [y/N]`.
  Yes asks for its [nickname](#nicknames), shares it and registers the dir.
  No stores the email in `declined_emails` in `config.json`, so you are not
  asked again.
- **not logged in** → listed, registered only if it already was
  (`not logged in (registered as julienning3)`).
- **a dir `new-config` created** whose account is not shared yet → offered
  like any other, with the nickname you gave `new-config` as the default
  (also for `--share EMAIL` without `--nick`). Saying no marks it personal
  and cancels the pending share. Without a terminal it is left to the
  session hooks, which share it on their own, and listed as
  `not shared yet (shares on login; registered as julienning3)`
  (`not logged in (registered as julienning3; shares on login)` before the
  login).

Without a terminal, or with `--yes`, nothing is asked: only emails given with
`--share EMAIL` (repeatable) are shared, under `--nick EMAIL=NAME` or the
default nickname; the rest are listed as
`not shared (re-run setup in a terminal or pass --share EMAIL)`. A taken
nickname fails that share
(`not shared (share failed: nickname "claude4" is already used by …; pass --nick claude4@example.com=NAME)`)
and setup exits 1 after the other steps. `--share` also overrides an earlier
decline. When the Worker cannot be reached, no email is offered.

A registered dir stays registered when its login changes; setup reports it
(`now logged in as x@y (not shared; registered as julienning3)`), and it stops
reporting until that email is shared.

### What it changes

- `~/.julienning/` (`$JULIENNING_HOME`): `config.json` (0600: dev, machine id,
  Worker URL and token, registered dirs, declined emails, name prefix) and
  `shared.json` (allowlist snapshot: shared emails and their nicknames).
- `<dir>/settings.json` of every registered dir, created if missing:

  ```json
  "statusLine": { "type": "command", "command": "/Users/you/.local/bin/julienning statusline" },
  "hooks": {
    "SessionStart": [ { "hooks": [ { "type": "command", "command": "/Users/you/.local/bin/julienning hook session-start" } ] } ],
    "SessionEnd":   [ { "hooks": [ { "type": "command", "command": "/Users/you/.local/bin/julienning hook session-end" } ] } ]
  }
  ```

  Every other key, the key order and 2-space indentation are kept; your own
  hooks stay. A statusLine that is not julienning's is replaced and saved in
  `~/.julienning/patches.json`; `forget` and `uninstall` put it back. The path
  is the `julienning` symlink that points at the running binary, looked up in
  `$JULIENNING_BIN_DIR`, `~/.local/bin`, then `PATH`, so updates never touch
  settings.json. When no such symlink exists (a binary run from the repo),
  setup writes the binary's own path and warns:
  `settings.json will run <path>, not the installed ~/.local/bin/julienning; ...`.
  An entry counts as julienning's when its command runs a file named
  `julienning` or a version file (`…/julienning/versions/<version>`), so
  re-runs update it instead of adding a second one.
- One line appended to `~/.zshrc` (zsh), or to `~/.bash_profile` on macOS /
  `~/.bashrc` on Linux (bash). The shell comes from `$SHELL` or `--shell`;
  `--no-rc` skips this step.

  ```sh
  command -v julienning >/dev/null 2>&1 && eval "$(julienning shell-init zsh)"  # julienning-shell-hook
  ```

  It defines a `claude` function that reads `~/.julienning/current` on every
  call (an exported `CLAUDE_CONFIG_DIR` always wins; with no selection you get
  plain `claude`), plus one `claude-<nickname>` function per team nickname in
  the cached allowlist, logged in here or not. `claude-alpha` asks
  `julienning resolve-dir alpha` for the dir logged into alpha at the moment
  you run it and starts `claude` there, passing its arguments through
  (`claude-alpha --resume`). It does not change the selection, never touches
  the network, and so never goes stale when a login moves to another dir; for
  an account no dir here is logged into it prints the sign-in hint (see
  [Nicknames](#nicknames)) and returns 1.
  Both are written as `function name {`, so an existing `alias claude=…`
  (Claude's installer adds one) cannot break the eval.
  A pre-existing `alias claude-<nickname>=` line hides the function of the
  same name and is reported
  (`` note: ~/.zshrc:1 has a hand-written `alias claude-alpha`; it hides julienning's claude-alpha function, so remove it ``),
  and so is an alias that selects a registered dir by path
  (`` note: ~/.zshrc:2 has a hand-written `alias claude-work` for ~/.claude-work; julienning's `claude-delta` function replaces it, so you can remove that line ``).
  Neither is edited. An `alias claude=` line gets a warning, because an alias
  that runs a path skips the function and the selection is ignored
  (`` julienning: warning: ~/.zshrc:1 defines `alias claude=…`; if it runs a path rather than `claude`, it bypasses julienning's claude wrapper and the selected config dir is ignored (make it call `claude`, or remove it) ``).
  It is not edited either.

Nothing else. Config dirs themselves are never created or deleted by setup.

**Safe to re-run**: steps that have nothing to do report `unchanged`, and the
rc line is found by its `# julienning-shell-hook` marker (a line with the
older `# julienning` marker is rewritten in place). Re-run it after logging a
dir into another account, or with `--token` when the token is rotated. New
`claude-<nickname>` functions appear in new terminals.

A dir whose settings.json cannot be patched (`Settings: work unchanged, broken failed`,
for example when the file is not a JSON object) or an account that could not
be shared does not stop setup: every step still runs, then setup exits 1
(`could not update settings.json of 1 config dir(s) (see above); fix and re-run setup to retry`).

### Default dir

Setting `CLAUDE_CONFIG_DIR=~/.claude` is not the same as leaving it unset
(Claude Code then reads another account file and credential). When the target
is `~/.claude` (registered as `default`), every launcher unsets the variable
instead: `next`, `use`, `login`, the `claude` function, and a
`claude-<nickname>` function whose account is logged in there, which runs
`env -u CLAUDE_CONFIG_DIR claude`.

## Daily use

```sh
julienning next                  # best shared account here, then start or resume a session
julienning use                   # stay on the current account, straight to its session picker
julienning use delta             # an account by nickname (or email, or config name), then the same
claude-delta                     # plain claude on delta; the selection stays as it is
julienning next --no-launch      # switch only
julienning use --clear           # no selection: plain claude uses your default config
```

```
julienning next [--all] [--limit N] [--no-launch] [--json]
julienning use [TARGET] [--all] [--limit N] [--no-launch] [--json] | use --clear
```

`use` without a target keeps the current selection and opens its session
picker without re-ranking; with nothing selected it behaves exactly like
`next`.

The selection is written to `~/.julienning/current` and is machine-wide: the
next `claude` in any terminal uses it.

**`next`** syncs this machine's claims, asks the Worker for the ranking (as
you: your own claims and reports never count as busy) and takes the best
account that has a registered, logged-in dir here. If several dirs are logged
into it, the selected one wins, else the first by config name. If your current
account ranks level with the winner, you stay. It warns when the winner is busy
(`julienning: warning: beta (claude2@example.com) is also in use by ali, can`)
and needs the Worker: without it,
`` julienning: cannot rank accounts (...); pick manually with `julienning use NICKNAME` ``.

**`use TARGET`** takes a nickname, an email or a config name (see
[Nicknames](#nicknames)) and works offline (it falls back to the cached
allowlist with a warning). A config name reaches any registered dir, also one
nobody is signed into; `use` then warns
(`julienning: warning: julienning3 is not logged in yet; run: julienning login julienning3`),
as it does for a dir whose account is not shared, and starts a plain new
`claude` there without the session list, so team sessions are never moved
into a personal account.

With `--no-launch`, or when stdin or stdout is not a terminal, they print one
line and exit:

```
Now using alpha (claude1@example.com) in ~/.claude-julienning1 — session 4% → 23:38, week 11% → Fri 00:28.
Staying on alpha (claude1@example.com) in ~/.claude-julienning1 (best available) — session 4% → 23:38, week 11% → Fri 00:28.
Now using delta (claude4@example.com) in ~/.claude-work — no usage reported yet.
Selection cleared; plain claude uses your default config.
```

A dir whose account has no nickname (not shared, not named yet, or not logged
in) is shown by its config name instead: `Now using julienning3.`

`--json` implies `--no-launch` and prints the Worker's account object plus
`"config"` (the config name) and `"nickname"`; for an account the Worker did
not list, just `config`, `email` and `nickname` (`null` when unknown).

### Session picker

In a terminal, `next` and `use` then list recent sessions for the current
directory's project from **every** registered dir (any account), newest first:
`--limit N` of them (default 100, max 500), or across all projects with `--all`.
With no sessions, a new `claude` starts directly.

```
Now using alpha (claude1@example.com) in ~/.claude-julienning1 — session 4% → 23:38, week 11% → Fri 00:28.
3 sessions in ~/Code/shop · type to filter
> New session  in alpha
  Fix the release script  delta · 20m ago · 3f2a9c1b (open in another terminal)
    The goreleaser step fails on arm64
  Billing refactor  delta · 2h ago · 11111111
    Refactor the billing module to use the new API
  Why does the login test flake?  alpha · 4h ago · aaaaaaaa
↑/↓ move · type to filter · Enter select · Esc cancel
```

- Rows: title (or the first prompt when there is no title), account (the
  nickname of the dir's current login, else its config name), last
  activity (`2h ago`, a local date and time after 6 days), the first 8
  characters of the id; the first prompt goes on a second line. `--all` adds
  the project directory. Titles stay bright; the metadata and the prompt line
  are dimmed (plain text under `NO_COLOR`), and long lines end in `…` rather
  than wrap.
- The line under the account says what is listed:
  `100 most recent sessions in ~/Code/shop` when the list reached `--limit`,
  `3 sessions in ~/Code/shop` when it shows everything found, without
  `in <dir>` under `--all`. While you type it becomes the filter and how many
  sessions match: `Filter: billing · 2 of 100`.
- `(open in another terminal)`: a running claude has it open; it is dimmed and
  cannot be picked. `(may be open)`: its dir has no live-session registry and
  the transcript changed in the last 2 minutes; it is dimmed, and moving it is
  refused.
- Keys: ↑/↓ (also j/k before you type, Ctrl-P/Ctrl-N), PgUp/PgDn, Home/End,
  Enter. Typing filters (every word must appear in the title, prompt, account,
  id or directory, any case); `/` starts a filter so `j`/`k` can be typed,
  Backspace edits, Ctrl-U clears. Esc, Ctrl-C or Ctrl-D leave without
  starting claude: `Not starting claude; alpha stays selected.`
- The selection is marked with `>` plus reverse video; `NO_COLOR` turns
  colors off. With `TERM=dumb` or no `/dev/tty` a numbered prompt is used
  instead: `Choose 1-3 (Enter = 1, text = filter, q = cancel):`.

**New session** runs `claude` in the target dir. A session already in the
target runs `claude --resume <id>` from the session's working directory. A
session in another dir is **moved** into the target first, once you confirm:

```
Move "Billing refactor" from delta to alpha?
  Its transcript, checkpoints, task list and session environment move to alpha; delta will no longer have it. Project memory is merged, never overwritten.
  Enter to continue · n or Esc to go back
```

Enter (or `y`) moves it and resumes it there:

```
Moved "Billing refactor" from delta to alpha (memory: 1 file added, 1 index line merged).
```

`n`, Esc or Ctrl-C go back to the same list with that session selected;
nothing is moved or printed. New session and sessions already in the target
never ask. Without a raw-mode terminal (`TERM=dumb`, no `/dev/tty`) the
question ends in a line prompt instead: `Continue? [Y/n]`.

Between two dirs logged into the same account, the question and the report
name the dirs instead (`from ~/.claude-work to ~/.claude-work2`).

The move takes `projects/<project>/<id>.jsonl`, `projects/<project>/<id>/`,
`file-history/<id>/`, `session-env/<id>/`, `tasks/<id>/`, `todos/<id>-*.json`
and `debug/<id>.txt`. Everything is copied under temporary names, fsynced,
renamed into place and size-checked before the source is deleted. A failure
before that, or Ctrl-C (also SIGTERM, or the terminal closing), removes the
partial copy from the target and leaves the source untouched
(`move of session 3f2a9c1b to julienning1 interrupted by Ctrl-C (nothing was changed)`).

The project memory is merged, never overwritten. Claude keeps it per
repository, so it comes from the folder of the session's git root
(`projects/<git root>/memory/`, shared by every worktree and subdirectory of
the repository; outside git, the session's own project folder). Missing files
are copied, a file that differs is kept as the target has it (with a
warning), and `MEMORY.md` gains the index lines it lacks, except lines that
link to a conflicting file or to a file the target's index already lists. A
`MEMORY.md` that is a symlink stays one; the file it points to gets the lines.
A move is refused while the session is open, when it may be open, or when the
target already has that id.

### Other commands

**`julienning accounts [--json]`** syncs claims, refreshes the allowlist and
prints the ranking:

```
#  NICK     ACCOUNT              SESSION      WEEK             STATE                     LOCAL                   UPDATED
1  alpha    claude1@example.com  4% → 23:38   11% → Fri 00:28  free                      *~/.claude-julienning1  2m ago
2  claude3  claude3@example.com  -            -                free                      -                       never
3  delta    claude4@example.com  -            -                free                      ~/.claude-work          never
4  beta     claude2@example.com  62% → 22:13  40% → Fri 00:28  in use by ali, can (12m)  -                       12m ago
```

- NICK: the team nickname; `-` for an account shared before nicknames that
  nobody has named yet.
- SESSION / WEEK: used % and reset time in your local zone: `HH:MM` today,
  `Mon HH:MM` within six days, else `YYYY-MM-DD HH:MM`; `0% → reset` once the
  reset has passed; `-` when never reported.
- STATE: `free`; `in use by …` when another dev reported usage in the last 15
  minutes (with the age of that report); `claimed by … (3m)` when other devs
  only hold claims (with the age of the oldest one). Every other dev is listed.
- LOCAL: the registered dirs here logged into that account, as paths, `*` on
  the selected one, `-` for none.
- Order: not busy first, then accounts with usage before those without, then
  session %, week %, session reset, email.
- `--json`: the Worker document (each account's `nickname` is `null` when it
  has none) with `local_config` and `local_configs` (config names) added to
  every account.

**`julienning current [--json]`** prints `none`, or:

```
alpha (claude1@example.com) in ~/.claude-julienning1
usage: session 4% → 23:38, week 11% → Fri 00:28
```

A dir whose account has no nickname shows its config name
(`julienning3 in ~/.claude-julienning3`). The usage line becomes
`usage: unavailable (<reason>)` offline, for a dir that is not logged in
(`config not logged in`) or for an account that is not shared
(`account is not shared`). `--json` prints `config`, `email`, `nickname`,
`account` (the Worker's object) and `usage_error`, `null` when unknown.

**`julienning configs [--json]`** lists registered dirs. NAME is the config
name the dir commands take; NICK is the team nickname of the dir's current
login (`-` when it has none or is not shared); SHARED is `?` until the
allowlist was fetched once, and `no (shares on login)` for a dir
`new-config` created whose account is not shared yet (`share_on_login` in
`--json`):

```
   NAME         NICK   DIR                    EMAIL                SHARED
*  julienning1  alpha  ~/.claude-julienning1  claude1@example.com  yes
   julienning2  delta  ~/.claude-work         claude4@example.com  yes
```

**`julienning new-config [--name NAME] [--copy-settings-from NAME] [--nick NICK] [--no-share] [--no-login]`**
creates and registers `~/.claude-julienning<N>` named `julienning<N>` (the
smallest N whose name and dir are both free; the prefix is the one from
`setup --name-prefix`) or `~/.claude-NAME` named `NAME`, and refuses an
existing dir (use `adopt`).
`--copy-settings-from` copies that registered dir's settings.json without
julienning's own statusLine and hooks (a statusLine julienning replaced there
is put back in the copy), then patches the copy like any other.

A new dir is for a team account, so the account you sign in with is shared
with the team automatically. Before creating anything new-config asks for its
[nickname](#nicknames) (Enter leaves it to the part before `@`; a nickname
the cached allowlist shows as taken is refused and asked again), then starts
claude in the new dir so you can sign in:

```
Nickname for this account [the email's name]: delta
Created ~/.claude-julienning3 (config "julienning3").
Starting claude in it so you can sign in.
The account you sign in with will be shared with the team as delta; julienning does that automatically when your first session starts.
```

The share runs in the background, in the claim sync the session hooks start:
normally at the first session start after you sign in, at the latest when
that session ends (or at the next `accounts`, `next` or `use`; `setup` in a
terminal offers it too). From then on the account is a team account like any
other: `claude-delta` in new terminals, usage in the status line, claims.
Until then `configs` shows
`no (shares on login)`. When the nickname turns out to be taken, or the
Worker cannot be reached, the share stays pending and is retried on every
session start and end; `errors.log` gets a `SHARE_FAILED` line, and
`julienning setup` offers the account with that nickname as the default so
you can pick another. An account declined as personal on this machine is
never shared this way. Without a terminal the nickname is `--nick NICK`, else
the email's local part. `--no-share` keeps the account personal: no question,
and `This account stays personal (not shared).` replaces the last line.

If claude is not installed, the dir stays registered and the error ends with
`sign in later with: julienning login julienning3`. `--no-login` (scripts)
only creates and registers the dir:

```
Created ~/.claude-julienning3 (config "julienning3").
Sign in: julienning login julienning3
The account you sign in with will be shared with the team as its email name; julienning does that automatically when your first session starts.
```

`--login`, from when signing in was opt-in, is still accepted and changes
nothing.

Until its account is shared and named, the new dir is reached by its config
name; once it is logged into an account that is already shared, its nickname
works right away.

**`julienning login TARGET [-- claude args...]`** runs `claude` against the
dir TARGET resolves to (see [Nicknames](#nicknames)) so you can sign in;
arguments after `--` go to claude. A fresh dir nobody is signed into has only
its config name (`julienning login julienning3`).

**`julienning share EMAIL [--nick NAME]`** adds an account to the team
allowlist under a nickname (default: the email's local part). Sharing an
account that is already shared keeps its nickname
(`claude1@example.com was already shared as alpha (rename with: julienning nick alpha NEW).`).
**`julienning nick TARGET NEW`** renames an account's nickname for the whole
team. **`julienning unshare TARGET [--yes]`** removes an account for everyone,
deleting its usage and claims on the Worker (asks first,
`Unshare beta (claude2@example.com)? This deletes its usage and claims for the whole team. [y/N]`;
without a terminal it needs `--yes`). A usage report or claim racing the
unshare cannot bring the account back. All three update `shared.json`.

```
Shared claude9@example.com with the team as claude9.
No registered dir here is logged in as it; `julienning setup` registers one once it is.
Renamed claude9 to gamma (claude9@example.com).
Shell function claude-gamma replaces claude-claude9 in new terminals (or run: source ~/.zshrc).
Unshared gamma (claude9@example.com); its usage and claims were removed from the Worker.
```

A taken nickname is refused:
`` julienning: nickname "alpha" is already used by claude1@example.com; pick another: julienning share claude8@example.com --nick NAME ``.

**`julienning adopt DIR [--name NAME]`** registers an existing dir and patches
its settings.json, named as in [Config names](#config-names) unless `--name`
is given; **`julienning forget NAME`** undoes both (see [Undo](#undo)).

```
Adopted ~/.claude-backup as config "julienning4" (settings.json added).
```

**`julienning rename OLD NEW`** renames a registered config. Config names are
internal labels, so no shell function changes; `nick` renames an account.
NEW must be a valid name (letters, digits, `_`, `-`, starting with a letter or
digit) that no other registered dir uses. Only `config.json` changes: the
selection stores the dir's path, so it stays, and settings.json does not
mention the name.

```
Renamed "julienning2" to "work" (~/.claude-work).
```

Also: `julienning version`, `julienning help [command]`. `statusline` and
`shell-init zsh|bash` are what settings.json and the rc line call, and the
hidden `resolve-dir TARGET` is what the `claude-<nickname>` functions call.

## How claims work

A claim is an advisory "in use" marker, one per dev + machine, so an account
can have several holders. Claims follow live Claude sessions, not
`use`/`next`:

1. The `SessionStart` / `SessionEnd` hooks run `julienning hook session-start|session-end`.
   The hook reads Claude Code's JSON from stdin and checks locally that
   julienning is set up and the dir is registered; `session-start` also needs
   the dir logged in with an email in `shared.json`, or a share pending from
   `new-config`. `session-end` does not,
   so a session that ended after `/logout` still releases its claim. It then
   starts a detached `julienning claim-sync` and exits 0. It never prints,
   never fails and never touches the network. A `SessionEnd` caused by
   `/clear` is ignored (the same process goes on).
2. `claim-sync` takes a per-machine lock (`~/.julienning/claim-sync.lock`),
   first shares the account of every dir `new-config` created that is now
   signed in (so it is claimed in the same run; see `new-config` above), then
   counts live sessions per shared email across all registered dirs (the
   registry `<dir>/sessions/<pid>.json` of running processes, minus the session
   that is ending), then claims (`PUT /accounts/<email>/claim`) while the count
   is above 0 and releases (`DELETE`) when it drops to 0. Only interactive and
   background sessions count; Claude's daemons and pre-spawned spare processes
   never hold a claim. A held claim is re-sent at most once an hour. Held
   claims are recorded in `~/.julienning/claims.json`, and only those are ever
   released.
3. The same reconciliation runs inside `accounts`, `next` and `use`, and in the
   usage reporter at most every 10 minutes, which cleans up after sessions that
   crashed without `SessionEnd`.

Safety nets: the Worker ignores holders older than `CLAIM_TTL_MIN` (720 minutes
by default), and a usage report re-stamps the reporter's own claim, if it has
one, once that claim is over an hour old. A dir without a live-session
registry can keep a claim but never releases it; the TTL does.

## How usage reaches KV

1. Claude Code runs `julienning statusline` and pipes it JSON. It prints
   `<model> · ctx <n>%` (for example `Opus 4.6 · ctx 31%`), adds
   ` · usage pending` until both rate-limit windows are present (normal before
   the first reply), always exits 0 and does no network I/O.
2. Gate, all local: set up, `CLAUDE_CONFIG_DIR` (or `~/.claude` when unset) is
   a registered dir, its email is readable and is in `shared.json`. Otherwise
   it prints the line and stops.
3. It starts a detached `julienning send-usage` and returns.
4. `send-usage` checks `shared.json` again and debounces per account (state in
   `~/.julienning/sent/<hash>.json`, named by a hash of the email): a
   changed reset time (a new window) goes out at once; the same numbers as the
   last successful send wait 30 minutes; anything else waits
   `send_min_interval_sec` (default 300, in `config.json`) since the last
   attempt. Then `PUT /accounts/<email>/usage` with a 5 s timeout. If the
   Worker answers `account is not shared`, the email is dropped from
   `shared.json` and the status line stops reporting it.
5. Upkeep rides along: claims are reconciled when the last reconcile is over
   10 minutes old, `shared.json` is refreshed when older than an hour, and on
   either occasion the daily update check runs afterwards (at most once per
   24 h; it installs a newer release unless auto-update is off, see
   [Install](#install)).

Failures go to `~/.julienning/errors.log`; the status line never breaks. KV
holds only the latest snapshot per account: a report older than the stored
one is dropped, and a `collected_at` ahead of the Worker's clock is clamped to
it, so one machine with a fast clock cannot make everyone else's reports look
stale.

## Undo

```sh
julienning use --clear           # drop the selection only
julienning forget NAME           # unregister one dir, undo its settings.json changes
julienning uninstall             # undo setup on this machine
julienning uninstall --purge     # ... and delete julienning's own data, installed versions and command
```

`forget` never deletes the dir, and clears the selection if it pointed there:

```
Forgot "work". ~/.claude-work was not deleted.
settings.json: removed statusLine, removed SessionStart hook, removed SessionEnd hook, removed empty hooks, deleted the settings.json julienning created.
It was the current config; nothing is selected now.
```

When another registered dir uses the same settings.json (a symlink), `forget`
leaves the file alone, since that dir still needs it:
`settings.json: left as is; it is the same file as the settings.json of work, which still uses julienning.`
If the unpatch fails, the dir stays registered so you can fix the file and
run `forget` again.

`uninstall [--purge] [--yes]`, for every registered dir: restores the saved
statusLine (or removes julienning's), removes only julienning's hook entries,
drops containers it created that are now empty and deletes a settings.json it
created that is now empty (a settings.json shared by several dirs is handled
once). It removes the hook line from `~/.zshrc`, `~/.bashrc` and
`~/.bash_profile`, releases this machine's claims (best effort; the TTL covers
the rest) and clears the selection. Safe to run twice (`Nothing to uninstall.`).

```
Settings: ~/.claude-julienning1/settings.json: restored previous statusLine, removed SessionStart hook, removed SessionEnd hook, removed empty hooks
Settings: ~/.claude-work/settings.json: removed statusLine, removed SessionStart hook, removed SessionEnd hook, removed empty hooks, deleted the settings.json julienning created
Shell:    removed the julienning hook line from ~/.zshrc
Current:  cleared the selected config dir
Kept ~/.julienning (identity, registered dirs): `julienning setup` re-installs, `julienning uninstall --purge` removes it and the julienning command.
```

`--purge` asks first
(`Delete ~/.julienning, ~/.local/share/julienning/versions, ~/.local/bin/julienning? This cannot be undone. [y/N]`)
and without a terminal needs `--yes`. It deletes only julienning's own files
there, and a directory only once that leaves it empty; anything else stays and
is listed. The `julienning` symlink goes only when it points into the versions
dir. A path that is `/`, `$HOME` or a parent of `$HOME` (a bad
`JULIENNING_HOME`, `JULIENNING_VERSIONS_DIR` or `JULIENNING_BIN_DIR`) is
refused before anything changes.

```
Purged:   julienning's files in ~/.julienning
Kept ~/.julienning: it also holds files julienning did not create (my-notes.txt).
Purged:   ~/.local/share/julienning/versions
Purged:   ~/.local/bin/julienning
```

When a settings.json could not be restored, or `config.json` cannot be read,
`--purge` deletes nothing and exits 1: `patches.json` holds the statusLines
still to restore, `config.json` lists the dirs still wired to julienning, and
the command stays installed so you can fix the problem and run it again
(`julienning: not purging: 1 settings.json could not be restored (see above); ...`).
The rest of the uninstall still runs. Claude config dirs and sessions are
never deleted, and nothing is unshared on the Worker.

## Web view

The Worker serves the same ranking to curl or a browser. Everything but
`/healthz` needs the team token:

```sh
read -rs TOKEN   # paste the team token; keeps it out of shell history
W=https://julienning.<your-subdomain>.workers.dev
curl -H "Authorization: Bearer $TOKEN" "$W/accounts"
curl -H "Authorization: Bearer $TOKEN" "$W/accounts?format=json"
curl -H "Authorization: Bearer $TOKEN" "$W/accounts?tz=Europe/Istanbul"
curl -H "Authorization: Bearer $TOKEN" "$W/accounts?dev=murat"
curl "$W/healthz"                                   # ok
```

```
#  NICK     ACCOUNT              SESSION      WEEK             STATE                     UPDATED
1  alpha    claude1@example.com  4% → 23:38   11% → Fri 00:28  free                      2m ago
2  claude3  claude3@example.com  -            -                free                      never
3  delta    claude4@example.com  -            -                free                      never
4  beta     claude2@example.com  62% → 22:13  40% → Fri 00:28  in use by ali, can (12m)  12m ago

generated 2026-09-29 20:28 (UTC)
```

- `format=text` (default) or `json` (the full ranked document).
- `tz=` any IANA zone, DST-correct; default is the Worker's `DEFAULT_TZ`
  (`UTC`). An invalid one is a 400.
- `dev=` ranks as that dev: their own claims and reports do not count as busy.
  Without it, every dev counts. The text STATE for claims is
  `claimed by ali, can` (no age); there is no LOCAL column.
- GET requests may pass `?token=<token>` instead of the header (it ends up in
  browser history and logs).

## Deploying the Worker

The Worker is in `worker/` (TypeScript, wrangler v4, KV binding `USAGE`,
secret `AUTH_TOKEN`). Every writer has its own KV key (`share:<email>`, which
also holds the nickname, `usage:<email>`, `claim:<email>:<dev>:<machine_id>`),
so concurrent requests never overwrite each other's data. A nickname rename is
the one rewrite, and the nickname check is read-then-write; the two races that
accepts are in [docs/CLOUDFLARE.md](CLOUDFLARE.md#14-kv-limits). [docs/CLOUDFLARE.md](CLOUDFLARE.md) walks through
it from a fresh Cloudflare account: API token, `npx wrangler kv namespace create USAGE`
(paste the id over `REPLACE_ME` in `worker/wrangler.jsonc`),
`openssl rand -hex 32` + `npx wrangler secret put AUTH_TOKEN`, `npm run deploy`,
token rotation, the `DEFAULT_TZ` / `CLAIM_TTL_MIN` / `ACTIVITY_TTL_MIN` vars,
GitHub Actions deploys (`.github/workflows/deploy-worker.yml`, secrets
`CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`) and the KV free-tier
limits. Commands only: [worker/README.md](../worker/README.md).

## Development

Go 1.25 (`go.mod`), one dependency (`golang.org/x/term`). The Makefile runs Go
as `GOTOOLCHAIN=local go` so the local toolchain is used and nothing is
downloaded; do the same for direct `go` commands.

```sh
make build         # bin/julienning, version from `git describe --tags --always --dirty` (dev outside git)
make install       # version dev into the installer layout (see Install)
make test          # test-go + test-worker
make test-go       # go test -race ./...
make test-worker   # cd worker && npm test
make lint          # gofmt -l, go vet ./..., worker typecheck
make clean         # rm -rf bin dist
```

Worker only: `cd worker && npm ci && npm test && npm run typecheck`.

CI (`.github/workflows/ci.yml`, every push and PR): gofmt, `go vet`,
`go test -race`; `bash -n` and shellcheck on `scripts/install.sh` plus
`goreleaser check`; worker `npm ci`, typecheck and tests.

**Releasing:** tag `vX.Y.Z` and push it. `make release` does that with
guards: it refuses a malformed version, a dirty tree, a branch other than
`main` or an existing tag, and runs the tests first.

```sh
make release VERSION=0.3.0     # == git tag -a v0.3.0 && git push origin v0.3.0, guarded
```

`.github/workflows/release.yml` runs `go test -race` and goreleaser
(`.goreleaser.yaml`), which publishes `julienning_<version>_<os>_<arch>.tar.gz`
for darwin/linux × amd64/arm64 plus `checksums.txt`: exactly what
`scripts/install.sh` and `julienning update` download. `vX.Y.Z-rc.N` tags
become prereleases, which `/releases/latest` skips.

[docs/SPEC.md](SPEC.md) is the contract between the CLI, the Worker and this plumbing.
`JULIENNING_NOW_EPOCH=<unix seconds>` freezes the clock for tests and bug
reproductions.

## Troubleshooting

`~/.julienning/errors.log` first. Lines look like

```
2026-09-29T23:23:40+03:00 NOT_SHARED config_dir=<unset> config dir is not registered with julienning
```

(`config_dir` is `CLAUDE_CONFIG_DIR`, `<unset>` for `~/.claude`). Every
status line code (errors and gate outcomes alike) and every hook code is
written at most once an hour per code; the status line's error row still
shows on every render. `SEND_FAILED` is written at most every 10 minutes per
account; `CLAIM_SYNC_FAILED`, `SHARE_FAILED`, `SHARED_REFRESH_FAILED` and
`UPDATE_CHECK_FAILED` at most every 10 minutes. The file is trimmed to its
newest 500 lines once it passes 1 MB.

| Code | Meaning |
|------|---------|
| `NOT_SETUP` | no `config.json`, or no Worker URL/token: run `julienning setup` |
| `NOT_SHARED` | the dir is not registered, its email is not in `shared.json` (or the file is unreadable), or the Worker said `account is not shared` (the email was dropped from `shared.json`). Expected for personal dirs |
| `ACCOUNT_FILE_UNREADABLE` | the account file (`~/.claude.json` or `<dir>/.claude.json`) is missing, unreadable or not JSON, or the dir cannot be resolved; sign in with `julienning login <config>` |
| `EMAIL_INVALID` | the account file has no valid `oauthAccount.emailAddress`; sign in again |
| `INVALID_STDIN` | the status line input was not a JSON object (the status line shows `julienning: status line input is not valid JSON`) |
| `USAGE_INVALID` | `rate_limits` had an unexpected shape; the line ends with a type signature such as `five_hour=string seven_day=absent`. Claude Code changed its format: report it |
| `CLOCK_INVALID` | `JULIENNING_NOW_EPOCH` is not integer epoch seconds; unset it |
| `SPAWN_FAILED` | the detached `send-usage` or `claim-sync` could not be started |
| `SEND_FAILED` | the usage PUT failed; the rest of the line says why (`401 unauthorized`, `request timed out after 5s`, `request failed: …`, `500 internal error`). At most every 10 minutes per account |
| `HOOK_INPUT_INVALID` | a hook got an argument other than `session-start` / `session-end` (it then does nothing) or unusable stdin (the sync still runs) |
| `CLAIM_SYNC_FAILED` | claim reconciliation failed (Worker unreachable, lock, unreadable dir) |
| `SHARE_FAILED` | the account of a dir `new-config` created could not be shared yet; the share stays pending and is retried. `nickname taken: "delta" belongs to another team account` → pick another with `julienning setup`; otherwise the Worker or the account file says why. Also written once when the account signed in there is marked personal on this machine (then it is never shared) |
| `SHARED_REFRESH_FAILED` | refreshing `shared.json` failed |
| `UPDATE_CHECK_FAILED` | the daily update check failed: the latest-release lookup on GitHub (retried on the next background run), or the auto-update install (download, checksum, the new binary's `version`, the symlink switch; retried at the next daily check, the active version is unchanged), or `JULIENNING_AUTO_UPDATE` is not a boolean. `julienning update` shows the same error right away |
| `LOG_FAILED` | errors.log itself could not be written; shown only in the status line |

Common messages:

- **`usage pending` in the status line**: normal until the first reply of a
  session.
- **Status line shows only model and context**: julienning replaced that dir's
  statusLine; `forget` or `uninstall` restores yours.
- **`Worker URL or token not configured (run: julienning setup, or set JULIENNING_REMOTE_URL and JULIENNING_TOKEN)`**:
  run `julienning setup`. `JULIENNING_REMOTE_URL` / `JULIENNING_TOKEN` override
  `config.json` at runtime and are never written to it.
- **`the team token was rejected by the Worker ...`** (setup, share, unshare),
  `401 unauthorized` elsewhere: the token was rotated; get the new one and run
  `julienning setup --token <token>`.
- **`cannot rank accounts (...)`**: the Worker is unreachable; check
  `curl <worker>/healthz`, then pick an account yourself with
  `julienning use <nickname>`.
- **`none of the N shared accounts is logged in on this machine ...`**: sign a
  registered dir into a shared account (`julienning new-config` or
  `julienning login <config>`); an account nobody shared yet also needs
  `julienning setup` or `julienning share EMAIL`.
- **`… is shared but not logged in on this machine; sign in with …`** (`use`,
  `login`, `claude-<nickname>`): no registered dir here is logged into that
  account right now. Sign one in (`julienning new-config`, or
  `julienning login <config>` for an existing dir); a dir julienning does not
  know yet also needs `julienning setup` or `julienning adopt DIR`.
- **`unknown target "…": not a team nickname, an email, or a config name ...`**:
  `julienning accounts` lists the nicknames (and refreshes the cached
  allowlist, in case a teammate just shared or renamed the account),
  `julienning configs` the config names.
- **`nickname "…" is already used by …`**: another team account holds it; pick
  another (`--nick NAME`, or `julienning nick`).
- **`` the team allowlist is empty; share an account with `julienning share EMAIL` ``**.
- **`claude not found on PATH (install Claude Code first)`**: julienning never
  bundles claude.
- **`session "…" is open in another terminal (…); exit it there first`** /
  **`… may still be open; exit it or try again shortly`** /
  **`<config> already has a session with id …`**: the move was refused and
  nothing changed.
- **`julienning update` says `… cannot manage this installation`**: install
  once with the curl one-liner.
- **A `claude-<nickname>` function is missing**: functions are generated at
  shell start from the cached allowlist; open a new terminal or `source` the
  rc file setup edited (`source ~/.zshrc`; bash: `~/.bash_profile` on macOS,
  `~/.bashrc` on Linux), after `julienning accounts` when a teammate just
  shared or renamed the account. A hand-written `alias claude-<nickname>`
  hides the function; setup points out the line.
- **`SEND_FAILED … 500 internal error`**: the Worker threw; `cd worker && npx wrangler tail`
  shows why (for example KV's free-tier daily write limit, see
  [docs/CLOUDFLARE.md](CLOUDFLARE.md#14-kv-limits)).

### Files in `~/.julienning`

| File | Purpose |
|------|---------|
| `config.json` | dev, machine id, Worker URL and token, registered dirs (with `share_on_login` while a share from `new-config` is pending), declined emails, `send_min_interval_sec`, `name_prefix`, `auto_update` (absent = on; `false` turns background auto-updates off) |
| `current` | path of the selected config dir (read by the `claude` function) |
| `shared.json` | allowlist snapshot: shared emails and their team nicknames (read by the status line, hooks and `claude-<nickname>`) |
| `claims.json` | emails this machine holds a claim on |
| `patches.json` | statusLine values replaced in settings.json, for restore |
| `update-check.json` | latest known release, when it was checked, and the version the last auto-update installed (until its one-time notice is shown) |
| `errors.log` | diagnostics, no emails |
| `sent/` | send-usage debounce cache and in-flight markers, one `<hash>.json` / `<hash>.inflight` per account (a hash of the email, never the address) |
| `claim-sync.lock`, `update.lock`, `.claims-reconciled`, `.last-*` | lock and throttle markers (per-account ones are also named by the hash) |
