/** Shapes shared by the Worker, KV and the Go CLI. See docs/SPEC.md. */

export interface Env {
  USAGE: KVNamespace;
  AUTH_TOKEN: string;
  DEFAULT_TZ: string;
  CLAIM_TTL_MIN: string;
  ACTIVITY_TTL_MIN: string;
}

/** A rate-limit window as reported by the status line. */
export interface UsageWindow {
  used: number;
  resets_at: string;
}

/** The two rate-limit windows: Claude Code's `five_hour` and `seven_day`. */
export type WindowName = "session" | "week";

/** Who did something: a developer on one machine. */
export interface Identity {
  dev: string;
  machine_id: string;
}

export type Reporter = Identity;

/** One holder of the advisory "in use" marker; `at` is server-stamped. */
export interface Claim extends Identity {
  at: string;
}

/**
 * Metadata of `share:<email>`; the key existing is what makes the email shared.
 * `nickname` is absent on records written before nicknames existed; those stay
 * shared and readable, and PUT /accounts/:email/nickname gives them one.
 */
export interface ShareRecord {
  added_by: Identity;
  added_at: string;
  nickname?: string;
}

/** Metadata of `usage:<email>`: the latest snapshot, `collected_at` clamped to the Worker's clock. */
export interface UsageRecord {
  session: UsageWindow;
  week: UsageWindow;
  collected_at: string;
  reporter: Reporter;
}

/** Metadata of `claim:<email>:<dev>:<machine_id>`; the holder is in the key. */
export interface ClaimRecord {
  at: string;
}

/**
 * Metadata of `exhausted:<email>:<dev>:<machine_id>`: Claude Code refused that
 * holder a request for `window`'s limit (HTTP 429, error type `rate_limit`).
 * `resets_at` is resolved when the report is written (see src/exhausted.ts)
 * and null when unknown; `at` is server-stamped.
 */
export interface ExhaustedRecord {
  window: WindowName;
  resets_at: string | null;
  at: string;
}

/** One stored refusal report with its reporter; the reporter is in the key. */
export interface ExhaustedReport extends Identity, ExhaustedRecord {}

/**
 * The logical account assembled from its `share:`, `usage:`, `claim:` and
 * `exhausted:` keys (see src/store.ts). `session`/`week`/`collected_at`/
 * `reporter` are absent until the first valid report; `added_by`/`added_at`
 * are optional only so ranking and formatting can be exercised without a share
 * record. `exhausted` is always set by the store; absent reads as none, so
 * fixtures need not spell it out.
 */
export interface StoredAccount {
  email: string;
  nickname?: string;
  added_by?: Identity;
  added_at?: string;
  session?: UsageWindow;
  week?: UsageWindow;
  collected_at?: string;
  reporter?: Reporter;
  claims: Claim[];
  exhausted?: ExhaustedReport[];
}

export interface RankedWindow extends UsageWindow {
  effective: number;
  reset_passed: boolean;
}

/** `exhausted` wins over the others: an account at 100% cannot take work, busy or not. */
export type AccountState = "free" | "in_use" | "claimed" | "exhausted";

export interface RankedAccount {
  rank: number;
  email: string;
  /** Team-wide name; null for accounts shared before nicknames existed. Always present. */
  nickname: string | null;
  added_by?: Identity;
  added_at?: string;
  session?: RankedWindow;
  week?: RankedWindow;
  collected_at?: string;
  reporter?: Reporter;
  /** Fresh holders only (expired ones are dropped), the querying dev included. */
  claims: Claim[];
  state: AccountState;
  /** Sorted, unique, never the querying dev; empty when nobody else is on it. Populated in every state. */
  busy_by: string[];
  /**
   * A present window is at 100% effective usage, or a live `exhausted:` record
   * says Claude refused a request for a limit (src/exhausted.ts). Always present.
   */
  exhausted: boolean;
  /**
   * When the account takes work again: the latest known reset among the
   * windows at 100% and the live refusal records (all of them must reset).
   * null when not exhausted, or exhausted with no known reset. Always present.
   */
  exhausted_until: string | null;
  /**
   * The window `exhausted_until` belongs to (week on a tie); with no known
   * reset, the refused window (week when both were). null only when not
   * exhausted. Always present.
   */
  exhausted_window: WindowName | null;
}

export interface AccountsResponse {
  generated_at: string;
  tz: string;
  accounts: RankedAccount[];
}
