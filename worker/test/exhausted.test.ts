import { SELF } from "cloudflare:test";
import { afterEach, beforeEach, describe, expect, it, vi, type MockInstance } from "vitest";
import {
  A,
  ALI,
  B,
  BASE,
  byEmail,
  call,
  CAN,
  claimKey,
  exhaustedBody,
  exhaustedKey,
  exhaustedOf,
  getJson,
  keyNames,
  listJson,
  MURAT,
  MURAT_LAPTOP,
  NOT_SHARED,
  putClaim,
  putExhausted,
  putUsage,
  putWindows,
  seed,
  sendExhausted,
  shared,
  shareKey,
  shareOf,
  TOKEN,
  unshare,
  usageKey,
  usageOf,
} from "./helpers";

/**
 * Like helpers.iso, but pinned to the start of each test: a reset sent in a
 * request and the one expected back are computed separately and must not
 * straddle a second boundary.
 */
let iso: (offsetMin: number) => string;

beforeEach(() => {
  const base = Date.now();
  iso = (offsetMin) => new Date(base + offsetMin * 60_000).toISOString().replace(/\.\d{3}Z$/, "Z");
});

const HOUR_SEC = 60 * 60;
const WEEK_SEC = 7 * 24 * HOUR_SEC;
const WEEK_MIN = WEEK_SEC / 60;

/** Asserts `expiration` is `ttlSec` after the request, which ran between `before` and now. */
function expectExpiresIn(expiration: number | undefined, ttlSec: number, before: number): void {
  expect(expiration).toBeDefined();
  expect(expiration!).toBeGreaterThanOrEqual(Math.floor(before / 1000) + ttlSec - 5);
  expect(expiration!).toBeLessThanOrEqual(Math.ceil(Date.now() / 1000) + ttlSec + 5);
}

function stateLine(text: string, email: string): string {
  const line = text.split("\n").find((l) => l.includes(email));
  expect(line).toBeDefined();
  return line!;
}

async function textListing(dev = "murat"): Promise<string> {
  return (await call(`/accounts?dev=${dev}`)).text();
}

describe("PUT /accounts/:email/exhausted: auth, routing and sharing", () => {
  it("needs the Bearer token; ?token= does not authorize a PUT", async () => {
    await shared(A);
    for (const init of [{ auth: false }, { auth: false, headers: { authorization: "Bearer nope" } }]) {
      const res = await call(`/accounts/${A}/exhausted`, { method: "PUT", body: exhaustedBody("week"), ...init });
      expect(res.status).toBe(401);
      expect(await res.json()).toEqual({ error: "unauthorized" });
    }
    const viaQuery = await SELF.fetch(`${BASE}/accounts/${A}/exhausted?token=${TOKEN}`, {
      method: "PUT",
      body: exhaustedBody("week"),
      headers: { "content-type": "application/json" },
    });
    expect(viaQuery.status).toBe(401);
    expect(await keyNames()).toEqual([shareKey(A)]);
  });

  it("404s with the exact not-shared body and writes nothing", async () => {
    const res = await sendExhausted(A, "week", iso(600));
    expect(res.status).toBe(404);
    expect(await res.json()).toEqual(NOT_SHARED);
    expect(await keyNames()).toEqual([]);
  });

  it("allows only PUT", async () => {
    for (const method of ["GET", "DELETE", "POST"]) {
      const res = await call(`/accounts/${A}/exhausted`, { method });
      expect(res.status).toBe(405);
      expect(res.headers.get("allow")).toBe("PUT");
    }
  });

  it("returns 400 {error, field} for bad bodies, before the allowlist check, writing nothing", async () => {
    const reporter = MURAT;
    const cases: [string | undefined, string][] = [
      [undefined, "body"],
      ["{nope", "body"],
      ["[]", "body"],
      [JSON.stringify({ reporter }), "window"],
      [JSON.stringify({ window: "day", reporter }), "window"],
      [JSON.stringify({ window: "Session", reporter }), "window"],
      [JSON.stringify({ window: "Week", reporter }), "window"],
      [JSON.stringify({ window: "five_hour", reporter }), "window"],
      [JSON.stringify({ window: 5, reporter }), "window"],
      // An older CLI's "session" passes the window check (it reads as "week").
      [JSON.stringify({ window: "session", resets_at: "tomorrow", reporter }), "resets_at"],
      [JSON.stringify({ window: "week", resets_at: "2026-10-13", reporter }), "resets_at"],
      [JSON.stringify({ window: "week", resets_at: "2026-10-13T17:00:00", reporter }), "resets_at"],
      [JSON.stringify({ window: "week", resets_at: 1760374800, reporter }), "resets_at"],
      [JSON.stringify({ window: "week", resets_at: "", reporter }), "resets_at"],
      [JSON.stringify({ window: "week" }), "reporter"],
      [JSON.stringify({ window: "week", reporter: "murat" }), "reporter"],
      [JSON.stringify({ window: "week", reporter: { dev: "Murat", machine_id: MURAT.machine_id } }), "reporter.dev"],
      [JSON.stringify({ window: "week", reporter: { dev: "murat" } }), "reporter.machine_id"],
      [JSON.stringify({ window: "week", reporter, pad: "x".repeat(4096) }), "body"],
    ];
    for (const email of [A, B]) {
      if (email === B) await shared(B);
      for (const [body, field] of cases) {
        const res = await call(`/accounts/${email}/exhausted`, {
          method: "PUT",
          ...(body !== undefined ? { body } : {}),
        });
        expect(res.status, `${email} ${body}`).toBe(400);
        const json = (await res.json()) as { error: string; field: string };
        expect(json.field, body).toBe(field);
        expect(json.error).toBeTruthy();
      }
    }
    expect(await keyNames()).toEqual([shareKey(B)]);
  });

  it("names week as the only window in the error", async () => {
    const res = await call(`/accounts/${A}/exhausted`, {
      method: "PUT",
      body: JSON.stringify({ window: "day", reporter: MURAT }),
    });
    expect(await res.json()).toEqual({ error: "window must be 'week'", field: "window" });
  });

  it("rejects a bad email in the path", async () => {
    const res = await call("/accounts/not-an-email/exhausted", { method: "PUT", body: exhaustedBody("week") });
    expect(res.status).toBe(400);
    expect(await res.json()).toMatchObject({ field: "email" });
  });
});

describe("PUT /accounts/:email/exhausted: storage", () => {
  beforeEach(async () => {
    await shared(A);
  });

  it("writes only the reporter's own key, {window, resets_at, at}, leaving the rest alone", async () => {
    await putUsage(A, ALI);
    await putClaim(A, ALI);
    const [share, usage] = [await shareOf(A), await usageOf(A)];
    const before = Date.now();
    const res = await sendExhausted("Claude1@SixtyNine.Agency", "week", iso(3000), {
      dev: "murat",
      machine_id: MURAT.machine_id.toUpperCase(),
    });
    expect(res.status).toBe(204);
    expect(await res.text()).toBe("");

    expect(await keyNames()).toEqual([claimKey(A, ALI), exhaustedKey(A, MURAT), shareKey(A), usageKey(A)].sort());
    expect(await shareOf(A)).toEqual(share);
    expect(await usageOf(A)).toEqual(usage);
    const { record } = (await exhaustedOf(A, MURAT))!;
    expect(record).toEqual({ window: "week", resets_at: iso(3000), at: record.at });
    expect(record.at).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
    expect(Date.parse(record.at)).toBeGreaterThanOrEqual(before - 1000);
    expect(Date.parse(record.at)).toBeLessThanOrEqual(Date.now() + 1000);
  });

  it("normalizes resets_at to UTC", async () => {
    const reset = new Date(Date.now() + 2 * 60 * 60_000);
    reset.setUTCMilliseconds(0);
    const pad = (n: number) => String(n).padStart(2, "0");
    // The same instant written at +03:00.
    const local = new Date(reset.getTime() + 3 * 60 * 60_000);
    const offset =
      `${local.getUTCFullYear()}-${pad(local.getUTCMonth() + 1)}-${pad(local.getUTCDate())}` +
      `T${pad(local.getUTCHours())}:${pad(local.getUTCMinutes())}:${pad(local.getUTCSeconds())}+03:00`;
    await putExhausted(A, "week", offset);
    expect((await exhaustedOf(A, MURAT))!.record.resets_at).toBe(reset.toISOString().replace(/\.000Z$/, "Z"));
  });

  it("keeps one record per reporter: a second report replaces the first", async () => {
    await putExhausted(A, "week", iso(3000));
    await putExhausted(A, "week", iso(60));
    await putExhausted(A, "week", iso(90), MURAT_LAPTOP);
    await putExhausted(A, "week", null, ALI);
    expect((await exhaustedOf(A, MURAT))!.record).toMatchObject({ window: "week", resets_at: iso(60) });
    expect((await exhaustedOf(A, MURAT_LAPTOP))!.record).toMatchObject({ window: "week", resets_at: iso(90) });
    expect((await exhaustedOf(A, ALI))!.record).toMatchObject({ window: "week", resets_at: null });
    expect(await keyNames("exhausted:")).toHaveLength(3);
  });

  it("stores an older CLI's session refusal as a week refusal", async () => {
    // Claude dropped the five-hour window; the refusal is real all the same.
    const before = Date.now();
    await putExhausted(A, "session", iso(90));
    const stored = (await exhaustedOf(A, MURAT))!;
    expect(stored.record).toEqual({ window: "week", resets_at: iso(90), at: stored.record.at });
    expectExpiresIn(stored.expiration, 90 * 60, before);
    expect(await getJson(A, "can")).toMatchObject({ exhausted: true, exhausted_until: iso(90), exhausted_window: "week" });
  });
});

describe("resets_at resolution and expiration", () => {
  beforeEach(async () => {
    await shared(A);
  });

  it.each([90, 3 * 24 * 60])("keeps a future reset %i minutes out and expires the key then", async (inMin) => {
    const before = Date.now();
    await putExhausted(A, "week", iso(inMin));
    const stored = (await exhaustedOf(A, MURAT))!;
    expect(stored.record.resets_at).toBe(iso(inMin));
    expectExpiresIn(stored.expiration, inMin * 60, before);
  });

  it.each(["week", "session"])("stores an unknown reset (window %s) as null and expires it after a week", async (window) => {
    for (const resetsAt of [undefined, null]) {
      const before = Date.now();
      await putExhausted(A, window, resetsAt);
      const stored = (await exhaustedOf(A, MURAT))!;
      expect(stored.record).toMatchObject({ window: "week", resets_at: null });
      expectExpiresIn(stored.expiration, WEEK_SEC, before);
    }
  });

  it("falls back to the usage record's week reset when the reported one has passed", async () => {
    await putWindows(A, { used: 70, resets_at: iso(5000) }, ALI);
    const want = iso(5000);
    for (const window of ["week", "session"]) {
      for (const reported of [iso(-30), undefined, null]) {
        const before = Date.now();
        await putExhausted(A, window, reported);
        const stored = (await exhaustedOf(A, MURAT))!;
        expect(stored.record, `${window} ${reported}`).toMatchObject({ window: "week", resets_at: want });
        expectExpiresIn(stored.expiration, (Date.parse(want) - before) / 1000, before);
      }
    }
  });

  it("prefers a future reported reset over the usage record's", async () => {
    await putWindows(A, { used: 70, resets_at: iso(5000) });
    await putExhausted(A, "week", iso(100));
    expect((await exhaustedOf(A, MURAT))!.record.resets_at).toBe(iso(100));
  });

  it("stores null when neither the report nor the usage record has a future reset", async () => {
    // The week in the usage record has reset already.
    await putWindows(A, { used: 70, resets_at: iso(-10) });
    const before = Date.now();
    await putExhausted(A, "week", iso(-5));
    const stored = (await exhaustedOf(A, MURAT))!;
    expect(stored.record.resets_at).toBeNull();
    expectExpiresIn(stored.expiration, WEEK_SEC, before);
  });

  it("does not believe a reset further out than a week can last", async () => {
    // A reset 30 days out (a misparsed refusal message): the usage record's
    // week reset, and unknown for an account that has no usage record.
    await putWindows(A, { used: 70, resets_at: iso(5000) });
    await putExhausted(A, "week", iso(30 * 24 * 60), ALI);
    expect((await exhaustedOf(A, ALI))!.record.resets_at).toBe(iso(5000));
    await shared(B);
    await putExhausted(B, "week", iso(30 * 24 * 60), ALI);
    expect((await exhaustedOf(B, ALI))!.record.resets_at).toBeNull();
    // Past a week plus an hour of tolerance: not believed either.
    await putExhausted(A, "week", iso(WEEK_MIN + 90), MURAT);
    expect((await exhaustedOf(A, MURAT))!.record.resets_at).toBe(iso(5000));
    // Within a week plus the tolerance is still believed.
    await putExhausted(A, "week", iso(WEEK_MIN + 30), CAN);
    expect((await exhaustedOf(A, CAN))!.record.resets_at).toBe(iso(WEEK_MIN + 30));
    // An older CLI's session refusal two days out is a believable week reset.
    await putExhausted(A, "session", iso(2 * 24 * 60), MURAT_LAPTOP);
    expect((await exhaustedOf(A, MURAT_LAPTOP))!.record.resets_at).toBe(iso(2 * 24 * 60));
  });

  it("never asks KV for an expiration under 60 s", async () => {
    const soon = new Date(Date.now() + 10_000).toISOString();
    const before = Date.now();
    await putExhausted(A, "week", soon);
    expectExpiresIn((await exhaustedOf(A, MURAT))!.expiration, 60, before);
  });
});

describe("ranking an account refused for a limit", () => {
  beforeEach(async () => {
    await shared(A, B);
  });

  it("marks it exhausted and ranks it below a busier usable account", async () => {
    await putUsage(A, ALI, 10);
    await putUsage(B, CAN, 90);
    // The usage records' week resets in 4000 minutes; the refusal names the same reset, to the minute.
    await putExhausted(A, "week", iso(3990), ALI);

    const res = await listJson("&dev=murat");
    expect(res.accounts.map((a) => a.email)).toEqual([B, A]);
    expect(byEmail(res, A)).toMatchObject({
      rank: 2,
      state: "exhausted",
      exhausted: true,
      exhausted_until: iso(3990),
      exhausted_window: "week",
      busy_by: ["ali"],
    });
    expect(byEmail(res, B)).toMatchObject({ exhausted: false, exhausted_until: null, exhausted_window: null });
    // The usage numbers are reported as they are; only the exhaustion fields change.
    expect(byEmail(res, A).week).toMatchObject({ used: 10, effective: 10 });

    const { rank: _rank, ...listed } = byEmail(res, A);
    expect(await getJson(A, "murat")).toEqual(listed);
  });

  it("is not cleared by a newer usage report under 100%", async () => {
    // After a refusal Claude Code keeps feeding the status line stale numbers.
    await putExhausted(A, "week", iso(4000));
    await putWindows(A, { used: 40, resets_at: iso(4000) });
    await putUsage(A, ALI, 0);
    expect(await getJson(A, "murat")).toMatchObject({ exhausted: true, exhausted_until: iso(4000) });
  });

  it("is not cleared by a usage reset a little later than the reported one", async () => {
    // The refusal text gives the reset to the minute; the status line to the second.
    await putExhausted(A, "week", iso(3990));
    await putWindows(A, { used: 40, resets_at: iso(3990 + 59) });
    expect((await getJson(A, "murat")).exhausted).toBe(true);
  });

  it("ignores a record superseded by a later week", async () => {
    await putExhausted(A, "week", iso(30));
    // A new week has started since the refusal.
    await putWindows(A, { used: 40, resets_at: iso(30 + WEEK_MIN) });
    expect(await getJson(A, "murat")).toMatchObject({
      state: "free",
      exhausted: false,
      exhausted_until: null,
      exhausted_window: null,
    });
  });

  it("ignores a record whose reset has passed, even before KV expires it", async () => {
    await seed(exhaustedKey(A, MURAT), { window: "week", resets_at: iso(-1), at: iso(-600) });
    // An old Worker's session record with no reset counts for a week, like any other.
    await seed(exhaustedKey(A, ALI), { window: "session", resets_at: null, at: iso(-WEEK_MIN - 1) });
    expect(await getJson(A, "can")).toMatchObject({ state: "free", exhausted: false, exhausted_window: null });
  });

  it("reads a session record stored by the previous Worker as a week refusal", async () => {
    await seed(exhaustedKey(A, ALI), { window: "session", resets_at: iso(60), at: iso(-10) });
    for (const account of [byEmail(await listJson(), A), await getJson(A, "can")])
      expect(account).toMatchObject({ exhausted: true, exhausted_until: iso(60), exhausted_window: "week" });
  });

  it("reports an unknown reset as null and sorts it after known ones", async () => {
    await putExhausted(A, "week", null);
    await putExhausted(B, "week", iso(200));
    const res = await listJson("&dev=murat");
    expect(res.accounts.map((a) => a.email)).toEqual([B, A]);
    expect(byEmail(res, A)).toMatchObject({ exhausted: true, exhausted_until: null, exhausted_window: "week" });
    expect(stateLine(await textListing(), A)).toMatch(/\sexhausted \(week\)\s/);
  });

  it("waits on the later of the week at 100% and a record", async () => {
    // The week at 100% resets last: it decides.
    await putWindows(A, { used: 100, resets_at: iso(4000) });
    await putExhausted(A, "week", iso(3990));
    expect(await getJson(A)).toMatchObject({ exhausted_until: iso(4000), exhausted_window: "week" });

    // The refusal names a slightly later reset: the record decides.
    await putWindows(B, { used: 100, resets_at: iso(3000) });
    await putExhausted(B, "week", iso(3030));
    expect(await getJson(B)).toMatchObject({ exhausted_until: iso(3030), exhausted_window: "week" });
    expect(stateLine(await textListing(), B)).toMatch(/\sexhausted \(week resets [^)]+\)\s/);
  });

  it("takes the latest known reset when one record has none", async () => {
    // No usage, so ali's record cannot learn its reset.
    await putExhausted(A, "week", null, ALI);
    await putExhausted(A, "week", iso(100), CAN);
    expect(await getJson(A)).toMatchObject({ exhausted_until: iso(100), exhausted_window: "week" });
  });

  it("names the week when only unknown resets remain, old session records included", async () => {
    await putExhausted(A, "session", null, ALI);
    await putExhausted(A, "week", null, CAN);
    expect(await getJson(A)).toMatchObject({ exhausted_until: null, exhausted_window: "week" });
  });

  it("keeps exhausted_window null and the field present when usable", async () => {
    await putUsage(A, MURAT, 99);
    const raw = await (await call(`/accounts/${A}?dev=murat`)).text();
    expect(raw).toContain('"exhausted_window":null');
  });
});

describe("usage above 100%", () => {
  beforeEach(async () => {
    await shared(A);
  });

  it("is stored as 100 and marks the account exhausted", async () => {
    await putWindows(A, { used: 250, resets_at: iso(4000) });
    const usage = (await usageOf(A))!;
    expect(usage).not.toHaveProperty("session");
    expect(usage.week.used).toBe(100);
    expect(await getJson(A)).toMatchObject({ exhausted: true, exhausted_until: iso(4000), exhausted_window: "week" });
  });

  it("still rejects negative and non-numeric usage", async () => {
    for (const used of [-1, "100", null]) {
      const res = await call(`/accounts/${A}/usage`, {
        method: "PUT",
        body: JSON.stringify({
          week: { used, resets_at: iso(4000) },
          collected_at: iso(0),
          reporter: MURAT,
        }),
      });
      expect(res.status).toBe(400);
      expect(await res.json()).toMatchObject({ field: "week.used" });
    }
  });
});

describe("unshare and stray keys", () => {
  let warn: MockInstance<typeof console.warn>;

  beforeEach(() => {
    warn = vi.spyOn(console, "warn").mockImplementation(() => {});
  });

  afterEach(() => {
    warn.mockRestore();
  });

  it("deletes every exhausted key of the account and no other", async () => {
    await shared(A, B);
    await putExhausted(A, "week", iso(60), MURAT);
    await putExhausted(A, "week", null, ALI);
    await putExhausted(B, "week", null, CAN);
    expect((await unshare(A)).status).toBe(204);
    expect(await keyNames()).toEqual([exhaustedKey(B, CAN), shareKey(B)]);
    const late = await sendExhausted(A, "week");
    expect(late.status).toBe(404);
    expect(await late.json()).toEqual(NOT_SHARED);
  });

  it("starts clean when shared again", async () => {
    await shared(A);
    await putExhausted(A, "week", null);
    await unshare(A);
    await shared(A);
    expect(await getJson(A)).toMatchObject({ exhausted: false, exhausted_window: null });
  });

  it("ignores exhausted keys without a share key", async () => {
    await shared(B);
    await seed(exhaustedKey(A, ALI), { window: "week", resets_at: null, at: iso(0) });
    expect((await listJson()).accounts.map((a) => a.email)).toEqual([B]);
    expect((await call(`/accounts/${A}`)).status).toBe(404);
  });

  it("drops only a corrupt record, warning about it", async () => {
    await shared(A);
    await putExhausted(A, "week", iso(60), CAN);
    const corrupt: [string, unknown][] = [
      [exhaustedKey(A, ALI), { window: "day", resets_at: null, at: iso(0) }],
      [exhaustedKey(A, MURAT), { window: "week", resets_at: "soon", at: iso(0) }],
      [exhaustedKey(A, MURAT_LAPTOP), { window: "week", at: iso(0) }],
      [exhaustedKey(A, { dev: "zoe", machine_id: "000000000001" }), { window: "week", resets_at: null }],
      [exhaustedKey(A, { dev: "eve", machine_id: "000000000002" }), undefined],
    ];
    for (const [key, meta] of corrupt) await seed(key, meta);

    for (const account of [byEmail(await listJson(), A), await getJson(A)])
      expect(account).toMatchObject({ exhausted: true, exhausted_until: iso(60), exhausted_window: "week" });
    const warned = warn.mock.calls.flat().join("\n");
    for (const [key] of corrupt) expect(warned).toContain(key);
  });
});
