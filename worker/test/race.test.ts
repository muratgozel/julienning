import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import {
  A,
  ALI,
  B,
  call,
  CAN,
  claimKey,
  claimsOf,
  ControlledKV,
  deleteClaim,
  getJson,
  holders,
  iso,
  keyNames,
  listJson,
  MURAT,
  NOT_SHARED,
  putClaim,
  putUsage,
  rename,
  seed,
  sendClaim,
  sendUsage,
  share,
  shared,
  shareKey,
  shareOf,
  unshare,
  usageKey,
  viaKv,
} from "./helpers";

// Each test holds one request at a KV call (after its reads, before its
// write), lets a conflicting request finish, then releases the first. With the
// old single-value-per-email layout every one of these lost or resurrected
// data; with one key per writer they cannot.

async function expectUnshared(email: string): Promise<void> {
  expect((await listJson()).accounts.map((a) => a.email)).not.toContain(email);
  const get = await call(`/accounts/${email}`);
  expect(get.status).toBe(404);
  expect(await get.json()).toEqual(NOT_SHARED);
  expect((await sendUsage(email)).status).toBe(404);
}

describe("unshare racing a write", () => {
  it("is not undone by a usage report that passed its share check first", async () => {
    await shared(A, B);
    const kv = new ControlledKV(env.USAGE);
    const gate = kv.pause("put", usageKey(A));
    const pending = sendUsage(A, ALI, 90, 90, iso(0), viaKv(kv));
    await gate.reached;

    expect((await unshare(A)).status).toBe(204);
    gate.release();
    expect((await pending).status).toBe(204);

    // The late write did land, so the race really happened, but only as an
    // orphan: the share key is gone and nothing lists the account.
    expect(await keyNames()).toEqual([shareKey(B), usageKey(A)]);
    await expectUnshared(A);
    expect((await listJson()).accounts.map((a) => a.email)).toEqual([B]);
  });

  it("is not undone by a claim that passed its share check first", async () => {
    await shared(A);
    const kv = new ControlledKV(env.USAGE);
    const gate = kv.pause("put", claimKey(A, ALI));
    const pending = sendClaim(A, ALI, viaKv(kv));
    await gate.reached;

    expect((await unshare(A)).status).toBe(204);
    gate.release();
    expect((await pending).status).toBe(204);

    expect(await keyNames()).toEqual([claimKey(A, ALI)]);
    await expectUnshared(A);
  });

  it("is not undone by a usage report re-stamping its reporter's claim", async () => {
    await shared(A);
    // Older than the hourly refresh threshold, so the report re-stamps it.
    await seed(claimKey(A, MURAT), { at: iso(-90) });
    const kv = new ControlledKV(env.USAGE);
    const gate = kv.pause("put", claimKey(A, MURAT));
    const pending = sendUsage(A, MURAT, 10, 10, iso(0), viaKv(kv));
    await gate.reached;

    expect((await unshare(A)).status).toBe(204);
    gate.release();
    expect((await pending).status).toBe(204);

    expect(await keyNames()).toContain(claimKey(A, MURAT));
    expect(await keyNames()).not.toContain(shareKey(A));
    await expectUnshared(A);
  });

  it("closes the account to writes as soon as the share key is gone", async () => {
    await shared(A);
    await putUsage(A);
    await putClaim(A, ALI);
    const kv = new ControlledKV(env.USAGE);
    // Held after the share key is deleted, before usage and claims are.
    const gate = kv.pause("list", `claim:${A}:`);
    const held = call(`/accounts/${A}`, { method: "DELETE" }, viaKv(kv));
    await gate.reached;

    expect((await listJson()).accounts).toEqual([]);
    const late = await sendClaim(A, CAN);
    expect(late.status).toBe(404);
    expect(await late.json()).toEqual(NOT_SHARED);
    expect((await sendUsage(A)).status).toBe(404);

    gate.release();
    expect((await held).status).toBe(204);
    expect(await keyNames()).toEqual([]);
  });
});

describe("concurrent claim writes", () => {
  it("keeps every holder when several claim at once", async () => {
    await shared(A);
    const kv = new ControlledKV(env.USAGE);
    const who = [MURAT, ALI, CAN];
    const gates = who.map((w) => kv.pause("put", claimKey(A, w)));
    const pending = who.map((w) => sendClaim(A, w, viaKv(kv)));
    // Every request has read before any of them writes.
    await Promise.all(gates.map((g) => g.reached));
    for (const g of gates.reverse()) g.release();

    for (const res of await Promise.all(pending)) expect(res.status).toBe(204);
    expect(holders(await claimsOf(A))).toEqual(holders(who));
    expect(holders((await getJson(A)).claims)).toEqual(holders(who));
  });

  it("keeps a new holder's claim written while another dev's usage report is in flight", async () => {
    await shared(A);
    await putClaim(A, MURAT);
    const kv = new ControlledKV(env.USAGE);
    const gate = kv.pause("put", usageKey(A));
    const usage = sendUsage(A, MURAT, 10, 10, iso(0), viaKv(kv));
    await gate.reached;

    await putClaim(A, ALI);
    gate.release();
    expect((await usage).status).toBe(204);

    const account = await getJson(A, "can");
    expect(holders(account.claims)).toEqual(holders([MURAT, ALI]));
    expect(account.session!.used).toBe(10);
  });
});

describe("releasing one holder", () => {
  it.each([
    ["the release lands first", true],
    ["the other claim lands first", false],
  ])("neither resurrects it nor drops a holder claiming concurrently (%s)", async (_, releaseFirst) => {
    await shared(A);
    await putClaim(A, MURAT);
    await putClaim(A, ALI);
    const kv = new ControlledKV(env.USAGE);
    const del = kv.pause("delete", claimKey(A, MURAT));
    const put = kv.pause("put", claimKey(A, CAN));
    const pendingDelete = deleteClaim(A, MURAT, viaKv(kv));
    const pendingPut = sendClaim(A, CAN, viaKv(kv));
    await Promise.all([del.reached, put.reached]);

    const [first, firstPending, second, secondPending] = releaseFirst
      ? [del, pendingDelete, put, pendingPut]
      : [put, pendingPut, del, pendingDelete];
    first.release();
    expect((await firstPending).status).toBe(204);
    second.release();
    expect((await secondPending).status).toBe(204);

    expect(holders(await claimsOf(A))).toEqual(holders([ALI, CAN]));
  });

  it.each([
    ["the release lands first", true],
    ["the refresh lands first", false],
  ])("is not undone by another holder's usage refresh (%s)", async (_, releaseFirst) => {
    await shared(A);
    await putClaim(A, MURAT);
    // Older than the hourly refresh threshold, so ali's report re-stamps it.
    await seed(claimKey(A, ALI), { at: iso(-90) });
    const aliBefore = (await claimsOf(A)).find((c) => c.dev === "ali")!.at;
    const kv = new ControlledKV(env.USAGE);
    const del = kv.pause("delete", claimKey(A, MURAT));
    const refresh = kv.pause("put", claimKey(A, ALI));
    const pendingDelete = deleteClaim(A, MURAT, viaKv(kv));
    const pendingUsage = sendUsage(A, ALI, 10, 10, iso(0), viaKv(kv));
    await Promise.all([del.reached, refresh.reached]);

    const order = releaseFirst
      ? ([[del, pendingDelete], [refresh, pendingUsage]] as const)
      : ([[refresh, pendingUsage], [del, pendingDelete]] as const);
    for (const [gate, pending] of order) {
      gate.release();
      expect((await pending).status).toBe(204);
    }

    const claims = await claimsOf(A);
    expect(holders(claims)).toEqual(holders([ALI]));
    expect(Date.parse(claims[0]!.at)).toBeGreaterThanOrEqual(Date.parse(aliBefore));
  });
});

// Nickname uniqueness is read-then-write (KV has no transactions or
// conditional puts), so unlike the cases above these races are accepted, not
// prevented. The tests pin the documented outcome (docs/CLOUDFLARE.md).
describe("nickname races (accepted)", () => {
  it("lets two emails take the same nickname when their shares interleave; a rename resolves it", async () => {
    const kv = new ControlledKV(env.USAGE);
    // A has passed its uniqueness check and is about to write.
    const gate = kv.pause("put", shareKey(A));
    const first = share(A, MURAT, "alpha", viaKv(kv));
    await gate.reached;

    // B's check cannot see A's record yet.
    expect((await share(B, ALI, "alpha")).status).toBe(201);
    gate.release();
    expect((await first).status).toBe(201);

    const listed = (await listJson()).accounts.map((a) => [a.email, a.nickname]);
    expect(listed.sort()).toEqual([
      [A, "alpha"],
      [B, "alpha"],
    ]);
    // Either side can be renamed away; renaming back into the clash is refused,
    // naming the first holder in key order.
    expect((await rename(B, "beta")).status).toBe(204);
    const back = await rename(B, "alpha");
    expect(back.status).toBe(409);
    expect(await back.json()).toEqual({ error: "nickname is taken", nickname: "alpha", by: A });
  });

  it("can put the share key back when a rename races an unshare of the same email", async () => {
    await share(A, MURAT, "alpha");
    await putUsage(A);
    await putClaim(A, ALI);
    const kv = new ControlledKV(env.USAGE);
    // The rename has read the share record and checked uniqueness.
    const gate = kv.pause("put", shareKey(A));
    const pending = rename(A, "gamma", viaKv(kv));
    await gate.reached;

    expect((await unshare(A)).status).toBe(204);
    gate.release();
    expect((await pending).status).toBe(204);

    // Shared again with no usage or claims; unsharing again finishes the job.
    expect(await keyNames()).toEqual([shareKey(A)]);
    expect((await shareOf(A)).nickname).toBe("gamma");
    expect((await unshare(A)).status).toBe(204);
    await expectUnshared(A);
  });
});
