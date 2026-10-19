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

/**
 * A window as a usage probe read it (the CLI runs Claude Code's `/usage`).
 * `resets_at` null means the window is not running: nothing was used since it
 * last reset, so `used` is 0 (validate.ts enforces that).
 */
export interface ProbeWindow {
  used: number;
  resets_at: string | null;
}

/**
 * Metadata of `probe:<email>`: the latest usage probe, read from Claude's
 * usage endpoint by a CLI with a local login, whoever used the account. It is
 * never activity: probing is not using the account, so it makes nobody busy
 * and refreshes no claim.
 */
export interface ProbeRecord {
  session: ProbeWindow;
  week: ProbeWindow;
  collected_at: string;
  reporter: Reporter;
}

/** Which record a listed account's numbers come from (the newer one). */
export type UsageSource = "statusline" | "probe";

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
 * The logical account assembled from its `share:`, `usage:`, `probe:`,
 * `claim:` and `exhausted:` keys (see src/store.ts). `session`/`week`/
 * `collected_at`/`reporter` are the status line's `usage:` record, absent
 * until its first valid report; `probe` is the `probe:` record, absent until
 * the first probe. `added_by`/`added_at` are optional only so ranking and
 * formatting can be exercised without a share record. `exhausted` is always
 * set by the store; absent reads as none, so fixtures need not spell it out.
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
  probe?: ProbeRecord;
  claims: Claim[];
  exhausted?: ExhaustedReport[];
}

/** `resets_at` null: the window is not running (only probes say so). */
export interface RankedWindow {
  used: number;
  resets_at: string | null;
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
  /**
   * `session`/`week` are the newer of the status line report and the probe,
   * `numbers_at` when they were read and `source` which record it was (all
   * absent with neither).
   */
  session?: RankedWindow;
  week?: RankedWindow;
  numbers_at?: string;
  source?: UsageSource;
  /**
   * The latest status line report: when, and who sent it. These keep their
   * meaning from before probes (only a status line report means someone is
   * on the account), so a probe never shows up here.
   */
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
  /**
   * This Worker takes POST /probes; every handler sets it (a CLI skips
   * probing when a listing lacks it: an older Worker).
   */
  probes?: true;
  accounts: RankedAccount[];
}

/** POST /probes answer: the listing with the batch merged in. */
export interface ProbesResponse extends AccountsResponse {
  /** Reported emails that are not on the allowlist; nothing was stored for them. */
  not_shared: string[];
}

/** POST /probes?listing=0 answer: no listing, so no list operation. */
export interface ProbesAck {
  probes: true;
  not_shared: string[];
}
