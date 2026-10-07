/// <reference types="@cloudflare/vitest-pool-workers/types" />

import type { Env as AppEnv } from "../src/types";

declare global {
  // `env` from "cloudflare:test" resolves to Cloudflare.Env; wire it to ours
  // instead of generating worker-configuration.d.ts (AUTH_TOKEN is a secret
  // and would not appear there).
  namespace Cloudflare {
    interface Env extends AppEnv {}
  }
}

declare module "cloudflare:test" {
  interface ProvidedEnv extends AppEnv {}
}
