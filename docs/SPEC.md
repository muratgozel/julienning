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
- **personal account**: any email not on the allowlist (`resolve.IsPersonal`;
  with no allowlist known, i.e. `shared.json` missing, unreadable or never
  fetched, only an email no registered dir is logged into). Its dir is found by
  `use EMAIL` (or a pin) wherever it is logged in, registered or not, and is
  never registered by that, never reports, never claims and never feeds the
  team picker (see "Personal accounts").
- **pin**: a directory tied to an account on this machine
  (`~/.julienning/pins.json`): plain `claude` and `julienning use` without a
  target open that account in the directory and below it (see "pin /
  unpin").
- **host**: another machine reachable over `ssh` that runs its own
  julienning; commands take `--host NAME`, and `transfer` moves sessions
  between machines (see "Hosts").

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
  decided at share time; an invalid answer is explained and asked again), or
  takes `--nick NICK`; the share itself happens on login (see new-config). A
  nickname the cached allowlist shows held by a team account names that
  account instead: new-config signs it in (see new-config). Sharing an
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
   `alpha (claude1@x.io) is shared but not logged in on this machine; sign it in here with: julienning login alpha`
   (without a nickname:
   `no registered config dir on this machine is logged in as x@y; sign it in here with: julienning login x@y`);
   when a registered dir without a login held it last (`last_email`), the
   error says so instead:
   `alpha (claude1@x.io) is signed out on this machine (~/.claude-x held it last and has no login now); sign in again with: julienning login alpha`;
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
| `config.json` | config | identity, remote, registered dirs (with pending shares from `new-config`), declined emails, remembered personal dirs, name prefix, tmux mode |
| `current` | config | absolute path of the selected config dir |
| `pins.json` | pins | `{version: 1, pins: [{dir, email}]}` sorted by dir; removed when the last pin goes (see "pin / unpin") |
| `hosts.json` | hosts | `{version: 1, hosts: [...]}` sorted by name, 0600 (see below and "Hosts") |
| `ssh/` | sshx | ssh control sockets, one per host, named by ssh's `%C` hash (0700) |
| `received/` | sessions | receipts of sessions this machine received by `transfer`, `<session id>.json` each, pruned after 30 days (see "Moving a session between machines") |
| `shared.json` | sharedcache | allowlist snapshot `{fetched_at, emails, nicknames}` (`nicknames`: email → nickname, omitted when none) |
| `claims.json` | claims | emails this machine currently holds a claim on |
| `patches.json` | claudecfg | per settings.json path: prior `statusLine` value for restore |
| `update-check.json` | selfupdate | `{checked_at, latest, installed?, installed_at?, notified?}` (the `installed*` fields: last background auto-update; absent `notified` = false) |
| `update.lock` | selfupdate | auto-update lock (O_EXCL, stale after 10 min) |
| `errors.log` | usage | diagnostics, never contains emails |
| `sent/` | usage | send-usage debounce cache + in-flight markers, named by a short hash of the email (no addresses in file names) |
| `probes/` | usageprobe | the last usage probe of each registered dir, `<hash of the dir path>.json`: `{dir, email, outcome, probed_at, session?, week?}` (outcome `ok`, `signed_out` or `failed`; the windows only with `ok`); the cache window, the numbers re-sent while the Worker's listing lags, and the signed-out mark (see "Usage probes") |
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
    { "name": "julienning1", "dir": "/Users/murat/.claude-julienning1",
      "last_email": "claude1@x.io" },
    { "name": "julienning2", "dir": "/Users/murat/.claude-julienning2",
      "share_on_login": { "nickname": "delta" } }
  ],
  "declined_emails": ["me@personal.com"],
  "personal_dirs": [{ "email": "me@personal.com", "dir": "/Users/murat/.claude" }],
  "send_min_interval_sec": 300,
  "name_prefix": "julienning",
  "auto_update": false,
  "tmux": { "enabled": true, "session": "claude" }
}
```

`share_on_login` is optional per dir: `new-config`'s pending share of the
dir's first login (see new-config); `nickname` absent = the email's local
part, decided at share time. It must pass the nickname rule (checked on load
and save). It is removed once the share lands, the email turns out to be
shared already, or the email is declined; `forget` drops it with the dir.
`last_email` is optional per dir: the last login julienning saw there
(lowercased, `config.RecordLogins`; recorded by every usage refresh, which
reads every registered dir's login, and by `login`). It outlives the login
on purpose: when Claude Code signs a dir out it can clear `oauthAccount`,
and `last_email` is then all that still ties the dir (and its sessions) to
the account. It is never read as the current login. `accounts` lists such a
dir under that account as signed out, `login NICKNAME` signs in there again,
and the `NotLocalError` names it. When a refresh finds a dir logged into
another account than recorded, it records the new one and prints
`note: ~/.claude-sixtynine was logged into claude2 (claude2@x.io) and is now logged into claude9 (claude9@x.io); the sessions in it stay there, under claude9`
once. A julienning that predates the field drops it when it saves
config.json (it is only a hint).
`personal_dirs` is optional: where `use EMAIL` or a pin last found each
personal account (lowercased email, absolute clean dir, one entry per email;
checked on load and save). It is a shortcut past discovery, never trusted
blindly (see "Personal accounts"). `config.RememberPersonalDir` re-reads
config.json before saving, like `UpdateDir`, because it also runs from the
claude wrapper.
`name_prefix` is optional (absent = `julienning`, `config.Prefix()`).
`auto_update` is optional (absent = on; `cfg.AutoUpdateEnabled()`, which also
honours `JULIENNING_AUTO_UPDATE`).
`tmux` is optional: tmux mode (see "tmux mode"); absent = never decided,
`enabled` false = off. `session` is required while enabled and must match
`^[A-Za-z0-9_-]{1,32}$` (tmux reserves `:` and `.` in targets; checked on load
and save; `cfg.TmuxSession()`).

`hosts.json` (`internal/hosts`), the other machines this one reaches (see
"Hosts"):

```json
{
  "version": 1,
  "hosts": [
    {
      "name": "box",
      "ssh": "devbox.example.com",
      "machine_id": "9be1c0a4d2f3",
      "home": "/home/dev",
      "os": "linux",
      "arch": "amd64",
      "paths": {
        "julienning": "/home/dev/.local/bin/julienning",
        "claude": "/home/dev/.local/bin/claude",
        "tmux": "/usr/bin/tmux"
      },
      "added_at": "2026-10-18T09:00:00Z",
      "checked_at": "2026-10-18T09:00:00Z"
    }
  ]
}
```

- `ssh` is the destination handed to ssh (an `~/.ssh/config` alias or
  `user@host`): non-empty, no whitespace or control characters, never starting
  with `-` (ssh would read it as an option such as `-oProxyCommand=…`), and
  always passed after `--`.
- `machine_id` is the host's own (its `config.json`). Every call checks it
  first (agent `hello`): an alias that now reaches another machine is refused,
  never acted on (a mistyped name can resolve to a router).
- `paths` are absolute paths on the host, found when it was added. A
  non-interactive ssh command gets a minimal PATH (Debian:
  `/usr/local/bin:/usr/bin:/bin:/usr/games`) and `.bashrc` usually returns
  early for it, so julienning, claude and tmux are never looked up through
  PATH there. `claude` and `tmux` may be null. `julienning` must not contain
  `'` or `\` (it goes on the remote command line in single quotes).
- `home` is the host's `$HOME`. Timestamps are UTC. Unknown fields are kept on
  save, so an older julienning never drops what a newer one added.
- Validated on load and save (`hosts.Host.Validate`): names and machine
  ids unique, `os` darwin or linux, `arch` amd64 or arm64.

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

`julienning setup [--dev NAME] [--remote-url URL] [--token TOKEN] [--name-prefix NAME] [--shell zsh|bash] [--no-rc] [--tmux SESSION | --no-tmux] [--yes] [--share EMAIL]... [--nick EMAIL=NAME]...`

Idempotent. Steps, one status line each:

1. Identity: as before (prompt with `$USER` default when interactive; machine
   id generated once). On a machine already set up, a `--dev` that differs
   renames through the steps of `dev NEW` (see "dev"), so the claims held
   under the old name move to the new one: `Identity: dev=murat
   machine=3fa9c2d1e07b (renamed from dev)`. `--name-prefix NAME` (validated up front, usage error
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
6. Shell hook line in the rc file, found by its `# julienning-shell-hook`
   marker. It calls julienning by its absolute path, so it does not depend on
   when a distribution's startup files put `~/.local/bin` on PATH (Debian's
   `~/.profile` adds it after sourcing `~/.bashrc`):
   `[ -x '/home/dev/.local/bin/julienning' ] && eval "$('/home/dev/.local/bin/julienning' shell-init bash)"  # julienning-shell-hook`
   (`paths.StableCommand`, single-quoted; when it is not stable, as for a
   build run from the repo, `command -v julienning >/dev/null 2>&1 && eval
   "$(julienning shell-init bash)"  # julienning-shell-hook`). An existing
   julienning line that differs is rewritten in place (`Shell:    ~/.bashrc
   hook line updated`) and never moved; uninstall removes it by its marker.
   Reports, never edits, hand-written
   `alias claude-<nick>=` lines for team nicknames (they hide the function),
   aliases that select a registered dir by path (the account's
   `claude-<nick>` function replaces them), `alias claude=` lines, and
   `claude` functions (`claude() {`, `claude ()`, `function claude`, not
   commented out): one defined before julienning's line is noted
   (`note: ~/.bashrc:128 defines a claude function before julienning's line;
   julienning's definition wins`), one after it is warned about
   (`~/.bashrc:150 defines a claude function after julienning's line, so plain
   claude skips julienning (selection, pins, tmux mode); move the line ending
   in # julienning-shell-hook below it`).
7. tmux mode (see "tmux mode").
8. Next steps.

### uninstall / forget

`julienning uninstall [--purge] [--yes] [--move-sessions]`: first, the
sessions. Without the wrapper plain `claude` opens only Claude's default dir
(`claudecfg.DefaultDir`), so uninstall lists every session of every
registered dir except that one (`sessions.List` with `Every`: all project
folders, untitled sessions too, no limit; a dir registered twice through a
symlink counts once) and, when there are any, prints `Sessions: N sessions in
M registered config dirs. Without julienning, plain claude opens ~/.claude
(<account>) and cannot resume them.` (account: `signed in as <email>`, `not
signed in yet; claude asks you to log in` or `its login cannot be read`),
`~/.claude does not exist yet; moving creates it.` when missing, and the
sessions open in a terminal (live registry), which stay unless exited first.
Then it asks `Move them into ~/.claude? Project memory is merged, never
overwritten. [Y/n]` (empty, y or yes moves; n or no keeps; end of input
keeps; anything else asks again). `--move-sessions` moves without asking;
`--yes` confirms only the purge and never moves sessions. Without a terminal
and without `--move-sessions` nothing is asked and the sessions stay; they are
then only counted (`sessions.Count`, no file opened), since listing reads
every transcript's head and tail. An
explicit `--move-sessions` is always answered: `config.json` unreadable is a
problem (exit 1, `cannot move sessions: the registered dirs are listed in
config.json, which could not be read ...`), and with no session in the
registered dirs it prints `Sessions: nothing to move; no registered config
dir holds a session.` Every
question, the purge one included, comes before any change.

The moves come first, so a Ctrl-C (held for the whole loop, and inside each
`sessions.Move`) stops the uninstall before anything else changes; running it
again moves the rest. The default dir is created 0700 when missing. Each
session goes through `sessions.Move` (copy, verify, delete the source) with
its live flag cleared, since Move re-reads both registries and a session
exited while the question waited must move. Refusals (open, changed in the
last 2 minutes without a registry, id already in the target) keep that
session and go on; so does any other error of one session (it changed during
the copy, its dir's registry is unreadable), which also counts as a problem.
A session deleted since the listing (`sessions.ErrGone`) is skipped. Only
ENOSPC, EDQUOT and EROFS, which every later move would hit too, and a target
that is not a directory stop the moves. After the moves, every `projects/*/memory/` of
each source is merged into the same folder of the target
(`sessions.MergeAllMemory`, never overwriting), so the memory of projects with
no session moved (all open, or transcripts already cleaned up by Claude) is
not left behind; with no session moved the line is `Sessions: merged project
memory into ~/.claude (…)`. A memory conflict is warned once per source dir,
memory folder and file, naming where the other version stays. Output: `Sessions: moving N
sessions from <dir>…` per dir, `Sessions: moved N sessions into ~/.claude
(memory: …)`, and for what stays `Sessions: N stay where they are; resume one
with CLAUDE_CONFIG_DIR set to its dir:` followed by up to 10 entries of
`"<label>" (<why>):` and `CLAUDE_CONFIG_DIR=<dir> claude --resume <id>` (the
dir `~`-shortened when it needs no quoting, else single-quoted; why is the
refusal, the error, or `not tried: the moves stopped`), then `… and N more in
<dir> (n), …`. Declined or
not asked: `Sessions: kept N sessions in the registered config dirs[ (no
terminal to ask, and --move-sessions was not passed)]; resume one with
CLAUDE_CONFIG_DIR=<its dir> claude --resume <id>[, or move them all with
`julienning uninstall --move-sessions`].` (the last part only without
`--purge`).

Then, for every registered dir, remove what julienning added to
settings.json (restore the prior `statusLine` recorded in `patches.json`,
remove only julienning hook entries, drop empty containers julienning
created); remove the rc hook line (new and legacy markers); release this
machine's claims (best effort); clear `current`; delete `pins.json`
(`Pins:     removed N pins`), because a terminal opened before the uninstall
keeps the wrapper, which follows pins while the file exists. `--purge` also
removes julienning's own files (known state files in `~/.julienning`, among
them `hosts.json`, `sent/`, `probes/`, the `ssh/` control sockets and `received/`; no uninstall
here ever touches a host, `uninstall --host NAME` does; version
files named `dev` or semver in the versions dir, and the command symlink only
when it points into the versions dir), then each directory only if it is
empty; anything else is kept and listed. It asks unless `--yes`, and deletes
nothing (exit 1) when any unpatch failed, `config.json` is unreadable, or a
session could not be listed or moved for any reason but a refusal, so the
undo data, the list of dirs still holding sessions and the command survive
for a retry. Prints what it changed, then, when something changed and
nothing failed, `New terminals run claude natively: plain claude opens
~/.claude (<account>).`, and when it removed an rc hook line `Terminals
already open keep julienning's shell functions until you close them or run:
exec $SHELL` (`unset -f` would need the exact names, and zsh fails on any
not defined). Safe to run twice.

`julienning forget TARGET [--delete|--keep]`: TARGET is a nickname, email or
config name resolved by `resolve.Target` (current selection preferred), with
two forget-only rules: an email logged into more than one registered dir
(`resolve.Dirs`) is refused with each dir's config name and path, asking for
the config name; and when a nickname shadows a config name of the same
spelling, the config name wins wherever the nickname is ambiguous or not
local. A nickname/email no dir here is logged into fails with
`<nick (email)|email> is not logged in on this machine; nothing to forget`.
Then unregister + unpatch that dir only (unpatch first; a failure leaves it
registered and undeleted). When another registered dir's settings.json
resolves to the same file (symlinked shared settings), the unpatch is skipped
and reported. Output: `Forgot <label> (<email>, <dir>).` (or `(<dir>, not
logged in)`, `(<dir>, login unreadable)`; label = `resolve.Label`), the
settings.json line, the cleared-selection line, and for a shared account
``<nick|email> stays shared with the team; `julienning unshare <nick|email>`
removes it for everyone.`` Forget never unshares.

Then the dir itself: `Nothing to delete: <dir> does not exist.` when it is
gone; `--keep` → `Kept <dir>.`. Otherwise guards run before any question,
and any hit keeps the dir: Claude's default dir (`claudecfg.IsDefaultDir`,
also through symlinks), `discover.IsUnsafeConfigDir` (`/`, `$HOME`, a parent
of `$HOME`), not a directory, another registered dir inside it or with its
settings.json resolving into it, or any live entry in its `sessions/`
registry (`livesess.List`, all kinds; a read error also blocks). A guard hit
prints `Kept <dir>: <reason>.`, or, under `--delete`, is the error
`kept <dir>: <reason>` (exit 1, after the forget). With no guard hit,
`--delete` deletes; else a terminal is asked
`Also delete <dir> and everything in it (its sessions and login)? [y/N] `
(default no); without a terminal → `Kept <dir>: no terminal to ask, and
--delete was not passed.` Deletion is `os.RemoveAll(<registered dir>)` only
(a symlinked dir loses only the link: `Deleted the link <dir>; the directory
it points to was kept.`), reported as `Deleted <dir>.`; a partial failure is
an error telling the user to remove the rest by hand.

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
  "SessionEnd":   [ { "hooks": [ { "type": "command", "command": "C hook session-end" } ] } ],
  "StopFailure":  [ { "matcher": "rate_limit", "hooks": [ { "type": "command", "command": "C hook stop-failure" } ] } ]
}
```

- Ownership test: a statusLine or hook command is julienning's when its
  command ends with ` statusline`, ` hook session-start`, ` hook session-end`
  or ` hook stop-failure` and its first word (shell-unquoted) either has the
  basename `julienning` or is a versions-dir file
  (`…/julienning/versions/<ver>`, `paths.IsVersionFile`). A prior statusLine
  that is itself julienning's is never recorded as the one to restore.
- Existing user hooks are preserved; julienning's entry is appended once and
  updated in place when the command path changes.
- The first time a non-julienning `statusLine` is replaced, its raw JSON is
  stored in `patches.json` so uninstall/forget can restore it.
- Report per dir: `added` / `updated` / `unchanged`. A re-run on a file patched
  by an older julienning adds only the entries it lacks (the StopFailure hook
  arrived in 0.7.0), records them as created, and reports `updated`.
- `accounts`, `next` and `use` check every registered dir's settings.json for
  julienning's four entries (`claudecfg.MissingEntries`) and print one warning
  naming the dirs and `run: julienning setup` when any is missing; the
  command then continues. Hidden commands never check.

### shell integration (`internal/shell`)

`julienning shell-init zsh|bash` prints, without touching the network:

- julienning is called by the absolute path of the rc line
  (`'/home/dev/.local/bin/julienning' resolve-dir …`, `paths.StableCommand`),
  or `command julienning` when it is not stable;
- the `claude` wrapper: an exported `CLAUDE_CONFIG_DIR` wins; then, only
  while `$JULIENNING_HOME/pins.json` exists, `julienning resolve-dir
  --pin` (a pin covering the working directory → its dir; no pin → empty);
  when that fails the wrapper returns 1 and never falls back to the selection
  (a pinned project must not open on another account); otherwise it reads
  `current` on every call. It runs `CLAUDE_CONFIG_DIR=<dir> command claude`,
  or plain `command claude` with no selection, a missing dir, or the default
  dir (default-dir rule; the default dir's path is baked in at generation
  time). A machine without pins never starts a julienning process from
  `claude`;
- one `claude-<nick>` function per nickname in the cached allowlist (sorted,
  deduplicated, logged in here or not): `_jl_dir=$(<julienning>
  resolve-dir '<nick>') || return 1`, then `env -u CLAUDE_CONFIG_DIR claude
  "$@"` when that is the default dir, else `CLAUDE_CONFIG_DIR="$_jl_dir"
  command claude "$@"`. Resolving per call means a re-login never leaves a
  stale function. It does not change `current`. A nickname failing the rule
  is skipped with a warning (nicknames become function names and cannot be
  quoted). Dir-name aliases no longer exist;
- in tmux mode (on, and tmux found by `launch.FindTmux` when the code is
  generated; otherwise nothing changes and nothing is printed), every launch
  above goes through `_julienning_claude DIR ARGS…` (`DIR` empty = the
  variable unset; the exported value for the wrapper with
  `CLAUDE_CONFIG_DIR` exported), the shell side of the launch rule with the
  tmux path and session baked in (see "tmux mode").

Before setup has run it prints nothing and exits 0. Every function is
defined as `function name { … }` (not `name() {`), so an existing alias of
the same name in the rc file cannot break the eval; setup reports such alias
lines (see setup step 6). A `claude()` function defined earlier in the rc file
(provisioning scripts write one) is simply redefined.

`julienning resolve-dir NICKNAME|EMAIL|CONFIG` (hidden): `resolve.Target`
with the cached allowlist, no network; prints only the cleaned dir path on
stdout (compared byte for byte with the baked-in default dir), or the error
on stderr and exit 1, which makes the function return 1.

`julienning resolve-dir --pin` (hidden, the wrapper's pin lookup, no
network): `pins.Match` on the working directory (`os.Getwd`, which follows
`$PWD`); no pins file or no covering pin → no output, exit 0. Otherwise
`resolve.Account(cfg, cache, email, current)`: a team email (not
`resolve.IsPersonal`) → the registered dir logged into it (`resolve.Dirs`,
the current selection first; an unreadable `current` is ignored with a
warning, since the pin alone names the account); any other email →
`resolve.Personal`, whose notes go to stderr as `julienning: note: …` and
whose find is remembered. Prints the cleaned dir; a failure is
`pins.UseError`:
``~/Code/side is pinned to me@x.io, which cannot be opened: <reason> (or remove the pin: julienning unpin ~/Code/side)``,
exit 1. A corrupt or newer `pins.json` is an error too.

### tmux mode (`internal/launch`, `internal/shell`, setup)

`config.json` `"tmux": {"enabled": true, "session": "claude"}` (absent:
never decided). tmux 2.1 or newer is required, checked against tmux's
CHANGES: exact `=name` targets arrived in 2.1 (without them a target `claude`
also matches a session `claude2`), `-c` on new-window and new-session in 1.9,
and since 2.0 a window command given as several arguments is executed
directly, without a shell. `launch.FindTmux` looks on PATH, then in
`~/.local/bin`, `/usr/local/bin`, `/opt/homebrew/bin`, `~/.nix-profile/bin`
and `/run/current-system/sw/bin`, and reads `tmux -V` (a build that prints no
number, `master`, counts as new enough).

setup step 7: when `tmux` is absent from config.json and tmux is found, an
interactive run asks `Run claude inside tmux session "claude"? Sessions
keep running when your connection drops. [Y/n] ` (`[Y/n]` when setup runs
over ssh, i.e. `SSH_CONNECTION` is set, else `[y/N]`; empty takes the
default; a no records `{"enabled": false}`). `--tmux NAME` enables with that
session name (validated up front; tmux must be found, else the error
`--tmux NAME: tmux is not installed (install tmux 2.1 or newer, or leave out
--tmux)` before anything changes), `--no-tmux` disables; together they are a
usage error. Non-interactive without either: not asked, stays undecided.
Once decided it is never asked again; the flags change it. Status line:
`Tmux:     claude runs in tmux session "claude" (enabled|updated|unchanged)`,
`Tmux:     off (enable with: julienning setup --tmux claude)` (off or
undecided), `Tmux:     not installed`, `Tmux:     tmux 1.8 is too old (tmux
mode needs 2.1 or newer)`, `Tmux:     on (session "claude"), but tmux is not
installed; claude runs in place until it is fixed (turn it off with:
julienning setup --no-tmux)`. julienning's own launches follow a change at
once; the shell functions in new terminals.

Launch rule, for `launch.Exec` (use, next, login with claude args after `--`, new-config), the `claude`
wrapper and the `claude-<nick>` functions: when tmux mode is on, stdin and
stdout are terminals, `$TMUX` is empty, `$TERM_PROGRAM` is not `vscode` and
tmux is found, claude runs in a window of that session instead of in place
(`launch.ExecTmux`):

- window command: `/bin/sh -c 'trap : INT; "$@"; exec "${SHELL:-/bin/sh}"'
  julienning env <account> <claude path> <args>` (`shell.WindowScript`,
  passed to tmux as separate arguments). When claude exits the window becomes
  a shell, and Ctrl-C cannot skip that (`trap :` installs a handler, which
  exec resets for claude, instead of ignoring the signal). `<account>` is
  `CLAUDE_CONFIG_DIR=<dir>`, or `-u CLAUDE_CONFIG_DIR` for Claude's default
  dir (default-dir rule); for the wrapper with `CLAUDE_CONFIG_DIR` exported,
  that value. It is explicit because a tmux window takes its environment
  from the tmux server, not from the shell that asked for it: the server
  keeps the environment of whichever shell started it, so an account left
  implicit would be the wrong one. For the same reason tmux itself runs
  without `CLAUDE_CONFIG_DIR` (Go: every call; shell: `new-session`, which
  may start the server). `<claude path>` is absolute (Go: `exec.LookPath`;
  bash `type -P claude`, zsh `whence -p claude`, never `command -v`, which
  names the wrapper);
- `tmux has-session -t =<session>` → `tmux new-window -t =<session>: -c <cwd>
  <command>`, then exec `tmux attach-session -t =<session>`; else exec `tmux
  new-session -s <session> -c <cwd> <command>`. `<cwd>` is the directory
  claude would have started in (a resumed session's cwd). The shell code
  quotes every target: zsh expands an unquoted word starting with `=` to a
  command's path;
- a new window becomes the current window of every client attached to that
  session (tmux behaviour; a provisioning `claude()` function doing the same
  behaves identically);
- tmux mode on but tmux not found: `launch.Exec` warns (`tmux mode is on, but
  tmux is not installed; running claude here (turn it off with: julienning
  setup --no-tmux)`) and the shell code says nothing; both run in place;
- otherwise claude runs in place, as before.

### new-config / adopt / rename / login / configs / dev

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
- **A team account's nickname signs that account in.** When the nickname
  (`--nick` or the answer) is one the cached allowlist shows a team account
  holding, new-config is signing that account in on this machine: no mark
  (a wrong account signed in by mistake stays personal). Before creating
  anything it refuses when a registered dir here is logged into the account
  (`alpha (claude1@x.io) is already signed in on this machine, in ~/.claude-x; use it with: julienning use alpha`)
  or held it last and has no login (the `NotLocalError` signed-out message,
  pointing at `login`). Otherwise, after `Created …`, it prints
  `alpha (claude1@x.io) is already shared with the team; signing it in here.`
  and runs login's sign-in against the new dir: `claude auth login --email`
  and the same read-back and messages (`verifyLogin`). A failed sign-in ends
  `; ~/.claude-julienning3 is created, sign in later with: julienning login alpha`;
  `--no-login` prints `Sign in: julienning login alpha` after `Created …`.
  Either way login's unused-dir rule reuses the dir.
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
  only its config name). An account no registered dir is logged into
  (`NotLocalError`) goes to the first of, each only when `claude auth status`
  there says not logged in (a dir with a working login is never signed over;
  failing to ask is an error naming the dir):
  1. the first dir by name whose `last_email` is that account and that has no
     login; note
     `~/.claude-x held alpha (claude1@x.io) last and has no login now; signing in there again (its sessions are still there)`;
  2. for a team account (`resolve.IsPersonal` false), the first unused
     registered dir by name: no `last_email`, no login, no sessions
     (`sessions.Count`; an error counts as used), and no `share_on_login`
     with a nickname (made for a new account); note
     `nobody has used ~/.claude-x yet; signing alpha (claude1@x.io) in there`;
  3. for a team account, a new dir created, patched and registered like
     new-config's (`<prefix><N>`, no mark: the account is shared); note
     `no dir here holds alpha (claude1@x.io); created ~/.claude-julienning3 (config "julienning3") for it`.

  Any other email:
  ``x@y is not shared with the team and not logged in on this machine; sign it into a new dir with `julienning new-config` (shares it; `--no-share` keeps it personal), or into an existing one with `julienning login CONFIG` (see `julienning configs`) ``.
  With claude args after `--`, `launch.Exec` starts plain claude with them,
  unchecked, as before. Otherwise `claude auth login` runs in this terminal
  (never in tmux) against the dir, with `--email <account>` for a nickname
  or email target (the login page is pre-filled; a config name pre-fills
  nothing), and its exit status is the error
  (`claude auth login did not finish (exit status 1)`). Then the dir's login
  is read back: none is
  `~/.claude-x still has no login after claude auth login; start claude there and sign in with /login`;
  it is recorded (`last_email`) and a usage probe's signed-out mark on the
  dir is dropped; when the dir held another account before,
  `note: ~/.claude-x was logged into beta (claude2@x.io); the sessions in it stay there, under alpha`;
  another account than the target is an error (exit 1):
  `~/.claude-x is now signed in as claude9 (claude9@x.io), not alpha (claude1@x.io): the browser was signed into claude9. Sign out of claude.ai there (or use a private window) and run: julienning login alpha`;
  success prints `Signed in: alpha (claude1@x.io) in ~/.claude-x.`.
- `configs [--json]`: `*` current, NAME, NICK (nickname of the dir's current
  login when shared, else `-`), DIR (home-shortened), EMAIL (`(not logged
  in)` / `(unreadable)`), SHARED (`yes` / `no` / `?` when the cache is
  empty / `no (shares on login)` for a dir with a pending share whose login
  is not shared yet; its NICK stays `-` until the share lands). JSON:
  `{"configs": [{name, dir, email, nickname, logged_in, shared, current,
  default, share_on_login, email_error?}]}` (`shared` null when the cache is
  empty; `share_on_login` true exactly when SHARED says `shares on login`).
- `dev [NEW]` (`internal/cli/dirs`): `julienning dev` prints the dev name.
  `julienning dev NEW` renames it, for julienning only (the operating-system
  user is not touched): NEW is trimmed, lowercased and checked against the
  dev rule (usage error otherwise); unchanged → `Your dev name is already
  murat.` Else (`renameDev`):

  1. Take the claim-sync lock, retrying for up to 5 s (`another julienning
     is syncing claims right now; try again in a moment`, nothing changed).
  2. Release every claim in `claims.json` under the old name
     (`claims.ReleaseAll` with the old identity). A failure is a warning:
     `could not release 2 claims held as dev (…); the Worker drops them when
     its claim TTL runs out (12 h by default)`. `claims.json` is emptied
     either way: what is left was claimed under the old name, and a fresh
     record would keep step 4 from claiming under the new one.
  3. Re-read config.json, set `dev`, save.
  4. Reconcile claims under the new name (`claims.Reconcile`, then
     `MarkReconciled`; a failure is a warning, `could not claim the accounts
     in use here as murat yet (…); the next claim sync does it`), so accounts
     in use are claimed as murat at once; then release the lock.
  5. `Your dev name is now murat (was dev).`

  The machine id and the records already in the Worker under the old name
  (the usage `reporter`, exhaustion reports) stay until the next report
  replaces them or they expire. `setup --dev NEW` on a machine already set
  up runs these same steps.

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

### sync (`internal/teamsync`, `internal/claudecfg`)

`julienning sync [--from NICKNAME|EMAIL|CONFIG | --check]` gives every shared
account on this machine the same Claude Code settings and user-level
`CLAUDE.md`. Accounts: the registered dirs logged into an email on the
allowlist (refreshed from the Worker when it answers, else `shared.json` with
a warning), labelled like picker rows (`resolve.Label`; two dirs on one
account add their `~`-shortened paths). Personal accounts, dirs not logged in
and unreadable logins (warned) are never read or written; a target whose
settings.json or CLAUDE.md resolves (symlinks) to the file of such a dir or of
Claude's default dir when that is not a target is left out of that part
(warned), so nothing is written through it. No allowlist known → error (`no
shared accounts are known on this machine: …`); fewer than two targets →
error. A settings.json that is not a JSON object leaves that account out of
the settings part, an unreadable CLAUDE.md out of the CLAUDE.md part (each
warned); with fewer than two readable settings.json files the settings
questions are skipped (`settings  only alpha's settings.json could be read;
nothing to sync`).

Settings (`teamsync.Settings`, top-level settings.json keys; fixed values in
order, the default first):

| Key | Fixed values | Default |
|-----|--------------|---------|
| `model` | `opus` | `opus` |
| `effortLevel` | `xhigh` | `xhigh` |
| `promptCacheTtl` | `5m`, `1h` | `5m` |
| `subagentPromptCacheTtl` | `5m`, `1h` | `5m` |
| `autoContinueAtUsageLimit` | `true`, `false` | `true` |
| `theme` | `dark`, `light`, `dark-daltonized`, `light-daltonized`, `dark-ansi`, `light-ansi`, `auto` | `dark` |
| `editorMode` | `normal`, `vim` | `normal` |

Nothing else is ever touched (julienning's statusLine and hooks included).
Values compare as canonical JSON (whitespace and key order ignored).

Interactive (stdin and stdout terminals, else the error `sync asks which value
every shared account should get and needs a terminal; …`): one question per
setting, always, in that order, with `tui.Pick` in choice mode (no filter;
numbered fallback without raw mode). Header `<key> (<n> of 7): <summary>`,
scope line `Every shared account gets the value you pick (<labels>) · Esc
cancels the sync, nothing is written`. Rows (`teamsync.Choices`): the default
(note `(default)`), the other fixed values, values accounts hold that are not
fixed (most accounts first, then by value), `not set (Claude Code's default)`
when an account lacks the key (choosing it removes the key), then `Don't
sync`; each row's metadata lists the accounts holding it (`no account yet`).
The cursor opens on the default. Then CLAUDE.md (`<dir>/CLAUDE.md`, symlinks
followed; dirs sharing one file count once): skipped with a note when no
account has one or every account has the same one; else rows `<label>
CLAUDE.md` with `updated YYYY-MM-DD HH:MM · N lines` (local time),
`(same as …)` for identical content and the first non-blank line, newest
first, then `Don't sync CLAUDE.md`. For each other account whose CLAUDE.md
exists and differs, `tui.Confirm`: `Overwrite beta's CLAUDE.md with alpha's?`
(Enter/y overwrites, n/Esc keeps, Ctrl-C/Ctrl-D or the end of input cancel
the sync; fallback `Overwrite? [Y/n]`); accounts
without one get it and identical ones are skipped, without asking. Esc or
Ctrl-C in any picker prints `Sync canceled; nothing was changed.` (exit 0);
nothing is written before the last answer.

Writes: settings through `claudecfg.EditSettings` (jsonedit: every other byte
kept, re-read at write time, atomic, through symlinks, only when changed); a
key whose value changed since it was read (a running Claude Code's `/model`)
is left as it is now (`left beta's model as it is: it changed while sync was
running`);
CLAUDE.md through `claudecfg.WriteClaudeMD`, which writes nothing when the
file is no longer what was read (`left beta's CLAUDE.md as it is: it changed
while sync was running`). A relative `@import` in the copied CLAUDE.md is
warned about (the imported files are not copied). One report line per key,
`%-26s` wide: `opus: set in beta, gamma`, `xhigh: already in every shared
account`, `not set (Claude Code's default): removed from beta`, `not
synced`; then `CLAUDE.md  alpha's: copied to gamma; kept beta's` or the
note. A file that cannot be written is warned and the rest go on; then exit 1
(`could not update N file(s) (see above); …`). These writes are the user's
edits: `uninstall` and `forget` never undo them.

`--from TARGET` (`resolve.Target`; must be one of the accounts above, else
`… is not a shared account logged in here; --from takes one of: …`): no
questions, works without a terminal; every account takes TARGET's value of
each key (removed where TARGET does not set it) and its CLAUDE.md, overwriting
without asking (TARGET without one: `alpha has none; every account keeps its
own`). When one of TARGET's two files cannot be read, that part is `not
synced: …` and the other still goes; both unreadable → error. `--check`: read only; one line per key (`same: dark`, `differs: opus
(alpha) · sonnet (beta)`) and for CLAUDE.md (`same in every shared account`,
`differs: alpha (updated …) · none: gamma`); any difference exits 1 (`N of 8
differ between the shared accounts; …`), else `Every shared account here has
the same settings and CLAUDE.md.`. A file that could not be read also exits 1
(`N file(s) could not be compared …`). The two flags exclude each other (usage
error).

### accounts

Columns `#  NICK  ACCOUNT  SESSION  WEEK  STATE  LOCAL  UPDATED`. As v1
with: NICK is the team nickname (`-` for legacy records); allowlisted
accounts that never reported show `-` windows; STATE shows every other holder
(`in use by ali, can (12m)`); LOCAL lists the registered dirs here logged
into that email as home-shortened paths, comma-separated, `*` on the
selected one (`*~/.claude-julienning1`), then the dirs signed out of it
with ` (signed out)` (`usageprobe.SignedOutDirs`: no login but `last_email`
names the account, or a usage probe found the current login signed out;
`~/.claude-sixtynine (signed out)`), `-` for none. Rows keep the
Worker's order (Ranking and state: not exhausted (exhausted ones by
`exhausted_until` asc) → not busy → known before unknown → session asc →
week asc → session reset asc → email).

Your own use (claims and usage reports of your dev, which the Worker leaves
out of `state`/`busy_by`) reads `in use by you` when every claim and recent
report of your dev is from this machine, else `in use by you on box`, `… on
box, gpu`, `… here and on box`, `… on another machine` for a machine id not
in hosts.json (`… on box and another machine`, `… on 2 other machines`); the
same wording follows `and` and the exhausted state (`viewer.you`). The JSON
passes the Worker document through unchanged.

STATE of an exhausted account (`exhausted` true or `state` `exhausted`;
wins over every other state): `exhausted (<window> resets <time>)`, the
window being the one at ≥ 100% whose `resets_at` equals `exhausted_until`
(`Account.ExhaustedWindow`: week on a tie, which with both full is the later
reset, matching the Worker's text), `<time>` in local time via
`usage.FormatReset`; `exhausted (resets <time>)` when no window matches,
`exhausted` when `exhausted_until` is null, `(… has reset)` when the local
clock is already past it. A non-empty `busy_by` appends
`, in use by ali, can` (no age). Examples: `exhausted (week resets Fri 10:00)`,
`exhausted (session resets 23:40), in use by ali`.

Syncing rows: after the listing refreshes `shared.json`
(`sharedcache.FromListing`, which keeps this machine's `local_adds` younger
than `LocalAddGrace` on the wall clock), every `local_adds` email the listing
lacks (case-insensitive) is appended, sorted by email:
`#` `-`, NICK from `shared.json`, SESSION/WEEK `-`, STATE
`syncing (just shared)`, LOCAL as above, UPDATED `-`. It goes once the
listing has the email or the grace is over. When the cache cannot be saved
(warned) there are none. An empty listing with syncing rows prints the table,
not `no shared accounts yet`.

`--json`: the Worker document (`nickname`, `exhausted`, `exhausted_until`
included as sent) plus `local_config` (the selected dir's config name, else
the first) and `local_configs` (config names) per account, and a top-level
`syncing` array, always present, of `{email, nickname (null when unknown),
local_configs}`; syncing accounts never go under `accounts`. Before listing,
run claim reconciliation (below) synchronously and refresh `shared.json`.
`signed_out_configs` (config names, always an array) is added next to
`local_configs`, in accounts and syncing rows; signed-out dirs are never in
`local_configs` or `local_config`.

Between the listing and the table, `accounts` refreshes stale usage through
usage probes and waits for them (see "Usage probes"); the table shows the
Worker's answer to the probes, and the refresh's warnings and notes go to
stderr first.

`current` adds `state: <STATE text>` under the usage line when the account
is exhausted (only then).

### next / use (`internal/cli/switching`, `internal/sessions`, `internal/tui`)

`julienning next [--all] [--limit N] [--no-launch] [--json]`
`julienning use [TARGET] [--all] [--limit N] [--no-launch] [--json]` (TARGET:
a nickname, email or config name, see Nicknames; an email not on the
allowlist takes the personal path, see "Personal accounts"; no TARGET: the
pin covering the working directory, as if its email had been typed, else
the current selection without re-ranking; nothing selected → same as
`next`. An explicit TARGET ignores pins; `next` never looks at them)
`julienning use --clear`

1. Target: `next` lists, refreshes stale usage through usage probes and
   waits for every probe (see "Usage probes"), then ranks on the Worker's
   answer to the probes, so the account it picks never changes after the
   picker shows it. It picks the first shared account
   with a registered, logged-in local dir that is not signed out
   (`usageprobe.SignedOutDirs`; prefer the current dir on ties:
   `sameRank` compares every Worker sort key but the email, exhausted and
   `exhausted_until` included). The Worker sinks exhausted accounts, so the
   first local one is exhausted only when all are; guarded anyway: a usable
   local account ranked lower wins over an exhausted one. When the winner is
   exhausted, `next` still switches and warns on stderr before the summary
   (and before the busy warning):
   `every usable shared account on this machine is exhausted; claude3 becomes usable at Fri 10:00`
   (nickname, else email; `exhausted_until` in local time via
   `usage.FormatReset`; `… becomes usable when its limit resets` when null,
   `… should be usable again (its reset has passed)` when the local clock is
   past it);
   `use` refreshes the allowlist from the Worker when it can (falling back to
   `shared.json` with a warning), then resolves TARGET (`resolve.Target`; a
   `NotLocalError` or unknown target is the error; when the target came from
   a pin, wrapped in `pins.UseError`). A TARGET with `@` that
   `resolve.IsPersonal` calls personal is validated (`"x" is not a valid email
   address`) and leaves this flow for "Personal accounts" before
   `resolve.Target`; offline with no allowlist known, a team email logged into
   a registered dir therefore stays here and is selected as before.
   A dir reached by config name may be not logged in or not shared — then warn
   (`julienning3 is not logged in yet; run: julienning login julienning3`),
   skip the session list's move/merge and launch plain.
   A shared target (on the listing; never offline, never a personal
   account) has its usage refreshed through usage probes, that account
   only, before the summary line shows it; the refresh warns when the dir
   is signed out (the switch still happens).
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
4. Otherwise print the summary line (preceded, when a pin chose the target,
   by `~/Code/shop is pinned to alpha (claude1@x.io).`) and, with `--json`
   instead, the
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

#### Personal accounts (`resolve.Personal`, `use EMAIL`)

An email not on the allowlist is opened where it is logged in, without
touching team state. `resolve.Personal(cfg, email)`:

1. The dir remembered in `personal_dirs`, when it is a directory, not
   `discover.IsUnsafeConfigDir`, and its live login (`claudecfg.ReadEmail`,
   so `~/.claude.json` for the default dir) is that email. An account file
   that cannot be read is an error (`cannot read the login in ~/.claude-x:
   …`), not a reason to look elsewhere.
2. Otherwise `discover.Find` (the default dir, the top level of `$HOME`,
   rc-file `CLAUDE_CONFIG_DIR` dirs, the current `$CLAUDE_CONFIG_DIR`) plus
   the registered dirs it does not cover; the first logged into the email in
   that order wins (default dir first, then by path, then registered dirs by
   name; unsafe dirs are skipped). Notes, printed as `julienning: note:`:
   `me@x.io is no longer logged in at ~/.claude; found it at ~/.claude-me`,
   `me@x.io is also logged in at ~/.claude-b; using ~/.claude-a`. The result
   is remembered (`PersonalMatch.Remember`; a failure is a warning).
3. None: `no Claude config dir on this machine is logged in as me@x.io
   (searched ~/.claude, the top level of your home directory, the
   CLAUDE_CONFIG_DIR settings in your shell startup files and the registered
   dirs); sign in first, for example: CLAUDE_CONFIG_DIR=~/.claude-me command
   claude` (`~/.claude-<default nickname>`, `personal` when there is none);
   after a remembered dir lost the login: `me@x.io is no longer logged in at
   ~/.claude-me, and no other … is (…); sign in again with:
   CLAUDE_CONFIG_DIR=~/.claude-me command claude` (`command claude` for the
   default dir). Searched dirs whose login could not be read are appended
   (`; the login could not be read in ~/.claude-x: <reason>`), since the
   account may be there; when the account is found they do not matter.

`use EMAIL` then never sets `current`, never registers or patches the dir,
and sends nothing about the account (the allowlist fetch `use` always makes
carries no email). Summary line:
`Using me@x.io in ~/.claude (personal; your selection is unchanged).`
`--json`: `{config (the config name when the dir is registered, else null),
email, nickname: null, dir, personal: true}`. Launch mode lists the sessions
of that dir only (rows labelled by the email, `New session` `in me@x.io`),
resumes in place and never moves; cancel prints `Not starting claude; your
selection is unchanged.`. Reaching a registered dir by its config name keeps
the team behaviour above (selected, warned, plain launch).

### pin / unpin (`internal/pins`)

`julienning pin [TARGET] | pin --list`, `julienning unpin [DIR]`. Pins are
local to the machine and keyed by directory; each names an account by email
(`Pin{Dir, Email}`), resolved to a dir and checked on every use, so a
nickname rename or a login moving to another dir never breaks one.
`pins.Match(list, cwd)`: the deepest pinned dir that is cwd or contains it,
comparing both paths as written and with symlinks resolved; depth is the
length of the pin's resolved path, so pins written through different links
compare in one namespace. A directory carries one pin: `pins.Set` replaces
every pin on the same directory under any spelling, and `unpin` removes
them all.

- `pin TARGET` pins the working directory (`os.Getwd`). No network: a TARGET
  with `@` that is not on the cached allowlist must resolve through
  `resolve.Personal` (remembered); anything else goes through
  `resolve.Target` with the cache and must be logged in (`julienning3 is not
  logged in, and a pin names an account; sign in first with: julienning login
  julienning3`); a config name whose dir holds a personal login remembers
  that dir for the email, so the pin opens the dir that was named. Output:
  `Pinned ~/Code/side to me@x.io (personal).` / `… to alpha (claude1@x.io).`
  / `…, replacing <old>.` / `~/Code/side is already pinned to <account>.`,
  then `Plain claude and julienning use open it in ~/Code/side and every
  directory below it; julienning next is unaffected. Remove the pin with:
  julienning unpin`, and when `pins.json` was just created `A terminal opened
  before this julienning version ignores pins until you run: source
  ~/.zshrc` (the rc file setup edits, as in Nicknames). Accounts are named
  `alpha (claude1@x.io)`, the bare email without a nickname, or `me@x.io
  (personal)` when not on the cached allowlist.
- `pin` shows `~/Code/side is pinned to <account>.`, `~/Code/side/web is
  inside ~/Code/side, which is pinned to <account>.` or `No pin covers ~/x;
  plain claude follows the selection.`, and warns `plain claude refuses to
  start here until this is fixed: <reason>` when the account cannot be
  opened (read only: nothing is remembered).
- `pin --list`: `DIR  ACCOUNT` table sorted by dir, or `No pins. Pin a
  directory with: julienning pin NICKNAME|EMAIL|CONFIG`.
- `unpin [DIR]` (DIR: `~` expanded, need not exist) removes the pin on that
  directory itself: `Unpinned ~/Code/side (was <account>).`; a directory only
  covered by a parent's pin is an error naming it (`~/Code/side/web has no
  pin of its own; it is inside ~/Code/side, which is pinned to <account>; to
  remove that pin: julienning unpin ~/Code/side`); no pin at all:
  `~/x is not pinned; nothing to unpin.`, exit 0.

`pins.json` is written atomically (0600) and deleted when the last pin goes,
so the wrapper stops asking. `uninstall` deletes it too, plain or `--purge`:
new terminals lose the wrapper with the rc line, but one opened before keeps
it, and without the file it never asks julienning about pins.

#### Session listing (`sessions.List`)

Sources: the target plus every registered dir whose current login is on the
allowlist (`sharedSources`: the refreshed cache, else `shared.json`); a dir
logged into a personal account, not logged in, or with an unreadable login
is left out, so personal sessions are never listed or moved. A personal
target lists its own dir only. Within a source: the project folder for the
current working directory, or all project folders with `--all`. Folder name:
`cwd` with every non-alphanumeric character replaced by `-`; when that
exceeds 200 characters Claude truncates and appends a hash, so match folders
by the 200-character prefix and confirm with the `cwd` field inside the
session. Candidates: `*.jsonl` directly in the folder (not `agent-*` files),
sorted by mtime, newest `limit` (default 100, `--limit` max 500) across all dirs.

Uninstall's listing (`Every`) is the one exception to these rules: it reads
every registered dir, personal ones included (they move into the user's own
default dir, never into a team account), every project folder, sessions
without a title or prompt included, with no limit (see "uninstall / forget").

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
`livesess.CountsAsSession` is true (interactive/bg, not spare, not daemons,
not julienning's usage probe: Claude Code registers `claude -p` as kind
`interactive`, so the probe passes `--name julienning-usage-probe` and
entries with that name are left out)
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

### Exhaustion from refusals (`hook stop-failure`, `send-exhausted`)

Usage numbers alone cannot show an exhausted account: Claude Code documents
`used_percentage` as going above 100 once the limit is exceeded, and after
the API refuses a request (HTTP 429, Claude Code error type `rate_limit`) the
status line may keep showing the last numbers from before the refusal. The
refusal itself is the signal. settings.json gets a `StopFailure` hook with
matcher `rate_limit` running `julienning hook stop-failure`; Claude Code
calls it when a turn ends on that error, with stdin JSON carrying the common
fields plus `error`, `error_details` and `last_assistant_message`. Exit code
and output are ignored by Claude Code.

The hook does nothing unless `error` is `rate_limit`, gates like
`session-start` (set up, registered, logged in, email in `shared.json`; a
pending share is not enough), decides the window and the reset locally, spawns
a detached `julienning send-exhausted --email E --window session|week
[--resets EPOCH]` and exits 0 within 100 ms, without network.

- Window: the refusal text (`error_details` + `last_assistant_message`)
  contains "week" (case-insensitive) → `week`; else the window with the higher
  `used` in the local sent cache (`~/.julienning/sent/<hash>.json`), where a
  window whose reset has passed since that report counts as 0 (the session
  rolls over every five hours while the week keeps filling); ties → `session`;
  no usable cache → `session` (`usage.RefusalWindow`).
- Reset: parsed from the text, best effort, forms `resets Oct 13 at 8pm
  (Europe/Istanbul)`, `resets Oct 13 at 8:30pm (…)`, `resets 3pm (…)`, `resets
  3:15am (…)`; the zone in parentheses is loaded with `time.LoadLocation`
  (unknown zone → no reset). The dated form takes the current year (next year
  when that lands more than 30 days in the past); the time-only form is today
  in that zone, or tomorrow when that is not after now; the result must be
  after now and at most 32 days ahead (a "resets Dec 31" read on Jan 2 must
  not pin an account for a year). Built with `time.Date` in the zone, never by
  adding hours (DST). `usage.ParseRefusalReset`, `usage.RefusalReset`.
  When parsing fails, the sent cache's reset for that window if still in the
  future; else no reset (the Worker falls back to its own usage record, then
  to a window-length TTL).

`send-exhausted` validates its flags, re-checks the allowlist, and
`PUT /accounts/<email>/exhausted` with a 5 s timeout. Failures go to
`errors.log` as `EXHAUSTED_FAILED` (throttled like `SEND_FAILED`, per account
by email hash; a plain 404 from a Worker that predates the route says it
needs a redeploy); a 404 `account is not shared` is logged as `NOT_SHARED`
and drops the email from `shared.json`, as send-usage does. Nothing is
returned, nothing printed.

### statusline / send-usage

As v1, plus: the gate is "config dir registered AND email in `shared.json`"
(no network); a pending share does not open it, so a new-config dir starts
reporting once its share has landed in `shared.json`. The config dir is resolved via `claudecfg.ActiveDir` and the
account file via `claudecfg.AccountFileForEnv` (an explicitly exported
`CLAUDE_CONFIG_DIR=~/.claude` reads `~/.claude/.claude.json`). Every
status-line error code is log-throttled. A `used_percentage` above 100 is
clamped to 100 at parse time and again in `send-usage` (Claude Code reports
more than 100 once a limit is exceeded; v0.6 and earlier rejected such
payloads as malformed, which hid exactly the exhausted accounts). A payload
with a window missing still renders `usage pending` and now also logs
`USAGE_PENDING` (throttled, hourly) with the shape signature, never numbers,
so a silent run can be diagnosed afterwards. The
send-usage child refreshes `shared.json` when stale and runs claim
reconciliation at most every 10 minutes; when it did either, it then runs
the daily update check / auto-update outside the claim-sync lock. A 404 `account is not shared` from
the Worker removes the email from `shared.json`. Every send-usage drops a
usage probe's signed-out mark on its own config dir (the status line had
rate limits, so that login works).

### Usage probes (`internal/usageprobe`)

The status line reports only while someone uses an account through
julienning. A teammate on plain Claude Code (or claude.ai) reports nothing,
and an account idle past its reset keeps old numbers (shown `0% → reset`,
gone after the 8-day `usage:` TTL). A usage probe is the second source:
`claude -p "/usage"` against the account's config dir, which makes Claude
Code fetch the account's usage from Claude's usage endpoint with its own
login. julienning never reads credentials; it reads the structured
`usage_report` Claude Code prints next to the /usage text and throws the
text away (it describes local sessions). /usage is a local command: no model
request, no quota, no 5-hour window started.

- **Command** (`usageprobe.Args`): `claude -p /usage --safe-mode
  --no-session-persistence --name julienning-usage-probe --output-format
  stream-json --verbose`, claude found like every launch (`launch.ClaudePath`),
  working directory the temp dir, stdin `/dev/null`, stderr discarded,
  its own process group killed after 10 s (`usageprobe.Timeout`), at most 4
  at once (`usageprobe.Parallel`; Claude Code serializes token refreshes
  across processes, so a probe next to a live session in the same dir is
  safe). `--safe-mode` keeps the dir's hooks (julienning's own included),
  plugins, MCP servers and CLAUDE.md out while auth works; the registry
  entry's name keeps claims away (see "Claims from sessions"). The
  environment drops every `CLAUDE*` and `ANTHROPIC_*` variable (Claude Code's
  session variables, which a probe started from a hook inherits, and
  anything that would authenticate as someone else, such as
  `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`), then selects the dir
  like `launch.Env` (unset for the default dir).
- **Answer**: one JSON message per line; the `system`/`init` message's
  `claude_code_version` and the first `assistant` message's
  `usage_report.rate_limits.limits[]`. The row of `kind` `session` is the
  session window, `weekly_all` the week (classified on `kind`, never on a
  label; `weekly_scoped` rows are ignored). `percent` is clamped to 100 and
  rounded to one decimal; `resets_at` (ISO 8601, fractional seconds, jitter
  of up to a second around the real boundary) is rounded to the nearest
  second, or null for a window that is not running, which is accepted only
  at 0%.
- **Outcomes**: `ok`; `signed_out` when there is no `usage_report` and
  `claude auth status --json` says `loggedIn: false` (exit 1 is normal
  then); `failed` otherwise, with an actionable error that never quotes
  claude's output: claude not found, `Claude Code X cannot report usage;
  julienning needs 2.1.301 or newer (run: claude update)`, `claude did not
  answer within 10s`, `claude exited with status N`, `Claude Code could not
  fetch the usage …` (`rate_limits` or `limits` null), `Claude Code is signed
  in but reported no plan usage …`, or `unexpected usage report from Claude
  Code` with a shape signature (kinds and JSON types, never values) for
  errors.log. A missing row is never read as 0%.
- **Refresh** (`usageprobe.Refresh`, given the listing just fetched): reads
  every registered dir's login and records it (`last_email`, see
  config.json). For every registered dir logged into an account on the
  listing (only those: personal usage never leaves the machine; `use`
  passes one email), it skips a login probed less than 5 minutes ago
  (`usageprobe.CacheWindow`, whatever the outcome; an `ok` result is
  re-sent instead, because the listing can lag the write by a minute, and
  a `signed_out` one is reported again), and probes when the account has
  no numbers, a window whose reset has passed, numbers older than 15
  minutes (`accounts`, `next`, `use`; `usageprobe.InteractiveStale`) or an
  hour (the background round), or when the dir is marked signed out. Each
  result is saved in `probes/`. One dir per account is probed (the first
  by config name; a dir marked signed out is asked again all the same). A
  listing without `"probes": true` (an older Worker) probes nothing and
  says the Worker needs a redeploy. The newest `ok` per account goes in
  one `POST /probes`, whose answer replaces the listing: a fresh one when
  newer than the listing's `numbers_at`, a cached one (a resend) only when
  the listing neither shows its numbers from a probe nor has numbers at
  least as new, so a machine whose clock runs ahead never resends forever.
  A 404 `not found` or any other failure keeps the listing and is
  reported. The interactive budget is 45 s; a probe still running then is
  killed, and one not started by then is skipped and reported (`the time
  for usage probes ran out`), neither cached, so nothing arrives after the
  command decided.
- **Exit status**: a missing `usage_report` is checked with `claude auth
  status` whatever `claude -p` exited with (a claude without a login may
  exit 1); without even an init line, `claude --version` decides whether
  Claude Code is too old (an old one rejects `--safe-mode` and exits 1).
- **Messages** (stderr, `julienning: warning:` unless noted):
  `alpha (claude1@x.io) is signed out in ~/.claude-x; sign in again with: julienning login alpha`;
  `could not refresh the usage of alpha (claude1@x.io) in ~/.claude-x (claude did not answer within 10s); its numbers are from 3h ago`
  (`it has no reported numbers yet`);
  `the Worker does not take usage probes yet, so the numbers were not refreshed; it needs a redeploy (GitHub Actions, workflow deploy-worker)`;
  `could not send the refreshed usage to the Worker (…); showing the last reported numbers`;
  the login-change note (see config.json).
- **Signed out** (`usageprobe.SignedOutDirs`): a dir without a login whose
  `last_email` names an account, or one whose last probe found its current
  login signed out (the mark is for that login only). `next` (and
  `transfer`'s pick) skip such dirs, `accounts` lists them ` (signed out)`.
  A later `ok` probe, a status line report from the dir or a verified
  `login` drops the mark.
- **Background round**: claim-sync and the send-usage child, after the
  claim-sync lock is released, run a refresh with the hour threshold on the
  listing their `shared.json` refresh fetched and post with `listing=0`, so
  it costs no list operation of its own and runs at most hourly per
  machine, only while Claude is used there. It records no logins (a
  change is told by the next interactive command). Problems go to
  errors.log (throttled per dir, no emails): `PROBE_FAILED`,
  `PROBE_INVALID` (with the shape), `SIGNED_OUT`.

## Hosts

> Built on branch `murat/hosts`. Rules that change other sections live
> there (Local state, uninstall, accounts, tmux mode, shell integration,
> dev, Repo plumbing); this section keeps what is specific to hosts.

A **host** is another machine reachable with `ssh` that runs its own
julienning: its own registered dirs, logins, selection, pins, hooks, status
line and claims, against the same team Worker. Every user-facing command acts
on the machine it runs on (**this machine**) unless `--host NAME` names a
host; then this machine forwards the command to julienning there and the host
does all the work. `transfer` moves a session from one machine to another.
The Worker does not change: claims, usage and exhaustion reports already
carry dev + machine id.

Hosts run macOS or Linux on amd64 or arm64 (what releases ship). Linux means
any distribution: nothing may assume one distribution's startup files, tool
locations or versions. Debian and Ubuntu source `~/.bashrc` from `~/.profile`
before `~/.local/bin` joins PATH, Fedora and Arch use `~/.bash_profile`,
Alpine has busybox and often no bash, NixOS keeps tools under
`/run/current-system/sw/bin`; containers may lack git, curl or tmux.

### Terms

- **host**: an entry in `hosts.json`: a name, an ssh destination, and what was
  learnt about the machine when it was added. Names follow the nickname rule
  (`^[a-z0-9][a-z0-9._-]{0,31}$`); `local` is reserved and means this machine.
- **agent**: the hidden `julienning agent VERB` command, the only thing another
  machine runs over ssh.
- **controller**: the machine where the command was typed. It reads
  `hosts.json` and talks to the agents. `transfer` draws its pickers and
  questions on the controller's terminal; a forwarded command draws its own on
  the host, through `ssh -t`.

### Local state

`hosts.json`, `ssh/` and `received/` are in "Local state" above.

### Transport (`internal/sshx`)

- The system `ssh` (OpenSSH), never a Go ssh library: dependencies stay the
  standard library plus x/term, and the user's `~/.ssh/config`, agent,
  ProxyJump and known_hosts apply unchanged.
- Every call: `ssh -o BatchMode=yes -o ConnectTimeout=10 -o ControlMaster=auto
  -o ControlPath=<state>/ssh/%C -o ControlPersist=60 -t|-T -- <ssh> <command>`
  (the ControlPath double-quoted when it holds whitespace; without
  `ControlMaster` when it holds a `"`).
  BatchMode means a call never stops at a password or host-key prompt: hosts
  need key-based auth and a known host key (`host add` explains how to fix
  either: run `ssh <dest>` once by hand, add a key with `ssh-copy-id`). `-t`
  exactly when this machine's stdin and stdout are terminals and the call runs
  a command or claude; `-T` otherwise. One control socket per host makes a
  command's several calls cost one handshake. When the socket path would
  exceed 100 bytes (macOS caps socket paths at 104), the call runs without
  `ControlMaster`. The socket path counts as ssh binds it: `%C` (40 hex
  characters) plus the `.` and 16 random characters of the temporary name
  OpenSSH binds before renaming, since a path that does not fit makes ssh
  exit 255 instead of falling back.
- The remote command line never carries anything variable in plain text:
  `exec '<paths.julienning>' agent <verb>` with the request as JSON on stdin,
  or, for `run` and `launch` (stdin is the terminal), as one base64url argument
  (RFC 4648 §5, unpadded: only `[A-Za-z0-9_-]`, inert in any shell, fish
  included). Before julienning is known (the probe, the fallback install) the
  command is `exec /bin/sh -s` with a POSIX script on stdin. The team token is
  only ever sent on stdin: other users of a shared host see command lines in
  `ps`.
- The controller runs ssh as a child, not by exec, ignores SIGINT and SIGQUIT
  while it runs (the terminal delivers them to ssh and through it to the
  host), and exits with the remote exit code. julienning never exits 255, so
  255 is ssh's own failure: `cannot reach box (<ssh's last stderr line>);
  check with: ssh devbox.example.com`.
- Agent answers (every verb except `run` and `launch`) start with the line
  `JULIENNING-AGENT/<proto>`; anything before it is discarded, because a
  startup file that prints for non-interactive shells would otherwise corrupt
  the JSON or the archive. No such line: `box: no answer from julienning at
  <path> (run: julienning host add box to repair the entry)`.
- This machine's `JULIENNING_*` environment overrides are never passed to a
  host: ssh runs without them, so even a `SendEnv` in the user's ssh config
  cannot forward them. `TERM` reaches it through `-t`; `NO_COLOR` and the
  display time zone through `run` (see `--host`).

### Agent (`julienning agent`, hidden)

| Verb | Input | Output | Used by |
|------|-------|--------|---------|
| `hello` | – | `{proto, version, os, arch, home, machine_id, dev, worker_url, set_up, managed, setup_warning, tmux, paths, claude_version, tmux_version}` (`machine_id`, `dev`, `worker_url` null before setup) | first call of every cross-machine command |
| `bootstrap` | stdin `{dev, worker_url, token, replace_worker, replace_token?}` | `{created, token_updated, worker_replaced, token_rejected?, worker_error?}` | `host add` |
| `inventory` | – | `{configs: [{name, dir, email, nickname, shared, current}]}` | `host list` |
| `run` | argument `{machine_id, argv, project, tz, no_color, host}` | none: it becomes `julienning ARGV` | `--host` |
| `locate` | stdin `{project, branch, machine, ran_in, path?}` | `{cwd, checkout, git_root, branch, how}` | `transfer` (destination) |
| `sessions` | stdin `{project, all, limit, machine}` | `{cwd, sessions: [...], labels, projects, warnings}` | `transfer` (source) |
| `choose` | stdin `{target, tz}` | `{config, dir, email, nickname, account, warnings}` | `transfer` (destination) |
| `export` | stdin `{id, config_dir}` | header line, then the archive | `transfer` (source) |
| `receive` | stdin: the controller's request line `{config_dir, email, cwd, checkout, git_root, from}`, then the export's header line and archive | `{committed, already, manifest_sha256, project_folder, rewrites, memory, warnings}` | `transfer` (destination) |
| `release` | stdin `{id, config_dir, manifest_sha256}` | `{released, warnings}` | `transfer` (source) |
| `launch` | argument `{machine_id, dir, cwd, args, host}` | none: it becomes claude | `transfer` (resume) |
| `install-self` | – | `{version, link}` | `host add` (fallback install) |

- `proto` is an integer, 1 at first. Within one proto a verb only gains
  optional fields. `hello`, `inventory` and the `run` envelope are frozen
  across protos (fields are only ever added), so `host add`, `host list`,
  `update --host` and `uninstall --host` work across any two versions that
  have the agent.
- `hello` (`agentHello`): `set_up` is config.json loading (a load error is
  `setup_warning`, set_up false); `managed` is `selfupdate.ActiveLink`
  succeeding; `setup_warning` is `no Worker is configured` or
  `claudecfg.SetupWarning` (settings.json entries missing), `""` when
  complete; `tmux` is config.json's tmux mode, null while undecided;
  `paths` are the stable command (else the running binary), then
  `launch.FindClaude` and `launch.FindTmux` (PATH, then `~/.local/bin`,
  `/usr/local/bin`, `/opt/homebrew/bin`, `~/.nix-profile/bin`,
  `/run/current-system/sw/bin`); `claude_version` from Claude Code's native
  layout (the command links to `…/claude/versions/<version>`), else `claude
  --version` (5 s).
- `bootstrap`: not set up → the Worker is checked from the host (`/healthz`,
  an authenticated `/accounts`), then config.json is created with a new
  machine id and the listing saved as `shared.json`; a failure is
  `WORKER_UNREACHABLE` and saves nothing. Set up without a Worker (setup
  `--yes` without `--remote-url`) → the sent one is checked and stored like
  on a fresh machine (`worker_replaced`). Set up for the same Worker URL →
  the host's token is checked: accepted → nothing changes; rejected →
  `token_rejected: true` and nothing changes, until a second call with
  `replace_token: true` (after the controller's question) stores the sent
  token, which must be accepted (`token_updated`); unreachable →
  `worker_error`. Another Worker URL → `WORKER_MISMATCH` unless
  `replace_worker`, which checks the new Worker from the host, stores URL and
  token and saves its allowlist (`worker_replaced`). The machine id is never
  regenerated, and the dev name is never changed by bootstrap.
- `run` (`agentRun`): the request's `machine_id` must be this machine's
  (both empty on a host without config.json, so `setup --host` works there;
  a config.json that does not load counts as none for `setup`, `update` and
  `uninstall`, the commands that repair or remove it, and stops the rest);
  `host` is the controller's name for this machine, used in messages the
  agent prints itself (`julienning: box: …`), since run's output is the
  command's. Then the directory (`project` given → "Locating a project";
  `pin`/`unpin` fail without a checkout, other commands get the note and run
  in the home; no `project` → the home), `PATH` with the dirs of this
  machine's julienning, claude and tmux first, `PWD`, `TZ` (when it loads)
  and `NO_COLOR=1`, then exec of `julienning ARGV`.
- `install-self`: the running binary (a temporary copy) installs itself as
  its own version (`selfupdate.InstallSelf`: versions dir, `version` run
  first, the command symlink switched, old versions pruned).
- The transfer verbs act only on registered config dirs (`INVALID`
  otherwise). `locate` searches without a picker (its stdin is the request):
  several checkouts are `AMBIGUOUS`, and the controller then asks for a
  path, which a second call checks (`path`: `~` is this machine's home; a
  checkout sharing a remote URL, the sub folder present). `sessions` locates
  the controller's project first (none with `all`), lists like
  `switching.TransferSessions` (shared accounts only) and returns, per
  listed `cwd`, the project computed here, which the destination locates.
  `choose` is `switching.Choose` (it becomes the selection), its warnings
  rendered in `tz`. `export` answers the frame line and then streams the
  header line and the archive; a refusal before the header is the framed
  error, a failure later cuts the stream (the receiver finds no manifest).
  `receive` takes the controller's request line ahead of the export, since
  where the session goes (the chosen account dir and the located directory)
  is decided before the move. `launch` checks the machine id, puts this
  machine's julienning, claude and tmux dirs first on PATH (as `run` does)
  and runs `launch.Exec` (tmux mode applies; `launch.ClaudePath` looks on
  PATH, then in `~/.local/bin` and the other install dirs).
- Errors: exit 1 and `{"error": {"code", "message"}}` after the frame line.
  Codes: `NOT_SET_UP`, `MACHINE_MISMATCH`, `WORKER_MISMATCH`,
  `WORKER_UNREACHABLE` (bootstrap: the Worker cannot be used from the host),
  `PROTO`,
  `NOT_LOCAL`, `PERSONAL`, `NO_CHECKOUT`, `AMBIGUOUS`, `PATH_MISSING`, `LIVE`,
  `MAYBE_OPEN`, `COLLISION`, `GONE`, `CHANGED`, `NO_SPACE`, `INVALID`.
  Messages are worded like this machine's own errors, name things on the host
  in the host's terms (`~` is the host's home) and are printed as
  `julienning: box: <message>`.
- Timestamps in answers are UTC instants; everything shown is rendered on the
  controller in its own zone. Titles and prompts in `sessions` are cleaned on
  the host (`cleanText`) and again on the controller, since they come from
  another machine.
- The agent never writes an email to `errors.log`; answers may carry emails to
  the controller, which prints them as local commands do.

### Versions

- Cross-machine commands (`--host`, `transfer`) need the same major.minor on
  both machines (`selfupdate` parsing; patch and pre-release may differ). In
  0.x the minor is the breaking number. Release rule: a change to the agent
  protocol or the archive format bumps the minor version. `proto` is the
  second check (`PROTO`), in case a release gets the bump wrong.
- Mismatch in a terminal, host older: `box runs julienning 0.9.2 and this
  machine 0.10.0; cross-machine commands need the same 0.10. Update box to
  0.10.0 now? [Y/n] ` → the installer on the host, pinned to this machine's
  version (see `host add` step 3), then the command goes on. Host newer: `…
  Update this machine to 0.10.1 now? [Y/n] ` → `update --version v0.10.1`
  here, then `Updated 0.10.0 → 0.10.1; run the command again.` (the running
  binary stays the old one). Declined, or no terminal: the error
  `box runs julienning 0.9.2 and this machine 0.10.0 (cross-machine commands need the same major.minor); update box: julienning update --host box --version v0.10.0`
  (or `update this machine: julienning update --version v0.10.1`).
- Never refused outright for a mismatch: `host list`, `host remove`, `update
  --host`, `uninstall --host`, and `host add`, which offers the update itself
  and stops before step 4 when it is declined. They are how a host gets fixed
  or removed.
- A development build on either side (`selfupdate.IsDevBuild`) skips the
  number check with a warning (`julienning: warning: this machine runs a
  development build (dev); only the agent protocol is checked against box`);
  `proto` still has to match.
- Each machine auto-updates on its own daily schedule, so after a release the
  two can differ for up to a day; the prompt fixes that in one step.

### host add / list / remove (`internal/hosts`, `internal/cli/hosts`)

`julienning host add NAME [SSH_DEST] [--yes]` (SSH_DEST defaults to NAME;
both validated before anything runs). Idempotent: re-running it on a known
name refreshes the entry. With `--yes` it asks nothing: questions that
default to yes (update a host, send the token, rename the dev name) are taken
as yes; questions that default to no (point the entry elsewhere, replace a
changed machine id) are refused. Without a terminal and without `--yes`,
every question is an error naming the command that answers it. In both cases
setup there runs with `--yes`. Steps, one status line each:

1. Entry. A new NAME is added. A known NAME with the same SSH_DEST is
   refreshed. A known NAME with another SSH_DEST asks `box is
   devbox.example.com now; point it at other.example.com instead? [y/N] `.
2. Probe (`exec /bin/sh -s`, POSIX script on stdin): `uname -sm`, `id -un`,
   `$HOME`, the login shell, `PRETTY_NAME` from `/etc/os-release` (or
   `sw_vers` on macOS), the server address from `$SSH_CONNECTION`, and the
   absolute paths of julienning, claude, tmux, bash, curl, tar and sha256sum or
   shasum: `command -v` on the non-interactive PATH, then `~/.local/bin`,
   `/usr/local/bin`, `/opt/homebrew/bin`, `~/.nix-profile/bin` and
   `/run/current-system/sw/bin`, then `command -v` inside `"$SHELL" -lc` (a
   separate call the controller stops after 5 s, since not every host has a
   `timeout` command; output read after a marker line; the inner command is
   plain `;`-separated commands so fish runs it too). Prints `Host:     box →
   devbox.example.com (100.64.0.7): Debian GNU/Linux 13 (trixie),
   linux/amd64, user dev, home /home/dev` (the address and the OS name are
   left out when unknown, the address also when it equals SSH_DEST), so a
   destination that reaches another machine is visible. Refused: an OS other than darwin/linux or an
   arch other than amd64/arm64 (`box is linux/armv7l; julienning releases
   exist for amd64 and arm64 only`), an unreachable host, a key or host-key
   problem (BatchMode), each with what to run (`its host key is not known
   yet: run `ssh devbox.example.com` once by hand to accept it`, `key-based
   login is needed: add a key with `ssh-copy-id devbox.example.com``).
3. julienning there:
   - missing → install;
   - present, but too old to have `agent` (exit 2 on it) → install;
   - present → `hello`; a version mismatch follows "Versions" (an update is an
     install of this machine's version);
   - present but not in the installer's layout (`selfupdate.ActiveLink`
     fails there: a hand-built binary) → used as it is, with `Julienning: 0.10.0
     at /opt/x/julienning (not from the installer: auto-update and update
     --host leave it alone)`; a mismatch is then an error asking to update it
     by hand.

   Status lines: `Julienning: installing 0.10.0 on box…` then `Julienning:
   0.10.0 at ~/.local/bin/julienning (installed)`, or `Julienning: 0.10.0 at
   ~/.local/bin/julienning` for one already there. The entry's
   `paths.julienning` is the stable command `hello` reports (the installer's
   link survives updates).

   Install: the official script run on the host, pinned to this machine's
   version (the latest release when this machine runs a development build),
   so the host gets exactly what a teammate's `curl | bash` gets; afterwards
   the entry uses the installer's link (`~/.local/bin/julienning`, checked
   with `hello`), never a PATH lookup, where an older julienning could come
   first. It is downloaded to a `mktemp` file first and run with bash from there (`curl
   -fsSL <installer URL> -o "$tmp"`, `bash "$tmp" v0.10.0`), so a failed
   download fails instead of feeding bash an empty script, with the found
   tools' dirs first on PATH. `JULIENNING_REPO` and `JULIENNING_RELEASES_BASE`
   set here are passed to the script (tests, mirrors); with
   `JULIENNING_RELEASES_BASE` set the installer URL is
   `<releases base>/raw/main/scripts/install.sh` (a path GitHub serves too),
   else `https://raw.githubusercontent.com/<repo>/main/scripts/install.sh`
   (`selfupdate.InstallerURL`). The script needs bash, curl, tar, sha256sum
   or shasum and access to GitHub on the host. When one is missing or the download fails, the
   fallback: this machine downloads `julienning_<ver>_<os>_<arch>.tar.gz` and
   `checksums.txt` for the host's os/arch, verifies the checksum (fail closed,
   `selfupdate` code), streams the binary through the probe shell into a
   `mktemp` file there, runs it once as `<tmp> version` (it must print that
   version) and lets it install itself (`agent install-self`: the same layout
   and steps as `update`). The binary travels base64-encoded inside the
   script (a here-document, decoded with `base64 -d`, else `-D`), not after
   it on stdin: dash, Debian's /bin/sh, reads its script in blocks and would
   swallow the data. Either way auto-update and `update --host` work as
   on any machine.
4. Team settings (`agent bootstrap`, this machine's dev name, Worker URL and
   token on stdin):
   - not set up there → config.json with its own machine id; the Worker is
     checked from the host (`/healthz` and an authenticated `/accounts`) and
     the allowlist saved; failure → error (`box cannot reach the Worker (…);
     its hooks report usage and claims from there`), nothing saved there;
   - set up, same Worker URL → kept; its token is checked from the host and
     when the Worker rejects it (rotated) → `The Worker rejects box's team
     token. Send this machine's? [Y/n] ` (yes: a second `bootstrap` with
     `replace_token: true`; no: a warning that its hooks cannot report until
     it has the current one); when the check itself fails, a warning;
   - set up, another Worker URL → `box is set up for another Worker
     (https://other.example.workers.dev); hosts must use this machine's. Switch
     box to it? Its shared accounts and claims stay with the other Worker.
     [y/N] ` → `bootstrap` with `replace_worker: true` (the token still only on
     stdin; never `setup --token` on a host's command line); declined →
     refused;
   - the dev name differs → `box knows you as dev. Rename it to murat? [Y/n] `
     → `julienning dev murat --host box`. Nothing depends on it, so without a
     terminal (and without `--yes`) it is a `Note:` line, not an error
     (`Note:     box knows you as dev (rename it with: julienning dev murat
     --host box)`);
   - the machine id is never regenerated (claims refer to it). A machine id
     already in hosts.json under another name is refused (`devbox.example.com
     is the same machine as gpu; remove that entry first: julienning host
     remove gpu`). A refreshed entry whose machine id changed asks `box now
     answers as machine 77aa01c3e9f0 (was 9be1c0a4d2f3): rebuilt, or
     devbox.example.com reaches another machine now. Replace the entry? [y/N] `
     (no terminal: refused). Both checks run before `bootstrap` when the host
     already has a machine id, so the token never goes to a refused machine.

   Status line: `Team:     set up on box (dev murat, Worker https://…)`,
   `Team:     box now uses this machine's Worker (https://…)`, `Team:     sent
   this machine's team token to box` or `Team:     unchanged on box`.
5. Setup there: `setup --host NAME` (interactive: share, nickname and tmux
   questions on this terminal) when step 4 created the config, the host's
   setup warning is not empty, or tmux is installed there and tmux mode was
   never decided (`Setup:    running julienning setup on box`, then its
   output); otherwise `Setup:    complete on box (change it with:
   julienning setup --host box)`. A failed setup there still saves the entry
   and exits 1 (`setup on box did not finish (see above); fix it and run:
   julienning setup --host box`).
6. Saves the entry: `Added box (devbox.example.com).` / `Refreshed box
   (devbox.example.com).`, then `Next: julienning use --host box, or
   julienning transfer --to box`.

`julienning host list [--json]`: `HOST  SSH  OS  JULIENNING  CLAUDE  TMUX
ACCOUNTS  STATUS`, every host asked in parallel (`hello` + `inventory`, 10 s
each, never a prompt) and its `checked_at` updated. ACCOUNTS: nicknames (else
emails) of the shared accounts logged in there, `*` on its selection, personal
accounts only counted (`+1 personal`). TMUX: the version and the mode,
`3.5a (on: claude)`, `3.5a (off)`, `3.5a (undecided)`, `-` when not
installed. STATUS: `ok`, `unreachable (…)`,
`julienning 0.9.2 (this machine 0.10.0)`, `machine id changed`, `not set up`,
`setup incomplete`. `--json`: `{"hosts": [{name, ssh, machine_id, os, arch,
julienning, claude, tmux, tmux_mode, accounts: [{email, nickname, config,
current}], status, error?}]}` (`julienning`, `claude` and `tmux` are the
versions there, null when unknown; `tmux_mode` is `hello`'s `tmux`). No hosts: `No hosts. Add one with: julienning host add
NAME [SSH_DEST]`.

`julienning host remove NAME`: removes the entry and closes its control socket
(`ssh -O exit`, best effort): `Removed box from this machine's hosts;
julienning on box is unchanged (remove it there first with: julienning
uninstall --host box)`. An unknown NAME is an error.

### --host

`cli.Main` takes `--host NAME` / `--host=NAME` off the arguments before the
command's flags are parsed, anywhere before a `--` (arguments after `--`, as
in `login alpha -- --model x`, are never inspected). Given twice, or without
a name → usage error. `--host local` is the same as no flag. An unknown NAME: `unknown host
"box" (see julienning host list; add it with: julienning host add box
SSH_DEST)`, exit 1.

Each command declares its host behaviour (`cli.Command.Host`):

| Behaviour | Commands | With `--host box` |
|-----------|----------|-------------------|
| Forward | setup, uninstall, update, dev, configs, current, adopt, new-config, rename, forget, login, sync, use, next, pin, unpin, accounts, share, unshare, nick | the whole command runs on box |
| Source | transfer | box is where the sessions come from |
| Local | host add / list / remove, help, version | usage error (`host list does not take --host`) |
| Internal | statusline, shell-init and every hidden command | usage error; they run where Claude or the shell runs |

Forwarding:

1. `hello` (machine id: another one → `box answers as machine 77aa01c3e9f0,
   not 9be1c0a4d2f3: it was rebuilt, or devbox.example.com reaches another
   machine now; check with: julienning host add box`; versions; not set up →
   `julienning is not set up on box; run: julienning host add box`, except for
   `setup`, `update` and `uninstall`, which are how a host gets set up, fixed
   or removed; `update` and `uninstall` are also never refused for a version
   mismatch). `setup --token …` is refused before anything runs (usage
   error): the forwarded arguments are on the host's command line, which other
   users there see in `ps`; `host add` sends the token on stdin.
2. `use`, `next`, `pin`, `unpin`, `login` and `new-config` depend on the
   working directory. This machine sends the project of its working directory
   (see "Locating a project") and the host finds its checkout. Found → the
   command runs there. Not found → `pin` and `unpin` fail with the
   locate message; the others run in the host's home after `julienning: note:
   ~/Code/shop has no checkout on box (…); running in ~ there`. Other
   commands run in the host's home. `pin --list` and `unpin DIR` name what
   they act on and send no project. On the host, several equally good
   checkouts are a picker there (ssh -t), else the `AMBIGUOUS` error.
3. `agent run` over ssh (`-t` with a terminal), carrying the arguments, the
   project, `NO_COLOR`, and this machine's display zone as an IANA name (`TZ`,
   else the `zoneinfo/` part of the `/etc/localtime` link target; omitted
   when it cannot be named, with a note). The agent checks the machine id,
   changes into the directory, puts the dirs of `paths` first on PATH, sets
   `TZ` and execs `julienning ARGV`. The binary embeds `time/tzdata`, so a
   host without zoneinfo still renders this machine's zone: `accounts --host
   box` shows resets in your time, not the host's (often UTC).
4. The host's output and exit code pass through unchanged; `--json` is the
   host's document.

What changes in meaning: `setup --host box` on a host without team settings
asks for them there, as setup does (`host add` sends them instead).
`uninstall --host box` uninstalls there (its sessions move into its
`~/.claude`); the entry stays in hosts.json. `use`/`next --host box` switch
and launch on box, the picker and claude on this terminal through ssh, in
box's tmux mode when it is on. `accounts --host box` lists the same Worker
data with box's LOCAL column. `share`/`unshare`/`nick` change the same team
state anywhere; only `Logged in here as` describes box. The shell's `claude`
and `claude-<nick>` functions are local and never take `--host`.

`accounts` here also says where your own claims come from (see
"accounts": `in use by you on box`).

### Locating a project (`internal/hosts`)

A working directory's **project**, computed on the machine where it is:

- the checkout: the top of the worktree containing it, its main worktree's top
  (`gitRoot`, the memory key), the directory relative to the checkout
  (`sub`), the checkout's folder name and its branch (from `HEAD`);
- the repository's remote URLs: every `url` under `[remote "…"]` in its config
  (for a linked worktree, the shared repository's), read from files like
  `gitRoot` does, never by running git. Normalized: scheme, user and port
  dropped, host lowercased, scp form `user@host:path` as `host/path`, a
  trailing `/` and `.git` removed, so `git@github.com:muratgozel/julienning.git`
  and `https://github.com/muratgozel/julienning` are both
  `github.com/muratgozel/julienning`. `url.*.insteadOf` is ignored. The
  repository's name is the last element of the first URL (`origin` first);
- the directory relative to home, when it is inside home;
- the clone URL for messages: `origin`'s as written.

On the other machine, only inside its home, stopping at the first step with a
match, and never searching further:

1. Projects Claude already used there: the `cwd` of the newest session in each
   projects folder of its registered dirs (a head read), when inside home;
   their checkouts are candidates.
2. Folders named like the repository or like this machine's checkout folder:
   the same path relative to home first, then a breadth-first walk of home to
   depth 4, not following symlinks, skipping hidden folders and
   `node_modules`, `vendor` and `target`, bounded to 5 s and 20 000 folders
   (`the search stopped after 5 s` when reached).

A candidate counts only when it shares a normalized remote URL with the
project: folder names nominate, the repository decides. Several matches: the
one on the session's branch (the transcript's last `gitBranch`; this
checkout's branch for a forwarded command), else the same path relative to
home, else a picker (no terminal: the `AMBIGUOUS` error listing them). On
macOS the walk leaves out the privacy-protected and media folders discovery
skips (Desktop, Documents, Downloads, Library, Movies, Music, Pictures,
Applications): stat-ing inside them can pop a permission dialog. A checkout
there is still found by step 1 or the same path relative to home, which are
direct lookups. Remotes are read from `[remote "…"]` sections of the
repository's `config` (values unquoted, inline comments dropped), branches
from the worktree's `HEAD`. Then
`sub` is appended and must exist there (`PATH_MISSING`: `box has
~/src/shop but not its web folder (the session ran in ~/Code/shop/web here);
check out the same branch there`).

Not in a git repository on this side, or in one without any remote (nothing
to verify a candidate by): the project is only its path relative to home,
mapped to the same path under the other home when it exists there; outside
home, nothing maps.

Nothing found, `transfer` in a terminal:

```
box has no checkout of github.com/muratgozel/julienning under /home/dev (checked 2 projects Claude used there and folders named julienning).
Clone it there, for example:
  git clone git@github.com:muratgozel/julienning.git ~/Code/muratgozel/julienning
then run the transfer again, or type the path of a checkout on box (Enter cancels):
```

The suggested target is this machine's path relative to home. A typed path
(`~` is the host's home; anywhere, since it is not a search) must be a
checkout sharing a remote URL, else `that is a checkout of github.com/x/y, not
github.com/muratgozel/julienning` and the question again. Enter cancels
(`Transfer canceled; nothing was moved.`). Not a git repository: `box has no
~/scratch/notes (the session ran there on this machine, outside a git
repository); create it there, for example: mkdir -p ~/scratch/notes, then run
the transfer again, or type a path on box (Enter cancels):`. Without a
terminal these are the error, without the question.

Nothing is stored: after the first transfer the checkout holds a session
there, so step 1 finds it the next time.

### transfer (`internal/cli/hosts`, `internal/sessions`)

`julienning transfer [--host SOURCE] [--to HOST] [TARGET] [--all] [--limit N] [--no-launch]`

Needs a terminal (stdin and stdout), else `transfer asks which session to
move and needs a terminal`. SOURCE is `--host` (default this machine).

1. `hello` to every host involved (versions, machine ids).
2. Destination: `--to HOST` (a host name or `local`), else `tui.Pick` in
   choice mode: header `Transfer a session from this machine to:` (or `from
   box`); rows `this machine` and each host (meta: its ssh destination and
   os/arch), the source left out; Esc → `Transfer canceled; nothing was
   moved.` (exit 0). No other machine → `no other machine to transfer to; add
   one with: julienning host add NAME [SSH_DEST]`. The picker does not probe;
   an unreachable host fails at the next step. Source equals destination →
   `box is both the source and the destination; to move a session between
   accounts there, use: julienning use --host box`.
3. Account, on the destination (`agent choose`, or in process here): TARGET
   resolved like `use TARGET` there (a shared account logged in there; a
   personal email → `PERSONAL`: `transfer moves sessions between team
   accounts only; me@x.io is personal`); without TARGET, chosen like `next`
   there (Worker ranking, first shared account logged in there, its selection
   preferred on ties, the exhausted and busy warnings prefixed `box: `). It
   becomes the destination's selection, as with `next`. Nothing usable: `none
   of the shared accounts is logged in on box; sign one in: julienning
   new-config --host box`. The summary line names the machine and heads the
   next picker: `Moving into alpha (claude1@x.io) in ~/.claude-x on box —
   session 12% → 17:30, week 28% → Tue 21:00.` (`on this machine` when local).
4. Session, on the source (`agent sessions`, or in process): listed like
   `use`'s picker (`sharedSources` of the source: dirs logged into shared
   accounts only, so personal sessions never appear; the source's checkout of
   this machine's working directory, located as for `--host` when the source
   is a host; `--all`, `--limit`), drawn here. No `New session` row. Open
   sessions are disabled, `(may be open)` as in `use`. Scope line `37 sessions
   in ~/Code/shop on this machine · type to filter`. None: `no session to
   transfer in ~/Code/shop on this machine (--all lists every project)`, exit
   1. Esc → `Transfer canceled; nothing was moved.` (the destination's
   selection already changed, as with `next`).
5. The session's project is located on the destination ("Locating a
   project"); the warning `the session was on branch feat/x; box's
   ~/src/shop is on main`. Claude Code versions (the user's rule, replacing
   the planned warning): patch releases may differ; a major or minor
   mismatch blocks, as early as it is known. Right after `hello` (step 1),
   two machines whose Claude Code differs in major.minor are refused (`This
   machine runs Claude Code 2.1.303 and box 2.0.14; a transfer needs the same
   major.minor on both machines (patch releases may differ); update Claude
   Code on box (claude update) and run the transfer again`, naming the older
   one); in the session picker, a session whose transcript was last written
   by another major.minor than the destination's Claude Code (its last
   `version`) is shown disabled, `(written by Claude Code 2.0; box runs
   2.1)`. An unknown version on either side does not block.
6. Confirm (`tui.Confirm`): `Move "Fix flaky clock tests" from beta on this
   machine to alpha on box?` / `  Its transcript, checkpoints, task list and
   session environment move to ~/src/shop on box; this machine will no longer
   have it. Paths inside it are rewritten for box. Project memory is merged,
   never overwritten.` Declined → back to the session picker on that row.
7. Move ("Moving a session between machines"). Sessions over 5 MB show
   progress (`Sending "Fix flaky clock tests" to box: 23 of 61 MB`). Report:
   `Moved "Fix flaky clock tests" from beta on this machine to alpha on box
   (memory: 1 file added; 214 paths rewritten for ~/src/shop).`, then memory
   conflicts and warnings as in `use`.
8. Resume on the destination: here `launch.Exec` from the session's new cwd;
   on a host `agent launch` over `ssh -t`. tmux mode applies on that machine.
   `--no-launch` prints instead `Resume it on box: cd ~/src/shop &&
   claude-alpha --resume <id>` (`CLAUDE_CONFIG_DIR=<dir> claude --resume <id>`
   for an account without a nickname, `command claude --resume <id>` for the
   default dir).

When nothing is found on the destination, or a host finds several
checkouts it cannot choose between, the controller prints the message (with
the clone suggestion for a repository, `mkdir -p` for a plain path) and asks
for the path of a checkout there; Enter cancels (`Transfer canceled; nothing
was moved.`), an unusable path is explained and asked again. A lost answer
from `receive` is ssh's failure (`cannot reach box (…)`, exit 1): the source
keeps the session, and the next transfer finishes it (step 4 below).

With both ends on hosts (`--host box --to gpu`) this machine relays: box's
export streams through this machine into gpu's receive, and the two never
connect to each other. A transfer back to this machine is `transfer --host
box --to local`.

### Moving a session between machines

Same guarantees as `sessions.Move`: the source is untouched until the
destination holds a verified copy, and a failure before that leaves the
destination without leftovers.

Items: exactly Move's (`projects/<enc>/<id>.jsonl`, `projects/<enc>/<id>/`,
`file-history/<id>/`, `session-env/<id>/`, `tasks/<id>/`, `todos/<id>-*.json`,
`debug/<id>.txt`) plus the session's project memory folder
(`projects/<memory project>/memory/`, `memoryProject`). Nothing else:
`.claude.json`, `.credentials.json`, `sessions/` and `*.key` files can never
be in an archive, and the receiver rejects them by name as well.

1. **Pack** (the source, `sessions.Pack`, read-only): Move's refusals (open in
   the source's registry, may be open, invalid id). Then a header line `{proto,
   id, title, source: {home, config_dir, project_folder, memory_folder, cwd,
   checkout, checkout_real, git_root}, claude_version, git_branch, bytes}`
   (`proto` is `sessions.ArchiveFormat`, 1; `bytes` the files' total, the
   receiver's bound before the manifest arrives and the progress total) and
   a gzip'd tar (`archive/tar`, `compress/gzip`) of the items, paths relative
   to the config dir (memory as `projects/<memory folder>/memory/…`), whose
   last entry is `MANIFEST.json` (`{id, entries}`): per entry its path, type,
   mode, mtime, size and SHA-256, computed while streaming. A file that
   changes size while it is read aborts the pack.
2. **Receive** (the destination, `sessions.Unpack`):
   - checks that the account dir `choose` picked is still logged into that
     account, the id is in no projects folder there (`COLLISION`) and not open
     there (`LIVE`), and the checkout exists; prunes receipts older than 30
     days;
   - the session's folder is `projects/<enc(destination cwd)>/` (`EncodeCwd` of
     the real path: the cwd, checkout and git root are resolved through
     symlinks first, since Claude Code records and keys folders by the
     physical directory). A name over 200 characters is hashed by Claude, so it is
     only used when a folder with that prefix exists whose sessions confirm the
     cwd; else refused (`box's path for this project is too long for a new
     project folder; open claude there once, then transfer again`). Memory goes
     to `projects/<enc(destination git root)>/memory/`;
   - every entry is validated before anything is written: a relative path
     under one of this session's item roots, no `..`, no absolute path; regular
     files, directories and symlinks only (no hard links or devices); nothing
     written through a symlink the archive created; no more bytes than the
     manifest says. Anything else aborts (`INVALID`);
   - files are written under Move's temporary names and fsynced, the received
     bytes checked against the manifest before the path rewrite, renamed into
     place and verified like Move (against what was written);
   - commit: every rename done and verified. Only then is a receipt written to
     `received/<id>.json` (`{manifest_sha256, transcript_sha256, from, at}`) and
     `{committed: true}` returned, and only then does the source release;
   - end of input, a checksum mismatch, Ctrl-C on the controller (ssh ends, the
     agent sees end of input or SIGHUP), ENOSPC (`NO_SPACE`) or any error
     before the commit → the undo of Move: temporaries and created folders
     removed;
   - memory is merged after the commit with `mergeMemory` (never overwriting,
     MEMORY.md lines appended, conflicts reported); its failures are warnings,
     as in Move.
3. **Release** (the source, `agent release` or in process): deletes the source
   items only when they are unchanged since the pack and not open. The
   session's fingerprint (`sessions.Fingerprint`) is the SHA-256 of its
   manifest without the project memory entries: memory is merged, never
   moved, and another session of the project may write it meanwhile. The
   source computes it afresh over its items (sizes, mtimes and SHA-256 of the
   transcript and every side file, nothing compressed) and compares it with
   the one the destination received (`manifest_sha256` of `receive`'s
   answer); the source keeps no state between export and release.
   Otherwise both copies stay:
   `"Fix flaky clock tests" changed on this machine while it was being sent;
   both copies are kept (box has it as it was before the change)`. Exit 1.
4. **Dropped connection after the commit**: the source still has the session.
   Transferring it again to the same machine, the destination finds the id
   with a receipt (`{manifest_sha256, dir, transcript_sha256, from, at}`)
   whose fingerprint matches the new pack and whose transcript, in the
   receipt's `dir`, is still `transcript_sha256`, answers `{already: true,
   dir}`, and the source releases: `box already had "Fix flaky clock tests"
   from a transfer that lost its connection; finished it.` The retry chooses
   the account again and may pick another one; the receipt's `dir` still
   finds the first copy (`… "Fix flaky clock tests" (in ~/.claude-a) from …`),
   so the session is never received twice, and it resumes there.

Path rewrite, while the destination writes:

- Applies to every `.json` and `.jsonl` item (transcripts, subagent
  transcripts, task files, `custom-title.json`) and to memory files before the
  merge compares them (a file that differs only in paths is not a conflict).
  Never to `file-history/` (copies of the user's files) or any other file. The
  source's files are never modified.
- Pairs, longest source path first: the checkout (as written and resolved)
  → the destination's checkout; the main worktree (`git_root`) → the
  destination's; the source config dir's project folder and memory folder
  (`<config dir>/projects/<enc>`) → the destination's; the source config dir →
  the destination's account dir; the source home → the destination home. A
  pair whose two sides are equal is skipped.
- In JSON files both sides of a pair are matched in their JSON-encoded form
  (`"` and `\` and control characters escaped, as Claude writes them). A match
  counts only when neither the byte before nor the byte after is in
  `[A-Za-z0-9._-]`, so `/Users/murat/Code/shop` never matches inside
  `/Users/murat/Code/shop2` or `/mnt/Users/murat`. In JSON a match may also
  follow an escape (`\n`, `\t`, `\uXXXX`, an unescaped backslash
  introducing it) or a terminal code written as `\u001b[…m`: every path that
  starts a line of a tool's output follows one. One left-to-right pass,
  longest match first; replaced text is never matched again. Lines that are
  not valid JSON are rewritten the same way, never dropped.
- Everything is rewritten (prompts, tool input and output, `cwd` fields), so
  the conversation reads as if it ran on the destination. The report counts
  the replacements.

### Privacy

Added to the policy note: session transcripts leave a machine only through
`transfer`, to a machine you added, over your own ssh connection; the team
token reaches a host only on ssh's stdin, during `host add`. Nothing about
hosts reaches the Worker beyond what each machine already reports as itself.

### Tests

Landed: hosts.json validation and kept fields, the ssh argv (`--`,
`-t`/`-T`, socket path length), agent framing and failures over a fake ssh,
`--host` parsing (`=`, twice, after `--`, `local`), remote URL normalization,
the locate search order, bounds and refusals, the version rule (patch,
pre-release, development builds, either side newer, declined, lenient),
`host add`'s refusals and questions, `host list`, the agent verbs, and the
fallback install script run in /bin/sh; e2e: `host add` on a fresh and on an
already set up host (dev rename, tmux mode, a minor-version update), an
unreachable destination, `--host` forwarding (`use`'s picker on the
pseudo-terminal in the host's checkout, `accounts`, `configs`, `pin` without
a checkout, the note for other commands, a launch into the host's tmux), a
claim from the host shown as `on box`, `uninstall --host` restoring the
host's rc file, `host remove`.

Transfer: unit tests for the path rewrite (JSON escapes, word boundaries,
`shop` versus `shop2`, non-ASCII paths, one pass), pack, receive and
release (every item kind, memory merged and rewritten, `file-history/`
untouched, the retry after a lost answer, a changed source kept), archive
validation (`..`, absolute paths, hard links, devices, a write through an
archive symlink, oversize, forbidden names, other sessions' items, a
checksum mismatch, a truncated stream, another format: nothing left
behind), `switching.Choose`, the Claude Code version rule and the agent's
refusal of unregistered dirs; e2e: `transfer` to a host (resumed in its
tmux session), back with `--host box --to local --no-launch`, and a
receive whose answer is lost after the commit, then the retry.

Never against a real host except read-only probes (and the verification
above, which the user set up).

### Out of scope

- Moving or syncing code: the checkout must already be on the destination
  (git's job).
- Personal accounts across machines; stored path mappings; direct
  host-to-host connections (this machine relays); Windows hosts; fish shell
  integration on hosts (forwarding still works there: it never goes through
  the login shell).

### Verified before building transfer

On a Debian host with Claude Code 2.1.303, in a throwaway config dir signed
in once:

- A transcript moved to another checkout of the same repository, with every
  path rewritten (the `cwd` fields, prompts, tool input and output, the
  memory folder path, and the `api-request*` request log, whose content
  hashes are then stale), resumes from the destination checkout
  (`claude -p --resume <id>` remembered the conversation and reported the new
  directory).
- Rewind works after the move. A checkpoint is
  `file-history/<id>/<first 16 hex of sha256(absolute file path)>@v<n>`;
  the transcript's `file-history-snapshot` records name it by
  `backupFileName`, under the path relative to the project with
  `realParentDir` set to the absolute directory. Rewritten, `realParentDir`
  points at the destination while the backup keeps its name; rewinding
  restored the file in the destination checkout. So `file-history/` travels
  unchanged and `realParentDir` must be rewritten (it is, as a `.jsonl`
  field).
- What an older Claude Code does with a newer transcript was not tested; the
  user's rule replaces it (see `transfer` step 5).
- Project folder names start with `-`: shell commands that touch them need
  `./` or `--`.

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
| `probe:<email>` | POST /probes (only if `share:` exists, and only a newer `collected_at` than the stored probe) | `{session, week, collected_at, reporter}`, a window's `resets_at` null when the window is not running (then `used` is 0); `expirationTtl` 8 days |
| `claim:<email>:<dev>:<machine_id>` | PUT/DELETE claim of that holder; usage refresh by the same reporter when the claim is older than 1 h (KV write budget) | `{at}`; `expirationTtl` = CLAIM_TTL_MIN (min 60 s) |
| `exhausted:<email>:<dev>:<machine_id>` | PUT exhausted of that reporter | `{window, resets_at, at}` (`window` `session`/`week`, `resets_at` ISO or null, `at` server-stamped); `expirationTtl` = until `resets_at` when known (min 60 s), else 5 h for `session`, 7 d for `week` |

An email is shared iff `share:<email>` exists. The listing shows only shared
emails; orphaned `usage:`/`probe:`/`claim:` keys (written by a request that
raced an unshare) are ignored and expire on their own. Unshare deletes the
share key, the usage and probe keys and every claim and exhausted key under
`claim:<email>:` and `exhausted:<email>:`. Each key's metadata
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
  "numbers_at": "…",
  "source": "statusline",
  "collected_at": "…",
  "reporter": { "dev": "murat", "machine_id": "…" },
  "claims": [ { "dev": "murat", "machine_id": "…", "at": "…" } ]
}
```

`session`/`week` are the newer of the `usage:` record (status line) and the
`probe:` record (the probe on a tie), `numbers_at` when they were read and
`source` which record it was (`statusline` or `probe`; both absent with
neither). `collected_at`/`reporter` keep their meaning: the latest status
line report (absent before the first), the only sign that someone is on the
account, so they, never a probe, drive `in_use`, its age and a CLI's "in use
by you". A CLI that predates probes shows UPDATED from `collected_at`
(older than the numbers when a probe is newer, never newer). A window with
`resets_at` null comes only from a probe: the window is not running, `used`
and `effective` are 0 and `reset_passed` is false. Every listing (and the
POST /probes answer) carries `"probes": true`, the CLI's sign that this
Worker takes probes.

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
| PUT | `/accounts/:email/exhausted` | `{window: "session"\|"week", resets_at?: ISO\|null, reporter}` | 204; 404 when not shared; 400 on a bad window, timestamp or reporter; Bearer only (`?token=` is 401). `resets_at` is resolved at write time (see the exhausted rules below). One record per reporter: a second report replaces the first |
| POST | `/probes` | `{reporter, reports: [{email, session: {used, resets_at}, week: {used, resets_at}, collected_at}]}` (1–32 reports, one per email; body up to 16 KiB); `dev`, `tz`, `listing` | 200: the GET /accounts JSON document ranked for `dev` (default the reporter's dev) with the batch merged in, plus `not_shared` (reported emails not on the allowlist, nothing stored for them); with `listing=0` only `{"probes": true, "not_shared": […]}`, without the list operation; 400 on a bad body (a `resets_at` null with `used` above 0 included); Bearer only. Each shared report writes `probe:<email>` when its clamped `collected_at` is newer than the stored probe's (an equal one is a resend). The batch is merged over the listing before ranking because listings lag writes |

Ranking and state: the windows are the account's numbers above (the newer of
the `usage:` and `probe:` records). Holders with `at` older than
`CLAIM_TTL_MIN` are ignored. `busy_by` = sorted unique devs ≠ querying `dev`
that hold a fresh claim or sent a status line report within
`ACTIVITY_TTL_MIN` (all such devs when no `dev` query); probes never count
and never refresh a claim.
`exhausted`: any present window with effective usage ≥ 100 (Claude refuses
work until it resets), or an `exhausted:` record that still counts
(`worker/src/exhausted.ts`). A record's stored `resets_at` is the reported
one when believable, else the reset of the same window in the account's
numbers (the newer of the usage and probe records) when believable, else
null; believable means in the future and at most one window
length plus an hour away (a misparsed far-future date must not pin an
account). A record counts until its `resets_at`, or until `at` plus one
window length (5 h / 7 d) when the reset is unknown, and is superseded
earlier when the usage window of the same name resets more than an hour
after that end (a new window started; the hour absorbs the refusal message's
minute precision against the status line's seconds). A newer usage report
under 100% never clears a record, because Claude Code keeps feeding
pre-refusal numbers after a refusal. `exhausted_until` =
the latest known reset among the windows at 100% and the valid records (null
when none is known); `exhausted_window` = `session`/`week` of that reset, or
of the records when no reset is known (`week` when both), null when not
exhausted. A window that is not running never blocks, and sorts like an
unknown session reset; numbers read after a record (a probe) that show its
window not running end the record (nothing in that window can be refused),
earlier ones do not. `state`: `exhausted` first, else `in_use`
when another dev reported within the activity TTL, `claimed` when only fresh
claims by others exist, else `free`; `busy_by` is populated in every state.
Sort: not exhausted first → not busy first → known usage before unknown →
effective session asc → effective week asc → session reset asc → email asc
(exhausted accounts order by `exhausted_until` asc first, unknown last). The CLI's
`accounts` also appends `syncing (just shared)` rows for emails this machine
shared within `sharedcache.LocalAddGrace` that the listing does not show yet. JSON: `claims` always an array,
`busy_by` always an array. `nickname` is always present (null for legacy
records). Text columns: `#  NICK  ACCOUNT  SESSION  WEEK  STATE  UPDATED`
(NICK `-` when null; the CLI adds LOCAL). Text STATE: `free`,
`in use by ali (12m)`, `claimed by ali, can`, `exhausted (week resets Tue
20:00)`, `exhausted (week)` when the reset is unknown.

Other v1 rules (validation, body limit, timestamps, text format, corrupt and
non-email keys skipped) stay.

## Repo plumbing

- Module `github.com/muratgozel/julienning`, Go 1.25 (toolchain local),
  dependency `golang.org/x/term` only.
- CI: gofmt, vet, `go test -race`, worker typecheck + tests.
- `make e2e` (`scripts/e2e.sh`): end-to-end in a throwaway HOME against
  `wrangler dev`, a fake releases host (it also serves the installer at
  `/raw/main/scripts/install.sh`) and a fake `tmux` (first on PATH, so the
  machine's tmux server is never reached), plus a second throwaway home that
  plays a host behind a fake `ssh` (it drops the options and the destination
  and runs the command with `/bin/sh -c` under that home, with a minimal
  PATH, keeping the pseudo-terminal; a marker file makes it lose the next
  `receive`'s answer, for the retry check); manual, not in CI.
- Release: goreleaser on tags. Deploy-worker workflow unchanged.
- No secrets in the repo: team token is distributed out of band.

## Non-goals

- No handling of Claude auth tokens or the `sessions/*.key` files.
- No Windows, no fish.
- No usage history; KV holds the latest snapshot.
