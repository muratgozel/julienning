import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import {
  A,
  ALI,
  ControlledKV,
  exhaustedOf,
  putExhausted,
  viaKv,
  B,
  byEmail,
  C,
  call,
  claimsOf,
  getJson,
  iso,
  keyNames,
  listJson,
  MURAT,
  postProbes,
  probeKey,
  probeOf,
  probeReport,
  putClaim,
  putUsage,
  rawKeys,
  seed,
  sendProbes,
  shared,
  shareKey,
  unshare,
  usageKey,
  type Via,
} from "./helpers";
import worker from "../src/index";

describe("POST /probes", () => {
  it("writes only probe:<email>, with an 8-day expiration, and answers the ranked listing", async () => {
    await shared(A);
    const res = await postProbes([probeReport(A, 30, 40)]);

    expect(await keyNames()).toEqual([probeKey(A), shareKey(A)]);
    const key = (await rawKeys(probeKey(A)))[0]!;
    const ttl = (key.expiration ?? 0) - Date.now() / 1000;
    expect(ttl).toBeGreaterThan(8 * 24 * 3600 - 120);
    expect(ttl).toBeLessThanOrEqual(8 * 24 * 3600 + 1);
    expect(await probeOf(A)).toMatchObject({ session: { used: 30 }, week: { used: 40 }, reporter: MURAT });

    expect(Object.keys(res).sort()).toEqual(["accounts", "generated_at", "not_shared", "probes", "tz"]);
    expect(res.not_shared).toEqual([]);
    const a = byEmail(res, A);
    expect(a.rank).toBe(1);
    expect(a.source).toBe("probe");
    expect(a.session).toMatchObject({ used: 30, effective: 30, reset_passed: false });
    expect(a.week).toMatchObject({ used: 40, effective: 40 });
    expect(a.numbers_at).toBeDefined();
    // collected_at/reporter are the status line report's: none yet.
    expect(a.collected_at).toBeUndefined();
    expect(a.reporter).toBeUndefined();
  });

  it("lists the probe on GET /accounts and GET /accounts/:email too", async () => {
    await shared(A);
    await postProbes([probeReport(A, 12, 34)]);
    expect(byEmail(await listJson("&dev=murat"), A)).toMatchObject({ source: "probe", session: { used: 12 } });
    expect(await getJson(A, "murat")).toMatchObject({ source: "probe", week: { used: 34 } });
  });

  it("stores nothing for an email that is not shared and names it in not_shared", async () => {
    await shared(A);
    const res = await postProbes([probeReport(A), probeReport(B)]);
    expect(res.not_shared).toEqual([B]);
    expect(await keyNames()).toEqual([probeKey(A), shareKey(A)]);
    expect(res.accounts.map((a) => a.email)).toEqual([A]);
  });

  it("lowercases emails", async () => {
    await shared(A);
    await postProbes([probeReport(A.toUpperCase())]);
    expect(await probeOf(A)).not.toBeNull();
  });

  it("ignores a probe older than the stored one, and writes an equal one only once", async () => {
    await shared(A);
    await postProbes([probeReport(A, 50, 50, iso(-1))]);
    const old = await postProbes([probeReport(A, 10, 10, iso(-5))]);
    expect((await probeOf(A))!.session.used).toBe(50);
    expect(byEmail(old, A).session!.used).toBe(50);

    const stamp = iso(0);
    await postProbes([probeReport(A, 60, 60, stamp)]);
    await postProbes([probeReport(A, 70, 70, stamp)]);
    expect((await probeOf(A))!.session.used).toBe(60);
  });

  it("clamps a future collected_at to the Worker's clock", async () => {
    await shared(A);
    await postProbes([probeReport(A, 20, 20, iso(600))]);
    expect(Date.parse((await probeOf(A))!.collected_at)).toBeLessThanOrEqual(Date.now() + 1000);
  });

  it("uses ?dev for the perspective, else the reporter's dev", async () => {
    await shared(A);
    await putClaim(A, MURAT);
    expect(byEmail(await postProbes([probeReport(A)], ALI), A).busy_by).toEqual(["murat"]);
    expect(byEmail(await postProbes([probeReport(A, 1, 1, iso(1))], ALI, "?dev=murat"), A).busy_by).toEqual([]);
  });
});

describe("probe and status line together", () => {
  it("shows the newer record's numbers", async () => {
    await shared(A, B);
    await putUsage(A, ALI, 80, 80, iso(-30));
    await postProbes([probeReport(A, 10, 20, iso(-2))]);
    await postProbes([probeReport(B, 10, 20, iso(-30))]);
    await putUsage(B, ALI, 80, 80, iso(-2));

    const res = await listJson("&dev=murat");
    expect(byEmail(res, A)).toMatchObject({ source: "probe", session: { used: 10 }, numbers_at: iso(-2), collected_at: iso(-30), reporter: ALI });
    expect(byEmail(res, B)).toMatchObject({ source: "statusline", session: { used: 80 }, numbers_at: iso(-2), collected_at: iso(-2), reporter: ALI });
  });

  it("keeps a teammate's in_use from the status line when a newer probe supplies the numbers", async () => {
    await shared(A);
    await putUsage(A, ALI, 80, 80, iso(-3));
    const res = await postProbes([probeReport(A, 85, 81)]);
    const a = byEmail(res, A);
    expect(a.source).toBe("probe");
    expect(a.session!.used).toBe(85);
    expect(a.state).toBe("in_use");
    expect(a.busy_by).toEqual(["ali"]);
    expect(a).toMatchObject({ collected_at: iso(-3), reporter: ALI });

    const text = await (await call("/accounts?dev=murat")).text();
    expect(text).toMatch(/in use by ali \(3m\)/);
  });

  it("never counts a probe as activity, and never refreshes the prober's claim", async () => {
    await shared(A);
    await seed(`claim:${A}:${ALI.dev}:${ALI.machine_id}`, { at: iso(-120) });
    await postProbes([probeReport(A)], ALI);
    const a = byEmail(await listJson("&dev=murat"), A);
    expect(a.state).toBe("claimed");
    expect((await claimsOf(A))[0]!.at).toBe(iso(-120));

    await unshare(A);
    await shared(A);
    await postProbes([probeReport(A)], ALI);
    const fresh = byEmail(await listJson("&dev=murat"), A);
    expect(fresh.state).toBe("free");
    expect(fresh.busy_by).toEqual([]);
  });

  it("marks an account exhausted when a probe reads a window at 100%", async () => {
    await shared(A, B);
    const reset = iso(90);
    await putUsage(A, ALI, 10, 10, iso(-60));
    await postProbes([probeReport(A, 100, 40, iso(0), { session: reset }), probeReport(B, 50, 50)]);
    const res = await listJson("&dev=murat");
    expect(res.accounts.map((a) => a.email)).toEqual([B, A]);
    expect(byEmail(res, A)).toMatchObject({ exhausted: true, exhausted_until: reset, exhausted_window: "session" });
  });
});

describe("windows that are not running", () => {
  it("accepts a null reset with 0% and lists it as known, effective 0", async () => {
    await shared(A, B);
    await postProbes([probeReport(A, 0, 0, iso(0), { session: null, week: null })]);
    const res = await listJson("&dev=murat");
    expect(res.accounts.map((a) => a.email)).toEqual([A, B]);
    expect(byEmail(res, A).session).toEqual({ used: 0, effective: 0, resets_at: null, reset_passed: false });

    const text = await (await call("/accounts?dev=murat")).text();
    expect(text.split("\n")[1]).toMatch(new RegExp(`^1\\s+claude1\\s+${A}\\s+0%\\s+0%\\s+free\\s+just now$`));
  });

  it("rejects a null reset with usage", async () => {
    await shared(A);
    const res = await sendProbes([probeReport(A, 5, 0, iso(0), { session: null })]);
    expect(res.status).toBe(400);
    expect(await res.json()).toMatchObject({ field: "reports[0].session.resets_at" });
    expect(await probeOf(A)).toBeNull();
  });
});

describe("listing lag", () => {
  /** A KV whose list() does not see probe keys yet, as a lagging location would. */
  function lagging(): Via {
    const inner = env.USAGE;
    const kv = {
      get: inner.get.bind(inner),
      getWithMetadata: inner.getWithMetadata.bind(inner),
      put: inner.put.bind(inner),
      delete: inner.delete.bind(inner),
      async list<M>(options?: KVNamespaceListOptions): Promise<KVNamespaceListResult<M, string>> {
        const page = await inner.list<M>(options);
        return { ...page, keys: page.keys.filter((k) => !k.name.startsWith("probe:")) };
      },
    } as unknown as KVNamespace;
    return (req) => worker.fetch(req, { ...env, USAGE: kv });
  }

  it("merges the batch into the answer even when the listing does not show the writes yet", async () => {
    await shared(A, B);
    await putUsage(A, ALI, 90, 90, iso(-60));
    await putUsage(B, ALI, 20, 20, iso(-60));
    const res = await sendProbes([probeReport(A, 5, 5)], MURAT, "", lagging());
    expect(res.status).toBe(200);
    const body = (await res.json()) as { accounts: { email: string; source?: string }[] };
    expect(body.accounts.map((a) => a.email)).toEqual([A, B]);
    expect(body.accounts[0]!.source).toBe("probe");
  });
});

describe("validation and limits", () => {
  it("rejects bad bodies with 400 {error, field} before anything is read", async () => {
    await shared(A);
    const cases: [unknown, string][] = [
      [{ reporter: MURAT, reports: [] }, "reports"],
      [{ reporter: MURAT }, "reports"],
      [{ reports: [probeReport(A)] }, "reporter"],
      [{ reporter: MURAT, reports: [probeReport(A), probeReport(A.toUpperCase())] }, "reports[1].email"],
      [{ reporter: MURAT, reports: [{ ...probeReport(A), email: "nope" }] }, "reports[0].email"],
      [{ reporter: MURAT, reports: [{ ...probeReport(A), collected_at: "yesterday" }] }, "reports[0].collected_at"],
      [{ reporter: MURAT, reports: [{ ...probeReport(A), week: { used: -1, resets_at: iso(10) } }] }, "reports[0].week.used"],
      [{ reporter: MURAT, reports: Array.from({ length: 33 }, (_, i) => probeReport(`u${i}@x.io`)) }, "reports"],
    ];
    for (const [body, field] of cases) {
      const res = await call("/probes", { method: "POST", body: JSON.stringify(body) });
      expect(res.status, field).toBe(400);
      expect(((await res.json()) as { field: string }).field).toBe(field);
    }
    expect(await keyNames()).toEqual([shareKey(A)]);
  });

  it("clamps usage above 100 like a status line report", async () => {
    await shared(A);
    await postProbes([probeReport(A, 104, 100)]);
    expect((await probeOf(A))!.session.used).toBe(100);
  });

  it("accepts a body up to 16 KB and rejects a larger one", async () => {
    const big = { reporter: MURAT, reports: [probeReport(A)], pad: "x".repeat(16 * 1024) };
    const res = await call("/probes", { method: "POST", body: JSON.stringify(big) });
    expect(res.status).toBe(400);
    expect(await res.json()).toMatchObject({ field: "body" });
  });

  it("is POST only, behind the Bearer token (no ?token=)", async () => {
    expect((await call("/probes")).status).toBe(405);
    expect((await call("/probes", { method: "POST", auth: false, body: "{}" })).status).toBe(401);
    const viaQuery = await call("/probes?token=test-token", {
      method: "POST",
      auth: false,
      body: JSON.stringify({ reporter: MURAT, reports: [probeReport(A)] }),
    });
    expect(viaQuery.status).toBe(401);
  });
});

describe("unshare", () => {
  it("deletes the probe key with the rest", async () => {
    await shared(A, C);
    await putUsage(A);
    await postProbes([probeReport(A), probeReport(C)]);
    expect((await unshare(A)).status).toBe(204);
    expect(await keyNames()).toEqual([probeKey(C), shareKey(C)]);
    expect(await keyNames(usageKey(A))).toEqual([]);
  });

  it("skips a corrupt probe record, keeping the account and its status line numbers", async () => {
    await shared(A);
    await putUsage(A, ALI, 33, 44);
    await seed(probeKey(A), { session: "nope" });
    const a = byEmail(await listJson("&dev=murat"), A);
    expect(a).toMatchObject({ source: "statusline", session: { used: 33 } });
  });
});

describe("the background round's batch", () => {
  it("answers only not_shared with ?listing=0, without a list operation", async () => {
    await shared(A);
    const kv = new ControlledKV(env.USAGE);
    const res = await sendProbes([probeReport(A), probeReport(B)], MURAT, "?listing=0", viaKv(kv));
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ probes: true, not_shared: [B] });
    expect(kv.calls.map((c) => c.op)).not.toContain("list");
    expect(await probeOf(A)).not.toBeNull();
  });
});

describe("refusals and probes", () => {
  it("fills a refusal's unknown reset from a probe newer than the status line", async () => {
    await shared(A);
    const weekReset = iso(2 * 24 * 60);
    await putUsage(A, ALI, 50, 50, iso(-120));
    await postProbes([probeReport(A, 50, 97, iso(-1), { week: weekReset })]);
    await putExhausted(A, "week");
    expect((await exhaustedOf(A, MURAT))!.record.resets_at).toBe(weekReset);
  });
});
