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

/**
 * Usage at which Claude refuses work until the window resets. The status line
 * reports at most 100 (validate.ts rejects more); `>=` keeps that a non-issue.
 */
const EXHAUSTED_AT = 100;

export type WindowName = "session" | "week";

export interface BlockingWindow {
  name: WindowName;
  window: RankedWindow;
}

/**
 * The window an exhausted account is waiting on, or undefined when it is not
 * exhausted. With both windows at 100% this is the one that resets LAST: the
 * account stays unusable until every exhausted window has reset. Ties go to
 * week. Shared with format.ts so the text STATE names the same window whose
 * reset is `exhausted_until`.
 */
export function blockingWindow(
  session: RankedWindow | undefined,
  week: RankedWindow | undefined,
): BlockingWindow | undefined {
  let blocking: BlockingWindow | undefined;
  for (const [name, w] of [
    ["session", session],
    ["week", week],
  ] as const) {
    if (!w || w.effective < EXHAUSTED_AT) continue;
    if (!blocking || Date.parse(w.resets_at) >= Date.parse(blocking.window.resets_at))
      blocking = { name, window: w };
  }
  return blocking;
}

interface Scored {
  account: RankedAccount;
  exhausted: boolean;
  /** `exhausted_until` in ms; only compared between two exhausted accounts. */
  until: number;
  busy: boolean;
  known: boolean;
  session: number;
  week: number;
  resets: number;
}

/**
 * Pure ranking. `now` is injected so both the Worker and the tests control it.
 * Ordering: not exhausted (an account at 100% sinks below every usable one,
 * whatever its other window says), then within the exhausted band the earliest
 * `exhausted_until`; then not busy, known usage, effective session %, effective
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
    const blocking = blockingWindow(session, week);
    // Exhaustion beats everything: nobody can use it, whoever is on it. busy_by
    // stays populated so clients still see who is. Activity beats a claim:
    // someone typing right now is the better signal.
    const state: AccountState = blocking
      ? "exhausted"
      : activeDev
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
      exhausted: blocking !== undefined,
      exhausted_until: blocking?.window.resets_at ?? null,
    };

    return {
      account,
      exhausted: blocking !== undefined,
      until: blocking ? Date.parse(blocking.window.resets_at) : Number.POSITIVE_INFINITY,
      busy: busy_by.length > 0,
      known: session !== undefined || week !== undefined,
      session: session?.effective ?? Number.POSITIVE_INFINITY,
      week: week?.effective ?? Number.POSITIVE_INFINITY,
      resets: session ? Date.parse(session.resets_at) : Number.POSITIVE_INFINITY,
    };
  });

  scored.sort((x, y) => {
    if (x.exhausted !== y.exhausted) return x.exhausted ? 1 : -1;
    if (x.exhausted && x.until !== y.until) return x.until - y.until;
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
