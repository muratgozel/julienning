import { freshFor, liveClaims } from "./claims";
import { liveExhausted } from "./exhausted";
import type {
  AccountState,
  ExhaustedReport,
  ProbeWindow,
  RankedAccount,
  RankedWindow,
  Reporter,
  StoredAccount,
  UsageSource,
  WindowName,
} from "./types";

export interface RankOptions {
  now: Date;
  /** The querying developer. Their own claims/activity never make an account busy. */
  dev?: string | undefined;
  claimTtlMin: number;
  activityTtlMin: number;
}

/** A window that is not running (null reset) has nothing used and nothing to pass. */
function rankWindow(w: ProbeWindow | undefined, nowMs: number): RankedWindow | undefined {
  if (!w) return undefined;
  const reset_passed = w.resets_at !== null && Date.parse(w.resets_at) <= nowMs;
  return {
    used: w.used,
    effective: reset_passed ? 0 : w.used,
    resets_at: w.resets_at,
    reset_passed,
  };
}

/** Milliseconds of a reset for sorting and comparing; a window not running sorts like an unknown one. */
function resetMs(w: RankedWindow | undefined): number {
  return w !== undefined && w.resets_at !== null ? Date.parse(w.resets_at) : Number.POSITIVE_INFINITY;
}

interface Numbers {
  session: ProbeWindow;
  week: ProbeWindow;
  collected_at: string;
  reporter: Reporter;
  source: UsageSource;
}

/**
 * The account's numbers: the newer of the status line's `usage:` record and
 * the `probe:` record, the probe on a tie (it is the server's own answer, the
 * status line can lag after a refusal). Both `collected_at` are clamped to
 * the Worker's clock when written, so neither can win by sitting in the
 * future.
 */
function numbers(a: StoredAccount): Numbers | undefined {
  const status: Numbers | undefined =
    a.session !== undefined && a.week !== undefined && a.collected_at !== undefined && a.reporter !== undefined
      ? { session: a.session, week: a.week, collected_at: a.collected_at, reporter: a.reporter, source: "statusline" }
      : undefined;
  const probe: Numbers | undefined = a.probe !== undefined ? { ...a.probe, source: "probe" } : undefined;
  if (status === undefined || probe === undefined) return status ?? probe;
  return Date.parse(probe.collected_at) >= Date.parse(status.collected_at) ? probe : status;
}

/**
 * Usage at which Claude refuses work until the window resets. The status line
 * reports more than 100 once a limit is exceeded; validate.ts clamps that to
 * 100, and `>=` would cover it anyway.
 */
const EXHAUSTED_AT = 100;

interface BlockingWindow {
  name: WindowName;
  window: RankedWindow;
}

/**
 * The window at 100% an account is waiting on, or undefined when none is.
 * With both windows at 100% this is the one that resets LAST: the account
 * stays unusable until every exhausted window has reset. Ties go to week.
 */
function blockingWindow(
  session: RankedWindow | undefined,
  week: RankedWindow | undefined,
): BlockingWindow | undefined {
  let blocking: BlockingWindow | undefined;
  for (const [name, w] of [
    ["session", session],
    ["week", week],
  ] as const) {
    // A window that is not running has nothing used, so it never blocks.
    if (!w || w.resets_at === null || w.effective < EXHAUSTED_AT) continue;
    if (!blocking || resetMs(w) >= resetMs(blocking.window))
      blocking = { name, window: w };
  }
  return blocking;
}

interface Exhaustion {
  window: WindowName;
  until: string | null;
}

/**
 * Why the account cannot take work, or undefined when it can: a window at
 * 100% and/or live refusal records (src/exhausted.ts). `until` is the latest
 * known reset among all of them, since every one must pass, and `window` the
 * window it belongs to (week on a tie). When none has a known reset, `until`
 * is null and `window` is the refused window, week when both were. format.ts
 * renders these fields, so the text STATE and the JSON always agree.
 */
function exhaustion(
  blocking: BlockingWindow | undefined,
  refusals: readonly ExhaustedReport[],
): Exhaustion | undefined {
  const known: { window: WindowName; until: string }[] = [];
  if (blocking?.window.resets_at) known.push({ window: blocking.name, until: blocking.window.resets_at });
  for (const r of refusals) if (r.resets_at !== null) known.push({ window: r.window, until: r.resets_at });

  let latest: { window: WindowName; until: string; ms: number } | undefined;
  for (const k of known) {
    const ms = Date.parse(k.until);
    if (!latest || ms > latest.ms || (ms === latest.ms && k.window === "week"))
      latest = { ...k, ms };
  }
  if (latest) return { window: latest.window, until: latest.until };
  if (refusals.length === 0) return undefined;
  return { window: refusals.some((r) => r.window === "week") ? "week" : "session", until: null };
}

interface Scored {
  account: RankedAccount;
  exhausted: boolean;
  /**
   * `exhausted_until` in ms, +Infinity when unknown so those sort after known
   * ones; only compared between two exhausted accounts.
   */
  until: number;
  busy: boolean;
  known: boolean;
  session: number;
  week: number;
  resets: number;
}

/**
 * Pure ranking. `now` is injected so both the Worker and the tests control it.
 * Ordering: not exhausted (an account at 100% or refused for a limit sinks
 * below every usable one, whatever its usage says), then within the exhausted
 * band the earliest `exhausted_until`, unknown last; then not busy, known
 * usage, effective session %, effective week %, earliest session reset, email.
 */
export function rank(accounts: StoredAccount[], opts: RankOptions): RankedAccount[] {
  const nowMs = opts.now.getTime();
  const scored: Scored[] = accounts.map((a) => {
    const nums = numbers(a);
    const session = rankWindow(nums?.session, nowMs);
    const week = rankWindow(nums?.week, nowMs);

    // Activity is the status line's alone: a probe is not someone using
    // the account, whoever ran it.
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
    const exhausted = exhaustion(
      blockingWindow(session, week),
      liveExhausted(a.exhausted ?? [], { session: nums?.session, week: nums?.week }, nowMs),
    );
    // Exhaustion beats everything: nobody can use it, whoever is on it. busy_by
    // stays populated so clients still see who is. Activity beats a claim:
    // someone typing right now is the better signal.
    const state: AccountState = exhausted
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
      ...(nums !== undefined
        ? { collected_at: nums.collected_at, reporter: nums.reporter, source: nums.source }
        : {}),
      ...(a.reporter !== undefined && a.collected_at !== undefined
        ? { last_activity: { ...a.reporter, at: a.collected_at } }
        : {}),
      claims,
      state,
      busy_by,
      exhausted: exhausted !== undefined,
      exhausted_until: exhausted?.until ?? null,
      exhausted_window: exhausted?.window ?? null,
    };

    return {
      account,
      exhausted: exhausted !== undefined,
      until:
        exhausted !== undefined && exhausted.until !== null
          ? Date.parse(exhausted.until)
          : Number.POSITIVE_INFINITY,
      busy: busy_by.length > 0,
      known: session !== undefined || week !== undefined,
      session: session?.effective ?? Number.POSITIVE_INFINITY,
      week: week?.effective ?? Number.POSITIVE_INFINITY,
      resets: resetMs(session),
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
