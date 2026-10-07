# julienning — design spec (v2)

Shared Claude Code account switching, session hand-off and usage tracking for
a small team rotating a pool of Claude.ai subscriptions. Public repo
`github.com/muratgozel/julienning`; nothing team-specific is compiled in.

This document is the contract between the CLI (Go), the Worker (TypeScript on
Cloudflare Workers + KV) and the repo plumbing. Keep it current.

## Vocabulary

- **config dir**: a Claude Code config directory. `~/.claude` is the
  **default dir** (used when `CLAUDE_CONFIG_DIR` is unset); others are
  selected with `CLAUDE_CONFIG_DIR=<dir>`.
- **account file**: `~/.claude.json` for the default dir,
  `<dir>/.claude.json` otherwise (`claudecfg.AccountFilePath`). The login
  email is `oauthAccount.emailAddress`, lowercased everywhere.
- **Default-dir rule (critical)**: setting `CLAUDE_CONFIG_DIR=~/.claude`
  explicitly is NOT the same as leaving it unset (Claude then reads another
  account file and credential key). Every launcher, shell function and
  wrapper must unset the variable for the default dir (`launch.Env`).
- **account**: identified by email. Logins move between dirs over time, so a
  dir's email is always read live, never trusted from a cache.
- **shared account**: an email on the **team allowlist** held by the Worker.
  Every account record in KV is an allowlisted email. Once any teammate shares
  an email, every machine treats any dir logged into it as shared.
- **registered dir**: a config dir julienning manages on this machine
  (settings.json statusLine + hooks). A registered dir only reports usage or
  claims while its current email is shared. It has a **config name**
  (`julienning1`), an internal label: it never becomes a shell name.
- **nickname**: the team-wide name of a shared account (`alpha`), stored with
  its allowlist entry in the Worker. What users type and see (see Nicknames).
- **dev** (e.g. `murat`) + **machine id** (12 random hex, generated once):
  identity sent with reports and claims.
- **claim**: advisory "in use" marker. Holders are dev+machine pairs; an
  account can have several. Claims are driven by live Claude sessions.
- **session**: a Claude Code conversation, `projects/<enc-cwd>/<id>.jsonl`.

## Nicknames

Users type and see **account nicknames**, never dir names. A nickname is
team-wide: it is stored with the allowlist entry in the Worker
(`share:<email>` metadata) and cached locally in `shared.json`.

- **Rule** (`shell.ValidNickname`): `^[a-z0-9][a-z0-9._-]{0,31}$`. The CLI
  trims and lowercases input before validating; the Worker also lowercases,
  so uniqueness is case-insensitive.
- **Default** (`config.DefaultNickname`): the email's local part, lowercased, every
  character outside `[a-z0-9._-]` replaced by `-`, leading `.`/`_`/`-`
  dropped, cut to 32 (`claude1@x.io` → `claude1`, `john+team@x.io` →
  `john-team`). When nothing valid is left the user must pass one.
- **Chosen once, by whoever shares**: setup asks
  `Nickname for claude1@x.io [claude1]: ` right after a yes to `Share …?`
  (Enter = default; an invalid answer is explained and asked again).
  Non-interactive setup uses `--nick EMAIL=NAME` (repeatable; validated up
  front: one name per email, one email per name) or the default;
  `julienning share EMAIL [--nick NAME]` the flag or the default.
  `new-config` asks before the login, when the email is not known yet:
  `Nickname for this account [the email's name]: ` (Enter = the default,
  decided at share time; an invalid answer or one the cached allowlist shows
  as taken is explained and asked again), or takes `--nick NICK`; the share
  itself happens on login (see new-config). Sharing an
  email that is already shared never changes its nickname (the Worker ignores
  the body's nickname); `share` then prints
  `claude1@x.io was already shared as alpha (rename with: julienning nick alpha NEW).`
- **Unique**: share and rename answer 409
  `{"error": "nickname is taken", "nickname", "by": <email>}`. The CLI names
  the holder (`share` and `nick` refresh the allowlist first):
  `nickname "alpha" is already used by claude1@x.io`, followed by
  `; choose another.` and a new prompt (interactive setup),
  `; pass --nick EMAIL=NAME` (non-interactive setup; that share fails),
  `; pick another: julienning share EMAIL --nick NAME` (`share`) or
  `; pick another name` (`nick`). The check is read-then-write, so two
  simultaneous requests can both succeed (accepted; docs/CLOUDFLARE.md
  "KV limits").
- **Rename**: `julienning nick TARGET NEW` (`PUT /accounts/:email/nickname`)
  prints `Renamed alpha to gamma (claude1@x.io).` and
  `Shell function claude-gamma replaces claude-alpha in new terminals (or run: source ~/.zshrc).`
  (an unnamed account: `Named claude1@x.io gamma.` and
  `Shell function claude-gamma is available in new terminals (or run: source ~/.zshrc).`;
  unchanged: `claude1@x.io is already called gamma.`). The `source` hint
  names the rc file setup edits for `$SHELL` (zsh: `~/.zshrc`; bash:
  `~/.bash_profile` on macOS, `~/.bashrc` on Linux); setup's next steps say
  `open a new terminal, or run: source ~/.zshrc` the same way, and the
  installer `Then open a new terminal, or run: source ~/.zshrc (zsh) /
  source ~/.bashrc (bash).` A 404 drops the email from `shared.json` and
  suggests `julienning share EMAIL --nick NEW`.
  Teammates pick a rename up when their cache refreshes.
- **Legacy records** (shared before nicknames: Worker `nickname: null`,
  shown `-`): setup step 4 names those logged into a dir on this machine;
  interactive asks as above, non-interactive assigns `--nick` or the default.
  Each success prints
  `Nickname: claude1@x.io is now claude1 (rename with: julienning nick claude1 NEW)`;
  a failure is a warning
  (`claude1@x.io has no nickname yet: …; name it with: julienning nick claude1@x.io NAME`),
  never a setup failure. `share` of a legacy email names it too. Until then
  the account is labelled by config name.

### Resolution (`internal/resolve`)

`resolve.Target(cfg, cache, arg, current)` runs at call time against the live
logins (accounts move between dirs), using the cached allowlist:

1. a nickname in the cache → its email → step 2's lookup;
2. an argument containing `@` (an email, shared or not) → the registered dirs
   currently logged into it (`resolve.Dirs`: the current selection first,
   then by config name; an unreadable account file counts as not logged in);
   the first wins. None → `*resolve.NotLocalError`:
   ``alpha (claude1@x.io) is shared but not logged in on this machine; sign in with `julienning login <config>` or `julienning new-config` ``
   (without a nickname:
   `no registered config dir on this machine is logged in as x@y; sign in with …`);
3. a registered config name → that dir, logged in or not (the only way to
   reach a fresh dir);
4. otherwise
   ``unknown target "x": not a team nickname, an email, or a config name (see `julienning accounts` and `julienning configs`)``.

A nickname wins over a config name spelled the same. `use` refreshes the
allowlist from the Worker before resolving when it can (so a nickname a
teammate just set wins) and falls back to the cache; `login` and
`resolve-dir` use the cache only. `nick` and `unshare` act on the team
account instead (`accountByTarget`): an email; else a cached nickname; else a
config name, standing for its dir's current login (refused when it is not
logged in); else one allowlist refresh and a retry as a nickname
(`unknown nickname "x" (…)`). The account need not be logged in here.

### Labels

A dir is shown by the nickname of the account it is logged into right now,
else by its config name (`resolve.Label`); an account as `alpha
(claude1@x.io)`, or the bare email without a nickname. Nicknames appear in
setup's Dirs listing, `accounts` (NICK column; LOCAL lists shortened dir
paths, `*` on the selected one), `configs` (NICK next to NAME), `current`
(`alpha (claude1@x.io) in ~/.claude-x`), the `next`/`use` summary line
(`Now using alpha (claude1@x.io) in ~/.claude-x — …`), picker rows, the move
report, the busy warning and the `next` "none logged in" error. `--json` of
`next`/`use` and `current` carries `nickname` (null when none), `configs
--json` too (`""` when none), and `accounts --json` passes the Worker's
through. Config names remain in
`configs`, setup's statuses (`shared (registered as julienning1)`),
`adopt`/`forget`/`rename`/`new-config`, `local_config`/`local_configs`, the
`share` line `Logged in here as: julienning1.`, and messages about one dir
(`julienning3 is not logged in yet; run: julienning login julienning3`, move
refusals and interruptions).

Shell integration generates one **function** per team nickname,
`claude-<nick>`, which resolves the dir at call time (see "shell
integration"); dir-name aliases no longer exist.

## Local state (`~/.julienning`, override `$JULIENNING_HOME`)

| File | Owner | Purpose |
|------|-------|---------|
| `config.json` | config | identity, remote, registered dirs (with pending shares from `new-config`), declined emails, name prefix |
| `current` | config | absolute path of the selected config dir |
| `shared.json` | sharedcache | allowlist snapshot `{fetched_at, emails, nicknames}` (`nicknames`: email → nickname, omitted when none) |
| `claims.json` | claims | emails this machine currently holds a claim on |
| `patches.json` | claudecfg | per settings.json path: prior `statusLine` value for restore |
| `update-check.json` | selfupdate | `{checked_at, latest, installed?, installed_at?, notified?}` (the `installed*` fields: last background auto-update; absent `notified` = false) |
| `update.lock` | selfupdate | auto-update lock (O_EXCL, stale after 10 min) |
| `errors.log` | usage | diagnostics, never contains emails |
| `sent/` | usage | send-usage debounce cache + in-flight markers, named by a short hash of the email (no addresses in file names) |
| `claim-sync.lock`, `.claims-reconciled` | claims | per-machine lock (stale after 60 s), 10-minute reconcile marker |
| `.last-<CODE>` | usage | error-log throttle markers (email-free names) |

`config.json`:

```json
{
  "version": 1,
  "dev": "murat",
  "machine_id": "3fa9c2d1e07b",
  "remote": { "url": "https://julienning.example.workers.dev", "token": "…" },
  "configs": [
    { "name": "default", "dir": "/Users/murat/.claude" },
    { "name": "julienning1", "dir": "/Users/murat/.claude-julienning1" },
    { "name": "julienning2", "dir": "/Users/murat/.claude-julienning2",
      "share_on_login": { "nickname": "delta" } }
  ],
  "declined_emails": ["me@personal.com"],
  "send_min_interval_sec": 300,
  "name_prefix": "julienning",
  "auto_update": false
}
```

`share_on_login` is optional per dir: `new-config`'s pending share of the
dir's first login (see new-config); `nickname` absent = the email's local
part, decided at share time. It must pass the nickname rule (checked on load
and save). It is removed once the share lands, the email turns out to be
shared already, or the email is declined; `forget` drops it with the dir.
`name_prefix` is optional (absent = `julienning`, `config.Prefix()`).
`auto_update` is optional (absent = on; `cfg.AutoUpdateEnabled()`, which also
honours `JULIENNING_AUTO_UPDATE`).

Remote URL and token come only from setup (prompt or `--remote-url` /
`--token`) or the env vars `JULIENNING_REMOTE_URL` / `JULIENNING_TOKEN`
(env wins at runtime). `config.RequireRemote` gives the actionable error.

**Config names** match `[A-Za-z0-9][A-Za-z0-9_-]*`. They are internal labels
(`configs`, setup's statuses, `adopt`, `forget`, `rename`, `new-config`, and
`login`/`use` of a dir nobody is signed into) and never become shell names;
users otherwise see nicknames (see Nicknames).
Dir basenames often name the team, so a name julienning picks never contains
any part of the basename except a trailing number. Every dir julienning
registers without `--name` (setup, `adopt`, `new-config`) is named
`<prefix><N>`, where the prefix is `name_prefix` (`setup --name-prefix NAME`;
a valid config name that does not end in a digit, so `<prefix><N>` is never
ambiguous; default `julienning`):

- registered dirs keep their stored name; nothing is ever renamed implicitly
  (changing the prefix only affects dirs registered afterwards; `rename`
  renames);
- Claude's default dir `~/.claude` → `default` (when `default` is taken by
  another dir it is numbered like any other);
- a basename ending in the decimal number N ≥ 1 (leading zeros dropped, `0`
  never reused) → `<prefix><N>` when that name is free: not registered and
  not assigned earlier in the same run (`~/.claude-x1` → `julienning1`);
- otherwise the smallest free N ≥ 1 (`~/.claude-x` → `julienning3` next to
  `~/.claude-x1` and `~/.claude-x2`).

Order within one run (`discover.AssignNames`): registered names are fixed;
then the preferred group (setup: dirs it registers in this run, so personal
dirs that are only listed never take a team dir's number), then the rest;
within a group, number reuse first, then smallest-free, each in path order.
`new-config` without `--name` creates `~/.claude-<prefix><N>` named
`<prefix><N>` (see below).

## Install layout and updates (`internal/paths`, `internal/selfupdate`)

Mirrors Claude Code's native installer:

- binaries: `${XDG_DATA_HOME:-~/.local/share}/julienning/versions/<version>`
  (one executable file per version; `<version>` without a leading `v`, or
  `dev`). Override: `JULIENNING_VERSIONS_DIR`.
- command: symlink `~/.local/bin/julienning` → the active version.
  Override dir: `JULIENNING_BIN_DIR`.
- Switching versions is atomic: create a temp symlink next to the link, then
  rename over it. Keep the 3 most recent versions (by mtime) plus the active
  one; delete older ones.
- settings.json commands use `paths.StableCommand()`: a `julienning`
  symlink that resolves to the running binary (`paths.FindLink`:
  `$JULIENNING_BIN_DIR`, then `~/.local/bin`, then the first `julienning` on
  `PATH`, because later runs rarely have the installer's env set); otherwise
  the running binary with a warning (dev build run from the repo).

`scripts/install.sh` (public, `curl -fsSL
https://raw.githubusercontent.com/muratgozel/julienning/main/scripts/install.sh | bash`,
optional `-s -- vX.Y.Z`): needs only curl, tar and shasum/sha256sum. Resolves
the latest tag from the `https://github.com/muratgozel/julienning/releases/latest`
redirect (no API rate limit), downloads
`julienning_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from
`https://github.com/muratgozel/julienning/releases/download/<tag>/`, verifies
the checksum (fail closed), installs into the versions dir, repoints the
symlink, prunes, prints a PATH hint if needed, and ends with `Next:
julienning setup`. Re-running upgrades. A version argument wins over
`JULIENNING_VERSION`; both accept an optional leading `v`. Env:
`JULIENNING_VERSION`, `JULIENNING_BIN_DIR`, `JULIENNING_VERSIONS_DIR`,
`JULIENNING_REPO` (default `muratgozel/julienning`), plus bash and mktemp,
`JULIENNING_RELEASES_BASE` (testing: replaces `https://github.com/<repo>`).
The whole script body runs inside a function so a truncated download does
nothing, and the new binary must run `version` before the link switches.

`julienning update [--version vX.Y.Z]`: same steps in Go, manual and
immediate. Up to date → `julienning 0.3.0 is up to date.` (or `… (newer than
the latest release 0.2.9).`); local builds (`dev`, commit hashes,
git-describe versions) always move to the latest release. `--version` allows
downgrades. Prints `Updated 0.2.1 → 0.3.0.`. Refuses with a curl-installer
hint when the command is not a symlink into the versions dir. Rewrites
`update-check.json` to `{checked_at, latest}` (dropping any pending
auto-update notice). After installing a version older than the latest while
auto-update is on and config.json loads, it adds `julienning: note:
auto-update will move to 0.3.0 again within a day; to stay on 0.2.9, set
"auto_update": false in <config.json path> (or export
JULIENNING_AUTO_UPDATE=0)` on stderr.

Auto-update (`selfupdate.AutoUpdateIfDue(ctx, cfg)`), background only: the
send-usage child (on its upkeep occasions) and claim-sync call it after
releasing the claim-sync lock, with a context that has no deadline of their
own (the function bounds itself to 5 min, below the lock's 10 min
staleness); errors go to errors.log as `UPDATE_CHECK_FAILED` (throttled,
email-free). Steps:

1. Skip local builds (`IsDevBuild` of the running version) entirely, and
   return when `update-check.json` is younger than 24 h (a `checked_at` more
   than a minute in the future counts as stale).
2. Take `update.lock` (O_EXCL; a lock older than 10 min, or more than a
   minute in the future, is broken; release removes it only while it still
   holds this pid). Not acquired → return. Re-read the cache; fresh → return.
3. Look up the latest tag. Failure → return the error, cache unchanged (the
   next background run retries). Nothing published → latest `""`.
4. Install only when `cfg.AutoUpdateEnabled()` (config `auto_update`, absent
   = on; `JULIENNING_AUTO_UPDATE` parsed by `strconv.ParseBool` can only turn
   it off, and a non-boolean value is an error that keeps it off), `ActiveLink`
   succeeds (`NotManagedError` → silently check-only; other errors are
   returned), the active version is not a local build, and the latest release
   is newer than the active version (not the running one: a child of an old
   process must not reinstall what another process activated). Then
   `Install` (download, checksum, `version` run), `Activate` (atomic symlink
   swap), `Prune`.
5. Write `{checked_at: now, latest}`, keeping earlier `installed*` fields; after
   an activation also `installed: "<ver>", installed_at: now` and
   `notified: false`. Written even when step 4 failed, so a broken release or
   slow link is retried at the next daily check, not on every run. A prune
   failure after activation is returned but still records the install.

Running processes keep their binary (one file per version; `Prune` never
removes the running one); the next command runs the new version.

`selfupdate.Hint(w)` (cache only, no network, nothing for dev builds), called
by interactive commands (`accounts`, `next`, `use`, `current`, `configs`,
`setup`), prints at most one stderr line:

- `julienning updated to 0.3.0` when the running version equals `installed`
  and `notified` is false, then sets `notified` (re-read before writing; a
  failed write shows it once more);
- otherwise `julienning 0.3.0 is available (you have 0.2.1): julienning
  update` when `latest` is newer than the running version and `installed` is
  not at least `latest`: auto-update is off, the install is unmanaged, or the
  last attempt failed.

Never from `statusline` or hooks.

`make install` builds version `dev` into the same layout; auto-update never
replaces it.

Releases: goreleaser on `v*` tags; archives
`julienning_{{.Version}}_{{.Os}}_{{.Arch}}.tar.gz` + `checksums.txt`.

## Discovery (`internal/discover`)

`discover.Find()` returns candidate config dirs with their email (or
not-logged-in), deduplicated by cleaned absolute path:

1. the default dir `~/.claude` when it exists;
2. every top-level entry of `$HOME` (hidden or not, following symlinks to
   directories; on macOS the privacy-protected folders Desktop, Documents,
   Downloads, Library, Movies, Music, Pictures and Applications are skipped
   so discovery never triggers a permission dialog) that looks like a config
   dir: contains `.claude.json`, or
   contains `projects/` together with one of `settings.json`,
   `history.jsonl`, `sessions/`;
3. paths assigned to `CLAUDE_CONFIG_DIR=` in `~/.zshrc`, `~/.zprofile`,
   `~/.zshenv`, `~/.bashrc`, `~/.bash_profile`, `~/.profile` (aliases and
   exports; the whole shell word is parsed, joining adjacent quoted and
   unquoted parts; `~`, `$HOME`, `${HOME}` expanded) when the path exists;
   values that resolve to `/`, `$HOME` or a parent of `$HOME` are rejected;
4. the current `$CLAUDE_CONFIG_DIR` when it exists and is not `/`, `$HOME`
   or a parent of `$HOME` (`discover.IsUnsafeConfigDir`; `adopt` refuses the
   same paths).

`~/.julienning` is never a candidate. Results are sorted default dir first,
then by path. Each candidate carries its registered name, or the name it
would be registered under (Config names, no preferred group); setup re-runs
the assignment with the dirs it registers as the preferred group.

## Commands

All commands: errors to stderr as `julienning: <message>`, exit 1; bad usage
exits 2. Never print secrets or tokens. `--json` prints one JSON document.

### setup

`julienning setup [--dev NAME] [--remote-url URL] [--token TOKEN] [--name-prefix NAME] [--shell zsh|bash] [--no-rc] [--yes] [--share EMAIL]... [--nick EMAIL=NAME]...`

Idempotent. Steps, one status line each:

1. Identity: as before (prompt with `$USER` default when interactive; machine
   id generated once). `--name-prefix NAME` (validated up front, usage error
   otherwise) sets `name_prefix` and prints
   `Names:    NAME1, NAME2, … for newly registered dirs (updated|unchanged)`;
   only dirs registered afterwards are affected.
2. Remote: flags, else existing config, else prompt for URL then token (token
   read without echo via `golang.org/x/term`). Verify with `GET /healthz` and
   an authenticated `GET /accounts`; on 401 say the token is wrong. Save.
3. Allowlist: fetch `/accounts`, save `shared.json` (emails and nicknames).
4. Discover config dirs. Classify each:
   - email on the allowlist → include (register) automatically;
   - email in `declined_emails` → skip, listed as `personal (declined)`;
   - other email → ask `Share <email> (found in ~/.claude-x) with the team?
     [y/N]`. Yes → ask its nickname (see Nicknames), `Share` in the Worker,
     include. No → add to `declined_emails`. Non-interactive: only emails
     passed via `--share` are shared (nickname from `--nick` or the default);
     others are listed as `not shared (re-run setup in a terminal or pass
     --share EMAIL)`. A failed share is listed as `not shared (share failed:
     …)` and makes setup exit 1 after the remaining steps.
   - not logged in → listed; included only if already registered.
   - a registered dir with a pending share from `new-config`
     (`share_on_login`) whose email is not shared yet: offered as above,
     with the mark's nickname as the default (the prompt's, and `--share`'s
     when no `--nick` names it). Non-interactive runs leave it to the
     session hooks unless `--share` names it. Sharing, declining, or finding
     the email on the allowlist or in `declined_emails` clears the mark; a
     failed share keeps it. Rows while it is pending: `not logged in
     (registered as julienning3; shares on login)`, `not shared yet (shares
     on login; registered as julienning3)`.
   Then shared accounts logged in here whose Worker record has no nickname
   are named (Nicknames, "Legacy records"), and every `--nick` that was not
   used is explained on a `Note:` line (not logged in here, Worker
   unreachable, already called something else, or not shared in this run).
   Already-registered dirs stay registered even if their email changed; their
   status is reported (`now logged in as x@y (not shared; registered as
   julienning3)`). One row per dir: dir, nickname (`-` when the account has
   none or is not shared), email, status. Config names appear only in the
   status: `shared (registered as julienning1)`, `not logged in (registered
   as julienning3)`,
   ``missing (registered as NAME; run `julienning forget NAME`)``.
5. Patch settings.json of every registered dir (see below).
6. Shell hook line in the rc file (unchanged format and detection:
   `… # julienning-shell-hook`). Reports, never edits, hand-written
   `alias claude-<nick>=` lines for team nicknames (they hide the function),
   aliases that select a registered dir by path (the account's
   `claude-<nick>` function replaces them), and `alias claude=` lines.
7. Next steps.

### uninstall / forget

`julienning uninstall [--purge] [--yes]`: for every registered dir, remove what
julienning added to settings.json (restore the prior `statusLine` recorded in
`patches.json`, remove only julienning hook entries, drop empty containers
julienning created); remove the rc hook line (new and legacy markers);
release this machine's claims (best effort); clear `current`. `--purge` also
removes julienning's own files (known state files in `~/.julienning`, version
files named `dev` or semver in the versions dir, and the command symlink only
when it points into the versions dir), then each directory only if it is
empty; anything else is kept and listed. It asks unless `--yes`, and deletes
nothing (exit 1) when any unpatch failed or `config.json` is unreadable, so
the undo data survives for a retry. Prints what it changed. Safe to run
twice.

`julienning forget NAME`: unregister + unpatch that dir only. When another
registered dir's settings.json resolves to the same file (symlinked shared
settings), the unpatch is skipped and reported.

### settings.json patch (`internal/jsonedit`, `internal/claudecfg`)

Edits preserve the file's key order and every value byte-for-byte except the
keys julienning touches (`internal/jsonedit` splice editing), insert new
members with the file's detected indentation (2 spaces by default; compact
for single-line files), strip a BOM, follow symlinks, write atomically and only
when the content changes. Non-object files are reported and skipped.

julienning adds, using `C = paths.StableCommand()`:

```json
"statusLine": { "type": "command", "command": "C statusline" },
"hooks": {
  "SessionStart": [ { "hooks": [ { "type": "command", "command": "C hook session-start" } ] } ],
  "SessionEnd":   [ { "hooks": [ { "type": "command", "command": "C hook session-end" } ] } ]
}
```

- Ownership test: a statusLine or hook command is julienning's when its
  command ends with ` statusline`, ` hook session-start` or
  ` hook session-end` and its first word (shell-unquoted) either has the
  basename `julienning` or is a versions-dir file
  (`…/julienning/versions/<ver>`, `paths.IsVersionFile`). A prior statusLine
  that is itself julienning's is never recorded as the one to restore.
- Existing user hooks are preserved; julienning's entry is appended once and
  updated in place when the command path changes.
- The first time a non-julienning `statusLine` is replaced, its raw JSON is
  stored in `patches.json` so uninstall/forget can restore it.
- Report per dir: `added` / `updated` / `unchanged`.

### shell integration (`internal/shell`)

`julienning shell-init zsh|bash` prints, without touching the network:

- the `claude` wrapper: an exported `CLAUDE_CONFIG_DIR` wins; otherwise it
  reads `current` on every call and runs `CLAUDE_CONFIG_DIR=<dir> command
  claude`, or plain `command claude` with no selection, a missing dir, or the
  default dir (default-dir rule; the default dir's path is baked in at
  generation time);
- one `claude-<nick>` function per nickname in the cached allowlist (sorted,
  deduplicated, logged in here or not): `_jl_dir=$(command julienning
  resolve-dir '<nick>') || return 1`, then `env -u CLAUDE_CONFIG_DIR claude
  "$@"` when that is the default dir, else `CLAUDE_CONFIG_DIR="$_jl_dir"
  command claude "$@"`. Resolving per call means a re-login never leaves a
  stale function. It does not change `current`. A nickname failing the rule
  is skipped with a warning (nicknames become function names and cannot be
  quoted). Dir-name aliases no longer exist.

Before setup has run it prints nothing and exits 0. Every function is
defined as `function name { … }` (not `name() {`), so an existing alias of
the same name in the rc file cannot break the eval; setup reports such alias
lines (see setup step 6).

`julienning resolve-dir NICKNAME|EMAIL|CONFIG` (hidden): `resolve.Target`
with the cached allowlist, no network; prints only the cleaned dir path on
stdout (compared byte for byte with the baked-in default dir), or the error
on stderr and exit 1, which makes the function return 1.

### new-config / adopt / rename / login / configs

- `new-config [--name NAME] [--copy-settings-from NAME] [--nick NICK] [--no-share] [--no-login]`:
  without `--name`, creates `~/.claude-<prefix><N>` named `<prefix><N>` (dir
  and name share N: the smallest N ≥ 1 whose name is not registered and whose
  dir does not exist, up to 999). `--name foo` (or `.claude-foo`) →
  `~/.claude-foo` named `foo`. After creating, registering and patching it
  starts claude in the new dir (`launch.Exec`) so the user can sign in;
  `--no-login` (scripts) stops after registering. `--login` is accepted as a
  no-op for compatibility. Otherwise as v1.
- **new-config is for team accounts: the account signed in is shared
  automatically.** Unless `--no-share`, before creating anything the
  nickname is settled: `--nick` (validated; usage error when invalid or
  combined with `--no-share`; an error when the cached allowlist shows it
  held by another email), else in a terminal the prompt from Nicknames, else
  the default. It is stored on the registered dir as
  `share_on_login: {nickname}` (`config.ShareOnLogin`, `""` = the email's
  local part). The share happens on login, in claim reconciliation step 0
  (Claims from sessions), and `setup` offers the dir with that nickname.
  `--no-share` registers the dir without a mark (setup offers it as usual).
- `adopt DIR [--name NAME]`: as v1 (register + patch); the name follows
  Config names unless `--name` is given.
- `new-config` prints `Created ~/.claude-julienning3 (config
  "julienning3").`, then `Starting claude in it so you can sign in.` and
  `The account you sign in with will be shared with the team as delta;
  julienning does that automatically when your first session starts.`
  (`as its email name` without a nickname; with `--no-share`: `This account
  stays personal (not shared).`) before the exec; when the exec fails
  (claude not on PATH) the dir stays registered and the error ends with
  `; ~/.claude-julienning3 is created, sign in later with: julienning login
  julienning3`. With `--no-login` the second line is `Sign in: julienning
  login julienning3` instead (no alias line). `adopt` prints `Adopted ~/.claude-x as
  config "julienning4" (settings.json added).`.
- `rename OLD NEW`: renames the internal label only (`nick` renames an
  account). NEW must be a valid config name not used by any registered dir;
  only `config.json` changes (`current` stores the dir path, shell functions
  are named after nicknames, settings.json never names the config). Prints
  `Renamed "julienning1" to "work" (~/.claude-julienning1).`
- `login NICKNAME|EMAIL|CONFIG [-- args]`: `resolve.Target` (a fresh dir has
  only its config name; a shared account with no local login is the
  `NotLocalError`), then `launch.Exec`.
- `configs [--json]`: `*` current, NAME, NICK (nickname of the dir's current
  login when shared, else `-`), DIR (home-shortened), EMAIL (`(not logged
  in)` / `(unreadable)`), SHARED (`yes` / `no` / `?` when the cache is
  empty / `no (shares on login)` for a dir with a pending share whose login
  is not shared yet; its NICK stays `-` until the share lands). JSON:
  `{"configs": [{name, dir, email, nickname, logged_in, shared, current,
  default, share_on_login, email_error?}]}` (`shared` null when the cache is
  empty; `share_on_login` true exactly when SHARED says `shares on login`).

### share / unshare / nick

`julienning share EMAIL [--nick NAME]` adds to the allowlist under a nickname
(Nicknames); prints `Shared x@y with the team as alpha.`, then `Logged in
here as: julienning1.` (config names) or
``No registered dir here is logged in as it; `julienning setup` registers one once it is.``.
`julienning unshare
NICKNAME|EMAIL|CONFIG [--yes]` removes the account (asks `Unshare alpha
(x@y)? This deletes its usage and claims for the whole team. [y/N]` unless
`--yes`, refuses without a terminal; removes its usage and claims) and prints
`Unshared alpha (x@y); its usage and claims were removed from the Worker.`.
`julienning nick NICKNAME|EMAIL|CONFIG NEW` renames (Nicknames). `share`
and `unshare` refresh `shared.json` and then apply their own change on top
(`sharedcache.AddWithNickname` / `Remove`, keeping `fetched_at`), because KV
listings lag writes; `nick` and setup apply theirs the same way.

### accounts

Columns `#  NICK  ACCOUNT  SESSION  WEEK  STATE  LOCAL  UPDATED`. As v1
with: NICK is the team nickname (`-` for legacy records); allowlisted
accounts that never reported show `-` windows; STATE shows every other holder
(`in use by ali, can (12m)`); LOCAL lists the registered dirs here logged
into that email as home-shortened paths, comma-separated, `*` on the
selected one (`*~/.claude-julienning1`), `-` for none. `--json`: the Worker
document (`nickname` included) plus `local_config` (the selected dir's config
name, else the first) and `local_configs` (config names) per account. Before
listing, run claim reconciliation (below) synchronously and refresh
`shared.json`.

### next / use (`internal/cli/switching`, `internal/sessions`, `internal/tui`)

`julienning next [--all] [--limit N] [--no-launch] [--json]`
`julienning use [TARGET] [--all] [--limit N] [--no-launch] [--json]` (TARGET:
a nickname, email or config name, see Nicknames; no TARGET: the current
selection without re-ranking; nothing selected → same as `next`)
`julienning use --clear`

1. Target: `next` ranks via the Worker and picks the first shared account
   with a registered, logged-in local dir (prefer the current dir on ties);
   `use` refreshes the allowlist from the Worker when it can (falling back to
   `shared.json` with a warning), then resolves TARGET (`resolve.Target`; a
   `NotLocalError` or unknown target is the error). A dir reached by config
   name may be not logged in or not shared — then warn
   (`julienning3 is not logged in yet; run: julienning login julienning3`),
   skip the session list's move/merge and launch plain.
2. Set `current` to the target.
3. Launch mode (stdin and stdout are terminals, not `--no-launch`/`--json`):
   list sessions (below) with the picker; `New session` is the first row and
   the default.
   - No sessions to show → start a new session directly.
   - New session → `launch.Exec(target, [])`.
   - Session already in the target dir → `launch.Exec(target, ["--resume", id], cwd)`.
   - Session in another dir → confirm (`tui.Confirm`), then move it (below)
     and resume as above. The ends are labelled like the move report
     (nicknames, else config names; the home-shortened dir paths when both
     are one account):
     `Move "Fix flaky clock tests" from beta to alpha?` /
     `  Its transcript, checkpoints, task list and session environment move to alpha; beta will no longer have it. Project memory is merged, never overwritten.` /
     `  Enter to continue · n or Esc to go back`.
     Enter or `y`/`Y` → move and resume. `n`/`N`, Esc, Ctrl-C (also Ctrl-D,
     end of input) → back to the picker with the same list (not re-read) and
     the cursor on that row (`tui.Options.Initial`); nothing moved, nothing
     printed. New session and sessions already in the target never ask.
   - Esc / Ctrl-C in the picker → exit 0 without launching (selection
     already switched).
4. Otherwise print the summary line and, with `--json` instead, the
   Worker's account object plus `config` (config name) and `nickname` (null
   when none); for an account the Worker did not list, `{config, email,
   nickname}`. Summary line (`summaryLine`), with a nickname:
   `Now using alpha (claude1@x.io) in ~/.claude-x — session 12% → 17:30, week 28% → Tue 21:00.`
   / `Staying on alpha (claude1@x.io) in ~/.claude-x (best available) — …`;
   without one, the config name in the v1 form:
   `Now using julienning3 (claude3@x.io) — …` /
   `Staying on julienning3 (best available) — …` (not logged in:
   `Now using julienning3.`). The usage part is `no usage reported yet` when
   the account never reported and absent when the Worker did not list it.
   The same line heads the picker; the line under it says what is listed
   (`scopeLine`): `100 most recent sessions in ~/Code/shop · type to filter`
   when the limit was reached, `3 sessions in ~/Code/shop · type to filter`
   otherwise (`1 session`), without `in <dir>` under `--all`; while a filter
   is typed it reads `Filter: <text> · <matches> of <total>` (the count is
   dropped when the terminal is too narrow for both). The fallback prompt
   prints the scope line, without `· type to filter`, under the header.
   The busy warning names the account:
   `alpha (claude1@x.io) is also in use by ali, can`. Picker cancel prints
   `Not starting claude; alpha stays selected.`.

`use --clear` removes `current` (replaces the removed `release`). `use` and
`next` no longer claim; claims come from sessions.

#### Session listing (`sessions.List`)

Sources: every registered dir (any email) — the project folder for the
current working directory, or all project folders with `--all`. Folder name:
`cwd` with every non-alphanumeric character replaced by `-`; when that
exceeds 200 characters Claude truncates and appends a hash, so match folders
by the 200-character prefix and confirm with the `cwd` field inside the
session. Candidates: `*.jsonl` directly in the folder (not `agent-*` files),
sorted by mtime, newest `limit` (default 100, `--limit` max 500) across all dirs.

Per session (head read ≤ 256 KB, tail read ≤ 256 KB — never the whole file):

- `id` (file name), `config` (dir name), `cwd` (from entries), `last_active`
  (last `timestamp` in the tail, else mtime), `size`.
- `title`: `projects/<enc>/<id>/custom-title.json` `customTitle`, else last
  `custom-title` entry, else last `ai-title` (`aiTitle`), else legacy
  `summary`, else empty.
- `first_prompt`: the first `type=user` entry with `isSidechain` false,
  `isMeta` not true, `origin` null/absent, whose content is a string (or the
  first `text` block of an array) that does not start with `<` and is not a
  tool result; whitespace collapsed.
- `live`: the id appears in a live `sessions/<pid>.json` of any registered
  dir (`livesess`), with that dir recorded.
- Sessions with neither title nor first prompt are skipped.

Picker rows: title (or first prompt when no title), account label (the
nickname of the session's dir's current login, else its config name;
`resolve.Label`), relative last active (`2h ago`, absolute local time for
> 6 days), short id (8 chars);
second line: first prompt (leading `>` quoting and code fences stripped),
truncated to the terminal width. Live sessions are marked
`(open in another terminal)` and cannot be selected for resume/move.

#### Moving a session (`sessions.Move`)

Refuse when the session is live, when the source has no registry and the
jsonl was modified in the last 2 minutes (warn: may be open), or when the
target already has that id. Items (relative to the config dir, `<enc>` taken
from the source path, same in the target):

- `projects/<enc>/<id>.jsonl`
- `projects/<enc>/<id>/` (subagents, tool-results, custom-title.json)
- `file-history/<id>/` (checkpoints)
- `session-env/<id>/`
- `tasks/<id>/` (Claude's per-session task list)
- `todos/<id>-*.json` and `debug/<id>.txt` when present
- (`plans/<slug>.md` is not moved: its slug is not derivable from the id.)

Copy into the target under temporary names in the same directories, preserving
modes and mtimes; fsync; rename into place; verify sizes; then delete the
source items. A failure before the renames leaves the source untouched and
removes the temporaries.

Project memory merge: Claude keys auto-memory by the canonical git root,
not the session cwd (worktrees and subdirectories share one memory dir), so
the folder is `projects/<enc(git root of cwd)>/memory/`, where the git root
is the main worktree's top level (resolve `git rev-parse --git-common-dir`
without running git: follow `.git` files/`commondir`), falling back to the
cwd when it is not in a repo. Merge the source's folder into the target's: copy files the target lacks; never overwrite; for `MEMORY.md`
append index lines the target lacks, except lines linking to a file that
conflicted or is already linked in the target; follow symlinks when writing
MEMORY.md; report conflicting files (kept as-is).

Report: `Moved "<title>" from beta to alpha (memory: 2 files added, 1 index
line merged, 1 conflict kept).` The ends are labelled like picker rows; when
both labels are equal (two dirs logged into one account) they are the
home-shortened dir paths instead (`from ~/.claude-a to ~/.claude-b`).
Refusals and interruptions name dirs by config name (`move of session
3f2a9c1b to julienning1 interrupted by Ctrl-C (nothing was changed)`).
Identical files are not
conflicts; MEMORY.md lines compare with surrounding whitespace trimmed.
After the verified copy, failures to delete the source or merge memory are
warnings, not errors.

#### Picker (`internal/tui`)

`golang.org/x/term` raw mode on /dev/tty, alternate screen: ↑/↓, Ctrl-N/P,
PgUp/PgDn, Home/End, k/j (only until a filter is typed; `/` starts a filter
explicitly), Enter, typing filters by title/prompt/account label/id/dir
(case-insensitive),
Backspace, Ctrl-U clears the filter, Esc/Ctrl-C cancel. Rows for sessions
whose jsonl changed in the last 2 minutes without registry evidence show
`(may be open)`; `--all` rows also show the session's directory.
Selection shown with a `>` marker plus reverse video on its first line (color
is never the only signal; honors `NO_COLOR`). Titles are normal; metadata,
prompt lines, the filter and help lines, and unavailable or `(may be open)`
rows are dim (SGR 2). Lines fit in width-1 columns, the title giving way
before the metadata. Falls back to a numbered prompt when raw mode is
unavailable. Always restores the terminal (defer + signal handling).

Confirm (`tui.Confirm`): the same raw-mode terminal handling and restore
(`openRaw`) and the alternate screen, so nothing stays on screen whichever
way it is answered. Single keys, no Enter needed for `y`/`n`; other keys are
ignored. An accept (Enter/`y`) within 300 ms of the prompt opening is
ignored too, so a repeated or auto-repeated Enter from the picker cannot
confirm a move; declines are honoured at once. The question and explanation are normal text, wrapped (never cut)
to width-1; the key line is dim (plain under `NO_COLOR`). It reads the
terminal synchronously and takes a lone ESC as Esc at once: the picker's
timed ESC wait leaves a read in flight, which on macOS (where `/dev/tty` is
not pollable) cannot be abandoned and would swallow the reopened picker's
first key. Without raw mode: the question and explanation as plain lines,
then `Continue? [Y/n] ` (empty, `y`, `yes` → yes; `n`, `no`, end of input →
no; anything else → `Answer y or n.` and ask again), reading one byte at a
time so the picker fallback that follows gets the rest of the input.

### Claims from sessions (`internal/claims`, hidden commands)

settings.json hooks call `julienning hook session-start|session-end`. The
hook reads its stdin JSON (`session_id`, `cwd`, `source`/`reason`; unknown
fields ignored), resolves the config dir (`claudecfg.ActiveDir`), and exits 0
immediately after spawning a detached `julienning claim-sync` (Setsid,
stdio to /dev/null). Hooks never print, never fail, never touch the network.
`session-start` does nothing when julienning is not set up, the dir is not
registered, the dir is not logged in, or the email is not in `shared.json`
and the dir has no pending share (`share_on_login`; for such a dir an
unreadable `shared.json` does not stop it either). `session-end` only
requires a registered dir (the login may be gone after `/logout`); when the
input lacks a session id, the ending session is identified by the registry
entry whose pid is the hook's parent or grandparent. Only entries for which
`livesess.CountsAsSession` is true (interactive/bg, not spare, not daemons)
count as live sessions for claims.

`claim-sync [--starting SESSION_ID] [--ending SESSION_ID | --ending-pid PID]` (detached, network
allowed; `--starting` waits up to 2 s for the new session to appear in the
registry). A SessionEnd with reason `clear` spawns nothing, because the
process continues under a new session.

0. Pending shares (`claims.ResolvePendingShares`): for every registered dir
   with `share_on_login`, read its email. Not logged in → skip (retried
   next run). On the allowlist (`shared.json`, else one `ListAccounts` per
   run, which is saved as `shared.json` so step 3 does not replace it with a
   listing that lags the share) → clear the mark. In `declined_emails` →
   clear the mark, never share, report it. Otherwise `Share(email, nickname
   or config.DefaultNickname(email), identity)`; success →
   `sharedcache.AddWithNickname`, clear the mark. 409 → keep the mark and
   report `config julienning3: nickname taken: "delta" belongs to another
   team account; pick another with: julienning setup`; any other failure →
   keep the mark, report, retry next run. Marks are cleared with
   `config.UpdateDir` (re-read config.json, change that dir, save) so a
   long-running process never undoes a concurrent config change. Messages
   name config dirs and nicknames, never emails; claim-sync and the
   send-usage child log them as `SHARE_FAILED` (throttled like the other
   background codes), `accounts`/`next`/`use` print them as warnings.
1. For every shared email logged in on this machine's registered dirs, count
   live sessions (`livesess.List` over all registered dirs logged into that
   email, excluding `--ending`).
2. Live > 0 → `PutClaim`; record in `claims.json`.
   Live = 0 and the email is in `claims.json` → `DeleteClaim`; remove it —
   unless some registered dir logged into that email has no registry or an
   unreadable account file, in which case nothing is released this run and
   the Worker's claim TTL is the backstop. A fresh claim (< 1 h) is not
   re-PUT; usage reports refresh it.
3. Refresh `shared.json` when stale.
4. One run per machine at a time (O_EXCL lock in `~/.julienning`, stale after
   60 s); errors go to `errors.log` (throttled).
5. After releasing the lock, the daily update check / auto-update
   (`selfupdate.AutoUpdateIfDue`, see Install layout and updates). A run that
   did not get the lock leaves it to the holder.

The same reconciliation (step 0 included, through `claims.Sync`) runs
synchronously in `accounts`/`next`/`use` (without spawning) and in the
send-usage child at most every 10 minutes, which cleans up after crashed
sessions whose SessionEnd never ran. A pending share usually lands at the
first SessionStart after the login, at the latest at that session's
SessionEnd (which needs only a registered dir).

### statusline / send-usage

As v1, plus: the gate is "config dir registered AND email in `shared.json`"
(no network); a pending share does not open it, so a new-config dir starts
reporting once its share has landed in `shared.json`. The config dir is resolved via `claudecfg.ActiveDir` and the
account file via `claudecfg.AccountFileForEnv` (an explicitly exported
`CLAUDE_CONFIG_DIR=~/.claude` reads `~/.claude/.claude.json`). Every
status-line error code is log-throttled. The
send-usage child refreshes `shared.json` when stale and runs claim
reconciliation at most every 10 minutes; when it did either, it then runs
the daily update check / auto-update outside the claim-sync lock. A 404 `account is not shared` from
the Worker removes the email from `shared.json`.

## Worker

TypeScript, wrangler v4, KV binding `USAGE`. Vars: `DEFAULT_TZ` (default
`UTC`), `CLAIM_TTL_MIN` (default `720`, safety net only), `ACTIVITY_TTL_MIN`
(15). Secret `AUTH_TOKEN` (never committed; `.dev.vars` is gitignored, see
`.dev.vars.example`; tests inject their own binding).

Auth unchanged (Bearer; `?token=` for GET only).

Storage (race-free by construction: every writer owns its own key; all data
lives in KV **metadata** so one `list()` call returns everything and the
listing needs no per-key reads):

| Key | Written by | Metadata |
|-----|-----------|----------|
| `share:<email>` | share / nickname rename / unshare only | `{added_by, added_at, nickname}` (nickname absent on records from before nicknames; read as null) |
| `usage:<email>` | PUT usage (only if `share:` exists) | `{session, week, collected_at, reporter}`; `expirationTtl` 8 days |
| `claim:<email>:<dev>:<machine_id>` | PUT/DELETE claim of that holder; usage refresh by the same reporter when the claim is older than 1 h (KV write budget) | `{at}`; `expirationTtl` = CLAIM_TTL_MIN (min 60 s) |

An email is shared iff `share:<email>` exists. The listing shows only shared
emails; orphaned `usage:`/`claim:` keys (written by a request that raced an
unshare) are ignored and expire on their own. Unshare deletes the share key,
the usage key and every claim key under `claim:<email>:`. Each key's metadata
is validated independently: an invalid claim key drops only that holder.
`collected_at` later than the Worker's clock is clamped to now before storing
and comparing. The JSON response shape per account (below) is unchanged.

Logical account view (as returned):

```json
{
  "email": "claude1@example.com",
  "nickname": "claude1",
  "added_by": { "dev": "murat", "machine_id": "3fa9c2d1e07b" },
  "added_at": "2026-09-29T10:00:00Z",
  "session": { "used": 50, "resets_at": "…" },
  "week": { "used": 28, "resets_at": "…" },
  "collected_at": "…",
  "reporter": { "dev": "murat", "machine_id": "…" },
  "claims": [ { "dev": "murat", "machine_id": "…", "at": "…" } ]
}
```

Routes:

| Method | Path | Body / query | Response |
|--------|------|--------------|----------|
| GET | `/healthz` | – | `200 ok` |
| GET | `/accounts` | `format`, `tz`, `dev` | ranked allowlist |
| GET | `/accounts/:email` | `dev` | one account; 404 `account is not shared` |
| PUT | `/accounts/:email` | `{nickname, added_by}` | 201 created / 204 existed (share; nickname required on create, else 400 `nickname is required to share a new account`; ignored when the email exists; 409 `nickname is taken` with `nickname` and `by`) |
| PUT | `/accounts/:email/nickname` | `{nickname}` | 204 (rename; 404 not shared; 409 taken) |
| DELETE | `/accounts/:email` | – | 204 (unshare; idempotent) |
| PUT | `/accounts/:email/usage` | as v1 | 204; 404 `account is not shared`; stale `collected_at` ignored; refreshes `at` of the claim held by the same reporter dev+machine |
| PUT | `/accounts/:email/claim` | `{dev, machine_id}` | 204 upsert own holder; 404 when not shared |
| DELETE | `/accounts/:email/claim` | `?dev=&machine_id=` (both required) | 204 remove own holder (idempotent) |

Ranking and state: holders with `at` older than `CLAIM_TTL_MIN` are ignored.
`busy_by` = sorted unique devs ≠ querying `dev` that hold a fresh claim or
reported within `ACTIVITY_TTL_MIN` (all such devs when no `dev` query).
`state`: `in_use` when another dev reported within the activity TTL,
`claimed` when only fresh claims by others exist, else `free`. Sort: not busy
first → known usage before unknown → effective session asc → effective week
asc → session reset asc → email asc. JSON: `claims` always an array,
`busy_by` always an array. `nickname` is always present (null for legacy
records). Text columns: `#  NICK  ACCOUNT  SESSION  WEEK  STATE  UPDATED`
(NICK `-` when null; the CLI adds LOCAL). Text STATE: `free`,
`in use by ali (12m)`, `claimed by ali, can`.

Other v1 rules (validation, body limit, timestamps, text format, corrupt and
non-email keys skipped) stay.

## Repo plumbing

- Module `github.com/muratgozel/julienning`, Go 1.25 (toolchain local),
  dependency `golang.org/x/term` only.
- CI: gofmt, vet, `go test -race`, worker typecheck + tests.
- Release: goreleaser on tags. Deploy-worker workflow unchanged.
- No secrets in the repo: team token is distributed out of band.

## Non-goals

- No handling of Claude auth tokens or the `sessions/*.key` files.
- No Windows, no fish.
- No usage history; KV holds the latest snapshot.
