# julienning: notes for coding agents

This file holds what the code and git history do not say. Design and the
CLI/Worker contract: `docs/SPEC.md`. Every command and output:
`docs/REFERENCE.md`. Worker operations: `docs/CLOUDFLARE.md`. `README.md` is a
quick start; keep it short and put detail in REFERENCE.md.

## Toolchain and dependencies

- Run Go as `GOTOOLCHAIN=local go` (the Makefile does). `go.mod` pins
  `go 1.25.0` and `golang.org/x/term v0.45.0`; a plain `go get -u` bumps the go
  directive to a toolchain CI does not install. Bump x/term only together with
  the go directive, on purpose.
- Go uses the standard library plus x/term, nothing else. The Worker uses
  wrangler, vitest and the Cloudflare vitest pool, nothing else.
  `compatibility_date` in `worker/wrangler.jsonc` is pinned to the workerd that
  ships with the installed wrangler; move the two together.
- `make test` is the gate (Go with `-race`, Worker vitest). `make lint` runs
  gofmt, go vet and the Worker typecheck. CI runs both plus shellcheck on
  `scripts/install.sh` and `scripts/e2e.sh`, and `goreleaser check`. The
  runner's preinstalled shellcheck is older than a current local one and
  flags things 0.11 does not (SC2015, `a && b || c`): write such checks as
  `if`.

## Hard rules

- Tests never touch the real home. Anything that reads config, settings.json,
  rc files or Claude dirs sets `HOME` (and `XDG_*`, `JULIENNING_*`) to a
  `t.TempDir()`. Never run `bin/julienning setup` or `uninstall` against the
  user's machine yourself; that is a manual step the user does. For end-to-end
  checks run `make e2e` (see below) or extend it; never improvise against the
  real home.
- Never add `"remote": true` to `worker/wrangler.jsonc`: it points `wrangler
  dev` and the test suite at the production KV namespace.
- Tests never reach a real tmux. `tmux new-window -t =claude:` succeeds
  against the developer's own server (and switches every attached client to
  the new window) even when the attach after it fails. Go tests inject
  `findTmux` (dirs) or run the launch helper with PATH starting at a fake bin
  dir and no fallback dirs (`internal/launch/tmux_test.go` TestMain refuses
  otherwise); shell tests bake a fake tmux's path; e2e's fake tmux is first
  on PATH from the start.
- Tests never run a real ssh or reach a real host. `sshx.Binary` points at a
  fake in `internal/sshx` tests; `internal/cli/hosts` tests replace
  `agentCall`, `listCall`, `runScript`, `runForwarded` and the agent's
  `findClaude`/`findTmux`. Never run `host add` or a `--host` command against
  a real machine yourself; probe hosts read-only, and only when asked.
- The team token exists only in `~/.julienning/config.json` (mode 0600), the
  Cloudflare secret `AUTH_TOKEN` and the gitignored `worker/.dev.vars`. Never
  print, log or commit it. `errors.log` must stay free of emails too.
- Never read Claude credentials: `sessions/*.key`, OAuth tokens, or anything in
  `.claude.json` other than `oauthAccount.emailAddress`.
- Edits to files julienning does not own are byte-preserving and reversible:
  `settings.json` through `internal/jsonedit`, rc files through the single line
  marked `# julienning-shell-hook`. `uninstall` and `forget` must undo exactly
  what `setup` added; the tests compare bytes. The exceptions are what the
  user chose: the session move they confirm (the picker, and `uninstall`
  into `~/.claude`) goes through `sessions.Move`, and `sync` edits
  settings.json keys (`claudecfg.EditSettings`) and `CLAUDE.md`; neither is
  undone.
- CI runs in UTC. A test whose expectation is in local time sets
  `t.Setenv("TZ", "Europe/Istanbul")`. `JULIENNING_NOW_EPOCH` freezes the
  clock; `sharedcache.nowFunc` and `tui.confirmClock` are the in-process seams.
- Secrets in GitHub Actions come from repository secrets only; the repo is
  public and workflow logs are world-readable.

## End-to-end harness

`make e2e` runs `scripts/e2e.sh` (about ten seconds): a throwaway `HOME`
with fake Claude config dirs, a fake `claude` and a fake `tmux` (logs its
argv to `$T/tmux.log`), `wrangler dev` on a local
KV namespace with its own token, and a fake GitHub releases host
(`scripts/e2e/fakereleases.go`, `//go:build ignore`, run via `go build`). It
drives the installer, setup, shell functions, switching, the session picker
and a move on a real pseudo-terminal (`scripts/e2e/ptydrive.go`, same build
style: `expect=`/`send=`/`sleep=` steps, stdlib ioctls for darwin and linux),
personal accounts and pins through the generated `claude` function, tmux
mode (`use` and the shell functions on the pseudo-terminal, in place inside
tmux and without a terminal), `sync`
(its questions on the pseudo-terminal, `--check`, and `--from` undoing the
first run byte for byte), the session hooks with a fake live session (a `sleep` registered in
`sessions/<pid>.json`) and a `dev` rename moving its claim, the status line and its detached usage report,
hosts (a second throwaway home `$T/host` behind a fake `ssh` in `fakebin/`
that drops the options and destination and runs the command with `/bin/sh
-c` under that home, PATH `$T/host/.local/bin:/usr/bin:/bin`, so a real
claude or tmux elsewhere on the machine is never used; its rc file ends with
a provisioning `claude()` function: `host add` with the real installer from
the fake releases host, `--host` forwarding with the picker on the
pseudo-terminal, a launch into the host's fake tmux, a minor-version update
of the host, `transfer` both ways and a retry after `touch $T/drop-receive`
made the fake ssh lose a receive's answer, `uninstall --host`, `host remove`),
`forget`, `update` and `uninstall` (its session move into `~/.claude`, with
an open session left behind, and plain `claude` checked native in a new shell
and in one that loaded the integration before), and asserts byte-for-byte
restoration at the end. Run it after any change to setup, uninstall, forget, the installer,
self-update, claims, switching, pins or the Worker API; add a check there
when you add a user-visible flow. It is not in CI (it needs a Worker
process); say in the final message whether it ran. On failure the sandbox is
kept and its path printed; `E2E_KEEP=1` keeps it always. Pass `TERM`
explicitly to anything run under ptydrive (`TERM=dumb` selects the numbered
fallback picker).

## Overrides for tests and sandboxes

`JULIENNING_HOME` (state dir), `JULIENNING_BIN_DIR`, `JULIENNING_VERSIONS_DIR`,
`JULIENNING_REMOTE_URL`, `JULIENNING_TOKEN`, `JULIENNING_NOW_EPOCH`,
`JULIENNING_AUTO_UPDATE`, `JULIENNING_RELEASES_BASE` (fake GitHub releases
host for update tests), `JULIENNING_REPO`, `JULIENNING_VERSION` (installer).
Env overrides are read at runtime and never written to `config.json`.

## Contracts that span files

- Release assets: `.goreleaser.yaml`, `scripts/install.sh` and
  `internal/selfupdate` agree on `julienning_<ver>_<os>_<arch>.tar.gz` plus
  `checksums.txt`, binary at the archive root. Change all three or none.
- CLI to CLI across machines (SPEC "Hosts"): the agent protocol is
  `internal/hosts/proto.go` (frame line, `Proto`, request and answer types)
  plus `internal/cli/hosts/agent.go`. A change to the agent protocol or the
  transfer archive format bumps julienning's minor version (cross-machine
  commands need the same major.minor), and `hosts.Proto` with it when a verb
  changes incompatibly. Within one proto verbs only gain optional fields;
  `hello`, `inventory` and the `run` envelope are frozen (fields only added),
  so `host add`, `host list`, `update --host` and `uninstall --host` keep
  working across versions. Answers start with `JULIENNING-AGENT/<proto>`
  (startup-file noise before it is dropped); the team token travels only on
  stdin; this machine's `JULIENNING_*` overrides never reach a host
  (`sshx.sshEnv`). `hosts.json` keeps fields it does not know on save.
  The transfer archive (`sessions.ArchiveFormat`, header line + gzip'd tar
  with `MANIFEST.json` last) is part of that contract: `sessions.Pack`,
  `sessions.Unpack` and `sessions.Release` must agree on the manifest
  (Release re-packs and compares its SHA-256, so the manifest must stay
  deterministic: entry order, JSON encoding). `agent receive` takes the
  controller's request line ahead of the export.
- CLI to Worker: the Worker section of `docs/SPEC.md`. The team lead deploys
  the Worker by hand and teammates' CLIs auto-update within a day, so a newer
  CLI meets an older Worker and vice versa. New Worker fields are optional,
  existing fields keep their meaning, and the CLI tolerates missing fields.
- Worker storage: one KV key per fact (`share:`, `usage:`, `claim:`,
  `exhausted:`), all data
  in metadata so `GET /accounts` is a single `list()`. No request may
  read-modify-write a key another request writes; `worker/test/race.test.ts`
  guards this.
- Shell: the `claude` wrapper and the `claude-<nick>` functions from
  `internal/shell` resolve dirs at call time through the hidden `resolve-dir`
  command, so a rename needs no re-source. A new nickname does need one.
  They call julienning by the absolute path the rc line uses
  (`paths.StableCommand`; `command julienning` only for a build run from
  the repo). Setup rewrites julienning's rc line in place and never moves
  it: its position decides whether a provisioning `claude()` function above
  it is overridden.
  The wrapper tests for `pins.json` by name (`internal/shell` cannot import
  `config`; a test pins it to `config.PinsFile`) and calls `resolve-dir
  --pin` only while it exists.
- tmux mode has two implementations of one launch rule: `launch.ExecTmux`
  (Go: use, next, login, new-config) and `_julienning_claude` in
  `internal/shell` (the wrapper and `claude-<nick>`, tmux path and session
  baked in by `shell-init`). They share `shell.WindowScript`; change both or
  neither. The account is always explicit on the window's command line, every
  target is `=name` (exact), and in shell code it is quoted (zsh expands an
  unquoted `=word` to a command path). tmux 2.1 is the minimum (checked
  against tmux's CHANGES; see SPEC "tmux mode").
- Pins and personal accounts: a pin that cannot be resolved must fail (exit
  1), never print nothing, or the wrapper falls back to the selection and
  opens a pinned project on another account. A personal account
  (`resolve.IsPersonal`: not on the allowlist, or with no allowlist known not
  logged into any registered dir) never becomes `current`, is never
  registered or patched by `use`/`pin`, and never feeds the team picker,
  whose sources are only dirs logged into shared accounts
  (`switching.sharedSources`).
- Ranking (Worker `rank.ts`, mirrored in REFERENCE.md): not exhausted, then
  not busy, then known usage before unknown, then session used ascending, week
  ascending, reset ascending, email. The Worker leaves the querying dev's own
  claims out of `state`, on purpose, so `next` never avoids your own account;
  the CLI adds `in use by you` back.

## Claude Code internals we depend on

Observed on Claude Code 2.1.29x; none of it is documented, so expect drift.
Each item names the only package that touches it.

- Account email: `<dir>/.claude.json`, key `oauthAccount.emailAddress`. The
  default dir keeps it in `~/.claude.json`. `CLAUDE_CONFIG_DIR=~/.claude` is
  not the same as unset; launchers unset the variable for the default dir
  (`internal/claudecfg`, `internal/launch`).
- Live sessions: `<dir>/sessions/<pid>.json` with pid, sessionId, cwd, status
  and `procStart` as a UTC ctime string, cross-checked against `ps`
  (`internal/livesess`).
- Session files: `<dir>/projects/<encoded cwd>/<id>.jsonl` (non-alphanumerics
  become `-`, long paths are truncated and hashed) plus the `<id>/`,
  `file-history/<id>/`, `session-env/<id>/` and `tasks/<id>/` dirs; a move
  carries all of them and merges auto-memory, which is keyed by git root
  (`internal/sessions`). Titles come from `custom-title`, `ai-title`,
  `last-prompt`, then the legacy `summary` entry. Entries carry `version`
  (the Claude Code that wrote them) and `gitBranch`; transfer reads the last
  of each.
- Checkpoints (2.1.303): `file-history/<id>/<first 16 hex of sha256(absolute
  path)>@v<n>`, named in the transcript's `file-history-snapshot` records by
  `backupFileName` under the project-relative path with `realParentDir`
  absolute. A transfer copies `file-history/` unchanged and rewrites
  `realParentDir`; rewind then restores into the destination (verified on a
  real host). The `api-request*` records are a hashed request log; rewriting
  paths in them did not stop a resume.
- Settings sync (`internal/teamsync` decides, `internal/claudecfg` writes):
  top-level `settings.json` keys `model`, `effortLevel`, `promptCacheTtl`,
  `subagentPromptCacheTtl` (`"5m"`/`"1h"`), `autoContinueAtUsageLimit`,
  `theme`, `editorMode`, and the user-level `<dir>/CLAUDE.md`. The list and
  each key's fixed answers and default were chosen by the user; change them
  only on request, in `teamsync.Settings` and both docs together. The
  installed claude binary embeds its settings schema (`strings` on it and
  grep `<key>:()=>`) when a key needs checking.
- Status line stdin JSON: `rate_limits.five_hour` and `seven_day` with
  `used_percentage` and `resets_at`; an unexpected shape is reported as
  `USAGE_INVALID` with a type signature (`internal/usage`).
- Hooks `SessionStart` and `SessionEnd` drive claims (`internal/claims`); the
  `StopFailure` hook with matcher `rate_limit` (stdin: `error`,
  `error_details`, `last_assistant_message`) drives exhausted reports
  (`internal/cli/accounts/hook.go`); the refusal text it parses for the window
  and reset (`You've hit your weekly limit · resets Oct 13 at 8pm
  (Europe/Istanbul)`) lives in `internal/usage/refusal.go`. Claude Code
  reports `used_percentage` above 100 once a limit is exceeded; never reject
  such values, clamp them.

## Releasing and docs

- CLI: `make release VERSION=X.Y.Z` (clean tree, on main, tests pass) tags and
  pushes; GitHub Actions builds and publishes. Check CI is green first.
- Worker: manual only, Actions tab, workflow `deploy-worker` (or
  `npm run deploy` in `worker/`). Whenever a change needs a Worker redeploy,
  say so explicitly in the final message; nothing deploys on merge.
- A behaviour change updates `docs/SPEC.md`, `docs/REFERENCE.md` and, if a
  teammate would notice it, `README.md`, in the same commit.
- Conventional commits. Reviews so far found most bugs at boundaries: clock
  and timezone edges, files changed under us, split terminal escape sequences,
  and Worker races. Add a regression test with every fix.
