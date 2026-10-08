import { env } from "cloudflare:test";
import { afterEach, beforeEach, describe, expect, it, vi, type MockInstance } from "vitest";
import {
  checkKvLimits,
  claimTtlSec,
  MAX_KEY_BYTES,
  MAX_METADATA_BYTES,
  parseKey,
  Store,
} from "../src/store";
import type { StoredAccount, UsageRecord } from "../src/types";
import {
  A,
  ALI,
  B,
  byEmail,
  C,
  call,
  CAN,
  claimKey,
  ControlledKV,
  exhaustedKey,
  getJson,
  holders,
  iso,
  listJson,
  MURAT,
  MURAT_LAPTOP,
  NOT_SHARED,
  putClaim,
  putExhausted,
  putUsage,
  seed,
  sendClaim,
  sendUsage,
  share,
  shared,
  shareKey,
  usageBody,
  usageKey,
  usageOf,
  viaKv,
} from "./helpers";

const D = "claude4@sixtynine.agency";
const E = "claude5@sixtynine.agency";

function validUsage(): UsageRecord {
  return JSON.parse(usageBody(MURAT, 40, 30, iso(-1))) as UsageRecord;
}

function byEmailSorted(accounts: StoredAccount[]): StoredAccount[] {
  return [...accounts].sort((x, y) => (x.email < y.email ? -1 : 1));
}

describe("key layout", () => {
  it("parses the four key kinds", () => {
    expect(parseKey(`share:${A}`)).toEqual({ kind: "share", email: A });
    expect(parseKey(`usage:${A}`)).toEqual({ kind: "usage", email: A });
    expect(parseKey(claimKey(A, ALI))).toEqual({ kind: "claim", email: A, holder: ALI });
    expect(parseKey(exhaustedKey(A, ALI))).toEqual({ kind: "exhausted", email: A, holder: ALI });
  });

  it("rejects foreign and non-canonical names", () => {
    for (const bad of [
      A,
      "share:",
      "share:Claude1@sixtynine.agency",
      "usage:nope",
      "claim:",
      `claim:${A}`,
      `claim:${A}:ali`,
      `claim:${A}:ali:${ALI.machine_id}:x`,
      `claim:${A}:Ali:${ALI.machine_id}`,
      `claim:${A}:ali:AABBCCDDEEFF`,
      `claim:Claude1@sixtynine.agency:ali:${ALI.machine_id}`,
      "exhausted:",
      `exhausted:${A}`,
      `exhausted:${A}:ali`,
      `exhausted:${A}:ali:${ALI.machine_id}:session`,
      `exhausted:${A}:Ali:${ALI.machine_id}`,
      `exhausted:${A}:ali:AABBCCDDEEFF`,
      `exhausted:Claude1@sixtynine.agency:ali:${ALI.machine_id}`,
      "SHARE:a@x.io",
    ])
      expect(parseKey(bad), bad).toBeNull();
  });

  it("uses the claim TTL as expiration, never below KV's 60 s minimum", () => {
    expect(claimTtlSec(720)).toBe(43_200);
    expect(claimTtlSec(1)).toBe(60);
    expect(claimTtlSec(0.5)).toBe(60);
    expect(claimTtlSec(1.25)).toBe(75);
  });
});

describe("KV size limits", () => {
  it("keeps the largest valid key and metadata well under the limits", () => {
    const email = `${"a".repeat(240)}@example.com`;
    expect(email.length).toBeLessThanOrEqual(254);
    const holder = { dev: "d".repeat(32), machine_id: "f".repeat(12) };
    const stamp = "2026-09-16T14:30:00.123Z";
    const usage: UsageRecord = {
      session: { used: 99.9, resets_at: stamp },
      week: { used: 99.9, resets_at: stamp },
      collected_at: stamp,
      reporter: holder,
    };
    const worst = [
      [`share:${email}`, { added_by: holder, added_at: stamp, nickname: "n".repeat(32) }],
      [`usage:${email}`, usage],
      [`claim:${email}:${holder.dev}:${holder.machine_id}`, { at: stamp }],
      [`exhausted:${email}:${holder.dev}:${holder.machine_id}`, { window: "session", resets_at: stamp, at: stamp }],
    ] as const;
    for (const [key, meta] of worst) {
      expect(() => checkKvLimits(key, meta)).not.toThrow();
      expect(new TextEncoder().encode(JSON.stringify(meta)).byteLength).toBeLessThan(MAX_METADATA_BYTES / 2);
      expect(new TextEncoder().encode(key).byteLength).toBeLessThan(MAX_KEY_BYTES);
    }
  });

  it("refuses oversized metadata or keys before writing", () => {
    expect(() => checkKvLimits("k", { pad: "x".repeat(MAX_METADATA_BYTES) })).toThrow(/metadata.*limit is 1024/);
    expect(() => checkKvLimits("k".repeat(MAX_KEY_BYTES + 1), {})).toThrow(/key.*limit is 512/);
  });
});

describe("GET /accounts reads", () => {
  beforeEach(async () => {
    await shared(A, B);
    await putUsage(A, MURAT);
    await putClaim(A, ALI);
    await putClaim(B, CAN);
  });

  it("does one list and no per-key reads", async () => {
    const kv = new ControlledKV(env.USAGE);
    const res = await call("/accounts?format=json&dev=murat", {}, viaKv(kv));
    expect(res.status).toBe(200);
    expect(kv.calls).toEqual([{ op: "list", key: "" }]);

    const body = (await res.json()) as { accounts: { email: string }[] };
    expect(body.accounts.map((a) => a.email).sort()).toEqual([A, B]);
  });

  it("uses targeted reads for GET /accounts/:email, and no list when not shared", async () => {
    const kv = new ControlledKV(env.USAGE);
    expect((await call(`/accounts/${A}`, {}, viaKv(kv))).status).toBe(200);
    expect(kv.calls.map((c) => `${c.op} ${c.key}`).sort()).toEqual([
      `getWithMetadata ${shareKey(A)}`,
      `getWithMetadata ${usageKey(A)}`,
      `list claim:${A}:`,
      `list exhausted:${A}:`,
    ]);

    kv.calls.length = 0;
    expect((await call(`/accounts/${C}`, {}, viaKv(kv))).status).toBe(404);
    expect(kv.calls.map((c) => c.op)).not.toContain("list");
  });

  it("returns the same account from the listing and the single read", async () => {
    const { rank: _rank, ...listed } = byEmail(await listJson("&dev=can"), A);
    expect(await getJson(A, "can")).toEqual(listed);
  });
});

describe("nickname KV cost", () => {
  it("costs a share one read, one list over share: and one write", async () => {
    await shared(B, C);
    const kv = new ControlledKV(env.USAGE);
    expect((await share(A, MURAT, "alpha", viaKv(kv))).status).toBe(201);
    expect(kv.calls).toEqual([
      { op: "getWithMetadata", key: shareKey(A) },
      { op: "list", key: "share:" },
      { op: "put", key: shareKey(A) },
    ]);
  });

  it("costs a share of an already-shared email one read and no list", async () => {
    await shared(A);
    const kv = new ControlledKV(env.USAGE);
    expect((await share(A, ALI, "other", viaKv(kv))).status).toBe(204);
    expect(kv.calls).toEqual([{ op: "getWithMetadata", key: shareKey(A) }]);
  });

  it("stops listing at the first holder of a taken nickname", async () => {
    await shared(A, B, C);
    const kv = new ControlledKV(env.USAGE);
    const res = await share(D, MURAT, "claude1", viaKv(kv));
    expect(res.status).toBe(409);
    expect(await res.json()).toEqual({ error: "nickname is taken", nickname: "claude1", by: A });
    expect(kv.calls.map((c) => c.op)).toEqual(["getWithMetadata", "list"]);
  });

  it("finds a holder past the first page of share: keys", async () => {
    await shared(A, B, C);
    const store = new Store(env.USAGE, { claimTtlSec: 43_200, listPageSize: 1 });
    expect(await store.nicknameHolder("claude3", D)).toBe(C);
    expect(await store.nicknameHolder("claude3", C)).toBeNull();
    expect(await store.nicknameHolder("nobody", D)).toBeNull();
  });
});

describe("list pagination", () => {
  it("assembles accounts whose keys span many pages", async () => {
    await shared(A, B, C);
    for (const email of [A, B, C]) {
      await putUsage(email, MURAT);
      for (const who of [MURAT, MURAT_LAPTOP, ALI, CAN]) await putClaim(email, who);
    }
    await seed(usageKey(D), validUsage()); // orphan, no share key

    const kv = new ControlledKV(env.USAGE);
    const paged = await new Store(kv.binding, { claimTtlSec: 43_200, listPageSize: 2 }).listAccounts();
    const whole = await new Store(env.USAGE, { claimTtlSec: 43_200 }).listAccounts();

    // 3 × (share + usage + 4 claims) + 1 orphan = 19 keys → at least 10 pages.
    expect(kv.calls.filter((c) => c.op === "list").length).toBeGreaterThanOrEqual(10);
    expect(kv.calls.every((c) => c.op === "list")).toBe(true);
    expect(byEmailSorted(paged)).toEqual(byEmailSorted(whole));
    expect(paged.map((a) => a.email).sort()).toEqual([A, B, C]);
    for (const account of paged) {
      expect(holders(account.claims)).toEqual(holders([MURAT, MURAT_LAPTOP, ALI, CAN]));
      expect(account.session?.used).toBe(50);
    }
  });

  it("pages claim keys under one email's prefix", async () => {
    await shared(A, B);
    const many = Array.from({ length: 7 }, (_, i) => ({ dev: `dev${i}`, machine_id: `00000000000${i}` }));
    for (const who of many) await putClaim(A, who);
    await putClaim(B, ALI);

    const kv = new ControlledKV(env.USAGE);
    const account = await new Store(kv.binding, { claimTtlSec: 43_200, listPageSize: 3 }).getAccount(A);
    expect(holders(account!.claims)).toEqual(holders(many));
    expect(kv.calls.filter((c) => c.op === "list").length).toBeGreaterThanOrEqual(3);
  });

  it("assembles refusal records whose keys span pages, for the listing and the single read", async () => {
    await shared(A, B);
    const many = Array.from({ length: 5 }, (_, i) => ({ dev: `dev${i}`, machine_id: `00000000000${i}` }));
    for (const who of many) await putExhausted(A, "week", null, who);
    await putExhausted(B, "session", null, ALI);

    const kv = new ControlledKV(env.USAGE);
    const store = new Store(kv.binding, { claimTtlSec: 43_200, listPageSize: 2 });
    const listed = (await store.listAccounts()).find((a) => a.email === A)!;
    const single = (await store.getAccount(A))!;
    for (const account of [listed, single]) {
      expect(holders(account.exhausted!)).toEqual(holders(many));
      expect(account.exhausted!.every((r) => r.window === "week" && r.resets_at === null)).toBe(true);
    }
    expect(kv.calls.filter((c) => c.op === "list" && c.key === `exhausted:${A}:`).length).toBeGreaterThanOrEqual(3);
  });

  it("follows the cursor past KV's 1000-key page", async () => {
    await shared(A);
    const holdersOf = Array.from({ length: 1005 }, (_, i) => ({
      dev: "bulk",
      machine_id: i.toString(16).padStart(12, "0"),
    }));
    await Promise.all(holdersOf.map((who) => seed(claimKey(A, who), { at: iso(-1) })));

    const kv = new ControlledKV(env.USAGE);
    const accounts = await new Store(kv.binding, { claimTtlSec: 43_200 }).listAccounts();
    expect(kv.calls.filter((c) => c.op === "list").length).toBe(2);
    expect(accounts).toHaveLength(1);
    expect(accounts[0]!.claims).toHaveLength(1005);
  });
});

describe("per-key validation", () => {
  let warn: MockInstance<typeof console.warn>;

  beforeEach(() => {
    warn = vi.spyOn(console, "warn").mockImplementation(() => {});
  });

  afterEach(() => {
    warn.mockRestore();
  });

  it("drops only the invalid claim holder", async () => {
    await shared(A);
    await putClaim(A, CAN);
    await seed(claimKey(A, ALI), { at: "yesterday" });
    await seed(claimKey(A, MURAT_LAPTOP), undefined);

    for (const account of [byEmail(await listJson("&dev=murat"), A), await getJson(A, "murat")]) {
      expect(holders(account.claims)).toEqual(holders([CAN]));
      expect(account.busy_by).toEqual(["can"]);
    }
    expect(warn).toHaveBeenCalled();
  });

  it("shows an account with an invalid usage record without windows", async () => {
    await shared(A);
    await putClaim(A, ALI);
    await seed(usageKey(A), { ...validUsage(), session: { used: -5, resets_at: iso(60) } });

    for (const account of [byEmail(await listJson("&dev=murat"), A), await getJson(A, "murat")]) {
      expect(account.added_by).toEqual(MURAT);
      expect(account).not.toHaveProperty("session");
      expect(account).not.toHaveProperty("week");
      expect(account).not.toHaveProperty("collected_at");
      expect(account).not.toHaveProperty("reporter");
      expect(holders(account.claims)).toEqual(holders([ALI]));
      expect(account.state).toBe("claimed");
    }

    // The next report replaces it.
    await putUsage(A, CAN, 12, 13);
    expect((await getJson(A)).session!.used).toBe(12);
  });

  it("hides only the account whose share record is invalid, until it is shared again", async () => {
    await shared(A);
    await seed(shareKey(B), { added_by: "murat", added_at: iso(0) });
    await seed(usageKey(B), validUsage());
    await seed(claimKey(B, ALI), { at: iso(-1) });
    await seed(shareKey(C), undefined);

    expect((await listJson()).accounts.map((a) => a.email)).toEqual([A]);
    for (const email of [B, C]) {
      const res = await call(`/accounts/${email}`);
      expect(res.status).toBe(404);
      expect(await res.json()).toEqual(NOT_SHARED);
    }

    expect((await share(B, CAN)).status).toBe(201);
    const repaired = byEmail(await listJson(), B);
    expect(repaired.added_by).toEqual(CAN);
    expect(repaired.session!.used).toBe(40);
    expect(holders(repaired.claims)).toEqual(holders([ALI]));
  });
});

describe("orphaned keys", () => {
  it("ignores usage and claim keys that have no share key", async () => {
    await shared(A);
    await seed(usageKey(E), validUsage());
    await seed(claimKey(E, ALI), { at: iso(-1) });

    expect((await listJson()).accounts.map((a) => a.email)).toEqual([A]);
    expect((await call(`/accounts/${E}`)).status).toBe(404);
    expect((await sendUsage(E)).status).toBe(404);
    expect((await sendClaim(E, CAN)).status).toBe(404);
    // Rejected writes add nothing to the orphans.
    expect((await usageOf(E))!.reporter).toEqual(MURAT);
  });
});
