import type { Claim, Identity, ShareRecord, StoredAccount, UsageRecord } from "./types";
import {
  canonicalEmail,
  canonicalHolder,
  parseClaimRecord,
  parseShareRecord,
  parseUsageRecord,
  storedNicknameDropped,
} from "./validate";

/**
 * KV layout (docs/SPEC.md "Worker"). Every writer owns its own key, so no
 * request ever read-modify-writes a value another request also writes, with
 * one exception: a nickname rename rewrites `share:<email>` (its races are
 * described on the rename handler in src/index.ts).
 *
 *   share:<email>                     share / rename / unshare {added_by, added_at, nickname}
 *   usage:<email>                     PUT usage                {session, week, collected_at, reporter}, 8-day TTL
 *   claim:<email>:<dev>:<machine_id>  that holder's claim PUT/DELETE, and its own usage refresh   {at}, claim TTL
 *
 * All data lives in KV *metadata* (the value is a constant) because `list()`
 * returns metadata: GET /accounts is one paginated list with no per-key reads.
 * An email is shared iff its `share:` key exists and is valid. `usage:`/`claim:`
 * keys without one (left by a request that raced an unshare) are ignored and
 * expire on their own. Emails, devs and machine ids never contain ':', so key
 * names split unambiguously. `nickname` is missing on share records written
 * before nicknames existed; such accounts list with `nickname: null`.
 *
 * KV is still eventually consistent (another location can see a write up to
 * ~60 s late); this layout removes lost updates, not staleness.
 */

export const USAGE_TTL_SEC = 8 * 24 * 60 * 60;
/** KV rejects an expirationTtl below 60 s. */
export const MIN_EXPIRATION_TTL_SEC = 60;
export const MAX_METADATA_BYTES = 1024;
export const MAX_KEY_BYTES = 512;

const SHARE = "share:";
const USAGE = "usage:";
const CLAIM = "claim:";
const MARKER = "1";

export const keys = {
  share: (email: string): string => SHARE + email,
  usage: (email: string): string => USAGE + email,
  claimPrefix: (email: string): string => `${CLAIM}${email}:`,
  claim: (email: string, holder: Identity): string =>
    `${CLAIM}${email}:${holder.dev}:${holder.machine_id}`,
};

export type ParsedKey =
  | { kind: "share"; email: string }
  | { kind: "usage"; email: string }
  | { kind: "claim"; email: string; holder: Identity };

/** Null for foreign keys and for names this Worker would never have written. */
export function parseKey(name: string): ParsedKey | null {
  if (name.startsWith(SHARE)) {
    const email = canonicalEmail(name.slice(SHARE.length));
    return email === null ? null : { kind: "share", email };
  }
  if (name.startsWith(USAGE)) {
    const email = canonicalEmail(name.slice(USAGE.length));
    return email === null ? null : { kind: "usage", email };
  }
  if (name.startsWith(CLAIM)) {
    const parts = name.slice(CLAIM.length).split(":");
    if (parts.length !== 3) return null;
    const email = canonicalEmail(parts[0]!);
    const holder = canonicalHolder(parts[1]!, parts[2]!);
    return email === null || holder === null ? null : { kind: "claim", email, holder };
  }
  return null;
}

function hasOwnPrefix(name: string): boolean {
  return name.startsWith(SHARE) || name.startsWith(USAGE) || name.startsWith(CLAIM);
}

export function claimTtlSec(claimTtlMin: number): number {
  return Math.max(MIN_EXPIRATION_TTL_SEC, Math.ceil(claimTtlMin * 60));
}

function byteLength(s: string): number {
  return new TextEncoder().encode(s).byteLength;
}

/**
 * Inputs are validated and bounded well below these limits, so this only
 * trips if a future field grows a record. It fails the request (500, logged)
 * instead of letting KV reject the write with a less obvious error.
 */
export function checkKvLimits(key: string, metadata: object): void {
  const keyBytes = byteLength(key);
  if (keyBytes > MAX_KEY_BYTES)
    throw new Error(`KV key is ${keyBytes} bytes; the limit is ${MAX_KEY_BYTES}`);
  const metaBytes = byteLength(JSON.stringify(metadata));
  if (metaBytes > MAX_METADATA_BYTES)
    throw new Error(`KV metadata for ${key} is ${metaBytes} bytes; the limit is ${MAX_METADATA_BYTES}`);
}

/** Not silent: an operator sees these in `wrangler tail`; clients never do. */
function skipInvalid(key: string): null {
  console.warn(`skipping KV key with invalid name or metadata: ${key}`);
  return null;
}

function assemble(
  email: string,
  share: ShareRecord,
  usage: UsageRecord | null,
  claims: Claim[],
): StoredAccount {
  return { email, ...share, ...(usage ?? {}), claims };
}

export interface StoreOptions {
  claimTtlSec: number;
  /** KV's own maximum (1000) unless a test forces pagination. */
  listPageSize?: number;
}

export class Store {
  constructor(
    private readonly kv: KVNamespace,
    private readonly opts: StoreOptions,
  ) {}

  /** Null when the email is not shared, including when its share record is invalid. */
  async getShare(email: string): Promise<ShareRecord | null> {
    const key = keys.share(email);
    const { value, metadata } = await this.kv.getWithMetadata<unknown>(key);
    if (value === null) return null;
    return this.shareFrom(key, metadata);
  }

  /**
   * The email of another shared account whose nickname is `nickname` (both
   * lowercased), or null. One paginated `list` over `share:`; the first match
   * in key order wins when a race left several holders.
   *
   * Read-then-write: KV has no transactions or conditional puts, so a caller
   * that checks this and then writes can race another caller doing the same
   * for a different email (see handleShare in src/index.ts).
   */
  async nicknameHolder(nickname: string, except: string): Promise<string | null> {
    for await (const k of this.listKeys(SHARE)) {
      const parsed = parseKey(k.name);
      if (parsed?.kind !== "share") {
        skipInvalid(k.name);
        continue;
      }
      if (parsed.email === except) continue;
      if (this.shareFrom(k.name, k.metadata)?.nickname === nickname) return parsed.email;
    }
    return null;
  }

  async putShare(email: string, record: ShareRecord): Promise<void> {
    await this.write(keys.share(email), record);
  }

  /** Null when there is no report yet or the stored one is invalid (the next report replaces it). */
  async getUsage(email: string): Promise<UsageRecord | null> {
    const key = keys.usage(email);
    const { value, metadata } = await this.kv.getWithMetadata<unknown>(key);
    if (value === null) return null;
    return parseUsageRecord(metadata) ?? skipInvalid(key);
  }

  async putUsage(email: string, record: UsageRecord): Promise<void> {
    await this.write(keys.usage(email), record, USAGE_TTL_SEC);
  }

  /** The holder's claim time, or null when it holds no (valid) claim. */
  async claimAt(email: string, holder: Identity): Promise<string | null> {
    const { value, metadata } = await this.kv.getWithMetadata(keys.claim(email, holder));
    if (value === null) return null;
    return parseClaimRecord(metadata)?.at ?? null;
  }

  async putClaim(email: string, holder: Identity, at: string): Promise<void> {
    await this.write(keys.claim(email, holder), { at }, this.opts.claimTtlSec);
  }

  async deleteClaim(email: string, holder: Identity): Promise<void> {
    await this.kv.delete(keys.claim(email, holder));
  }

  async listClaims(email: string): Promise<Claim[]> {
    const out: Claim[] = [];
    for await (const k of this.listKeys(keys.claimPrefix(email))) {
      const claim = this.claimFrom(k);
      if (claim !== null && claim.email === email) out.push(claim.claim);
    }
    return out;
  }

  async unshare(email: string): Promise<void> {
    // The share key goes first and on its own: from then on the account is
    // hidden and every usage/claim PUT 404s, so a write that passed its share
    // check just before can only leave an orphan, which listings ignore and
    // KV expires. If a later delete fails the account is already unshared and
    // a retry (unshare is idempotent) finishes the cleanup.
    await this.kv.delete(keys.share(email));
    const claimKeys: string[] = [];
    for await (const k of this.listKeys(keys.claimPrefix(email))) claimKeys.push(k.name);
    await Promise.all([
      this.kv.delete(keys.usage(email)),
      ...claimKeys.map((k) => this.kv.delete(k)),
    ]);
  }

  async getAccount(email: string): Promise<StoredAccount | null> {
    const [share, usage] = await Promise.all([this.getShare(email), this.getUsage(email)]);
    // Checked before listing claims: list operations share a small daily quota.
    if (share === null) return null;
    return assemble(email, share, usage, await this.listClaims(email));
  }

  /** Every shared account, from one paginated list and no per-key reads. */
  async listAccounts(): Promise<StoredAccount[]> {
    const shares = new Map<string, ShareRecord>();
    const usage = new Map<string, UsageRecord>();
    const claims = new Map<string, Claim[]>();

    for await (const k of this.listKeys()) {
      const parsed = parseKey(k.name);
      if (parsed === null) {
        if (hasOwnPrefix(k.name)) skipInvalid(k.name);
        continue;
      }
      switch (parsed.kind) {
        case "share": {
          const record = this.shareFrom(k.name, k.metadata);
          if (record !== null) shares.set(parsed.email, record);
          break;
        }
        case "usage": {
          const record = parseUsageRecord(k.metadata);
          if (record === null) skipInvalid(k.name);
          else usage.set(parsed.email, record);
          break;
        }
        case "claim": {
          const claim = this.claimFrom(k);
          if (claim === null) break;
          const list = claims.get(claim.email);
          if (list) list.push(claim.claim);
          else claims.set(claim.email, [claim.claim]);
          break;
        }
      }
    }

    return [...shares].map(([email, share]) =>
      assemble(email, share, usage.get(email) ?? null, claims.get(email) ?? []),
    );
  }

  private shareFrom(key: string, metadata: unknown): ShareRecord | null {
    const record = parseShareRecord(metadata);
    if (record === null) return skipInvalid(key);
    if (storedNicknameDropped(metadata, record))
      console.warn(`ignoring invalid nickname in KV key ${key}; renaming the account replaces it`);
    return record;
  }

  private claimFrom(k: KVNamespaceListKey<unknown>): { email: string; claim: Claim } | null {
    const parsed = parseKey(k.name);
    if (parsed?.kind !== "claim") return skipInvalid(k.name);
    const record = parseClaimRecord(k.metadata);
    if (record === null) return skipInvalid(k.name);
    return { email: parsed.email, claim: { ...parsed.holder, at: record.at } };
  }

  private async *listKeys(prefix?: string): AsyncGenerator<KVNamespaceListKey<unknown>> {
    let cursor: string | undefined;
    for (;;) {
      const page = await this.kv.list<unknown>({
        ...(prefix !== undefined ? { prefix } : {}),
        ...(cursor !== undefined ? { cursor } : {}),
        ...(this.opts.listPageSize !== undefined ? { limit: this.opts.listPageSize } : {}),
      });
      yield* page.keys;
      if (page.list_complete) return;
      cursor = page.cursor;
    }
  }

  private async write(key: string, metadata: object, expirationTtl?: number): Promise<void> {
    checkKvLimits(key, metadata);
    await this.kv.put(key, MARKER, {
      metadata,
      ...(expirationTtl !== undefined ? { expirationTtl } : {}),
    });
  }
}
