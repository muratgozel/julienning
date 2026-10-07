import type { Claim } from "./types";

/** Whether `ts` is younger than `ttlMin`. Unparsable or missing is never fresh. */
export function freshFor(ts: string | undefined, nowMs: number, ttlMin: number): boolean {
  if (!ts) return false;
  const ms = Date.parse(ts);
  if (!Number.isFinite(ms)) return false;
  // A future timestamp (clock skew on a reporter) still counts as fresh.
  return nowMs - ms < ttlMin * 60_000;
}

/**
 * Holders younger than the claim TTL. Claim keys also expire in KV after the
 * TTL, but this still filters: KV expiry is not instant, and a shortened
 * CLAIM_TTL_MIN must apply to keys written under the old, longer one.
 */
export function liveClaims(claims: readonly Claim[], nowMs: number, ttlMin: number): Claim[] {
  return claims.filter((c) => freshFor(c.at, nowMs, ttlMin));
}
