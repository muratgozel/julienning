---
name: release
description: julienning's release protocol. Use when a fix, feature or other user-visible change is finished and verified (offer a release before the final message), and when the user asks to release, ship, publish, merge and push, or put changes live. Covers branch, commit, local merge, version bump, `make release` (pushes main and the tag) and the Worker deploy.
---

# Releasing julienning

Pushing main does not reach anyone. Teammates run released binaries: their
CLIs auto-update from GitHub releases within a day, and `julienning update`
installs only releases. The Worker deploys only when someone runs the
`deploy-worker` workflow. A change is live once it is in a release and, if
it touched the Worker, deployed.

## When to offer

- A finished change whose commits include `fix`, `feat`, `perf` or a breaking
  change (`!`, `BREAKING CHANGE:`): end with an offer, e.g. "Release this as
  v0.13.3 (Worker deploy needed: yes)?". Do not release without a yes.
- The user asks to merge, push, ship or put something live: that request
  includes the release. Offer it in the same step; never push main and leave
  the release out without saying so.
- Only `docs`, `test`, `chore`, `refactor`, `ci`, `style` or `build` commits
  since the last tag: no release. Merge and `git push origin main`, unless the
  user asks for a release anyway.

A yes covers the whole flow below, the Worker deploy included.

## Protocol

1. **Verify.** `make lint` and `make test` pass; `make e2e` ran if the change
   touches what CLAUDE.md lists for it (setup, uninstall, claims, switching,
   the Worker API, ...). Docs updated in the same commit (CLAUDE.md
   "Releasing and docs").
2. **Branch.** From an up-to-date main (`git fetch`; main must equal
   `origin/main`): `git checkout -b <user>/<topic>`, e.g.
   `murat/drop-session-window`. The branch stays local.
3. **Commit.** Conventional commit, `<type>[scope]: <description>`, a body
   that says why, the session's attribution trailer.
4. **Merge locally.** `git checkout main && git merge --ff-only <branch>`
   (rebase the branch on main first if that refuses), then
   `git branch -d <branch>`.
5. **Pick the version.** `last=$(git describe --tags --abbrev=0)`, then
   `git log --oneline "$last"..HEAD`. The highest bump wins:
   - breaking change: major. Ask the user before cutting it.
   - `feat`: minor.
   - `fix`, `perf`: patch.
   - any change to the host agent protocol or the transfer archive
     (`internal/hosts/proto.go`, `internal/cli/hosts/agent*.go`,
     `sessions.ArchiveFormat`): at least minor, whatever the type (CLAUDE.md
     "Contracts that span files"). Commands that reach another machine need
     the same major.minor on both, so say in the offer that hosts will need
     `julienning update --host`.
6. **Worker deploy needed?**
   ```sh
   sha=$(gh run list --workflow deploy-worker --status success --limit 1 --json headSha --jq '.[0].headSha')
   git diff --quiet "$sha" HEAD -- worker/src worker/wrangler.jsonc worker/package.json worker/package-lock.json || echo "deploy needed"
   ```
   No successful deploy on record, or `gh` failing, means: ask the user.
7. **Release.** `make release VERSION=X.Y.Z`. It runs `make lint test`, tags
   `vX.Y.Z` and pushes main and the tag atomically. GitHub has answered
   pushes with `Internal Server Error` before. A failed push removes the local
   tag and publishes nothing, so run the same command again.
8. **Deploy the Worker** (when step 6 says so), right after the push:
   ```sh
   gh workflow run deploy-worker --ref main
   sleep 5; id=$(gh run list --workflow deploy-worker --limit 1 --json databaseId --jq '.[0].databaseId')
   gh run watch "$id" --exit-status
   ```
   Do it in the same turn, not later: the Worker must accept the new CLI
   before teammates' CLIs update. By contract it stays compatible with older
   CLIs, so deploying it early is safe. The workflow stays manual on purpose
   (see its header); do not add a push trigger.
9. **Watch.** `gh run list --workflow release --limit 1`, then
   `gh run watch <id> --exit-status`; also the `ci` run on main. If the
   release workflow fails, report it with the failing step's log and fix
   forward with the next patch version. A pushed tag is public: never move,
   delete or re-push it without asking.
10. **Report.** In the final message, give:
    - the version and what is in it, with the release URL
      (`gh release view vX.Y.Z --json url --jq .url`);
    - whether the Worker was deployed, with its version id, and the CI
      result;
    - whether `make e2e` ran.

    Teammates get the release within a day; `julienning update` installs it
    now.

## Never

- Release from a dirty tree, from a branch other than main, or with failing
  lint/tests (`make release` refuses these; do not work around it).
- Force-push main, or move or delete a pushed tag.
- Push main for a fix or feature and stop there without offering the
  release.
