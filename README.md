# julienning

julienning lets a team share a pool of Claude subscriptions from the shell.
Every shared account has a team-wide nickname: `julienning use alpha` switches
to it, `julienning next` picks the best free one, and the session picker hands
a conversation over from one account to another. A small Cloudflare Worker
collects each account's usage and who has it open, so `julienning accounts`
shows everyone who is using what and how much of each account's session and
weekly limit is left. macOS and Linux, zsh and bash; Claude Code must already
be installed.

- **No auth tokens.** julienning never reads, copies or sends Claude credentials; signing in stays Claude Code's own flow.
- **Only `CLAUDE_CONFIG_DIR`.** That is how it switches accounts; from Claude it takes just the account email and the usage numbers the status line shows.
- **Reports go to your team's own Worker:** that email and its nickname, those numbers, whether you have it open, your username and a random machine id. Session lists stay local.
- **Personal accounts never report.** An account that is not shared with the team sends no usage and no claims.

Details: [Policy note](docs/REFERENCE.md#policy-note).

## For teammates

Get the **Worker URL** and the **team token** from your team lead, then:

1. Install:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/muratgozel/julienning/main/scripts/install.sh | bash
   ```

   If it prints `… is not on your PATH. Add it:`, run the `zsh:` line it
   shows, then `exec zsh`.

2. Set up this machine:

   ```sh
   julienning setup
   ```

   It asks for:

   - **your dev name**, the username teammates see (Enter keeps `$USER`);
   - **the Worker URL and the team token** (the token is hidden as you type;
     both are checked before anything is saved);
   - **`Share <email> (found in <dir>) with the team? [y/N]`** for each Claude
     account it finds here that the team does not share yet: `y` for team
     accounts, `n` for personal ones (it remembers). Accounts the team already
     shares are picked up without asking;
   - **`Nickname for <email> [<default>]:`** for each account you share. Enter
     takes the part before `@`; the nickname is the same for the whole team.

   It then adds julienning's status line and session hooks to each team
   account's `settings.json` (a status line of your own there is saved and
   restored by `julienning uninstall`) and one line to `~/.zshrc`.

3. Load the new shell functions:

   ```sh
   exec zsh
   ```

No separate Claude config dir for a shared account yet?
`julienning new-config --login` creates one and starts Claude in it so you can
sign in; if that account is new to the team, run `julienning setup` again to
share it.

## Daily use

```sh
julienning next              # switch to the best shared account on this machine, then pick a session
julienning use               # stay on the current account, straight to its session picker
julienning use <nickname>    # switch to that account, then pick a session
claude-<nickname>            # plain claude on that account; your selection stays as it is
julienning accounts          # every shared account: usage, who is on it, which dir here has it
```

The selection is machine-wide: after `next` or `use`, plain `claude` in any
terminal runs on that account.

```
$ julienning accounts
#  NICK     ACCOUNT              SESSION      WEEK             STATE                     LOCAL                   UPDATED
1  alpha    claude1@example.com  4% → 23:38   11% → Fri 00:28  free                      *~/.claude-julienning1  2m ago
2  claude3  claude3@example.com  -            -                free                      -                       never
3  delta    claude4@example.com  -            -                free                      ~/.claude-work          never
4  beta     claude2@example.com  62% → 22:13  40% → Fri 00:28  in use by ali, can (12m)  -                       12m ago
```

SESSION and WEEK are the used % and when it resets, in your local time. STATE
names the teammates on the account. LOCAL is the dir here that is logged into
it (`*` is your selection). `next` takes the highest account that is logged in
on this machine.

### Session picker

In a terminal, `next` and `use` list recent sessions for the current project
from all your accounts, newest first, with **New session** on top (`--all`
lists every project).

- ↑/↓ to move (j/k too, before you type), type to filter, Enter to start or
  resume, Esc to cancel (the selection stays).
- Picking a session that lives under another account **moves** it into the
  account you switched to, project memory included (merged, never
  overwritten), and resumes it there.
- A session that is open in another terminal is greyed out and can't be picked
  or moved; exit it there first.

## Good to know

- **Claims follow open sessions.** Opening Claude on a shared account shows it
  as yours in everyone's `accounts` (`claimed by`, then `in use by` once usage
  comes in); closing your last session on it releases it. Switching alone
  claims nothing.
- **Update** with `julienning update`. Commands print a one-line hint when a
  newer release is out.
- **Undo** with `julienning uninstall`: it reverts what setup changed in your
  Claude settings and shell (settings.json entries, the shell line), releases
  your claims and clears the selection. `--purge` also deletes julienning's
  own data and the command. Claude config dirs, logins and sessions are never
  touched, and nothing is unshared for the team.
- **Back to plain claude:** `julienning use --clear`.
- **Errors:** commands print theirs; the status line, hooks and background
  reports write to `~/.julienning/errors.log` (it never contains emails). See
  [Troubleshooting](docs/REFERENCE.md#troubleshooting).

## For the team lead

1. Deploy the Worker and generate the team token:
   [docs/CLOUDFLARE.md](docs/CLOUDFLARE.md) walks through it from a fresh
   Cloudflare account; the free tier is enough to start.
2. Send teammates the Worker URL, and the token through your password manager
   only.
3. Manage the team's accounts (a nickname or an email works as the target):

   ```sh
   julienning share <email> [--nick <nickname>]   # add an account under a nickname (default: the part before @)
   julienning nick <nickname> <new>               # rename it for the whole team
   julienning unshare <nickname>                  # remove it for everyone, with its usage and claims (asks first)
   ```

Teammates pick changes up on the hourly background refresh (right away with
`julienning accounts`) and get new `claude-<nickname>` functions in their next
terminal. After rotating the token, everyone runs
`julienning setup --token <new token>`.

## More

- [docs/REFERENCE.md](docs/REFERENCE.md): every command, flag and output, what
  setup changes, and troubleshooting.
- [docs/SPEC.md](docs/SPEC.md): the design, and the contract between the CLI
  and the Worker.
- **Development:** Go 1.25. `make build` (to `bin/julienning`), `make test` (Go
  and Worker tests); release by pushing a `vX.Y.Z` tag. See
  [Development](docs/REFERENCE.md#development).

MIT licensed; see [LICENSE](LICENSE).
