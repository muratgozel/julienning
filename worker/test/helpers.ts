import { env, SELF } from "cloudflare:test";
import { expect } from "vitest";
import worker from "../src/index";
import type {
  AccountsResponse,
  Claim,
  ExhaustedRecord,
  RankedAccount,
  ShareRecord,
  UsageRecord,
  UsageWindow,
} from "../src/types";

export const TOKEN = "test-token";
export const BASE = "https://julienning.test";
export const A = "claude1@sixtynine.agency";
export const B = "claude2@sixtynine.agency";
export const C = "claude3@sixtynine.agency";
export const MURAT = { dev: "murat", machine_id: "3fa9c2d1e07b" };
export const MURAT_LAPTOP = { dev: "murat", machine_id: "0123456789ab" };
export const ALI = { dev: "ali", machine_id: "aabbccddeeff" };
export const CAN = { dev: "can", machine_id: "c0ffee000001" };
export const NOT_SHARED = { error: "account is not shared" };

export type Who = { dev: string; machine_id: string };

/** How a request reaches the Worker: the real binding by default, or a wrapped KV. */
export type Via = (req: Request) => Promise<Response>;

export const viaSelf: Via = (req) => SELF.fetch(req);

export function viaKv(kv: ControlledKV): Via {
  return (req) => worker.fetch(req, { ...env, USAGE: kv.binding });
}

export function iso(offsetMin: number): string {
  return new Date(Date.now() + offsetMin * 60_000).toISOString().replace(/\.\d{3}Z$/, "Z");
}

export function call(
  path: string,
  init: RequestInit & { auth?: boolean } = {},
  via: Via = viaSelf,
): Promise<Response> {
  const { auth = true, ...rest } = init;
  const headers = new Headers(rest.headers);
  if (auth) headers.set("authorization", `Bearer ${TOKEN}`);
  if (rest.body !== undefined) headers.set("content-type", "application/json");
  return via(new Request(`${BASE}${path}`, { ...rest, headers }));
}

export function usageBody(dev: Who = MURAT, session = 50, week = 28, collectedAt = iso(0)): string {
  return JSON.stringify({
    session: { used: session, resets_at: iso(120) },
    week: { used: week, resets_at: iso(4000) },
    collected_at: collectedAt,
    reporter: dev,
  });
}

/** The CLI's default nickname: the email's local part, lowercased (`claude1@x.io` -> `claude1`). */
export function defaultNick(email: string): string {
  return email.slice(0, email.indexOf("@")).toLowerCase();
}

/** `nickname: null` leaves the field out, as a client predating nicknames would. */
export function shareBody(by: Who = MURAT, nickname: string | null = null): string {
  return JSON.stringify(nickname === null ? { added_by: by } : { added_by: by, nickname });
}

export function share(
  email: string,
  by: Who = MURAT,
  nickname: string | null = defaultNick(email),
  via: Via = viaSelf,
): Promise<Response> {
  return call(`/accounts/${email}`, { method: "PUT", body: shareBody(by, nickname) }, via);
}

export function rename(email: string, nickname: unknown, via: Via = viaSelf): Promise<Response> {
  return call(
    `/accounts/${email}/nickname`,
    { method: "PUT", body: JSON.stringify({ nickname }) },
    via,
  );
}

/** A share record as written before nicknames existed. */
export function legacyShare(by: Who = MURAT, addedAt = iso(-60)): ShareRecord {
  return { added_by: by, added_at: addedAt };
}

export async function shared(...emails: string[]): Promise<void> {
  for (const email of emails) expect((await share(email)).status).toBe(201);
}

export function unshare(email: string): Promise<Response> {
  return call(`/accounts/${email}`, { method: "DELETE" });
}

export function sendUsage(
  email: string,
  dev: Who = MURAT,
  session = 50,
  week = 28,
  collectedAt = iso(0),
  via: Via = viaSelf,
): Promise<Response> {
  return call(
    `/accounts/${email}/usage`,
    { method: "PUT", body: usageBody(dev, session, week, collectedAt) },
    via,
  );
}

export async function putUsage(
  email: string,
  dev: Who = MURAT,
  session = 50,
  week = 28,
  collectedAt = iso(0),
): Promise<void> {
  expect((await sendUsage(email, dev, session, week, collectedAt)).status).toBe(204);
}

export function sendClaim(email: string, who: Who, via: Via = viaSelf): Promise<Response> {
  return call(`/accounts/${email}/claim`, { method: "PUT", body: JSON.stringify(who) }, via);
}

export async function putClaim(email: string, who: Who = MURAT): Promise<void> {
  expect((await sendClaim(email, who)).status).toBe(204);
}

export function deleteClaim(email: string, who: Who, via: Via = viaSelf): Promise<Response> {
  return call(
    `/accounts/${email}/claim?dev=${who.dev}&machine_id=${who.machine_id}`,
    { method: "DELETE" },
    via,
  );
}

/**
 * The PUT /accounts/:email/exhausted body. `resetsAt` undefined leaves the
 * field out (unknown, as the CLI sends when it could not parse a reset).
 */
export function exhaustedBody(window: string, resetsAt?: string | null, who: Who = MURAT): string {
  return JSON.stringify({
    window,
    ...(resetsAt !== undefined ? { resets_at: resetsAt } : {}),
    reporter: who,
  });
}

export function sendExhausted(
  email: string,
  window: string,
  resetsAt?: string | null,
  who: Who = MURAT,
  via: Via = viaSelf,
): Promise<Response> {
  return call(
    `/accounts/${email}/exhausted`,
    { method: "PUT", body: exhaustedBody(window, resetsAt, who) },
    via,
  );
}

export async function putExhausted(
  email: string,
  window: string,
  resetsAt?: string | null,
  who: Who = MURAT,
): Promise<void> {
  expect((await sendExhausted(email, window, resetsAt, who)).status).toBe(204);
}

/** A usage report with explicit windows, for tests that care about resets. */
export async function putWindows(
  email: string,
  session: UsageWindow,
  week: UsageWindow,
  who: Who = MURAT,
  collectedAt = iso(0),
): Promise<void> {
  const res = await call(`/accounts/${email}/usage`, {
    method: "PUT",
    body: JSON.stringify({ session, week, collected_at: collectedAt, reporter: who }),
  });
  expect(res.status).toBe(204);
}

export async function listJson(query = ""): Promise<AccountsResponse> {
  const res = await call(`/accounts?format=json${query}`);
  expect(res.status).toBe(200);
  return (await res.json()) as AccountsResponse;
}

export async function getJson(email: string, dev?: string): Promise<RankedAccount> {
  const res = await call(`/accounts/${email}${dev ? `?dev=${dev}` : ""}`);
  expect(res.status).toBe(200);
  return (await res.json()) as RankedAccount;
}

export function byEmail(res: AccountsResponse, email: string): RankedAccount {
  const found = res.accounts.find((a) => a.email === email);
  expect(found).toBeDefined();
  return found!;
}

export function holders(claims: readonly Who[]): string[] {
  return claims.map((c) => `${c.dev}/${c.machine_id}`).sort();
}

// Raw KV access. Key names are spelled out here rather than imported from
// src/store.ts so a change to the layout fails these tests instead of passing
// silently.

export const shareKey = (email: string): string => `share:${email}`;
export const usageKey = (email: string): string => `usage:${email}`;
export const claimKey = (email: string, who: Who): string =>
  `claim:${email}:${who.dev}:${who.machine_id}`;
export const exhaustedKey = (email: string, who: Who): string =>
  `exhausted:${email}:${who.dev}:${who.machine_id}`;

/** Every key in the namespace, with its metadata and expiration. */
export async function rawKeys(prefix?: string): Promise<KVNamespaceListKey<unknown>[]> {
  const out: KVNamespaceListKey<unknown>[] = [];
  let cursor: string | undefined;
  for (;;) {
    const page = await env.USAGE.list<unknown>({
      ...(prefix !== undefined ? { prefix } : {}),
      ...(cursor !== undefined ? { cursor } : {}),
    });
    out.push(...page.keys);
    if (page.list_complete) return out;
    cursor = page.cursor;
  }
}

export async function keyNames(prefix?: string): Promise<string[]> {
  return (await rawKeys(prefix)).map((k) => k.name);
}

/** Metadata of `key`, or null when the key does not exist. */
export async function metaOf<T>(key: string): Promise<T | null> {
  const { value, metadata } = await env.USAGE.getWithMetadata<T>(key);
  return value === null ? null : metadata;
}

export async function shareOf(email: string): Promise<ShareRecord> {
  const record = await metaOf<ShareRecord>(shareKey(email));
  expect(record).not.toBeNull();
  return record!;
}

export function usageOf(email: string): Promise<UsageRecord | null> {
  return metaOf<UsageRecord>(usageKey(email));
}

/** Holders stored under `claim:<email>:`, straight from KV (expired `at` included). */
export async function claimsOf(email: string): Promise<Claim[]> {
  return (await rawKeys(`claim:${email}:`)).map((k) => {
    const [, , dev, machine_id] = k.name.split(":");
    return { dev: dev!, machine_id: machine_id!, at: (k.metadata as { at: string }).at };
  });
}

/** The stored refusal record and its absolute expiration (epoch seconds), or null. */
export async function exhaustedOf(
  email: string,
  who: Who,
): Promise<{ record: ExhaustedRecord; expiration: number | undefined } | null> {
  const key = (await rawKeys(exhaustedKey(email, who))).find((k) => k.name === exhaustedKey(email, who));
  if (key === undefined) return null;
  return { record: key.metadata as ExhaustedRecord, expiration: key.expiration };
}

/** Writes a key the way a bug, an old deployment or a hand edit might. */
export async function seed(key: string, metadata: unknown): Promise<void> {
  await env.USAGE.put(key, "1", metadata === undefined ? {} : { metadata });
}

type Op = "get" | "getWithMetadata" | "put" | "delete" | "list";

interface Gate {
  op: Op;
  key: string;
  arrive: () => void;
  released: Promise<void>;
}

/**
 * Wraps the real KV binding so a test can hold one request at a chosen KV call
 * while other requests run to completion, then let it continue. This replays
 * interleavings production can hit, deterministically. Every call is recorded
 * (`list` under its prefix, "" for none).
 */
export class ControlledKV {
  readonly calls: { op: Op; key: string }[] = [];
  private readonly gates: Gate[] = [];

  constructor(private readonly inner: KVNamespace) {}

  get binding(): KVNamespace {
    return this as unknown as KVNamespace;
  }

  /** The next `op` on `key` waits for `release()`; `reached` resolves once it is waiting. */
  pause(op: Op, key: string): { reached: Promise<void>; release: () => void } {
    let arrive!: () => void;
    let release!: () => void;
    const reached = new Promise<void>((r) => (arrive = r));
    const released = new Promise<void>((r) => (release = r));
    this.gates.push({ op, key, arrive, released });
    return { reached, release };
  }

  private async enter(op: Op, key: string): Promise<void> {
    this.calls.push({ op, key });
    const i = this.gates.findIndex((g) => g.op === op && g.key === key);
    if (i === -1) return;
    const [gate] = this.gates.splice(i, 1);
    gate!.arrive();
    await gate!.released;
  }

  async get(key: string): Promise<string | null> {
    await this.enter("get", key);
    return this.inner.get(key);
  }

  async getWithMetadata<M>(key: string): Promise<KVNamespaceGetWithMetadataResult<string, M>> {
    await this.enter("getWithMetadata", key);
    return this.inner.getWithMetadata<M>(key);
  }

  async put(key: string, value: string, options?: KVNamespacePutOptions): Promise<void> {
    await this.enter("put", key);
    return this.inner.put(key, value, options);
  }

  async delete(key: string): Promise<void> {
    await this.enter("delete", key);
    return this.inner.delete(key);
  }

  async list<M>(options?: KVNamespaceListOptions): Promise<KVNamespaceListResult<M, string>> {
    await this.enter("list", options?.prefix ?? "");
    return this.inner.list<M>(options);
  }
}
