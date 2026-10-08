import type { ExhaustedReport, UsageRecord, UsageWindow, WindowName } from "./types";

/**
 * Refusal reports (PUT /accounts/:email/exhausted): Claude Code refused a
 * request for a rate limit (HTTP 429, error type `rate_limit`), reported by the
 * CLI's `StopFailure` hook. After a refusal Claude Code keeps feeding the status
 * line its stale pre-refusal numbers, so the usage record may never reach 100%;
 * these records are what marks such an account exhausted. A newer usage report
 * under 100% must therefore never clear one. Only time does: the record's own
 * reset passing, or a usage report showing that a new window has started.
 */

/**
 * How long each window lasts. A refusal cannot outlast its window, so this is
 * the expiration of a record whose reset is unknown, how long such a record
 * counts, and the furthest a believable reset can lie from now.
 */
export const WINDOW_SEC: Record<WindowName, number> = {
  session: 5 * 60 * 60,
  week: 7 * 24 * 60 * 60,
};

/**
 * Disagreement between two readings of the same window's reset that is noise,
 * not a different window: the CLI parses the refusal's reset from Claude Code's
 * message text, which gives it to the minute or the hour, while the status line
 * gives it to the second; clocks skew too. A new window resets at least most of
 * a window length after the previous one, far beyond this.
 */
const RESET_TOLERANCE_SEC = 60 * 60;

/**
 * Whether `ts` is a reset `window` can have: after now and within one window
 * length (plus tolerance). A reset further out cannot belong to a window that
 * is running now (a misparsed refusal message, a wrong year); believing it
 * would keep the account exhausted for that long with nothing to clear it.
 */
function believableReset(ts: string | null, window: WindowName, nowMs: number): boolean {
  if (ts === null) return false;
  const ms = Date.parse(ts);
  return Number.isFinite(ms) && ms > nowMs && ms - nowMs <= (WINDOW_SEC[window] + RESET_TOLERANCE_SEC) * 1000;
}

/**
 * The reset stored with a new report: the reported one if believable, else the
 * account's usage record's reset for the same window if believable (a reset
 * still ahead means that window is the one running, so the one refused), else
 * null (unknown).
 */
export function resolveExhaustedResetsAt(
  window: WindowName,
  reported: string | null,
  usage: UsageRecord | null,
  now: Date,
): string | null {
  const nowMs = now.getTime();
  for (const candidate of [reported, usage?.[window].resets_at ?? null])
    if (believableReset(candidate, window, nowMs)) return candidate;
  return null;
}

/**
 * The latest moment the refused window can reset: its reset when known, else
 * the report time plus one window length (the window was running when the
 * refusal came, so it resets within one length of it).
 */
function latestReset(r: ExhaustedReport): number {
  return r.resets_at !== null
    ? Date.parse(r.resets_at)
    : Date.parse(r.at) + WINDOW_SEC[r.window] * 1000;
}

/**
 * Records that still block the account. A record lapses when its window has
 * reset (an unknown reset counts as one window length after `at`, matching the
 * key's expiration; KV expiry is not instant, so this filters too), or when the
 * usage window of the same name resets later than the refused window can
 * (beyond tolerance): a new window has started since the refusal. Usage
 * percentages are deliberately ignored, see the top of this file.
 */
export function liveExhausted(
  reports: readonly ExhaustedReport[],
  usage: { session?: UsageWindow | undefined; week?: UsageWindow | undefined },
  nowMs: number,
): ExhaustedReport[] {
  return reports.filter((r) => {
    const end = latestReset(r);
    if (!Number.isFinite(end) || end <= nowMs) return false;
    const current = usage[r.window];
    if (current === undefined) return true;
    return !(Date.parse(current.resets_at) > end + RESET_TOLERANCE_SEC * 1000);
  });
}
