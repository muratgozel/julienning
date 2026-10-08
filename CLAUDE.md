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
  `scripts/install.sh` and `goreleaser check`.

## Hard rules

- Tests never touch the real home. Anything that reads config, settings.json,
  rc files or Claude dirs sets `HOME` (and `XDG_*`, `JULIENNING_*`) to a
  `t.TempDir()`. Never run `bin/julienning setup` or `uninstall` against the
  user's machine yourself; that is a manual step the user does. For end-to-end
  checks run `make e2e` (see below) or extend it; never improvise against the
  real home.
- Never add `"remote": true` to `worker/wrangler.jsonc`: it points `wrangler
  dev` and the test suite at the production KV namespace.
- The team token exists only in `~/.julienning/config.json` (mode 0600), the
  Cloudflare secret `AUTH_TOKEN` and the gitignored `worker/.dev.vars`. Never
  print, log or commit it. `errors.log` must stay free of emails too.
- Never read Claude credentials: `sessions/*.key`, OAuth tokens, or anything in
  `.claude.json` other than `oauthAccount.emailAddress`.
- Edits to files julienning does not own are byte-preserving and reversible:
  `settings.json` through `internal/jsonedit`, rc files through the single line
  marked `# julienning-shell-hook`. `uninstall` and `forget` must undo exactly
  what `setup` added; the tests compare bytes.
- CI runs in UTC. A test whose expectation is in local time sets
  `t.Setenv("TZ", "Europe/Istanbul")`. `JULIENNING_NOW_EPOCH` freezes the
  clock; `sharedcache.nowFunc` and `tui.confirmClock` are the in-process seams.
- Secrets in GitHub Actions come from repository secrets only; the repo is
  public and workflow logs are world-readable.

## End-to-end harness

`make e2e` runs `scripts/e2e.sh` (about ten seconds): a throwaway `HOME`
with fake Claude config dirs and a fake `claude`, `wrangler dev` on a local
KV namespace with its own token, and a fake GitHub releases host
(`scripts/e2e/fakereleases.go`, `//go:build ignore`, run via `go build`). It
drives the installer, setup, shell functions, switching, the session hooks
with a fake live session (a `sleep` registered in `sessions/<pid>.json`),
the status line and its detached usage report, `forget`, `update` and
`uninstall`, and asserts byte-for-byte restoration at the end. Run it after
any change to setup, uninstall, forget, the installer, self-update, claims or
the Worker API; add a check there when you add a user-visible flow. It is
not in CI (it needs a Worker process); say in the final message whether it
ran. On failure the sandbox is kept and its path printed; `E2E_KEEP=1` keeps
it always. The session picker is not covered; it needs a terminal, see the
`internal/tui` and `internal/sessions` tests.

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
  `last-prompt`, then the legacy `summary` entry.
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
