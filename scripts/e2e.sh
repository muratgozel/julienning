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
# email, directory pins through the claude wrapper, settings sync, the status line, usage
# reports, claims driven by the session hooks and a fake live session,
# forget, self-update, and that uninstall moves the registered dirs' sessions
# into ~/.claude, leaves plain claude native (in new terminals and in one
# left open) and restores every file setup touched byte for byte.
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

# wait_gone LABEL SECONDS NEEDLE CMD...: polls CMD until its output no
# longer contains NEEDLE.
wait_gone() {
  local label="$1" secs="$2" needle="$3" i
  shift 3
  for ((i = 0; i < secs * 2; i++)); do
    out="$("$@" 2>&1 || true)"
    case "$out" in *"$needle"*) ;; *)
      ok "$label"
      return
      ;;
    esac
    sleep 0.5
  done
  fail "$label: '$needle' still there after ${secs}s; last output:" "$out"
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
host_session_pid=""
T=""
cleanup() {
  local rc=$?
  for p in "$session_pid" "$host_session_pid" "$host_pid" "$wrangler_pid"; do
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

# A claude that only reports which config dir it was started with (and a
# version, which transfer compares between machines).
# shellcheck disable=SC2016 # the variables are for the fake claude to expand
printf '#!/bin/sh\nif [ "$1" = --version ]; then echo "2.1.303 (Claude Code)"; exit 0; fi\necho "FAKE CLAUDE dir=${CLAUDE_CONFIG_DIR:-<unset>} args=$*"\n' >"$T/fakebin/claude"
chmod +x "$T/fakebin/claude"
# A tmux that logs every call as [arg][arg]... to $T/tmux.log and keeps one
# session (the file $T/tmux-session). A window runs its command at once, with
# SHELL set to a fake that says it took the window over. It is on PATH before
# any real tmux, so e2e never touches the machine's tmux server.
cat >"$T/fakebin/tmux" <<TMUX
#!/bin/sh
[ "\$1" = -V ] && { echo 'tmux 3.5a'; exit 0; }
{ printf '[%s]' "\$@"; echo; } >>"$T/tmux.log"
case "\$1" in
has-session) [ -f "$T/tmux-session" ] ;;
new-session) dir=\$5; shift 5; : >"$T/tmux-session"; cd "\$dir" && SHELL="$T/fakebin/window-shell" exec "\$@" ;;
new-window) dir=\$5; shift 5; cd "\$dir" && SHELL="$T/fakebin/window-shell" exec "\$@" ;;
attach-session) echo "TMUX ATTACHED \$3" ;;
*) echo "fake tmux: unexpected \$*" >&2; exit 1 ;;
esac
TMUX
printf '#!/bin/sh\necho "WINDOW SHELL"\n' >"$T/fakebin/window-shell"
chmod +x "$T/fakebin/tmux" "$T/fakebin/window-shell"

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
build_release 9.1.0
cp "$repo/scripts/install.sh" "$T/releases/install.sh" # what a host runs during host add
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
expect_contains "setup without a terminal leaves tmux mode undecided" "Tmux:     off (enable with: julienning setup --tmux claude)" "$out"
run julienning configs
expect_contains "configs lists alpha's dir" ".claude-work" "$out"
expect_contains "configs lists bravo's dir" ".claude-two" "$out"
expect_missing "the personal dir is not registered" ".claude/" "$out"
expect_contains "setup added the shell hook line" "# julienning-shell-hook" "$(cat "$rc")"
expect_contains "the rc line calls julienning by its absolute path" "[ -x '$JULIENNING_BIN_DIR/julienning' ] && eval \"\$('$JULIENNING_BIN_DIR/julienning' shell-init bash)\"" "$(cat "$rc")"
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

# --- tmux mode, with the fake tmux ---------------------------------------------------------------
run julienning setup --yes --shell bash --tmux claude
expect_contains "setup --tmux turns tmux mode on" 'Tmux:     claude runs in tmux session "claude" (enabled)' "$out"
expect_contains "the shell functions run tmux by its path" "'$T/fakebin/tmux' has-session -t '=claude'" "$(julienning shell-init bash)"
rm -f "$T/tmux.log" "$T/tmux-session"
steps=('expect=New session' 'sleep=300ms' 'send=\r' 'expect=WINDOW SHELL')
pty env TERM=xterm-256color julienning use alpha
expect_contains "use starts claude in the tmux session, on the account" "FAKE CLAUDE dir=$HOME/.claude-work args=" "$out"
window="[/bin/sh][-c][trap : INT; \"\$@\"; exec \"\${SHELL:-/bin/sh}\"][julienning][env]"
expect_contains "no session yet: a new one, exact name, account explicit" \
  "[new-session][-s][claude][-c][$T/proj]${window}[CLAUDE_CONFIG_DIR=$HOME/.claude-work][$T/fakebin/claude]" "$(cat "$T/tmux.log")"
rm -f "$T/tmux.log"
steps=('expect=TMUX ATTACHED')
# shellcheck disable=SC2016 # expanded by the inner shell
pty env TERM=xterm-256color bash -c 'eval "$(julienning shell-init bash)"; claude -p hi'
expect_contains "plain claude opens a window in the existing session" "FAKE CLAUDE dir=$HOME/.claude-work args=-p hi" "$out"
expect_contains "the window goes into the session by its exact name, then attaches" \
  "[new-window][-t][=claude:][-c][$T/proj]${window}[CLAUDE_CONFIG_DIR=$HOME/.claude-work][$T/fakebin/claude][-p][hi]
[attach-session][-t][=claude]" "$(cat "$T/tmux.log")"
rm -f "$T/tmux.log"
steps=('expect=FAKE CLAUDE')
# shellcheck disable=SC2016 # expanded by the inner shell
pty env TERM=xterm-256color TMUX=/tmp/tmux-e2e,1,0 bash -c 'eval "$(julienning shell-init bash)"; claude-bravo'
expect_contains "inside tmux claude runs in place" "FAKE CLAUDE dir=$HOME/.claude-two" "$out"
# shellcheck disable=SC2016 # expanded by the inner shell
run bash -c 'eval "$(julienning shell-init bash)"; claude -p piped'
expect_contains "without a terminal claude runs in place" "FAKE CLAUDE dir=$HOME/.claude-work args=-p piped" "$out"
[ ! -s "$T/tmux.log" ] || fail "tmux was used where claude must run in place" "$(cat "$T/tmux.log")"
ok "in place means no tmux call at all"
run julienning setup --yes --shell bash --no-tmux
expect_contains "setup --no-tmux turns it off" "Tmux:     off (enable with: julienning setup --tmux claude)" "$out"
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
ok "the teamx pin stays for uninstall to remove"

# --- the "run setup" warning for a settings.json an older julienning patched -----------------
old_layout
run julienning accounts
expect_contains "accounts warns when a dir lacks the new hook" "run: julienning setup" "$out"
run julienning setup --dev e2e --remote-url "$W" --token "$token" --yes --shell bash \
  --share alpha@example.com --share beta@example.com --share gamma@example.com --nick beta@example.com=bravo
run julienning accounts
expect_missing "the warning is gone after setup" "run: julienning setup" "$out"

# --- sync: the same settings and CLAUDE.md in every shared account ------------------------
# Answers on a real terminal, then `sync --from gamma` (gamma put back as it
# was) removes every key the first run added, byte for byte, so uninstall's
# restore checks below still hold.
run julienning sync --from alpha
expect_contains "sync --from adds a key the target lacks" "theme                      dark: set in gamma" "$out"
expect_contains "sync --from with no CLAUDE.md leaves them alone" "alpha has none; every account keeps its own" "$out"
for d in work two old; do cp "$HOME/.claude-$d/settings.json" "$T/before/sync-$d.json"; done
cp "$HOME/.claude-mine/settings.json" "$T/before/sync-mine.json"
printf '# e2e team rules\n' >"$HOME/.claude-work/CLAUDE.md"
printf '# old rules\n' >"$HOME/.claude-two/CLAUDE.md"
touch -t 202601011000 "$HOME/.claude-two/CLAUDE.md"
run_fail julienning sync --check
expect_contains "sync --check lists a setting that agrees" "theme                      same: dark" "$out"
expect_contains "sync --check lists differing CLAUDE.md files" "none: gamma" "$out"
run_fail julienning sync
expect_contains "sync without a terminal points at --from and --check" "needs a terminal" "$out"
steps=('expect=model (1 of 7)' 'send=\r' 'expect=effortLevel (2 of 7)' 'send=\r'
  'expect=promptCacheTtl (3 of 7)' 'send=\x1b[B' 'sleep=100ms' 'send=\r'
  'expect=subagentPromptCacheTtl (4 of 7)' 'send=\x1b[B\x1b[B\x1b[B' 'sleep=100ms' 'send=\r'
  'expect=autoContinueAtUsageLimit (5 of 7)' 'send=\r' 'expect=theme (6 of 7)' 'send=\x1b[F' 'sleep=100ms' 'send=\r'
  'expect=editorMode (7 of 7)' 'send=j' 'sleep=100ms' 'send=\r'
  'expect=CLAUDE.md: which one' 'send=\r'
  "expect=Overwrite bravo's CLAUDE.md with alpha's?" 'sleep=500ms' 'send=y' 'expect=CLAUDE.md   ')
pty env TERM=xterm-256color julienning sync
expect_contains "sync reports a value it set everywhere" "promptCacheTtl             1h: set in alpha, bravo, gamma" "$out"
expect_contains "sync reports a value already everywhere" "model                      opus: already in every shared account" "$out"
expect_contains "sync reports a skipped setting" "subagentPromptCacheTtl     not synced" "$out"
expect_contains "sync reports the CLAUDE.md copies" "CLAUDE.md                  alpha's: copied to bravo, gamma" "$out"
for d in work two old; do
  expect_contains "sync wrote editorMode into ~/.claude-$d" '"editorMode": "vim"' "$(cat "$HOME/.claude-$d/settings.json")"
  expect_contains "sync kept julienning's hooks in ~/.claude-$d" "hook session-start" "$(cat "$HOME/.claude-$d/settings.json")"
done
expect_same "the overwritten CLAUDE.md is alpha's" "$HOME/.claude-work/CLAUDE.md" "$HOME/.claude-two/CLAUDE.md"
expect_same "an account without a CLAUDE.md got alpha's" "$HOME/.claude-work/CLAUDE.md" "$HOME/.claude-old/CLAUDE.md"
expect_same "sync left the personal settings.json alone" "$T/before/personal.json" "$HOME/.claude/settings.json"
expect_same "sync left the registered personal dir alone" "$T/before/sync-mine.json" "$HOME/.claude-mine/settings.json"
[ ! -e "$HOME/.claude/CLAUDE.md" ] || fail "sync wrote a CLAUDE.md into the personal dir"
ok "sync wrote no CLAUDE.md into the personal dir"
run julienning sync --check
expect_contains "sync --check agrees on what was synced" "editorMode                 same: vim" "$out"
expect_contains "sync --check agrees on CLAUDE.md" "CLAUDE.md                  same in every shared account" "$out"
cp "$T/before/sync-old.json" "$HOME/.claude-old/settings.json"
run julienning sync --from gamma
expect_contains "sync --from removes keys the source lacks" "editorMode                 not set (Claude Code's default): removed from alpha, bravo" "$out"
expect_same "sync --from restores alpha's settings.json byte for byte" "$T/before/sync-work.json" "$HOME/.claude-work/settings.json"
expect_same "sync --from restores bravo's settings.json byte for byte" "$T/before/sync-two.json" "$HOME/.claude-two/settings.json"

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
# Renaming the dev moves the claim of the account in use to the new name.
run julienning dev
expect_contains "dev prints the dev name" "e2e" "$out"
run julienning dev E2E-Renamed
expect_contains "dev renames" "Your dev name is now e2e-renamed (was e2e)." "$out"
claims_of() { account_json "$1" | sed -n '/"claims": \[/,/\]/p'; }
wait_for "the claim moves to the new dev name" 15 '"dev": "e2e-renamed"' claims_of alpha
expect_missing "no claim is left under the old dev name" '"dev": "e2e"' "$(claims_of alpha)"
run julienning dev e2e
wait_for "renaming back moves the claim back" 15 '"dev": "e2e"' claims_of alpha
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
# The refusal names the reset of the week window reported above, as Claude
# does: a reset more than an hour before it would read as an old window the
# Worker already saw roll over (and a fixed date breaks the run once passed).
week_reset=$((now + 86400))
if date -r 0 >/dev/null 2>&1; then
  resets="$(TZ=Europe/Istanbul LC_ALL=C date -r "$week_reset" '+%b %-d at %-I:%M%p')"
else
  resets="$(TZ=Europe/Istanbul LC_ALL=C date -d "@$week_reset" '+%b %-d at %-I:%M%p')"
fi
printf '{"session_id":"e2e-session-1","hook_event_name":"StopFailure","error":"rate_limit","error_details":"You have hit your weekly limit · resets %s (Europe/Istanbul) (error type rate_limit)","last_assistant_message":"You have hit your weekly limit · resets %s (Europe/Istanbul)"}' "$resets" "$resets" |
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

# --- hosts: a second sandbox home plays the host, over a fake ssh ---------------------
# The fake ssh drops the options and the destination and runs the command
# with /bin/sh -c under the host's home, with the minimal PATH a
# non-interactive ssh command gets: no ~/.local/bin, where claude and
# julienning live (as on a real host), and first a dir holding only the fake
# tmux, so a real tmux in /usr/bin is never found. It keeps the pseudo-
# terminal it was given. Its rc file ends with a provisioning claude()
# function, which julienning's line, appended after it, must override.
H="$T/host"
mkdir -p "$H/.local/bin" "$H/.claude-team/projects"
case "$os" in darwin) host_rc="$H/.bash_profile" ;; *) host_rc="$H/.bashrc" ;; esac
cat >"$host_rc" <<'RC'
# provisioned
claude() {
  echo PROVISIONED
}
RC
cp "$host_rc" "$T/before/host_rc"
# Another patch release than this machine's: transfer must allow it.
# shellcheck disable=SC2016 # the variables are for the fake claude to expand
printf '#!/bin/sh\nif [ "$1" = --version ]; then echo "2.1.300 (Claude Code)"; exit 0; fi\necho "HOST CLAUDE dir=${CLAUDE_CONFIG_DIR:-<unset>} args=$*"\n' >"$H/.local/bin/claude"
sed -e "s|$T/tmux.log|$T/host-tmux.log|" -e "s|$T/tmux-session|$T/host-tmux-session|" -e "s|$T/fakebin/window-shell|$H/.local/bin/window-shell|" \
  "$T/fakebin/tmux" >"$H/.local/bin/tmux"
printf '#!/bin/sh\necho "HOST WINDOW SHELL"\n' >"$H/.local/bin/window-shell"
chmod +x "$H/.local/bin/claude" "$H/.local/bin/tmux" "$H/.local/bin/window-shell"
mkdir -p "$T/hostpath"
ln -s "$H/.local/bin/tmux" "$T/hostpath/tmux"
printf '{"oauthAccount":{"emailAddress":"alpha@example.com"}}' >"$H/.claude-team/.claude.json"
printf '{\n  "theme": "dark"\n}\n' >"$H/.claude-team/settings.json"
cat >"$T/fakebin/ssh" <<SSH
#!/bin/sh
dest=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -O) exit 0 ;;
    --) shift; dest=\$1; shift; break ;;
  esac
  shift
done
printf '%s %s\n' "\$dest" "\$*" >>"$T/ssh.log"
if [ "\$dest" != box ]; then echo "ssh: Could not resolve hostname \$dest: nodename nor servname provided" >&2; exit 255; fi
cd "$H" || exit 255
remote() {
  env -i HOME="$H" USER=dev LOGNAME=dev SHELL=/bin/bash PATH="$T/hostpath:/usr/bin:/bin" \
    SSH_CONNECTION="127.0.0.1 50000 127.0.0.2 22" TERM="\${TERM:-dumb}" /bin/sh -c "\$1"
}
# \$T/drop-receive: the next receive runs, but its answer is lost.
case "\$1" in *" agent receive"*)
  if [ -f "$T/drop-receive" ]; then
    rm -f "$T/drop-receive"
    remote "\$1" >/dev/null
    echo "Connection to box closed by remote host." >&2
    exit 255
  fi ;;
esac
remote "\$1"
SSH
chmod +x "$T/fakebin/ssh"
on_host() { env -i HOME="$H" USER=dev SHELL=/bin/bash PATH="$H/.local/bin:/usr/bin:/bin" "$@"; }

run julienning host list
expect_contains "no hosts yet" "No hosts. Add one with: julienning host add NAME [SSH_DEST]" "$out"
run_fail julienning host add nowhere
expect_contains "an unreachable destination says what to check" "cannot reach nowhere (ssh: Could not resolve hostname nowhere" "$out"
run julienning host add box
expect_contains "host add names the machine it reached" "Host:     box → box (127.0.0.2): " "$out"
expect_contains "host add reports the platform, user and home" "$os/$arch, user $(id -un), home $H" "$out"
expect_contains "host add installs julienning there with the installer" "Julienning: installing 9.0.1 on box" "$out"
expect_contains "julienning lands in the installer's layout there" "Julienning: 9.0.1 at ~/.local/bin/julienning (installed)" "$out"
expect_contains "host add sends the team settings" "Team:     set up on box (dev e2e, Worker $W)" "$out"
expect_contains "setup runs there" "Setup:    running julienning setup on box" "$out"
expect_contains "setup there registers its shared account" "shared (registered as" "$out"
expect_contains "setup there notes the provisioning claude function" "defines a claude function before julienning's line; julienning's definition wins" "$out"
expect_contains "host add saves the entry" "Added box (box)." "$out"
host_id="$(sed -n 's/.*"machine_id": "\([0-9a-f]*\)".*/\1/p' "$H/.julienning/config.json")"
[ -n "$host_id" ] || fail "the host has no machine id"
expect_contains "hosts.json records the host's own machine id" "\"machine_id\": \"$host_id\"" "$(cat "$JULIENNING_HOME/hosts.json")"
expect_missing "the team token never travels on a host's command line" "$token" "$(cat "$T/ssh.log")"
# shellcheck disable=SC2016 # expanded by the inner shell
run on_host bash -c 'source "$1"; claude -p hi' _ "$host_rc"
expect_contains "julienning's line overrides the provisioning claude function" "HOST CLAUDE dir=<unset> args=-p hi" "$out"
expect_missing "the provisioning function no longer runs" "PROVISIONED" "$out"

run julienning accounts --host box
expect_contains "accounts --host shows the host's own dirs" "/.claude-team" "$(printf '%s\n' "$out" | awk '$2 == "alpha"')"
run julienning configs --host box
expect_contains "configs --host lists the host's registered dirs" " ~/.claude-team" "$out"
run julienning host list
expect_contains "host list shows the host as ok" "ok" "$(printf '%s\n' "$out" | awk '$1 == "box"')"
expect_contains "host list shows the shared account logged in there" "alpha" "$(printf '%s\n' "$out" | awk '$1 == "box"')"
run julienning host list --json
expect_contains "host list --json carries the status" '"status": "ok"' "$out"
run_fail julienning host list --host box
expect_contains "host list does not take --host" "host list does not take --host" "$out"

# Forwarding finds the host's checkout of this machine's working directory
# by the repository, not the path: ~/Code/shop here, ~/src/shop there.
git_repo() { # DIR BRANCH ORIGIN
  mkdir -p "$1/.git" "$1/web"
  printf 'ref: refs/heads/%s\n' "$2" >"$1/.git/HEAD"
  printf '[remote "origin"]\n\turl = %s\n' "$3" >"$1/.git/config"
}
git_repo "$HOME/Code/shop" main git@github.com:team/shop.git
git_repo "$H/src/shop" main https://github.com/team/shop
host_enc="$(printf '%s' "$H/src/shop/web" | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$H/.claude-team/projects/$host_enc"
printf '{"type":"user","isSidechain":false,"cwd":"%s","sessionId":"%s","message":{"role":"user","content":"host work"},"timestamp":"2026-01-01T10:00:00.000Z"}\n{"type":"ai-title","aiTitle":"Host shop task","sessionId":"%s"}\n' \
  "$H/src/shop/web" 66666666-6666-4666-8666-666666666666 66666666-6666-4666-8666-666666666666 \
  >"$H/.claude-team/projects/$host_enc/66666666-6666-4666-8666-666666666666.jsonl"
touch -t 202601011000 "$H/.claude-team/projects/$host_enc/66666666-6666-4666-8666-666666666666.jsonl"
steps=('expect=Host shop task' 'sleep=300ms' 'send=\x1b' 'expect=Not starting claude')
out="$(cd "$HOME/Code/shop/web" && "$T/ptydrive" "${steps[@]}" -- env TERM=xterm-256color julienning use alpha --host box 2>&1)" ||
  fail "use --host on a terminal failed" "$out"
expect_contains "use --host draws the host's picker here, in its checkout" "Host shop task" "$out"
expect_contains "the forwarded picker's cancel comes from the host" "Not starting claude; alpha stays selected." "$out"
mkdir -p "$HOME/notes"
run_fail in_dir "$HOME/notes" julienning pin alpha --host box
expect_contains "pin --host without a counterpart fails with the locate message" "julienning: box: box has no ~/notes" "$out"
run in_dir "$HOME/notes" julienning use alpha --host box --no-launch
expect_contains "other commands run in the host's home after a note" "julienning: note: ~/notes has no checkout on box" "$out"
expect_contains "and still run there" "Now using alpha (alpha@example.com) in ~/.claude-team" "$out"

# A claim made on the host shows where it comes from.
sleep 600 &
host_session_pid=$!
disown
host_proc_start="$(TZ=UTC LC_ALL=C ps -o lstart= -p "$host_session_pid" | sed 's/^ *//;s/ *$//')"
mkdir -p "$H/.claude-team/sessions"
printf '{"pid":%d,"sessionId":"host-session-1","cwd":"%s","name":"","status":"idle","kind":"interactive","procStart":"%s","startedAt":%d,"updatedAt":%d}\n' \
  "$host_session_pid" "$H/src/shop/web" "$host_proc_start" "$now_ms" "$now_ms" >"$H/.claude-team/sessions/$host_session_pid.json"
printf '{"session_id":"host-session-1","cwd":"%s","source":"startup"}' "$H/src/shop/web" |
  on_host env CLAUDE_CONFIG_DIR="$H/.claude-team" "$H/.local/bin/julienning" hook session-start
wait_for "a claim from the host says it is on the host" 15 "and on box" row alpha
kill "$host_session_pid" 2>/dev/null || true
wait "$host_session_pid" 2>/dev/null || true
host_session_pid=""
rm -f "$H/.claude-team/sessions/"*.json
printf '{"session_id":"host-session-1","reason":"exit"}' |
  on_host env CLAUDE_CONFIG_DIR="$H/.claude-team" "$H/.local/bin/julienning" hook session-end
wait_gone "the host's claim is released" 15 "on box" row alpha

# Refreshing a set-up host: the dev name, then tmux mode there.
run julienning dev other --host box
expect_contains "dev --host renames on the host" "Your dev name is now other (was e2e)." "$out"
run julienning host add box --yes
expect_contains "host add --yes renames the dev back" "Your dev name is now e2e (was other)." "$out"
expect_contains "a known host is refreshed" "Refreshed box (box)." "$out"
run julienning setup --host box --yes --tmux claude
expect_contains "setup --host turns tmux mode on there" 'Tmux:     claude runs in tmux session "claude" (enabled)' "$out"
run julienning host add box
expect_contains "a complete host is not set up again" "Setup:    complete on box (change it with: julienning setup --host box)" "$out"
rm -f "$T/host-tmux.log" "$T/host-tmux-session"
steps=('expect=New session' 'sleep=300ms' 'send=\r' 'expect=HOST WINDOW SHELL')
out="$(cd "$HOME/Code/shop/web" && "$T/ptydrive" "${steps[@]}" -- env TERM=xterm-256color julienning use alpha --host box 2>&1)" ||
  fail "use --host into tmux failed" "$out"
expect_contains "use --host launches claude in the host's tmux session" "HOST CLAUDE dir=$H/.claude-team" "$out"
expect_contains "in the host's checkout" "[new-session][-s][claude][-c][$H/src/shop/web]" "$(cat "$T/host-tmux.log")"
[ ! -s "$T/tmux.log" ] || fail "this machine's tmux was used for a host launch" "$(cat "$T/tmux.log")"
ok "the launch used the host's tmux, not this machine's"

# A newer minor here: forwarded commands refuse, host add updates the host.
run julienning update --version v9.1.0
run_fail julienning accounts --host box
expect_contains "a minor mismatch is refused without a terminal" \
  "box runs julienning 9.0.1 and this machine 9.1.0 (cross-machine commands need the same major.minor); update box: julienning update --host box --version v9.1.0" "$out"
run julienning host add box --yes
expect_contains "host add --yes updates the host to this machine's version" "Updated julienning on box to 9.1.0." "$out"
run julienning accounts --host box
expect_contains "forwarding works again after the update" "alpha" "$out"

# Transfer: a session of this machine's checkout moves to box's checkout of
# the same repository and resumes there; it comes back with --no-launch;
# a transfer whose answer is lost after box committed finishes on retry.
sid_move=77777777-7777-4777-8777-777777777777
enc_shop="$(printf '%s' "$HOME/Code/shop/web" | sed 's/[^A-Za-z0-9]/-/g')"
host_enc_shop="$(printf '%s' "$H/src/shop/web" | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$HOME/.claude-work/projects/$enc_shop" "$HOME/.claude-work/file-history/$sid_move"
printf '{"type":"user","isSidechain":false,"cwd":"%s","sessionId":"%s","version":"2.1.303","gitBranch":"main","message":{"role":"user","content":"fix %s/app.go"},"timestamp":"2026-01-02T10:00:00.000Z"}\n{"type":"ai-title","aiTitle":"Travelling task","sessionId":"%s"}\n' \
  "$HOME/Code/shop/web" "$sid_move" "$HOME/Code/shop/web" "$sid_move" >"$HOME/.claude-work/projects/$enc_shop/$sid_move.jsonl"
printf 'package main // %s\n' "$HOME/Code/shop/web" >"$HOME/.claude-work/file-history/$sid_move/0123456789abcdef@v1"
touch -t 202601021000 "$HOME/.claude-work/projects/$enc_shop/$sid_move.jsonl"
rm -f "$T/host-tmux.log" "$T/host-tmux-session"
steps=('expect=Travelling task' 'send=Travelling' 'sleep=300ms' 'send=\r' 'expect=Move "Travelling task"' 'sleep=500ms' 'send=\r' 'expect=HOST WINDOW SHELL')
out="$(cd "$HOME/Code/shop/web" && "$T/ptydrive" "${steps[@]}" -- env TERM=xterm-256color julienning transfer --to box 2>&1)" ||
  fail "transfer --to box failed" "$out"
expect_contains "transfer names the account it moves into on the host" "Moving into alpha (alpha@example.com) in ~/.claude-team on box" "$out"
expect_contains "transfer reports the move and the rewrite" 'Moved "Travelling task" from alpha on this machine to alpha on box (' "$out"
expect_contains "the paths were rewritten for the host's checkout" "paths rewritten for ~/src/shop/web)" "$out"
expect_contains "the session resumes on box, in its tmux session" "HOST CLAUDE dir=$H/.claude-team args=--resume $sid_move" "$out"
moved="$H/.claude-team/projects/$host_enc_shop/$sid_move.jsonl"
[ -f "$moved" ] || fail "the transcript is not in box's project folder"
expect_contains "the transcript's cwd is box's checkout" "\"cwd\":\"$H/src/shop/web\"" "$(cat "$moved")"
expect_missing "no path of this machine is left in it" "$HOME/Code/shop" "$(cat "$moved")"
expect_contains "file-history is copied as it was" "$HOME/Code/shop/web" "$(cat "$H/.claude-team/file-history/$sid_move/0123456789abcdef@v1")"
[ ! -e "$HOME/.claude-work/projects/$enc_shop/$sid_move.jsonl" ] || fail "this machine still has the moved session"
ok "this machine no longer has the moved session"

steps=('expect=Travelling task' 'send=Travelling' 'sleep=300ms' 'send=\r' 'expect=Move "Travelling task"' 'sleep=500ms' 'send=\r' 'expect=Resume it on this machine')
out="$(cd "$HOME/Code/shop/web" && "$T/ptydrive" "${steps[@]}" -- env TERM=xterm-256color julienning transfer --host box --to local alpha --no-launch 2>&1)" ||
  fail "transfer --host box --to local failed" "$out"
expect_contains "transfer --host lists box's sessions in its checkout" "in ~/src/shop/web on box" "$out"
expect_contains "the session comes back" 'Moved "Travelling task" from alpha on box to alpha on this machine (' "$out"
expect_contains "--no-launch says how to resume it" "Resume it on this machine: cd ~/Code/shop/web && claude-alpha --resume $sid_move" "$out"
expect_contains "its cwd is this machine's checkout again" "\"cwd\":\"$HOME/Code/shop/web\"" "$(cat "$HOME/.claude-work/projects/$enc_shop/$sid_move.jsonl")"
[ ! -e "$moved" ] || fail "box still has the session it sent"
ok "box no longer has the session it sent"

touch "$T/drop-receive"
steps=('expect=Travelling task' 'send=Travelling' 'sleep=300ms' 'send=\r' 'expect=Move "Travelling task"' 'sleep=500ms' 'send=\r' 'expect=cannot reach box')
out="$(cd "$HOME/Code/shop/web" && "$T/ptydrive" "${steps[@]}" -- env TERM=xterm-256color julienning transfer --to box --no-launch 2>&1)" || true
expect_contains "a lost answer is reported" "cannot reach box (Connection to box closed by remote host.)" "$out"
[ -f "$moved" ] || fail "box did not commit the session before the answer was lost"
[ -f "$HOME/.claude-work/projects/$enc_shop/$sid_move.jsonl" ] || fail "the source let go of a session without an answer"
ok "after a lost answer both machines hold the session"
steps=('expect=Travelling task' 'send=Travelling' 'sleep=300ms' 'send=\r' 'expect=Move "Travelling task"' 'sleep=500ms' 'send=\r' 'expect=finished it')
out="$(cd "$HOME/Code/shop/web" && "$T/ptydrive" "${steps[@]}" -- env TERM=xterm-256color julienning transfer --to box --no-launch 2>&1)" ||
  fail "the retried transfer failed" "$out"
expect_contains "the retry finishes the transfer" 'box already had "Travelling task" from a transfer that lost its connection; finished it.' "$out"
[ ! -e "$HOME/.claude-work/projects/$enc_shop/$sid_move.jsonl" ] || fail "the retry did not release this machine's copy"
ok "the retry released this machine's copy"

run julienning uninstall --host box --purge --yes
expect_same "uninstall --host restores the host's rc file byte for byte" "$T/before/host_rc" "$host_rc"
[ ! -e "$H/.julienning" ] || fail "uninstall --host --purge left the host's state dir"
ok "uninstall --host --purge removed julienning there"
expect_contains "the entry stays after uninstall --host" '"name": "box"' "$(cat "$JULIENNING_HOME/hosts.json")"
run julienning host remove box
expect_contains "host remove says the host is unchanged" "Removed box from this machine's hosts; julienning on box is unchanged" "$out"
run_fail julienning accounts --host box
expect_contains "a removed host is unknown" 'unknown host "box"' "$out"

# --- uninstall ------------------------------------------------------------------------------
# Plain uninstall on a terminal, run from a shell that loaded the integration
# before it (a terminal left open). It offers to move the registered dirs'
# sessions into ~/.claude, leaves an open one where it is, drops the pins,
# and afterwards plain claude is native: the real command in new terminals,
# and the default dir even in the old one, inside a dir that was pinned.
mkdir -p "$HOME/.claude-work/projects/$enc/memory" "$HOME/.claude-work/sessions"
printf 'remember the build flags\n' >"$HOME/.claude-work/projects/$enc/memory/notes.md"
# Memory of a project whose transcripts Claude already cleaned up.
mkdir -p "$HOME/.claude-work/projects/-gone-project/memory"
printf 'old decisions\n' >"$HOME/.claude-work/projects/-gone-project/memory/decisions.md"
sleep 600 &
session_pid=$!
disown
proc_start="$(TZ=UTC LC_ALL=C ps -o lstart= -p "$session_pid" | sed 's/^ *//;s/ *$//')"
printf '{"pid":%d,"sessionId":"%s","cwd":"%s","name":"","status":"idle","kind":"interactive","procStart":"%s","startedAt":%d,"updatedAt":%d}\n' \
  "$session_pid" "$sid_alpha" "$T/proj" "$proc_start" "$now_ms" "$now_ms" >"$HOME/.claude-work/sessions/$session_pid.json"
steps=('expect=Move them into ~/.claude?' 'send=\r' 'expect=Terminals already open')
# shellcheck disable=SC2016 # expanded by the inner shell
out="$(cd "$T/proj/teamx" && bash -c 'eval "$(julienning shell-init bash)"; "$@" || exit; claude -p open-terminal' _ \
  "$T/ptydrive" "${steps[@]}" -- env TERM=dumb julienning uninstall 2>&1)" || fail "uninstall on a terminal failed" "$out"
expect_contains "uninstall counts the sessions and names the account plain claude will use" \
  "Sessions: 3 sessions in 2 registered config dirs. Without julienning, plain claude opens ~/.claude (signed in as personal@example.com) and cannot resume them." "$out"
expect_contains "uninstall names an open session before asking" "\"Team alpha task\" in ~/.claude-work" "$out"
expect_contains "uninstall moves the sessions and merges memory" "Sessions: moved 2 sessions into ~/.claude (memory: 2 files added)" "$out"
expect_contains "the open session stays, with the command that resumes it" "CLAUDE_CONFIG_DIR=~/.claude-work claude --resume $sid_alpha" "$out"
expect_contains "plain uninstall drops the pins" "Pins:     removed 1 pin" "$out"
expect_contains "uninstall says new terminals run claude natively" "New terminals run claude natively: plain claude opens ~/.claude (signed in as personal@example.com)." "$out"
expect_contains "a terminal opened before uninstall runs claude natively in a formerly pinned dir" "dir=<unset> args=-p open-terminal" "$out"
for moved in "$sid_bravo" 33333333-3333-4333-8333-333333333333; do
  [ -f "$HOME/.claude/projects/$enc/$moved.jsonl" ] || fail "session $moved is not in ~/.claude"
done
if [ -e "$HOME/.claude-work/projects/$enc/$sid_bravo.jsonl" ] || [ -e "$HOME/.claude-mine/projects/$enc/33333333-3333-4333-8333-333333333333.jsonl" ]; then
  fail "a moved session is still in its old dir"
fi
ok "the team and registered personal sessions are now in ~/.claude only"
[ -f "$HOME/.claude-work/projects/$enc/$sid_alpha.jsonl" ] || fail "the open session moved"
ok "the open session stays in its dir"
if [ ! -f "$HOME/.claude/projects/$enc/$sid_default.jsonl" ] || [ ! -f "$HOME/.claude-side/projects/$enc/55555555-5555-4555-8555-555555555555.jsonl" ]; then
  fail "uninstall touched sessions outside the registered dirs"
fi
ok "sessions outside the registered dirs stay put"
expect_same "project memory is merged into ~/.claude" "$HOME/.claude-work/projects/$enc/memory/notes.md" "$HOME/.claude/projects/$enc/memory/notes.md"
expect_same "memory of a project without sessions is merged too" "$HOME/.claude-work/projects/-gone-project/memory/decisions.md" "$HOME/.claude/projects/-gone-project/memory/decisions.md"
# shellcheck disable=SC2016 # expanded by the inner shell
run bash -c 'source "$1"; type -t claude; claude -p new-terminal' _ "$rc"
if [ "$(printf '%s\n' "$out" | head -n 1)" != file ]; then fail "a new terminal does not run the real claude command" "$out"; fi
ok "a new terminal runs the real claude command"
expect_contains "a new terminal opens the default dir" "dir=<unset> args=-p new-terminal" "$out"
expect_same "uninstall restores the work settings.json byte for byte" "$T/before/work.json" "$HOME/.claude-work/settings.json"
expect_same "uninstall restores the rc file byte for byte" "$T/before/rc" "$rc"
expect_same "the personal settings.json was never touched" "$T/before/personal.json" "$HOME/.claude/settings.json"
expect_same "the personal login was never touched" "$T/before/claude.json" "$HOME/.claude.json"
expect_same "the unregistered personal dir's settings.json was never touched" "$T/before/side.json" "$HOME/.claude-side/settings.json"
expect_missing "uninstall strips the upgraded dir too" "julienning" "$(cat "$HOME/.claude-old/settings.json")"

# Once that session has exited, --purge --move-sessions moves it without a
# question and removes julienning.
# A killed process can linger as a zombie, which kill -0 and ps still report
# alive: wait until it is gone, then drop its entry as Claude does on exit.
kill "$session_pid" 2>/dev/null || true
for _ in $(seq 1 50); do
  if ! kill -0 "$session_pid" 2>/dev/null; then break; fi
  sleep 0.2
done
if kill -0 "$session_pid" 2>/dev/null; then fail "the fake session $session_pid did not exit"; fi
rm -f "$HOME/.claude-work/sessions/$session_pid.json"
session_pid=""
run julienning uninstall --purge --yes --move-sessions
expect_contains "uninstall --move-sessions moves the rest without asking" "Sessions: moved 1 session into ~/.claude" "$out"
[ -f "$HOME/.claude/projects/$enc/$sid_alpha.jsonl" ] || fail "the exited session did not move"
[ ! -e "$JULIENNING_HOME" ] || fail "uninstall --purge left $JULIENNING_HOME"
[ ! -e "$JULIENNING_BIN_DIR/julienning" ] || fail "uninstall --purge left the command"
[ ! -e "$JULIENNING_VERSIONS_DIR" ] || fail "uninstall --purge left $JULIENNING_VERSIONS_DIR"
ok "uninstall --purge removed julienning's own files"
[ -f "$HOME/.claude-work/.claude.json" ] || fail "uninstall touched a Claude config dir"
ok "Claude config dirs survive uninstall"

printf '\ne2e: ok (%d checks)\n' "$checks"
