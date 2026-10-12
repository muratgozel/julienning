#!/usr/bin/env bash
# End-to-end check of the installed tool against a real local Worker, in a
# throwaway HOME. Nothing on the machine is read or written outside that
# directory, the Go build caches and worker/node_modules.
#
#   make e2e                 # or: bash scripts/e2e.sh
#   E2E_KEEP=1 make e2e      # keep the sandbox dir for inspection
#   E2E_WORKER_PORT=8799     # port for `wrangler dev` (default 8799)
#
# What it proves, in order: the installer against a fake GitHub releases host
# (scripts/e2e/fakereleases.go), non-interactive setup over fake Claude config
# dirs, shell functions, switching, the session picker and a move on a real
# pseudo-terminal (scripts/e2e/ptydrive.go), personal accounts opened by
# email, directory pins through the claude wrapper, the status line, usage
# reports, claims driven by the session hooks and a fake live session,
# forget, self-update, and that uninstall restores every file setup touched
# byte for byte.
#
# Needs go, node (worker/node_modules installed, `npm ci` runs if missing),
# bash, curl, tar, sha256sum or shasum, ps and pgrep. The sandbox is kept
# when a check fails; its path is printed.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
module="github.com/muratgozel/julienning"
keep="${E2E_KEEP:-0}"
wport="${E2E_WORKER_PORT:-8799}"

# --- helpers -----------------------------------------------------------------
checks=0
ok() {
  checks=$((checks + 1))
  printf 'ok %2d  %s\n' "$checks" "$1"
}

fail() {
  printf '\nFAIL  %s\n' "$1" >&2
  if [ -n "${2:-}" ]; then printf '%s\n' "$2" >&2; fi
  if [ -f "${JULIENNING_HOME:-/nonexistent}/errors.log" ]; then
    printf '\n--- errors.log ---\n' >&2
    cat "$JULIENNING_HOME/errors.log" >&2
  fi
  exit 1
}

# run CMD...: runs it, keeps combined output in $out, fails on a non-zero exit.
out=""
run() {
  local rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ]; then fail "'$*' exited $rc" "$out"; fi
}

# run_fail CMD...: like run but the command must fail.
run_fail() {
  local rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then fail "'$*' succeeded, expected a failure" "$out"; fi
}

expect_contains() { # LABEL NEEDLE HAYSTACK
  case "$3" in
    *"$2"*) ok "$1" ;;
    *) fail "$1: expected to find '$2' in:" "$3" ;;
  esac
}

expect_missing() { # LABEL NEEDLE HAYSTACK
  case "$3" in
    *"$2"*) fail "$1: did not expect '$2' in:" "$3" ;;
    *) ok "$1" ;;
  esac
}

expect_same() { # LABEL FILE_BEFORE FILE_NOW
  if cmp -s "$2" "$3"; then
    ok "$1"
  else
    fail "$1: $3 differs from $2:" "$(diff "$2" "$3" || true)"
  fi
}

# wait_for LABEL SECONDS NEEDLE CMD...: polls CMD until its output contains
# NEEDLE (background work: detached claim-sync, wrangler startup).
wait_for() {
  local label="$1" secs="$2" needle="$3" i
  shift 3
  for ((i = 0; i < secs * 2; i++)); do
    out="$("$@" 2>&1 || true)"
    case "$out" in *"$needle"*)
      ok "$label"
      return
      ;;
    esac
    sleep 0.5
  done
  fail "$label: no '$needle' after ${secs}s; last output:" "$out"
}

# row NICK: the `accounts` table row for that nickname.
row() {
  julienning accounts 2>/dev/null | awk -v n="$1" '$2 == n' || true
}

# account_json NICK: that account's object from `accounts --json` (pretty-printed,
# one account per 4-space-indented object).
account_json() {
  julienning accounts --json 2>/dev/null | awk -v n="$1" '
    /^    \{/ { buf = ""; collecting = 1 }
    collecting { buf = buf $0 "\n" }
    /^    \}/ { collecting = 0; if (index(buf, "\"nickname\": \"" n "\"")) print buf }' || true
}

kill_tree() {
  local pid="$1" child
  for child in $(pgrep -P "$pid" 2>/dev/null || true); do kill_tree "$child"; done
  kill "$pid" 2>/dev/null || true
}

wrangler_pid=""
host_pid=""
session_pid=""
T=""
cleanup() {
  local rc=$?
  for p in "$session_pid" "$host_pid" "$wrangler_pid"; do
    if [ -n "$p" ]; then kill_tree "$p"; fi
  done
  if [ -z "$T" ]; then return; fi
  if [ "$rc" -ne 0 ] || [ "$keep" = 1 ]; then
    printf '\nsandbox kept at %s (wrangler.log, home/.julienning/errors.log)\n' "$T"
  else
    chmod -R u+w "$T" 2>/dev/null || true
    rm -rf "$T"
  fi
}
trap cleanup EXIT
# set -e exits without a word; name the line first.
trap 'printf "\ne2e: command failed at line %s\n" "$LINENO" >&2' ERR

# --- prerequisites -------------------------------------------------------------
for tool in go node npm curl tar ps pgrep awk cmp; do
  command -v "$tool" >/dev/null 2>&1 || { echo "e2e: $tool is required" >&2; exit 2; }
done
if command -v sha256sum >/dev/null 2>&1; then
  sha_cmd=(sha256sum)
else
  sha_cmd=(shasum -a 256)
fi
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "e2e: unsupported OS $(uname -s)" >&2; exit 2 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "e2e: unsupported arch $(uname -m)" >&2; exit 2 ;;
esac
if [ ! -x "$repo/worker/node_modules/.bin/wrangler" ]; then
  echo "e2e: installing worker dependencies (npm ci)"
  (cd "$repo/worker" && npm ci --no-audit --no-fund >/dev/null)
fi

# Go must keep using the real caches after HOME moves into the sandbox.
GOMODCACHE="$(go env GOMODCACHE)"
GOCACHE="$(go env GOCACHE)"
GOPATH="$(go env GOPATH)"
export GOMODCACHE GOCACHE GOPATH GOTOOLCHAIN=local GOFLAGS=-mod=mod

# --- sandbox -----------------------------------------------------------------------
T="$(mktemp -d "${TMPDIR:-/tmp}/julienning-e2e.XXXXXX")"
T="$(cd "$T" && pwd -P)" # macOS: /var/... is a symlink to /private/var/...
echo "e2e: sandbox $T"
mkdir -p "$T/home/.local/bin" "$T/fakebin" "$T/proj" "$T/before" "$T/worker" "$T/releases"

export HOME="$T/home" USER=e2e SHELL=/bin/bash NO_COLOR=1 CI=true WRANGLER_SEND_METRICS=false
export JULIENNING_HOME="$T/home/.julienning"
export JULIENNING_BIN_DIR="$T/home/.local/bin"
export JULIENNING_VERSIONS_DIR="$T/home/.local/share/julienning/versions"
export PATH="$T/home/.local/bin:$T/fakebin:$PATH"
# setup patches the file bash reads on this OS (rcPath in internal/cli/dirs/setup.go).
case "$os" in darwin) rc="$HOME/.bash_profile" ;; *) rc="$HOME/.bashrc" ;; esac
unset CLAUDE_CONFIG_DIR XDG_DATA_HOME XDG_CONFIG_HOME JULIENNING_REMOTE_URL JULIENNING_TOKEN \
  JULIENNING_NOW_EPOCH JULIENNING_AUTO_UPDATE JULIENNING_RELEASES_BASE JULIENNING_REPO JULIENNING_VERSION

# A claude that only reports which config dir it was started with.
# shellcheck disable=SC2016 # the variables are for the fake claude to expand
printf '#!/bin/sh\necho "FAKE CLAUDE dir=${CLAUDE_CONFIG_DIR:-<unset>} args=$*"\n' >"$T/fakebin/claude"
chmod +x "$T/fakebin/claude"

# Three logged-in Claude config dirs: the default one stays personal.
mkdir_claude() { # DIR EMAIL ACCOUNT_FILE
  mkdir -p "$1/projects"
  printf '{"oauthAccount":{"emailAddress":"%s"},"numStartups":3}' "$2" >"$3"
  printf '{\n  "model": "opus",\n  "statusLine": {\n    "type": "command",\n    "command": "~/bin/my-statusline.sh"\n  },\n  "theme": "dark"\n}\n' >"$1/settings.json"
}
mkdir_claude "$HOME/.claude" personal@example.com "$HOME/.claude.json"
mkdir_claude "$HOME/.claude-work" alpha@example.com "$HOME/.claude-work/.claude.json"
mkdir_claude "$HOME/.claude-two" beta@example.com "$HOME/.claude-two/.claude.json"
# A second personal account in its own dir, never registered.
mkdir_claude "$HOME/.claude-side" side@example.com "$HOME/.claude-side/.claude.json"
# A dir an older julienning patched: statusLine and session hooks, no StopFailure.
mkdir -p "$HOME/.claude-old/projects"
printf '{"oauthAccount":{"emailAddress":"gamma@example.com"}}' >"$HOME/.claude-old/.claude.json"
old_layout() {
  printf '{\n  "model": "opus",\n  "statusLine": {\n    "type": "command",\n    "command": "%s statusline"\n  },\n  "hooks": {\n    "SessionStart": [\n      {\n        "hooks": [\n          {\n            "type": "command",\n            "command": "%s hook session-start"\n          }\n        ]\n      }\n    ],\n    "SessionEnd": [\n      {\n        "hooks": [\n          {\n            "type": "command",\n            "command": "%s hook session-end"\n          }\n        ]\n      }\n    ]\n  }\n}\n' \
    "$JULIENNING_BIN_DIR/julienning" "$JULIENNING_BIN_DIR/julienning" "$JULIENNING_BIN_DIR/julienning" >"$HOME/.claude-old/settings.json"
}
old_layout
cat >"$rc" <<'RC'
# hand-written before julienning
alias claude-two='CLAUDE_CONFIG_DIR=~/.claude-two claude'
export PATH="$HOME/.local/bin:$PATH"
RC
cp "$rc" "$T/before/rc"
cp "$HOME/.claude/settings.json" "$T/before/personal.json"
cp "$HOME/.claude-side/settings.json" "$T/before/side.json"
cp "$HOME/.claude.json" "$T/before/claude.json"
cp "$HOME/.claude-work/settings.json" "$T/before/work.json"
cp "$HOME/.claude-two/settings.json" "$T/before/two.json"

# --- local Worker ------------------------------------------------------------------
token="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"
compat="$(sed -n 's/^[[:space:]]*"compatibility_date":[[:space:]]*"\([^"]*\)".*/\1/p' "$repo/worker/wrangler.jsonc")"
[ -n "$compat" ] || fail "cannot read compatibility_date from worker/wrangler.jsonc"
# Our own config so the repo's .dev.vars (if any) and .wrangler/state stay
# untouched; the KV id is arbitrary for a local namespace.
cat >"$T/worker/wrangler.jsonc" <<CFG
{
  "name": "julienning-e2e",
  "main": "$repo/worker/src/index.ts",
  "compatibility_date": "$compat",
  "kv_namespaces": [{ "binding": "USAGE", "id": "e2e-local" }],
  "vars": { "DEFAULT_TZ": "UTC", "CLAIM_TTL_MIN": "720", "ACTIVITY_TTL_MIN": "15" }
}
CFG
printf 'AUTH_TOKEN=%s\n' "$token" >"$T/worker/.dev.vars"
W="http://127.0.0.1:$wport"
(
  cd "$repo/worker" && exec ./node_modules/.bin/wrangler dev --config "$T/worker/wrangler.jsonc" \
    --port "$wport" --persist-to "$T/worker/state" --log-level warn \
    --show-interactive-dev-session=false >"$T/wrangler.log" 2>&1
) &
wrangler_pid=$!
disown
wait_for "worker answers on $W" 60 'ok' curl -fsS -m 2 "$W/healthz"
run curl -fsS -m 5 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$W/accounts"
expect_contains "team token is accepted" 200 "$out"

# --- fake releases and the installer ---------------------------------------------------
build_release() { # VERSION
  local v="$1" tag="v$1" asset
  asset="julienning_${v}_${os}_${arch}.tar.gz"
  mkdir -p "$T/build/$v" "$T/releases/$tag"
  (cd "$repo" && go build -trimpath -ldflags "-s -w -X $module/internal/version.Version=$v" \
    -o "$T/build/$v/julienning" ./cmd/julienning)
  tar -czf "$T/releases/$tag/$asset" -C "$T/build/$v" julienning
  (cd "$T/releases/$tag" && "${sha_cmd[@]}" "$asset" >checksums.txt)
}
build_release 9.0.0
build_release 9.0.1
printf 'v9.0.0\n' >"$T/releases/LATEST"
(cd "$repo" && go build -o "$T/fakereleases" scripts/e2e/fakereleases.go)
(cd "$repo" && go build -o "$T/ptydrive" scripts/e2e/ptydrive.go)
"$T/fakereleases" -root "$T/releases" >"$T/fakereleases.out" 2>&1 &
host_pid=$!
disown
wait_for "fake releases host is up" 10 'listening on' cat "$T/fakereleases.out"
R="$(sed -n 's/^listening on //p' "$T/fakereleases.out")"
export JULIENNING_RELEASES_BASE="$R"

run bash "$repo/scripts/install.sh"
expect_contains "install.sh installs the latest release" "Installed julienning 9.0.0" "$out"
expect_contains "install.sh suggests setup" "Next: julienning setup" "$out"
run julienning version
expect_contains "installed binary reports 9.0.0" "9.0.0" "$out"
[ -L "$JULIENNING_BIN_DIR/julienning" ] || fail "julienning is not a symlink in $JULIENNING_BIN_DIR"
ok "command is a symlink into the versions dir"

# --- setup ------------------------------------------------------------------------------
run julienning setup --dev e2e --remote-url "$W" --token "$token" --yes --shell bash \
  --share alpha@example.com --share beta@example.com --share gamma@example.com --nick beta@example.com=bravo
expect_contains "setup upgrades a settings.json from an older julienning" "updated" "$out"
expect_contains "the upgrade adds the StopFailure hook" "hook stop-failure" "$(cat "$HOME/.claude-old/settings.json")"
expect_contains "the StopFailure hook carries the rate_limit matcher" '"matcher": "rate_limit"' "$(cat "$HOME/.claude-work/settings.json")"
expect_contains "setup registers the work dir" ".claude-work" "$out"
run julienning configs
expect_contains "configs lists alpha's dir" ".claude-work" "$out"
expect_contains "configs lists bravo's dir" ".claude-two" "$out"
expect_missing "the personal dir is not registered" ".claude/" "$out"
expect_contains "setup added the shell hook line" "# julienning-shell-hook" "$(cat "$rc")"
expect_contains "setup patched the shared settings.json" "julienning" "$(cat "$HOME/.claude-work/settings.json")"
expect_same "setup left the personal settings.json alone" "$T/before/personal.json" "$HOME/.claude/settings.json"
run julienning shell-init bash
expect_contains "shell-init defines claude-alpha" "claude-alpha" "$out"
expect_contains "shell-init defines claude-bravo" "claude-bravo" "$out"

# --- switching and the shell functions ---------------------------------------------------
run julienning use alpha --no-launch
run julienning current
expect_contains "use alpha selects the work dir" ".claude-work" "$out"
run bash -c 'eval "$(julienning shell-init bash)"; claude -p hi'
expect_contains "plain claude follows the selection" "dir=$HOME/.claude-work args=-p hi" "$out"
run bash -c 'eval "$(julienning shell-init bash)"; claude-bravo'
expect_contains "claude-<nick> resolves its dir at call time" "dir=$HOME/.claude-two" "$out"
run julienning use --clear
run bash -c 'eval "$(julienning shell-init bash)"; claude'
expect_contains "use --clear returns plain claude to the default dir" "dir=<unset>" "$out"
run julienning use bravo --no-launch
run_fail julienning use nobody --no-launch
expect_contains "use of an unknown target fails with a hint" "nobody" "$out"

# --- the session picker, on a pseudo-terminal -------------------------------------------------
# A registered dir signed into a personal account, and one session for
# $T/proj in every dir. The team picker lists shared accounts only.
run julienning new-config --name mine --no-share --no-login
printf '{"oauthAccount":{"emailAddress":"mine@example.com"}}' >"$HOME/.claude-mine/.claude.json"
enc="$(printf '%s' "$T/proj" | sed 's/[^A-Za-z0-9]/-/g')"
write_session() { # DIR ID TITLE (old mtime: nothing looks open)
  mkdir -p "$1/projects/$enc"
  printf '{"type":"user","isSidechain":false,"cwd":"%s","sessionId":"%s","message":{"role":"user","content":"about %s"},"timestamp":"2026-01-01T10:00:00.000Z"}\n{"type":"ai-title","aiTitle":"%s","sessionId":"%s"}\n' \
    "$T/proj" "$2" "$3" "$3" "$2" >"$1/projects/$enc/$2.jsonl"
  touch -t 202601011000 "$1/projects/$enc/$2.jsonl"
}
sid_alpha=11111111-1111-4111-8111-111111111111
sid_bravo=22222222-2222-4222-8222-222222222222
sid_default=44444444-4444-4444-8444-444444444444
write_session "$HOME/.claude-work" "$sid_alpha" "Team alpha task"
write_session "$HOME/.claude-two" "$sid_bravo" "Team bravo task"
write_session "$HOME/.claude-mine" 33333333-3333-4333-8333-333333333333 "Mine private task"
write_session "$HOME/.claude" "$sid_default" "Personal default task"
write_session "$HOME/.claude-side" 55555555-5555-4555-8555-555555555555 "Side project task"

# pty CMD...: runs CMD from $T/proj on a pseudo-terminal, typing the steps in
# the array $steps (see scripts/e2e/ptydrive.go); output in $out.
pty() {
  local rc=0
  out="$(cd "$T/proj" && "$T/ptydrive" "${steps[@]}" -- "$@" 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ]; then fail "'$*' on a terminal exited $rc" "$out"; fi
}
steps=('expect=Team bravo task' 'sleep=300ms' 'send=\x1b' 'expect=Not starting claude')
pty env TERM=xterm-256color julienning use alpha
expect_contains "the team picker lists the selected account's sessions" "Team alpha task" "$out"
expect_contains "the team picker lists other shared accounts' sessions" "Team bravo task" "$out"
expect_missing "the team picker hides a registered personal dir's sessions" "Mine private task" "$out"
expect_missing "the team picker hides the personal default dir's sessions" "Personal default task" "$out"
expect_missing "the team picker hides unregistered dirs' sessions" "Side project task" "$out"
expect_contains "Esc in the picker cancels and keeps the selection" "Not starting claude; alpha stays selected." "$out"
steps=('expect=Choose 1-' 'send=q\r' 'expect=Not starting claude')
pty env TERM=dumb julienning use alpha --all
expect_contains "the numbered fallback lists team sessions" "Team bravo task" "$out"
expect_missing "the numbered fallback with --all hides personal sessions" "Mine private task" "$out"
steps=('expect=Team bravo task' 'send=Team bravo' 'sleep=300ms' 'send=\r'
  'expect=Move "Team bravo task" from bravo to alpha?' 'sleep=500ms' 'send=\r' 'expect=FAKE CLAUDE')
pty env TERM=xterm-256color julienning use alpha
expect_contains "a confirmed move is reported" 'Moved "Team bravo task" from bravo to alpha' "$out"
expect_contains "the moved session resumes in alpha's dir" "dir=$HOME/.claude-work args=--resume $sid_bravo" "$out"
if [ ! -f "$HOME/.claude-work/projects/$enc/$sid_bravo.jsonl" ] || [ -e "$HOME/.claude-two/projects/$enc/$sid_bravo.jsonl" ]; then
  fail "the moved transcript is not in alpha's dir only"
fi
ok "the transcript moved from bravo's dir to alpha's"
run julienning use bravo --no-launch

# --- personal accounts by email -------------------------------------------------------------
run julienning use personal@example.com --no-launch
expect_contains "use EMAIL opens a personal account in the default dir" "Using personal@example.com in ~/.claude (personal; your selection is unchanged)." "$out"
run julienning current
expect_contains "a personal account leaves the selection alone" ".claude-two" "$out"
expect_contains "the personal account's dir is remembered" '"email": "personal@example.com"' "$(cat "$JULIENNING_HOME/config.json")"
expect_missing "the personal default dir is not registered" "\"dir\": \"$HOME/.claude\"" "$(julienning configs --json)"
steps=('expect=Personal default task' 'send=Personal default' 'sleep=300ms' 'send=\r' 'expect=FAKE CLAUDE')
pty env TERM=xterm-256color julienning use personal@example.com
expect_missing "the personal picker hides team sessions" "Team alpha task" "$out"
expect_missing "the personal picker hides other personal dirs" "Side project task" "$out"
expect_missing "the personal picker hides a registered personal dir" "Mine private task" "$out"
expect_contains "a personal session resumes in its dir, CLAUDE_CONFIG_DIR unset" "dir=<unset> args=--resume $sid_default" "$out"
expect_missing "nothing moves into a personal account" "Moved " "$out"
run julienning current
expect_contains "the personal picker leaves the selection alone" ".claude-two" "$out"
run julienning use side@example.com --json
expect_contains "use EMAIL finds an unregistered dir" "\"dir\": \"$HOME/.claude-side\"" "$out"
run julienning use mine@example.com --json
expect_contains "use EMAIL finds a registered dir with a personal login" '"config": "mine"' "$out"
mkdir_claude "$HOME/.claude-side2" side@example.com "$HOME/.claude-side2/.claude.json"
printf '{"oauthAccount":{"emailAddress":"other@example.com"}}' >"$HOME/.claude-side/.claude.json"
run julienning use side@example.com --no-launch
expect_contains "a moved personal login is found again" "no longer logged in at ~/.claude-side; found it at ~/.claude-side2" "$out"
expect_contains "the moved account opens in its new dir" "Using side@example.com in ~/.claude-side2" "$out"
run_fail julienning use nobody@example.com --no-launch
expect_contains "an account logged in nowhere says how to sign in" "CLAUDE_CONFIG_DIR=~/.claude-nobody command claude" "$out"
expect_missing "the Worker never sees the personal accounts" "side@example.com" "$(julienning accounts --json 2>/dev/null)"
expect_missing "the Worker never sees a registered personal dir's account" "mine@example.com" "$(julienning accounts --json 2>/dev/null)"

# --- directory pins through the claude wrapper ------------------------------------------------
in_dir() { # DIR CMD...
  local d="$1"
  shift
  (cd "$d" && "$@")
}
claude_in() { # DIR SHELL: plain claude in DIR in a fresh shell with the integration loaded
  # shellcheck disable=SC2016 # expanded by the inner shell
  in_dir "$1" "$2" -c 'eval "$(julienning shell-init '"$2"')"; claude -p hi'
}
mkdir -p "$T/proj/side/web" "$T/proj/teamx" "$T/proj/side2"
[ ! -e "$JULIENNING_HOME/pins.json" ] || fail "pins.json exists before any pin"
run in_dir "$T/proj/side" julienning pin personal@example.com
expect_contains "pin names the directory and the account" "Pinned $T/proj/side to personal@example.com (personal)." "$out"
expect_contains "the first pin says how to update old terminals" "ignores pins until you run: source ~/$(basename "$rc")" "$out"
run in_dir "$T/proj/teamx" julienning pin alpha
expect_contains "a team account can be pinned" "Pinned $T/proj/teamx to alpha (alpha@example.com)." "$out"
run in_dir "$T/proj/side2" julienning pin side@example.com
run claude_in "$T/proj/side/web" bash
expect_contains "plain claude below a pinned dir opens the pinned personal account" "dir=<unset> args=-p hi" "$out"
run claude_in "$T/proj/teamx" bash
expect_contains "plain claude in a dir pinned to a team account opens it" "dir=$HOME/.claude-work args=-p hi" "$out"
run claude_in "$T/proj/side2" bash
expect_contains "a pin follows its account to the dir it moved to" "dir=$HOME/.claude-side2 args=-p hi" "$out"
run claude_in "$T/proj" bash
expect_contains "outside every pin plain claude follows the selection" "dir=$HOME/.claude-two args=-p hi" "$out"
# shellcheck disable=SC2016 # expanded by the inner shell
run in_dir "$T/proj/side/web" env CLAUDE_CONFIG_DIR="$HOME/.claude-work" bash -c 'eval "$(julienning shell-init bash)"; claude -p hi'
expect_contains "an exported CLAUDE_CONFIG_DIR still wins over a pin" "dir=$HOME/.claude-work args=-p hi" "$out"
if command -v zsh >/dev/null 2>&1; then
  run claude_in "$T/proj/side/web" zsh
  expect_contains "zsh: plain claude follows the pin" "dir=<unset> args=-p hi" "$out"
fi
run in_dir "$T/proj/side/web" julienning use --no-launch
expect_contains "use without a target names the pin" "$T/proj/side is pinned to personal@example.com (personal)." "$out"
expect_contains "use without a target opens the pinned personal account" "Using personal@example.com in ~/.claude (personal" "$out"
run julienning current
expect_contains "a personal pin leaves the selection alone" ".claude-two" "$out"
run in_dir "$T/proj/teamx" julienning use --no-launch
expect_contains "use in a dir pinned to a team account selects it" "Now using alpha (alpha@example.com)" "$out"
run in_dir "$T/proj/side/web" julienning use bravo --no-launch
expect_missing "an explicit target ignores the pin" "pinned" "$out"
run in_dir "$T/proj/side/web" julienning next --no-launch
expect_missing "next is unaffected by pins" "pinned" "$out"
run julienning use bravo --no-launch
run in_dir "$T/proj/side/web" julienning pin
expect_contains "pin shows the pin that covers a subdirectory" "$T/proj/side/web is inside $T/proj/side, which is pinned to personal@example.com (personal)." "$out"
run julienning pin --list
expect_contains "pin --list lists every pin" "$T/proj/teamx" "$out"
# Logged out: plain claude refuses instead of opening the selection.
printf '{"oauthAccount":{}}' >"$HOME/.claude.json"
run_fail claude_in "$T/proj/side/web" bash
expect_contains "a broken pin stops plain claude with the reason" "is pinned to personal@example.com, which cannot be opened" "$out"
expect_contains "a broken pin names the way out" "julienning unpin $T/proj/side" "$out"
expect_missing "a broken pin never falls back to the selection" "FAKE CLAUDE" "$out"
cp "$T/before/claude.json" "$HOME/.claude.json"
run_fail in_dir "$T/proj/side/web" julienning unpin
expect_contains "unpin below a pin points at the pinned dir" "to remove that pin: julienning unpin $T/proj/side" "$out"
run in_dir "$T/proj/side" julienning unpin
expect_contains "unpin removes the pin" "Unpinned $T/proj/side (was personal@example.com (personal))." "$out"
run julienning unpin "$T/proj/side2"
run claude_in "$T/proj/side/web" bash
expect_contains "after unpin plain claude follows the selection again" "dir=$HOME/.claude-two args=-p hi" "$out"
[ -f "$JULIENNING_HOME/pins.json" ] || fail "pins.json is gone while teamx is still pinned"
ok "the teamx pin stays for uninstall --purge to remove"

# --- the "run setup" warning for a settings.json an older julienning patched -----------------
old_layout
run julienning accounts
expect_contains "accounts warns when a dir lacks the new hook" "run: julienning setup" "$out"
run julienning setup --dev e2e --remote-url "$W" --token "$token" --yes --shell bash \
  --share alpha@example.com --share beta@example.com --share gamma@example.com --nick beta@example.com=bravo
run julienning accounts
expect_missing "the warning is gone after setup" "run: julienning setup" "$out"

# --- accounts ---------------------------------------------------------------------------
run julienning accounts
expect_contains "accounts lists alpha" "alpha" "$out"
expect_contains "accounts lists bravo" "bravo" "$out"
expect_contains "unreported accounts are free" "free" "$(row alpha)"
run julienning accounts --json
expect_contains "accounts --json names the account" '"alpha@example.com"' "$out"

# --- claims from a live session ---------------------------------------------------------------
# A registry entry for a process that is alive, as Claude Code writes one.
sleep 600 &
session_pid=$!
disown
proc_start="$(TZ=UTC LC_ALL=C ps -o lstart= -p "$session_pid" | sed 's/^ *//;s/ *$//')"
now_ms="$(($(date +%s) * 1000))"
mkdir -p "$HOME/.claude-work/sessions"
printf '{"pid":%d,"sessionId":"e2e-session-1","cwd":"%s","name":"","status":"idle","kind":"interactive","procStart":"%s","startedAt":%d,"updatedAt":%d}\n' \
  "$session_pid" "$T/proj" "$proc_start" "$now_ms" "$now_ms" >"$HOME/.claude-work/sessions/$session_pid.json"
printf '{"session_id":"e2e-session-1","cwd":"%s","source":"startup"}' "$T/proj" |
  CLAUDE_CONFIG_DIR="$HOME/.claude-work" julienning hook session-start
wait_for "a live session claims the account" 15 "in use by you" row alpha
expect_contains "the Worker lists the claim with this dev" '"dev": "e2e"' "$(account_json alpha | sed -n '/"claims": \[/,/\]/p')"
kill "$session_pid" 2>/dev/null || true
wait "$session_pid" 2>/dev/null || true
session_pid=""
rm -f "$HOME/.claude-work/sessions/"*.json
printf '{"session_id":"e2e-session-1","reason":"exit"}' |
  CLAUDE_CONFIG_DIR="$HOME/.claude-work" julienning hook session-end
wait_for "ending the session releases the claim" 15 "free" row alpha
expect_contains "the Worker lists no claim any more" '"claims": []' "$(account_json alpha)"

# --- usage: the status line and its detached report ---------------------------------------
# The status line prints `<model> · ctx N%` and spawns a detached send-usage;
# the numbers show up in `accounts` once that child has reached the Worker.
now="$(date +%s)"
sl_input="$(printf '{"model":{"display_name":"Opus"},"context_window":{"used_percentage":42},"rate_limits":{"five_hour":{"used_percentage":23,"resets_at":%d},"seven_day":{"used_percentage":40,"resets_at":%d}}}' \
  $((now + 3600)) $((now + 86400)))"
out="$(printf '%s' "$sl_input" | CLAUDE_CONFIG_DIR="$HOME/.claude-work" julienning statusline 2>&1)"
expect_contains "status line renders model and context" "Opus · ctx 42%" "$out"
expect_missing "status line shows no error row" "julienning:" "$out"
wait_for "usage reaches the Worker and the listing" 15 "23%" row alpha
expect_contains "your own report shows as in use by you" "in use by you" "$(row alpha)"
out="$(printf '%s' "$sl_input" | julienning statusline 2>&1)" # default dir: personal, not registered
expect_contains "status line still renders for a personal dir" "Opus · ctx 42%" "$out"
expect_contains "the personal dir is logged as not shared" "NOT_SHARED" "$(cat "$JULIENNING_HOME/errors.log")"
sleep 1
expect_missing "the Worker never sees a personal account" "personal@example.com" "$(julienning accounts --json 2>/dev/null)"

# --- exhaustion from a rate-limit refusal ---------------------------------------------------
# Claude Code runs the StopFailure hook when a turn ends on a 429; the hook
# reports the refusal through a detached send-exhausted.
printf '{"session_id":"e2e-session-1","hook_event_name":"StopFailure","error":"rate_limit","error_details":"You have hit your weekly limit · resets Oct 13 at 8pm (Europe/Istanbul) (error type rate_limit)","last_assistant_message":"You have hit your weekly limit · resets Oct 13 at 8pm (Europe/Istanbul)"}' |
  CLAUDE_CONFIG_DIR="$HOME/.claude-work" julienning hook stop-failure
wait_for "a rate-limit refusal marks the account exhausted" 15 "exhausted (week" row alpha
[ "$(row alpha | awk '{ print $1 }')" = "3" ] || fail "the exhausted account should rank last, got:" "$(julienning accounts 2>&1)"
ok "the exhausted account ranks last"
expect_contains "the Worker names the exhausted window" '"exhausted_window": "week"' "$(account_json alpha)"
expect_contains "the stale 93%-style numbers stay visible" "23%" "$(row alpha)"
printf '{"session_id":"e2e-session-1","hook_event_name":"StopFailure","error":"overloaded","error_details":"Overloaded"}' |
  CLAUDE_CONFIG_DIR="$HOME/.claude-two" julienning hook stop-failure
sleep 1
expect_contains "other StopFailure errors change nothing" "free" "$(row bravo)"

# --- forget -----------------------------------------------------------------------------------
run julienning forget bravo --keep
run julienning configs
expect_missing "forget unregisters bravo's dir" ".claude-two" "$out"
expect_same "forget restores bravo's settings.json byte for byte" "$T/before/two.json" "$HOME/.claude-two/settings.json"
[ -f "$HOME/.claude-two/.claude.json" ] || fail "forget --keep deleted the config dir"
ok "forget --keep keeps the dir"
run julienning accounts
expect_contains "the account stays shared for the team" "bravo" "$out"

# --- self-update ----------------------------------------------------------------------------
printf 'v9.0.1\n' >"$T/releases/LATEST"
run julienning update
expect_contains "update installs the newer release" "9.0.1" "$out"
run julienning version
expect_contains "the command now runs 9.0.1" "9.0.1" "$out"
[ -f "$JULIENNING_VERSIONS_DIR/9.0.0" ] || fail "update removed the previous version"
ok "the previous version is kept"
run julienning update
expect_contains "update is a no-op when current" "9.0.1" "$out"

# --- uninstall ------------------------------------------------------------------------------
run julienning uninstall --purge --yes
expect_same "uninstall restores the work settings.json byte for byte" "$T/before/work.json" "$HOME/.claude-work/settings.json"
expect_same "uninstall restores the rc file byte for byte" "$T/before/rc" "$rc"
expect_same "the personal settings.json was never touched" "$T/before/personal.json" "$HOME/.claude/settings.json"
expect_same "the unregistered personal dir's settings.json was never touched" "$T/before/side.json" "$HOME/.claude-side/settings.json"
[ ! -e "$JULIENNING_HOME" ] || fail "uninstall --purge left $JULIENNING_HOME"
[ ! -e "$JULIENNING_BIN_DIR/julienning" ] || fail "uninstall --purge left the command"
[ ! -e "$JULIENNING_VERSIONS_DIR" ] || fail "uninstall --purge left $JULIENNING_VERSIONS_DIR"
ok "uninstall --purge removed julienning's own files"
expect_missing "uninstall strips the upgraded dir too" "julienning" "$(cat "$HOME/.claude-old/settings.json")"
[ -f "$HOME/.claude-work/.claude.json" ] || fail "uninstall touched a Claude config dir"
ok "Claude config dirs survive uninstall"

printf '\ne2e: ok (%d checks)\n' "$checks"
