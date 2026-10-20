# julienning

julienning lets a team share a pool of Claude subscriptions from the shell.
Each shared account has a team-wide nickname: `julienning next` switches to
the best free one, `julienning accounts` shows everyone's usage and who is on
what, and the session picker carries a conversation from one account to
another. Usage and "in use" claims go to a small Cloudflare Worker your team
runs. macOS and Linux, zsh and bash; Claude Code must already be installed.

**Privacy.** julienning never reads, copies or sends Claude credentials; it
switches accounts only through `CLAUDE_CONFIG_DIR`. Only accounts you share
with the team report anything, and only to your team's Worker: the email and
its nickname, session and weekly usage, rate-limit refusals, whether you have
it open, your dev name and a random machine id. Personal accounts and session
lists stay on your machine. Details: [Policy note](docs/REFERENCE.md#policy-note).

## Install

Get the Worker URL and the team token from your team lead, then:

```sh
curl -fsSL https://raw.githubusercontent.com/muratgozel/julienning/main/scripts/install.sh | bash
julienning setup
```

If the installer says `~/.local/bin` is not on your PATH, run the line it
prints. Setup asks for your dev name (what teammates see), the Worker URL and
token, which Claude accounts on this machine to share and under which
nickname, and, when tmux is installed, whether claude should run inside tmux.
It adds a status line and hooks to each shared account's
`settings.json` and one line to your shell rc file; open a new terminal
afterwards. It is safe to re-run.

For another account later, `julienning new-config` creates a config dir and
starts claude in it so you can sign in; the account is shared when your first
session starts (`--no-share` keeps it personal). On another machine,
`julienning login <nickname>` signs a team account in there, in a new dir
when none can take it.

## Daily use

```sh
julienning accounts          # every shared account: usage and who is on it
julienning next              # switch to the best free account, then pick a session
julienning use [nickname]    # switch to that account (none: stay), then pick a session
claude                       # runs on the selected account, in any terminal
claude-<nickname>            # claude on that account; the selection stays
julienning use <email>       # a personal (not shared) account; the selection stays
julienning pin <target>      # plain claude in this directory and below opens that account
julienning sync              # the same Claude Code settings and CLAUDE.md on every shared account
julienning login <nickname>  # sign that account in here (a new dir, or again after a sign-out)
```

```
$ julienning accounts
#  NICK   ACCOUNT              SESSION      WEEK              STATE                              UPDATED
1  alpha  claude1@example.com  4% → 23:38   11% → Fri 00:28   free                               2m ago
2  beta   claude2@example.com  62% → 22:13  40% → Fri 00:28   in use by ali, can (12m)           12m ago
3  gamma  claude5@example.com  23% → 22:40  100% → Fri 10:00  exhausted (week resets Fri 10:00)  5m ago
```

SESSION and WEEK are the used % and reset time in your local zone. STATE
ends in `, signed out here` when this machine lost an account's login
(`julienning login <nickname>` signs it in again). `next` takes the
best-ranked account logged in on this machine; `julienning configs` shows
which dir here holds which account.

In a terminal, `next` and `use` then list recent sessions of the current
project from all your shared accounts (`--all` for every project). Picking one
that lives under another account moves it into the account you switched to,
after asking; project memory is merged, never overwritten. A session open in
another terminal can't be picked.

## How it stays current

- The status line reports usage as you work. For accounts nobody is using
  through julienning, `accounts`, `next` and `use` ask Claude Code itself
  (`claude -p "/usage"`, Claude Code 2.1.301 or newer; no quota used).
- Session hooks claim an account while you have a session open on it and
  release it when the last one closes.
- julienning updates itself once a day; `julienning update` updates now, and
  `"auto_update": false` in `~/.julienning/config.json` turns it off.
- The status line, hooks and background reports log to
  `~/.julienning/errors.log` (never emails). See
  [Troubleshooting](docs/REFERENCE.md#troubleshooting).

## Other machines

A machine you reach over ssh can run julienning against the same team:

```sh
julienning host add box          # install and set up julienning on box
julienning use --host box        # pick a session and run claude on box, in this terminal
julienning transfer --to box     # move a conversation into box's checkout of this repo and resume it
```

Most commands take `--host NAME`. See
[Other machines](docs/REFERENCE.md#other-machines).

## Undo

- `julienning use --clear`: back to plain `claude` on your default config.
- `julienning forget <target>`: unregister one config dir and undo its
  settings.json changes; it offers to delete the dir.
- `julienning uninstall`: undo setup on this machine and offer to move every
  session into `~/.claude`, where plain `claude` finds them. `--purge` also
  removes julienning itself.

None of these unshares an account for the team.

## For the team lead

1. Deploy the Worker and generate the team token with
   [docs/CLOUDFLARE.md](docs/CLOUDFLARE.md); the free tier is enough to start.
2. Send teammates the Worker URL, and the token through a password manager.
3. Manage the shared accounts:

   ```sh
   julienning share <email> [--nick <nickname>]   # share an account (nickname default: the part before @)
   julienning nick <nickname> <new>               # rename it for the whole team
   julienning unshare <nickname>                  # remove it for everyone, with its usage and claims
   ```

After rotating the token, everyone runs `julienning setup --token <new token>`.

## More

- [docs/REFERENCE.md](docs/REFERENCE.md): every command, flag and output, what
  setup changes, and troubleshooting.
- [docs/SPEC.md](docs/SPEC.md): the design and the CLI–Worker contract.
- Development: `make build`, `make test`, `make e2e`; see
  [Development](docs/REFERENCE.md#development).

MIT licensed; see [LICENSE](LICENSE).
