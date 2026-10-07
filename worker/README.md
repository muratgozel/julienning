# julienning worker

Cloudflare Worker + KV (binding `USAGE`) behind `julienning accounts`, `next`,
`share`, `nick` (team-wide account nicknames) and the usage reports.
Contract: [../docs/SPEC.md](../docs/SPEC.md).
First-time Cloudflare setup, the team token, vars and KV limits:
[../docs/CLOUDFLARE.md](../docs/CLOUDFLARE.md).

```sh
npm ci                                  # install (lockfile pinned; .npmrc sets legacy-peer-deps)
npm test                                # vitest in workerd with KV emulation; injects its own AUTH_TOKEN
npm run typecheck                       # tsc --noEmit

cp .dev.vars.example .dev.vars          # once; put a throwaway AUTH_TOKEN in it (gitignored)
npm run dev                             # wrangler dev, reads AUTH_TOKEN from .dev.vars

export CLOUDFLARE_API_TOKEN=...         # docs/CLOUDFLARE.md steps 1-3
export CLOUDFLARE_ACCOUNT_ID=...
npx wrangler kv namespace create USAGE  # or: npm run kv:create; paste the id over REPLACE_ME in wrangler.jsonc
openssl rand -hex 32                    # the team token; store it in the password manager
npx wrangler secret put AUTH_TOKEN      # paste it when prompted
npm run deploy                          # wrangler deploy; prints the Worker URL
npx wrangler tail                       # live logs of the deployed Worker

read -rs TOKEN                          # verify
curl -s https://julienning.<your-subdomain>.workers.dev/healthz                                    # ok
curl -s -H "Authorization: Bearer $TOKEN" https://julienning.<your-subdomain>.workers.dev/accounts  # no accounts reported yet
```

`AUTH_TOKEN` is a Cloudflare secret, never a `vars` entry and never committed.
Pushes to `main` that touch `worker/` deploy through
`.github/workflows/deploy-worker.yml` (repository secrets
`CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`).
