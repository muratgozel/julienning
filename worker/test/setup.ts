import { reset } from "cloudflare:test";
import { afterEach } from "vitest";

// vitest-pool-workers v0.22 no longer isolates storage per test automatically.
afterEach(async () => {
  await reset();
});
