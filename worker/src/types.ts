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
 * The logical account assembled from its `share:`, `usage:` and `claim:` keys
 * (see src/store.ts). `session`/`week`/`collected_at`/`reporter` are absent
 * until the first valid report; `added_by`/`added_at` are optional only so
 * ranking and formatting can be exercised without a share record.
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
}

export interface RankedWindow extends UsageWindow {
  effective: number;
  reset_passed: boolean;
}

export type AccountState = "free" | "in_use" | "claimed";

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
  /** Sorted, unique, never the querying dev; empty when nobody else is on it. */
  busy_by: string[];
}

export interface AccountsResponse {
  generated_at: string;
  tz: string;
  accounts: RankedAccount[];
}
