import type { AccountsResponse, RankedAccount, RankedWindow } from "./types";

interface ZonedParts {
  year: number;
  month: number;
  day: number;
  hour: string;
  minute: string;
  weekday: string;
}

/** hourCycle h23 keeps midnight at 00, not 24, across ICU versions. */
function zonedParts(tz: string, d: Date): ZonedParts {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: tz,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    weekday: "short",
  }).formatToParts(d);
  const map: Record<string, string> = {};
  for (const p of parts) map[p.type] = p.value;
  return {
    year: Number(map.year),
    month: Number(map.month),
    day: Number(map.day),
    hour: map.hour ?? "00",
    minute: map.minute ?? "00",
    weekday: map.weekday ?? "",
  };
}

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

function dayIndex(p: ZonedParts): number {
  return Date.UTC(p.year, p.month - 1, p.day) / 86_400_000;
}

/**
 * `HH:MM` today, `Mon HH:MM` within the next 6 days, else `YYYY-MM-DD HH:MM`.
 * Calendar days are compared in `tz`, so DST shifts never move a date.
 */
export function formatResetTime(iso: string, tz: string, now: Date): string {
  const target = zonedParts(tz, new Date(iso));
  const today = zonedParts(tz, now);
  const diff = dayIndex(target) - dayIndex(today);
  const hm = `${target.hour}:${target.minute}`;
  if (diff === 0) return hm;
  if (diff >= 1 && diff <= 6) return `${target.weekday} ${hm}`;
  return `${target.year}-${pad2(target.month)}-${pad2(target.day)} ${hm}`;
}

export function formatStamp(d: Date, tz: string): string {
  const p = zonedParts(tz, d);
  return `${p.year}-${pad2(p.month)}-${pad2(p.day)} ${p.hour}:${p.minute}`;
}

function percent(n: number): string {
  return `${Number.isInteger(n) ? n : n.toFixed(1)}%`;
}

function windowCell(w: RankedWindow | undefined, tz: string, now: Date): string {
  if (!w) return "-";
  if (w.reset_passed) return "0% → reset";
  return `${percent(w.effective)} → ${formatResetTime(w.resets_at, tz, now)}`;
}

function duration(ms: number): string {
  const m = Math.max(0, Math.floor(ms / 60_000));
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

function ago(iso: string | undefined, now: Date): string {
  if (!iso) return "never";
  const ms = now.getTime() - Date.parse(iso);
  if (!Number.isFinite(ms)) return "never";
  if (ms < 60_000) return "just now";
  return `${duration(ms)} ago`;
}

/**
 * `exhausted (week resets Fri 10:00)`: `exhausted_window` and
 * `exhausted_until` as ranked (the later reset when several block, see
 * `exhaustion` in rank.ts), never recomputed here, so text and JSON agree.
 * `exhausted (week)` when the reset is unknown (a refusal report without one).
 * Who is on an exhausted account is left to the JSON `busy_by`.
 * `in use by ali, can (12m)`: every other dev on it, with the age of the last
 * report (the in_use signal). Claims carry no age: with several holders there
 * is no single meaningful one.
 */
function stateCell(a: RankedAccount, tz: string, now: Date): string {
  if (a.state === "exhausted") {
    // Unreachable while `state` comes from rank(); the bare word keeps the row honest.
    if (a.exhausted_window === null) return "exhausted";
    if (a.exhausted_until === null) return `exhausted (${a.exhausted_window})`;
    return `exhausted (${a.exhausted_window} resets ${formatResetTime(a.exhausted_until, tz, now)})`;
  }
  if (a.state === "free" || a.busy_by.length === 0) return "free";
  const who = a.busy_by.join(", ");
  if (a.state === "in_use")
    return `in use by ${who} (${duration(now.getTime() - Date.parse(a.collected_at ?? ""))})`;
  return `claimed by ${who}`;
}

const HEADERS = ["#", "NICK", "ACCOUNT", "SESSION", "WEEK", "STATE", "UPDATED"] as const;

/**
 * The CLI adds its own LOCAL column; the Worker cannot know which config dirs
 * exist on a machine, so it is absent here.
 */
export function formatText(res: AccountsResponse, now: Date): string {
  if (res.accounts.length === 0) return "no accounts reported yet\n";

  const rows: string[][] = res.accounts.map((a) => [
    String(a.rank),
    a.nickname ?? "-",
    a.email,
    windowCell(a.session, res.tz, now),
    windowCell(a.week, res.tz, now),
    stateCell(a, res.tz, now),
    ago(a.collected_at, now),
  ]);

  const widths = HEADERS.map((h, i) =>
    rows.reduce((w, r) => Math.max(w, (r[i] ?? "").length), h.length),
  );
  const line = (cells: readonly string[]): string =>
    cells
      .map((c, i) => c.padEnd(widths[i] ?? 0))
      .join("  ")
      .trimEnd();

  const out = [line(HEADERS), ...rows.map(line), ""];
  out.push(`generated ${formatStamp(now, res.tz)} (${res.tz})`);
  return out.join("\n") + "\n";
}
