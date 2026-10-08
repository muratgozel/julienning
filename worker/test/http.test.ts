import { env, SELF } from "cloudflare:test";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  A,
  ALI,
  B,
  BASE,
  byEmail,
  call,
  CAN,
  claimKey,
  claimsOf,
  deleteClaim,
  getJson,
  holders,
  iso,
  keyNames,
  listJson,
  MURAT,
  MURAT_LAPTOP,
  NOT_SHARED,
  putClaim,
  putUsage,
  rawKeys,
  seed,
  sendUsage,
  share,
  shared,
  shareKey,
  shareOf,
  TOKEN,
  unshare,
  usageBody,
  usageKey,
  usageOf,
  type Who,
} from "./helpers";

describe("health and auth", () => {
  it("serves /healthz without a token", async () => {
    const res = await SELF.fetch(`${BASE}/healthz`);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("ok");
  });

  it("rejects missing and wrong tokens with 401 JSON", async () => {
    for (const init of [
      { auth: false },
      { auth: false, headers: { authorization: "Bearer nope" } },
      { auth: false, headers: { authorization: "Basic test-token" } },
      { auth: false, headers: { authorization: "Bearer test-toke" } },
    ]) {
      const res = await call("/accounts", init);
      expect(res.status).toBe(401);
      expect(await res.json()).toEqual({ error: "unauthorized" });
    }
  });

  it("accepts ?token= on GET only", async () => {
    const ok = await SELF.fetch(`${BASE}/accounts?token=${TOKEN}`);
    expect(ok.status).toBe(200);

    const writes: [string, string, string | undefined][] = [
      [`/accounts/${A}/usage`, "PUT", usageBody()],
      [`/accounts/${A}/claim`, "PUT", JSON.stringify(MURAT)],
      [`/accounts/${A}/claim?dev=murat&machine_id=${MURAT.machine_id}`, "DELETE", undefined],
      [`/accounts/${A}`, "PUT", JSON.stringify({ added_by: MURAT, nickname: "alpha" })],
      [`/accounts/${A}/nickname`, "PUT", JSON.stringify({ nickname: "alpha" })],
      [`/accounts/${A}/exhausted`, "PUT", JSON.stringify({ window: "week", reporter: MURAT })],
      [`/accounts/${A}`, "DELETE", undefined],
    ];
    for (const [path, method, body] of writes) {
      const sep = path.includes("?") ? "&" : "?";
      const res = await SELF.fetch(`${BASE}${path}${sep}token=${TOKEN}`, {
        method,
        ...(body !== undefined ? { body, headers: { "content-type": "application/json" } } : {}),
      });
      expect(res.status).toBe(401);
      expect(await res.json()).toEqual({ error: "unauthorized" });
    }
    expect(await keyNames()).toEqual([]);
  });

  it("prefers the Authorization header over ?token=", async () => {
    const res = await SELF.fetch(`${BASE}/accounts?token=nope`, {
      headers: { authorization: `Bearer ${TOKEN}` },
    });
    expect(res.status).toBe(200);
  });

  it("uses the injected test token, not a developer's .dev.vars", () => {
    expect(env.AUTH_TOKEN).toBe(TOKEN);
  });
});

describe("routing", () => {
  it("returns 404 JSON for unknown routes", async () => {
    for (const path of ["/", "/nope", "/accounts/a@x.io/nope", "/accounts/a@x.io/claim/extra"]) {
      const res = await call(path);
      expect(res.status).toBe(404);
      expect(res.headers.get("content-type")).toContain("application/json");
      expect(await res.json()).toEqual({ error: "not found" });
    }
  });

  it("returns 405 JSON with Allow for method mismatches", async () => {
    const cases: [string, string, string][] = [
      ["/accounts", "POST", "GET"],
      [`/accounts/${A}`, "POST", "GET, PUT, DELETE"],
      [`/accounts/${A}/usage`, "DELETE", "PUT"],
      [`/accounts/${A}/claim`, "POST", "PUT, DELETE"],
      [`/accounts/${A}/nickname`, "GET", "PUT"],
      [`/accounts/${A}/exhausted`, "DELETE", "PUT"],
      ["/healthz", "POST", "GET"],
    ];
    for (const [path, method, allow] of cases) {
      const res = await call(path, { method });
      expect(res.status).toBe(405);
      expect(res.headers.get("allow")).toBe(allow);
      expect(await res.json()).toEqual({ error: "method not allowed" });
    }
  });

  it("checks auth before routing", async () => {
    const res = await call("/nope", { auth: false });
    expect(res.status).toBe(401);
  });
});

describe("validation", () => {
  it("returns 400 {error, field} for a bad email in the path", async () => {
    for (const [path, method] of [
      ["/accounts/not-an-email/usage", "PUT"],
      ["/accounts/not-an-email", "PUT"],
      ["/accounts/not-an-email", "DELETE"],
      ["/accounts/not-an-email", "GET"],
    ] as const) {
      const res = await call(path, { method, ...(method === "PUT" ? { body: usageBody() } : {}) });
      expect(res.status).toBe(400);
      expect(await res.json()).toMatchObject({ field: "email" });
    }
  });

  it("returns 400 {error, field} for bad usage bodies, before the allowlist check", async () => {
    const cases: [unknown, string][] = [
      [{ ...JSON.parse(usageBody()), session: { used: -1, resets_at: iso(60) } }, "session.used"],
      [{ ...JSON.parse(usageBody()), week: { used: 5, resets_at: "nope" } }, "week.resets_at"],
      [{ ...JSON.parse(usageBody()), collected_at: "2026-01-01" }, "collected_at"],
      [{ ...JSON.parse(usageBody()), reporter: { dev: "MURAT", machine_id: "3fa9c2d1e07b" } }, "reporter.dev"],
      [{ ...JSON.parse(usageBody()), reporter: { dev: "murat", machine_id: "xyz" } }, "reporter.machine_id"],
    ];
    for (const [body, fieldName] of cases) {
      const res = await call(`/accounts/${A}/usage`, { method: "PUT", body: JSON.stringify(body) });
      expect(res.status).toBe(400);
      const json = (await res.json()) as { error: string; field: string };
      expect(json.field).toBe(fieldName);
      expect(json.error).toBeTruthy();
    }
  });

  it("rejects a body over 4 KB", async () => {
    await shared(A);
    const body = JSON.stringify({ ...JSON.parse(usageBody()), pad: "x".repeat(4096) });
    const res = await call(`/accounts/${A}/usage`, { method: "PUT", body });
    expect(res.status).toBe(400);
    expect(await res.json()).toMatchObject({ field: "body" });
  });

  it("rejects an invalid tz and format", async () => {
    const tz = await call("/accounts?tz=Mars/Olympus");
    expect(tz.status).toBe(400);
    expect(await tz.json()).toMatchObject({ field: "tz" });

    const fmt = await call("/accounts?format=yaml");
    expect(fmt.status).toBe(400);
    expect(await fmt.json()).toMatchObject({ field: "format" });

    const dev = await call("/accounts?dev=NotADev");
    expect(dev.status).toBe(400);
    expect(await dev.json()).toMatchObject({ field: "dev" });
  });
});

describe("share and unshare", () => {
  it("writes only share:<email> with added_by and a server added_at (201)", async () => {
    const before = Date.now();
    const res = await share("Claude1@SixtyNine.Agency", MURAT);
    expect(res.status).toBe(201);

    expect(await keyNames()).toEqual([shareKey(A)]);
    const [key] = await rawKeys();
    expect(key!.expiration).toBeUndefined();
    const record = await shareOf(A);
    expect(record).toEqual({ added_by: MURAT, added_at: record.added_at, nickname: "claude1" });
    const addedAt = Date.parse(record.added_at);
    expect(record.added_at).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
    expect(addedAt).toBeGreaterThanOrEqual(before - 1000);
    expect(addedAt).toBeLessThanOrEqual(Date.now() + 1000);
  });

  it("returns 204 and writes nothing when the email is already shared", async () => {
    await shared(A);
    await putUsage(A, MURAT, 42, 7);
    await putClaim(A, ALI);
    const before = await rawKeys();

    const again = await share(A, ALI);
    expect(again.status).toBe(204);
    expect(await again.text()).toBe("");
    expect(await rawKeys()).toEqual(before);
    expect((await shareOf(A)).added_by).toEqual(MURAT);
  });

  it("validates the share body", async () => {
    const cases: [string | undefined, string][] = [
      [undefined, "body"],
      ["{nope", "body"],
      ["[]", "body"],
      ["{}", "added_by"],
      [JSON.stringify({ added_by: { dev: "murat" } }), "added_by.machine_id"],
      [JSON.stringify({ added_by: { dev: "Murat!", machine_id: MURAT.machine_id } }), "added_by.dev"],
    ];
    for (const [body, fieldName] of cases) {
      const res = await call(`/accounts/${A}`, { method: "PUT", ...(body !== undefined ? { body } : {}) });
      expect(res.status).toBe(400);
      expect(await res.json()).toMatchObject({ field: fieldName });
    }
    expect(await keyNames()).toEqual([]);
  });

  it("lists a shared account that never reported, with empty arrays", async () => {
    await shared(A);
    const account = byEmail(await listJson("&dev=murat"), A);
    expect(account).toEqual({
      rank: 1,
      email: A,
      nickname: "claude1",
      added_by: MURAT,
      added_at: account.added_at,
      claims: [],
      state: "free",
      busy_by: [],
      exhausted: false,
      exhausted_until: null,
      exhausted_window: null,
    });

    const text = await (await call("/accounts?dev=murat")).text();
    expect(text.split("\n")[1]).toMatch(new RegExp(`^1\\s+claude1\\s+${A}\\s+-\\s+-\\s+free\\s+never$`));
  });

  it("unshares with 204, deleting the share, usage and every claim key", async () => {
    await shared(A, B);
    await putUsage(A);
    await putClaim(A, ALI);
    await putClaim(A, MURAT);
    await putClaim(A, MURAT_LAPTOP);
    await putClaim(B, CAN);

    const res = await unshare(A);
    expect(res.status).toBe(204);
    expect(await keyNames()).toEqual([claimKey(B, CAN), shareKey(B)]);
    expect((await listJson()).accounts.map((a) => a.email)).toEqual([B]);

    const usage = await call(`/accounts/${A}/usage`, { method: "PUT", body: usageBody() });
    expect(usage.status).toBe(404);
    expect(await usage.json()).toEqual(NOT_SHARED);
  });

  it("unshares idempotently", async () => {
    expect((await unshare(A)).status).toBe(204);
    expect((await unshare(A)).status).toBe(204);
    expect(await keyNames()).toEqual([]);
  });

  it("can share again after unsharing, starting clean", async () => {
    await shared(A);
    await putUsage(A);
    await putClaim(A, MURAT);
    await unshare(A);

    expect((await share(A, ALI)).status).toBe(201);
    expect((await shareOf(A)).added_by).toEqual(ALI);
    expect(await usageOf(A)).toBeNull();
    expect(await claimsOf(A)).toEqual([]);
  });
});

describe("accounts that are not shared", () => {
  it("404s usage, claim and GET with the exact not-shared body and writes nothing", async () => {
    const usage = await call(`/accounts/${A}/usage`, { method: "PUT", body: usageBody() });
    expect(usage.status).toBe(404);
    expect(await usage.json()).toEqual(NOT_SHARED);

    const claim = await call(`/accounts/${A}/claim`, { method: "PUT", body: JSON.stringify(MURAT) });
    expect(claim.status).toBe(404);
    expect(await claim.json()).toEqual(NOT_SHARED);

    const get = await call(`/accounts/${A}`);
    expect(get.status).toBe(404);
    expect(get.headers.get("content-type")).toContain("application/json");
    expect(await get.json()).toEqual(NOT_SHARED);

    expect(await keyNames()).toEqual([]);
  });

  it("treats an invalid share record as not shared, and sharing again repairs it", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      for (const bad of [undefined, { added_by: "murat" }, { added_by: MURAT, added_at: "yesterday" }]) {
        await seed(shareKey(A), bad);
        const usage = await sendUsage(A);
        expect(usage.status).toBe(404);
        expect(await usage.json()).toEqual(NOT_SHARED);
        expect((await call(`/accounts/${A}/claim`, { method: "PUT", body: JSON.stringify(ALI) })).status).toBe(404);
        expect((await call(`/accounts/${A}`)).status).toBe(404);
        expect((await listJson()).accounts).toEqual([]);
      }
      expect(warn).toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }

    expect((await share(A, ALI)).status).toBe(201);
    expect((await shareOf(A)).added_by).toEqual(ALI);
    await putUsage(A);
    expect((await usageOf(A))!.session.used).toBe(50);
  });

  it("accepts DELETE claim with 204 and creates nothing", async () => {
    const res = await deleteClaim(A, MURAT);
    expect(res.status).toBe(204);
    expect(await keyNames()).toEqual([]);
  });
});

describe("usage round-trip", () => {
  beforeEach(async () => {
    await shared(A);
  });

  it("stores a report under the lowercased email and computes fields on read", async () => {
    await putUsage("Claude1@SixtyNine.Agency");
    expect(await keyNames()).toEqual([shareKey(A), usageKey(A)]);

    const account = await getJson(A, "murat");
    expect(account.email).toBe(A);
    expect(account.added_by).toEqual(MURAT);
    expect(account.session!.used).toBe(50);
    expect(account.session!.effective).toBe(50);
    expect(account.session!.reset_passed).toBe(false);
    expect(account.week!.effective).toBe(28);
    expect(account.reporter).toEqual(MURAT);
    expect(account.state).toBe("free");
    expect(account.busy_by).toEqual([]);
    expect(account.claims).toEqual([]);
    expect(account).not.toHaveProperty("rank");
  });

  it("writes only usage:<email>, with an 8-day expiration, leaving share and claims alone", async () => {
    const shareBefore = await shareOf(A);
    await putClaim(A, ALI);
    const claimsBefore = await claimsOf(A);
    const before = Date.now();
    await putUsage(A, MURAT);

    expect(await shareOf(A)).toEqual(shareBefore);
    expect(await claimsOf(A)).toEqual(claimsBefore);
    const usage = await usageOf(A);
    expect(usage).toEqual({
      session: { used: 50, resets_at: usage!.session.resets_at },
      week: { used: 28, resets_at: usage!.week.resets_at },
      collected_at: usage!.collected_at,
      reporter: MURAT,
    });

    const key = (await rawKeys()).find((k) => k.name === usageKey(A))!;
    const eightDays = 8 * 24 * 60 * 60;
    expect(key.expiration).toBeGreaterThanOrEqual(Math.floor(before / 1000) + eightDays - 5);
    expect(key.expiration).toBeLessThanOrEqual(Math.ceil(Date.now() / 1000) + eightDays + 5);
  });

  it("overwrites the previous report", async () => {
    await putUsage(A, MURAT, 10, 10);
    await putUsage(A, ALI, 90, 80);
    const after = await usageOf(A);
    expect(after!.session.used).toBe(90);
    expect(after!.reporter).toEqual(ALI);
  });

  it("ignores a report older than the stored one (lagging clock) with a silent 204", async () => {
    const newer = iso(0);
    await putUsage(A, MURAT, 50, 28, newer);
    const before = await rawKeys();

    const res = await sendUsage(A, ALI, 90, 80, iso(-10));
    expect(res.status).toBe(204);
    expect(await rawKeys()).toEqual(before);
  });

  it("applies a newer report, and one with an equal collected_at", async () => {
    await putUsage(A, MURAT, 50, 28, iso(-10));
    await putUsage(A, ALI, 90, 80, iso(0));
    expect((await usageOf(A))!.reporter).toEqual(ALI);

    const same = (await usageOf(A))!.collected_at;
    await putUsage(A, MURAT, 11, 12, same);
    const after = await usageOf(A);
    expect(after!.reporter).toEqual(MURAT);
    expect(after!.session.used).toBe(11);
  });

  it("accepts an old snapshot for an account that has no report yet", async () => {
    await putClaim(A, ALI);
    await putUsage(A, MURAT, 50, 28, iso(-600));
    expect((await usageOf(A))!.session.used).toBe(50);
  });

  it("replaces an invalid stored report instead of comparing against it", async () => {
    await seed(usageKey(A), { collected_at: iso(60), session: "garbage" });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await putUsage(A, MURAT, 12, 34, iso(-30));
    } finally {
      warn.mockRestore();
    }
    expect((await usageOf(A))!.session.used).toBe(12);
  });
});

describe("clock skew", () => {
  beforeEach(async () => {
    await shared(A);
  });

  it("clamps a future collected_at to the Worker's clock before storing it", async () => {
    const before = Date.now();
    await putUsage(A, ALI, 90, 90, iso(600)); // a reporter clock 10 h fast
    const stored = Date.parse((await usageOf(A))!.collected_at);
    expect(stored).toBeGreaterThanOrEqual(before - 1000);
    expect(stored).toBeLessThanOrEqual(Date.now());

    const account = await getJson(A, "murat");
    expect(Date.parse(account.collected_at!)).toBe(stored);
  });

  it("does not let a fast clock's report block later real reports", async () => {
    await putUsage(A, ALI, 90, 90, iso(600));
    await putUsage(A, MURAT, 20, 20, iso(0));
    expect((await usageOf(A))!.reporter).toEqual(MURAT);
    expect((await usageOf(A))!.session.used).toBe(20);
  });

  it("still orders against the clamped stamp: an older report is ignored", async () => {
    await putUsage(A, ALI, 90, 90, iso(600));
    const before = await rawKeys();
    await putUsage(A, MURAT, 20, 20, iso(-5));
    expect(await rawKeys()).toEqual(before);
  });

  it("clamps a stored future stamp too when comparing", async () => {
    // As a record written before clamping existed (or by hand) would be.
    await seed(usageKey(A), JSON.parse(usageBody(ALI, 90, 90, iso(600))));
    await putUsage(A, MURAT, 20, 20, iso(0));
    expect((await usageOf(A))!.reporter).toEqual(MURAT);
  });
});

describe("usage refreshes the reporter's claim", () => {
  const old = iso(-90);

  beforeEach(async () => {
    await shared(A);
    for (const who of [MURAT, MURAT_LAPTOP, ALI]) await seed(claimKey(A, who), { at: old });
  });

  it("re-stamps only the holder matching reporter dev+machine", async () => {
    const before = Date.now();
    await putUsage(A, MURAT);

    const after = await claimsOf(A);
    expect(holders(after)).toEqual(holders([MURAT, MURAT_LAPTOP, ALI]));
    const at = (who: Who) =>
      after.find((c) => c.dev === who.dev && c.machine_id === who.machine_id)!.at;
    expect(Date.parse(at(MURAT))).toBeGreaterThanOrEqual(before - 1000);
    expect(at(MURAT_LAPTOP)).toBe(old);
    expect(at(ALI)).toBe(old);

    // The re-stamp also renews the key's expiration.
    const key = (await rawKeys()).find((k) => k.name === claimKey(A, MURAT))!;
    expect(key.expiration).toBeGreaterThanOrEqual(Math.floor(before / 1000) + 720 * 60 - 5);
  });

  it("does not re-stamp a claim younger than an hour (KV write budget)", async () => {
    const recent = iso(-30);
    await seed(claimKey(A, MURAT), { at: recent });
    await putUsage(A, MURAT);
    const at = (await claimsOf(A)).find((c) => c.dev === MURAT.dev && c.machine_id === MURAT.machine_id)!.at;
    expect(at).toBe(recent);
  });

  it("never creates a holder for a reporter without a claim", async () => {
    await putUsage(A, CAN);
    expect(holders(await claimsOf(A))).toEqual(holders([MURAT, MURAT_LAPTOP, ALI]));
  });

  it("does not refresh anything when the report is stale", async () => {
    await putUsage(A, ALI, 10, 10, iso(0));
    const before = await rawKeys();
    await putUsage(A, MURAT, 10, 10, iso(-5));
    expect(await rawKeys()).toEqual(before);
  });
});

describe("claims", () => {
  beforeEach(async () => {
    await shared(A, B);
  });

  it("keeps one holder per dev+machine, from different devs and machines", async () => {
    await putClaim(A, MURAT);
    await putClaim(A, MURAT_LAPTOP);
    await putClaim(A, ALI);
    await putClaim(A, MURAT); // refresh, not a duplicate

    expect(holders(await claimsOf(A))).toEqual(holders([MURAT, MURAT_LAPTOP, ALI]));
    expect(await keyNames("claim:")).toEqual(
      [claimKey(A, ALI), claimKey(A, MURAT_LAPTOP), claimKey(A, MURAT)].sort(),
    );
  });

  it("writes one key per holder that expires after CLAIM_TTL_MIN", async () => {
    const before = Date.now();
    await putClaim(A, ALI);
    const [key] = await rawKeys("claim:");
    expect(key!.name).toBe(claimKey(A, ALI));
    expect(key!.expiration).toBeGreaterThanOrEqual(Math.floor(before / 1000) + 720 * 60 - 5);
    expect(key!.expiration).toBeLessThanOrEqual(Math.ceil(Date.now() / 1000) + 720 * 60 + 5);
  });

  it("deleting one holder keeps the others", async () => {
    await putClaim(A, MURAT);
    await putClaim(A, ALI);
    await putClaim(A, MURAT_LAPTOP);

    const res = await deleteClaim(A, MURAT);
    expect(res.status).toBe(204);
    expect(holders(await claimsOf(A))).toEqual(holders([ALI, MURAT_LAPTOP]));

    const theirs = await getJson(A, "ali");
    expect(theirs.state).toBe("claimed");
    expect(theirs.busy_by).toEqual(["murat"]);

    expect((await deleteClaim(A, MURAT_LAPTOP)).status).toBe(204);
    expect((await getJson(A, "ali")).state).toBe("free");
    expect((await getJson(A, "murat")).busy_by).toEqual(["ali"]);
  });

  it("returns 204 and leaves every other key alone when the holder is absent", async () => {
    await putClaim(A, ALI);
    await putUsage(A, ALI);
    const before = await rawKeys();

    // Same dev, other machine: not a match.
    const res = await deleteClaim(A, { dev: "ali", machine_id: "000000000000" });
    expect(res.status).toBe(204);
    expect(await rawKeys()).toEqual(before);
  });

  it("deletes a holder's claim even after the email was unshared", async () => {
    await putClaim(A, ALI);
    // As an orphan left by a claim PUT that raced the unshare would be.
    await unshare(A);
    await seed(claimKey(A, ALI), { at: iso(0) });
    expect((await deleteClaim(A, ALI)).status).toBe(204);
    expect(await keyNames()).toEqual([shareKey(B)]);
  });

  it("requires both dev and machine_id to delete a claim", async () => {
    await putClaim(A, ALI);
    for (const [query, fieldName] of [
      ["", "dev"],
      ["?dev=ali", "machine_id"],
      [`?machine_id=${ALI.machine_id}`, "dev"],
      [`?dev=ALI&machine_id=${ALI.machine_id}`, "dev"],
      ["?dev=ali&machine_id=xyz", "machine_id"],
    ] as const) {
      const res = await call(`/accounts/${A}/claim${query}`, { method: "DELETE" });
      expect(res.status).toBe(400);
      expect(await res.json()).toMatchObject({ field: fieldName });
    }
    expect(await claimsOf(A)).toHaveLength(1);
  });

  it("stamps claims[].at server-side with whole seconds", async () => {
    const before = Date.now();
    await putClaim(A, MURAT);
    const at = (await claimsOf(A))[0]!.at;
    expect(at).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
    expect(Date.parse(at)).toBeGreaterThanOrEqual(before - 1000);
    expect(Date.parse(at)).toBeLessThanOrEqual(Date.now() + 1000);
  });

  it("rejects a claim body missing dev or machine_id", async () => {
    const res = await call(`/accounts/${A}/claim`, {
      method: "PUT",
      body: JSON.stringify({ dev: "murat" }),
    });
    expect(res.status).toBe(400);
    expect(await res.json()).toMatchObject({ field: "machine_id" });
  });

  it("ranks a claimed-only account in the unknown band", async () => {
    await putUsage(A, MURAT, 99, 99);
    await putClaim(B, MURAT);

    expect(await usageOf(B)).toBeNull();
    expect(await claimsOf(B)).toHaveLength(1);

    const res = await listJson("&dev=murat");
    expect(res.accounts.map((a) => a.email)).toEqual([A, B]);
    expect(byEmail(res, B).state).toBe("free");
  });
});

describe("claim TTL", () => {
  beforeEach(async () => {
    await shared(A);
  });

  it("ignores holders older than CLAIM_TTL_MIN (720) whose key has not expired yet", async () => {
    expect(env.CLAIM_TTL_MIN).toBe("720");
    await seed(claimKey(A, ALI), { at: iso(-721) });
    await seed(claimKey(A, CAN), { at: iso(-719) });

    const account = await getJson(A, "murat");
    expect(account.state).toBe("claimed");
    expect(account.busy_by).toEqual(["can"]);
    expect(holders(account.claims)).toEqual([`can/${CAN.machine_id}`]);
    expect(holders(byEmail(await listJson("&dev=murat"), A).claims)).toEqual([`can/${CAN.machine_id}`]);
  });

  it("frees an account whose only holder expired", async () => {
    await seed(claimKey(A, ALI), { at: iso(-800) });
    const account = await getJson(A, "murat");
    expect(account.state).toBe("free");
    expect(account.busy_by).toEqual([]);
    expect(account.claims).toEqual([]);
  });
});

describe("kv hygiene", () => {
  it("skips foreign keys and malformed key names, warning only about its own prefixes", async () => {
    await shared(A);
    const share = await shareOf(A);
    // Written straight to the binding, as a stray or hand-made key would be.
    await env.USAGE.put("not-an-email", JSON.stringify({ email: "not-an-email" }));
    await env.USAGE.put("_schema_version", "3");
    await env.USAGE.put(B, JSON.stringify({ email: B, claims: [] }));
    const malformed = [
      "share:not-an-email",
      "share:Claude3@SixtyNine.Agency",
      "usage:not-an-email",
      `claim:${A}:ali`,
      `claim:${A}:ALI:${ALI.machine_id}`,
      `claim:${A}:ali:AABBCCDDEEFF`,
      `claim:${A}:ali:${ALI.machine_id}:extra`,
      `exhausted:${A}:ali`,
      `exhausted:${A}:ali:AABBCCDDEEFF`,
    ];
    for (const key of malformed) await seed(key, key.startsWith("share:") ? share : { at: iso(0) });

    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const res = await listJson();
      expect(res.accounts.map((a) => a.email)).toEqual([A]);
      expect(byEmail(res, A).claims).toEqual([]);
      expect(warn).toHaveBeenCalledTimes(malformed.length);
      for (const key of malformed) expect(warn.mock.calls.flat().join("\n")).toContain(key);

      const text = await (await call("/accounts")).text();
      expect(text).not.toContain("not-an-email");
      expect(text).not.toContain("_schema_version");
      expect(text).not.toContain(B);
    } finally {
      warn.mockRestore();
    }
  });
});

describe("listing perspectives", () => {
  beforeEach(async () => {
    await shared(A, B);
    await putUsage(A, MURAT, 10, 10);
    await putClaim(A, MURAT);
    await putUsage(B, ALI, 90, 90);
  });

  it("hides a dev's own claim and activity from them", async () => {
    const mine = await listJson("&dev=murat");
    expect(byEmail(mine, A).state).toBe("free");
    expect(byEmail(mine, A).busy_by).toEqual([]);
    expect(byEmail(mine, A).claims).toMatchObject([{ dev: "murat" }]);
  });

  it("shows the same account as busy to another dev", async () => {
    const theirs = await listJson("&dev=ali");
    // murat reported within ACTIVITY_TTL, so activity wins over the claim.
    expect(byEmail(theirs, A).state).toBe("in_use");
    expect(byEmail(theirs, A).busy_by).toEqual(["murat"]);
    expect(byEmail(theirs, B).state).toBe("free");
    expect(theirs.accounts.map((a) => a.email)).toEqual([B, A]);
  });

  it("treats every active dev as other when no dev is given", async () => {
    const anon = await listJson();
    expect(byEmail(anon, A).busy_by).toEqual(["murat"]);
    expect(byEmail(anon, B).busy_by).toEqual(["ali"]);
  });

  it("reflects the perspective on GET /accounts/:email too", async () => {
    expect((await getJson(A, "murat")).state).toBe("free");
    expect((await getJson(A, "ali")).state).toBe("in_use");
  });
});

describe("busy_by with several holders", () => {
  beforeEach(async () => {
    await shared(A);
    await putClaim(A, MURAT);
    await putClaim(A, ALI);
  });

  it("names the other holder from each dev's perspective", async () => {
    const mine = byEmail(await listJson("&dev=murat"), A);
    expect(mine.state).toBe("claimed");
    expect(mine.busy_by).toEqual(["ali"]);

    const theirs = byEmail(await listJson("&dev=ali"), A);
    expect(theirs.state).toBe("claimed");
    expect(theirs.busy_by).toEqual(["murat"]);

    expect(byEmail(await listJson("&dev=can"), A).busy_by).toEqual(["ali", "murat"]);
    expect(byEmail(await listJson(), A).busy_by).toEqual(["ali", "murat"]);
  });

  it("renders every other holder in the text STATE", async () => {
    const text = await (await call("/accounts?dev=can")).text();
    expect(text).toContain("claimed by ali, murat");
    const mine = await (await call("/accounts?dev=murat")).text();
    expect(mine).toContain("claimed by ali");
    expect(mine).not.toContain("claimed by ali,");
  });

  it("merges an active reporter with the holders and shows in use", async () => {
    await putUsage(A, CAN);
    const account = await getJson(A, "murat");
    expect(account.state).toBe("in_use");
    expect(account.busy_by).toEqual(["ali", "can"]);

    const text = await (await call("/accounts?dev=murat")).text();
    expect(text).toMatch(/in use by ali, can \(\d+m\)/);
  });
});

describe("response formats", () => {
  it("returns the documented JSON envelope with UTC as the default tz", async () => {
    expect(env.DEFAULT_TZ).toBe("UTC");
    await shared(A);
    await putUsage(A);

    const res = await listJson("&dev=murat");
    expect(res.tz).toBe("UTC");
    expect(res.generated_at).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
    expect(res.accounts[0]!.rank).toBe(1);
    expect(Object.keys(res).sort()).toEqual(["accounts", "generated_at", "tz"]);
    expect(Object.keys(res.accounts[0]!).sort()).toEqual([
      "added_at",
      "added_by",
      "busy_by",
      "claims",
      "collected_at",
      "email",
      "exhausted",
      "exhausted_until",
      "exhausted_window",
      "nickname",
      "rank",
      "reporter",
      "session",
      "state",
      "week",
    ]);

    expect((await listJson("&tz=America/New_York")).tz).toBe("America/New_York");
  });

  it("serializes exhausted as a boolean and exhausted_until as null when usable", async () => {
    await shared(A);
    await putUsage(A, MURAT, 99, 99);
    const raw = await (await call("/accounts?format=json&dev=murat")).text();
    expect(raw).toContain('"exhausted":false');
    expect(raw).toContain('"exhausted_until":null');
    const one = await (await call(`/accounts/${A}?dev=murat`)).text();
    expect(one).toContain('"exhausted":false');
    expect(one).toContain('"exhausted_until":null');
  });

  it("sinks an exhausted account below a usable one, end to end", async () => {
    await shared(A, B);
    await putUsage(A, ALI, 0, 100);
    await putUsage(B, MURAT, 4, 13);

    const res = await listJson("&dev=murat");
    expect(res.accounts.map((a) => a.email)).toEqual([B, A]);
    const exhausted = byEmail(res, A);
    expect(exhausted.exhausted).toBe(true);
    expect(exhausted.exhausted_until).toBe(exhausted.week!.resets_at);
    expect(exhausted.state).toBe("exhausted");
    // ali reported just now: still listed, though exhaustion owns `state`.
    expect(exhausted.busy_by).toEqual(["ali"]);
    expect(byEmail(res, B)).toMatchObject({ exhausted: false, exhausted_until: null, state: "free" });

    const one = await getJson(A, "murat");
    expect(one.exhausted).toBe(true);
    expect(one.exhausted_until).toBe(exhausted.week!.resets_at);

    const lines = (await (await call("/accounts?dev=murat")).text()).split("\n");
    expect(lines[1]).toContain(B);
    expect(lines[1]).toMatch(/\sfree\s/);
    expect(lines[2]).toContain(A);
    expect(lines[2]).toMatch(/\sexhausted \(week resets [^)]+\)\s/);
    expect(lines[2]).not.toContain("in use by");
  });

  it("defaults to an aligned text table in UTC", async () => {
    await shared(A);
    await putUsage(A, MURAT, 50, 28);
    const res = await call("/accounts?dev=murat");
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    const text = await res.text();
    expect(text.split("\n")[0]).toMatch(/^#\s+NICK\s+ACCOUNT\s+SESSION\s+WEEK\s+STATE\s+UPDATED$/);
    expect(text).toContain(A);
    expect(text).toContain("50% →");
    expect(text.trimEnd().endsWith("(UTC)")).toBe(true);
  });

  it("renders reset-passed windows as reset with 0%", async () => {
    await shared(A);
    await call(`/accounts/${A}/usage`, {
      method: "PUT",
      body: JSON.stringify({
        session: { used: 88, resets_at: iso(-5) },
        week: { used: 44, resets_at: iso(4000) },
        collected_at: iso(0),
        reporter: MURAT,
      }),
    });

    const json = await listJson("&dev=murat");
    expect(byEmail(json, A).session).toMatchObject({ used: 88, effective: 0, reset_passed: true });

    const text = await (await call("/accounts?dev=murat")).text();
    expect(text).toContain("0% → reset");
    expect(text).not.toContain("88%");
  });

  it("says so when nothing is shared", async () => {
    expect(await (await call("/accounts")).text()).toBe("no accounts reported yet\n");
    expect((await listJson()).accounts).toEqual([]);
  });
});
