# Deploying the julienning Worker

For whoever runs the team's Worker, starting from a fresh Cloudflare account.
Everything happens in `worker/`. The free tier is enough to start; see "KV
limits" at the end.

This repository is public. The team token is a secret: it lives in Cloudflare,
in your password manager and in each teammate's `~/.julienning/config.json`,
and never in git, an issue, a PR or a chat log.

## 1. Account ID

Log in to <https://dash.cloudflare.com>, open **Workers & Pages** → **Overview**.
The **Account ID** is in the right-hand sidebar. Copy it.

## 2. API token

**My Profile** → **API Tokens** → **Create Token** → use the **Edit Cloudflare Workers**
template. It grants:

- Account · Workers Scripts: Edit
- Account · Workers KV Storage: Edit
- Account · Workers Tail: Read
- Account · Account Settings: Read
- User · User Details: Read
- Zone · Workers Routes: Edit (only needed for custom domains)

Scope **Account Resources** to your account. If the template does not list
**Workers KV Storage: Edit**, add it manually — `wrangler kv namespace create`
fails without it. Create the token and copy it; it is shown once.

## 3. Export credentials

```sh
export CLOUDFLARE_API_TOKEN=<token from step 2>
export CLOUDFLARE_ACCOUNT_ID=<account id from step 1>
```

## 4. Install

```sh
cd worker
npm ci
```

## 5. Create the KV namespace

```sh
npx wrangler kv namespace create USAGE
```

It prints something like:

```
{ "binding": "USAGE", "id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9" }
```

Paste only that `id` into `worker/wrangler.jsonc`, replacing `REPLACE_ME`.
Newer wrangler versions also print a `"remote": true` line: leave it out. With
it, `wrangler dev` and `npm test` would read and write the production
namespace and demand `CLOUDFLARE_API_TOKEN`; without it they use a local
emulated namespace, which is what you want. Committing the id is fine: a
namespace id is not a secret and is useless without an API token. A fork
deploying its own Worker replaces it with its own id.

## 6. Generate and set the team token

Generate a fresh random token (64 hex characters):

```sh
openssl rand -hex 32
```

Store it in the team's password manager right away, then set it as the
Worker's secret and paste it when prompted:

```sh
npx wrangler secret put AUTH_TOKEN
```

Share it with teammates only out of band, through the password manager. Never
commit it, and never put it in `wrangler.jsonc` `vars` (those are plain text
and visible in the dashboard). Secrets survive deploys; re-run this step only to
rotate the token.

**Rotating** (a token leaked, or someone left the team): generate a new one,
run `npx wrangler secret put AUTH_TOKEN` again, update the password manager, and
have everyone re-run `julienning setup --token <new token>`. The old token stops
working immediately.

## 7. Choose the workers.dev subdomain

Every Worker on the account is served at `<worker name>.<subdomain>.workers.dev`.
Cloudflare picks the subdomain from the account name with a lossy
transliteration (an account called "Gözel" becomes `g-zel`), so set it before
the first deploy or change it afterwards. It is one setting for the whole
account: changing it moves every Worker at once, and the old subdomain stops
resolving immediately. If teammates already ran setup, they must re-run
`julienning setup --remote-url <new url>`.

Rules: letters, digits and dashes only, no leading or trailing dash, and it
must be unused by any other Cloudflare account.

**Dashboard:** on the account-level **Workers & Pages** page (the breadcrumb
root, not a Worker's own page), the right-hand **Account details** panel shows
**Your subdomain** with a **Change** link. If your dashboard has no such
link, use the API.

**API** (same token as step 2, which has Workers Scripts Write). The PUT only
*creates* a subdomain; on an account that already has one it answers error
`10036 Account already has an associated subdomain`, so a change is a delete
followed by a create. Between the two calls every workers.dev URL on the
account is dead for a few seconds, and the Worker may need one redeploy to
re-attach its workers.dev route:

```sh
API="https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/workers/subdomain"
H="Authorization: Bearer $CLOUDFLARE_API_TOKEN"
curl -s "$API" -H "$H"                                   # current slug (keep it in case the new one is taken)
curl -s -X DELETE "$API" -H "$H"
curl -s -X PUT "$API" -H "$H" -H "Content-Type: application/json" --data '{"subdomain":"gozel"}'
```

If the PUT answers that the name is taken, immediately PUT the old slug back
so the account is not left without one, then pick another name or use a
domain you own. After a successful PUT, check the Worker page: if
**Domains and routes** no longer lists a workers.dev route, run
`npm run deploy` once (wrangler re-enables it unless `workers_dev` is false).

**Custom domain instead:** if the team has a domain on Cloudflare (an active
zone), add to `worker/wrangler.jsonc` and redeploy:

```jsonc
"routes": [{ "pattern": "julienning.example.com", "custom_domain": true }],
"workers_dev": false
```

The Worker then answers at `https://julienning.example.com`, which never
changes when the account is renamed. Leave out `workers_dev: false` to keep
the workers.dev URL working as well.

## 8. Deploy

```sh
npm run deploy
```

Note the URL it prints, e.g. `https://julienning.<your-subdomain>.workers.dev`.

## 9. Verify

```sh
read -rs TOKEN   # paste the team token; keeps it out of shell history
curl -s https://julienning.<your-subdomain>.workers.dev/healthz          # -> ok
curl -s -H "Authorization: Bearer $TOKEN" \
  https://julienning.<your-subdomain>.workers.dev/accounts               # -> no accounts reported yet
```

A 401 means the secret did not take; re-run step 6 and redeploy.

## 10. Onboard teammates

Send each teammate the Worker URL (not secret) and point them to the token in
the password manager. They run:

```sh
julienning setup
```

and paste the Worker URL and the token when asked (the token is read without
echo). Setup checks both against the Worker, then offers to share each
logged-in account with the team and asks for a **nickname** for each account
it shares (default: the email's local part, `claude1@example.com` →
`claude1`). Nothing team-specific is compiled into the binary; for
non-interactive installs use
`julienning setup --remote-url <url> --token <token>`, or the
`JULIENNING_REMOTE_URL` / `JULIENNING_TOKEN` env vars (these win at runtime).

Nicknames are team-wide: the Worker stores one per shared account, everyone
types and sees it instead of the email, and whoever shares an account first
picks it for the whole team. A nickname is 1–32 characters of `a-z`, `0-9`,
`.`, `_`, `-`, starts with a letter or digit, and is case-insensitive and
unique across the team: the Worker refuses one that another account holds and
names that account. `julienning share EMAIL --nick NAME` sets one when
sharing from the command line; `julienning nick TARGET NEW` renames.
Accounts shared before nicknames existed show `-` until setup or `nick` gives
them one.

The Worker requires a nickname when an email is shared for the first time
(`PUT /accounts/:email`; renames are `PUT /accounts/:email/nickname`, see
"Worker" in `docs/SPEC.md`). A julienning CLI from before nicknames gets
`400 nickname is required to share a new account` when sharing a new email
and has to be updated; everything else it does keeps working.

## 11. Local development

```sh
cd worker
cp .dev.vars.example .dev.vars   # then put any local-only token in it
npm run dev
```

`.dev.vars` is gitignored; it only feeds `wrangler dev`. Use a throwaway value
there, not the production token. The tests never read it: they inject their own
`AUTH_TOKEN` binding (see `worker/vitest.config.ts`), so `npm test` works on a
fresh clone.

## 12. Configuration

Plain vars in `worker/wrangler.jsonc` (change, commit, deploy):

| Var | Default | Meaning |
|-----|---------|---------|
| `DEFAULT_TZ` | `UTC` | Time zone for the text table when the request has no `?tz=`. An invalid value is logged and treated as `UTC`. |
| `CLAIM_TTL_MIN` | `720` | Claims older than this are ignored. A safety net only: the CLI releases claims when sessions end. |
| `ACTIVITY_TTL_MIN` | `15` | A usage report younger than this marks the account `in_use`. |

## 13. GitHub Actions

`.github/workflows/deploy-worker.yml` deploys when you run it from the
repository's **Actions** tab (it is manual on purpose: nothing deploys just
because a change was merged). It needs two repository secrets (**Settings** → **Secrets and
variables** → **Actions** → **New repository secret**):

- `CLOUDFLARE_API_TOKEN`
- `CLOUDFLARE_ACCOUNT_ID`

Use repository secrets, never workflow `env:` literals: this repo is public and
its workflow files and logs are world-readable. GitHub does not pass secrets to
workflows triggered from forks, so a fork's pull request cannot deploy.

`AUTH_TOKEN` is not needed there — it lives in Cloudflare and survives deploys.

## 14. KV limits

Each writer owns its own key (`share:<email>`, `usage:<email>`,
`claim:<email>:<dev>:<machine_id>`; see "Worker" in `docs/SPEC.md`), so
concurrent requests never overwrite each other's data. All data lives in KV
metadata, which keeps listings to a single operation.

KV has no transactions or conditional writes, so two nickname edge cases are
accepted rather than prevented. Both need two people acting at the same
moment:

- **Duplicate nickname.** The uniqueness check reads the existing `share:`
  keys, then writes. Two people sharing (or renaming) *different* emails to
  the same nickname at the same second can both succeed; KV listings can lag
  writes by up to about a minute across Cloudflare locations, which widens
  that window for teammates far apart. The account listing then shows both
  with the same nickname; rename one of them with `julienning nick`.
- **Rename during unshare.** A rename is the one request that rewrites a key
  another request also writes (`share:<email>`). If one person renames an
  account while another unshares it, the share key can come back with the new
  nickname and no usage or claims. Unshare it again.

The free tier allows **1,000 KV writes per day across the whole account**.
Writes happen on:

- `PUT usage`: one write to `usage:<email>`, plus a re-stamp of the
  reporter's own claim key only when that claim is older than an hour (at
  most one extra write per holder per hour). The CLI sends at most
  one report per account per machine every 5 minutes
  (`send_min_interval_sec`) while a session is active, except that a new
  rate-limit window (changed reset time) is reported immediately. A report
  older than the stored one writes nothing;
- `PUT claim`: one write to that holder's own key, on session start plus a
  refresh at most hourly;
- share (`PUT /accounts/:email`) and nickname rename
  (`PUT /accounts/:email/nickname`): one write each, none when the email is
  already shared or the nickname is unchanged. Rare.

That is roughly 100–110 writes per developer per 8-hour working day (96
reports at the 5-minute cap plus about 8 hourly claim refreshes and a few
session starts), so the free tier covers a team of about five to eight
actively working developers.
Deletes have their own 1,000/day quota: one per `DELETE claim` (session end),
and on unshare one each for the share key, the usage key and every claim key.
Reads (100,000/day) are not a concern. List operations (1,000/day) cost one
per `GET /accounts` (one page holds 1,000 keys, and each shared account uses
two plus one per active holder), one per `GET /accounts/:email`, one per
unshare, and one per new share or rename (the nickname uniqueness check lists
the `share:` keys). The CLI lists when you run `accounts`, `next`, `use` or `current`, and
when a machine refreshes its cached allowlist (at most hourly).

When the account is over the daily write limit, KV writes throw inside the
Worker, which answers `500 {"error":"internal error"}`; the CLI logs that as
`SEND_FAILED … 500 internal error` in `~/.julienning/errors.log`. Confirm the
cause with `npx wrangler tail`, then switch to **Workers Paid** ($5/month),
which includes 1 million KV writes per month (then $5 per additional million).
