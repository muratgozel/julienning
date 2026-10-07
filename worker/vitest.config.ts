import { cloudflareTest } from "@cloudflare/vitest-pool-workers";
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    setupFiles: ["./test/setup.ts"],
  },
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      // Test-only token. Tests must pass on a fresh clone with no .dev.vars, and
      // this binding also wins over a developer's local .dev.vars if present.
      miniflare: { bindings: { AUTH_TOKEN: "test-token" } },
    }),
  ],
});
