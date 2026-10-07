import { freshFor, liveClaims } from "./claims";
import type {
  AccountState,
  RankedAccount,
  RankedWindow,
  StoredAccount,
  UsageWindow,
} from "./types";

export interface RankOptions {
  now: Date;
  /** The querying developer. Their own claims/activity never make an account busy. */
  dev?: string | undefined;
  claimTtlMin: number;
  activityTtlMin: number;
}

function rankWindow(w: UsageWindow | undefined, nowMs: number): RankedWindow | undefined {
  if (!w) return undefined;
  const reset_passed = Date.parse(w.resets_at) <= nowMs;
  return {
    used: w.used,
    effective: reset_passed ? 0 : w.used,
    resets_at: w.resets_at,
    reset_passed,
  };
}

interface Scored {
  account: RankedAccount;
  busy: boolean;
  known: boolean;
  session: number;
  week: number;
  resets: number;
}

/**
 * Pure ranking. `now` is injected so both the Worker and the tests control it.
 * Ordering: not busy, then known usage, then effective session %, effective
 * week %, earliest session reset, email.
 */
export function rank(accounts: StoredAccount[], opts: RankOptions): RankedAccount[] {
  const nowMs = opts.now.getTime();
  const scored: Scored[] = accounts.map((a) => {
    const session = rankWindow(a.session, nowMs);
    const week = rankWindow(a.week, nowMs);

    const reporterDev = a.reporter?.dev;
    const activeDev =
      reporterDev !== undefined &&
      reporterDev !== opts.dev &&
      freshFor(a.collected_at, nowMs, opts.activityTtlMin)
        ? reporterDev
        : null;

    const claims = liveClaims(a.claims, nowMs, opts.claimTtlMin);
    const claimingDevs = claims.map((c) => c.dev).filter((d) => d !== opts.dev);

    const busy_by = [...new Set([...(activeDev ? [activeDev] : []), ...claimingDevs])].sort();
    // Activity beats a claim: someone typing right now is the better signal.
    const state: AccountState = activeDev
      ? "in_use"
      : claimingDevs.length > 0
        ? "claimed"
        : "free";

    const account: RankedAccount = {
      rank: 0,
      email: a.email,
      nickname: a.nickname ?? null,
      ...(a.added_by !== undefined ? { added_by: a.added_by } : {}),
      ...(a.added_at !== undefined ? { added_at: a.added_at } : {}),
      ...(session ? { session } : {}),
      ...(week ? { week } : {}),
      ...(a.collected_at !== undefined ? { collected_at: a.collected_at } : {}),
      ...(a.reporter !== undefined ? { reporter: a.reporter } : {}),
      claims,
      state,
      busy_by,
    };

    return {
      account,
      busy: busy_by.length > 0,
      known: session !== undefined || week !== undefined,
      session: session?.effective ?? Number.POSITIVE_INFINITY,
      week: week?.effective ?? Number.POSITIVE_INFINITY,
      resets: session ? Date.parse(session.resets_at) : Number.POSITIVE_INFINITY,
    };
  });

  scored.sort((x, y) => {
    if (x.busy !== y.busy) return x.busy ? 1 : -1;
    if (x.known !== y.known) return x.known ? -1 : 1;
    if (x.session !== y.session) return x.session - y.session;
    if (x.week !== y.week) return x.week - y.week;
    if (x.resets !== y.resets) return x.resets - y.resets;
    return x.account.email < y.account.email ? -1 : x.account.email > y.account.email ? 1 : 0;
  });

  return scored.map((s, i) => {
    s.account.rank = i + 1;
    return s.account;
  });
}
