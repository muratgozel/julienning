import type {
  Claim,
  ClaimRecord,
  Identity,
  Reporter,
  ShareRecord,
  UsageRecord,
  UsageWindow,
} from "./types";

/** Rejected at the boundary; the router turns this into 400 {error, field}. */
export class ValidationError extends Error {
  constructor(
    readonly field: string,
    message: string,
  ) {
    super(message);
    this.name = "ValidationError";
  }
}

export const MAX_BODY_BYTES = 4096;

/** Used when the matching wrangler var is missing or unusable. */
export const FALLBACK_TZ = "UTC";
export const DEFAULT_CLAIM_TTL_MIN = 720;
export const DEFAULT_ACTIVITY_TTL_MIN = 15;

const EMAIL_RE = /^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$/;
const DEV_RE = /^[a-z0-9][a-z0-9._-]{0,31}$/;
// Checked before lowercasing, and ASCII-only on purpose: String.toLowerCase
// folds some non-ASCII letters into ASCII (U+212A KELVIN SIGN -> "k"), which
// would let look-alike input through as a different name.
const NICKNAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$/;
const MACHINE_ID_RE = /^[0-9a-fA-F]{12}$/;
// RFC 3339 date-time: offset is required, fractional seconds optional.
const RFC3339_RE =
  /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:\d{2})$/;

export function validateEmail(raw: string, field = "email"): string {
  const email = raw.trim();
  if (email.length === 0) throw new ValidationError(field, "email is required");
  if (email.length > 254)
    throw new ValidationError(field, "email must be at most 254 characters");
  if (!EMAIL_RE.test(email))
    throw new ValidationError(field, "email is not a valid address");
  return email.toLowerCase();
}

/**
 * The email if `raw` is already in the form the Worker writes into KV keys
 * (valid and lowercased), else null. Hand-written or foreign keys fail this.
 */
export function canonicalEmail(raw: string): string | null {
  try {
    return validateEmail(raw) === raw ? raw : null;
  } catch {
    return null;
  }
}

/** The holder if `dev`/`machine_id` are exactly as the Worker writes them into claim keys. */
export function canonicalHolder(dev: string, machineId: string): Identity | null {
  try {
    const id = { dev: validateDev(dev), machine_id: validateMachineId(machineId) };
    return id.machine_id === machineId ? id : null;
  } catch {
    return null;
  }
}

export function validateDev(raw: unknown, field = "dev"): string {
  if (typeof raw !== "string")
    throw new ValidationError(field, "dev must be a string");
  if (!DEV_RE.test(raw))
    throw new ValidationError(
      field,
      "dev must be 1-32 chars of [a-z0-9._-] and start with [a-z0-9]",
    );
  return raw;
}

/**
 * Team-wide account nickname, lowercased. Uniqueness is decided on this form,
 * so `Alpha` and `alpha` are the same nickname.
 */
export function validateNickname(raw: unknown, field = "nickname"): string {
  if (raw === undefined || raw === null || raw === "")
    throw new ValidationError(field, "nickname is required");
  if (typeof raw !== "string")
    throw new ValidationError(field, "nickname must be a string");
  if (raw.includes("@"))
    throw new ValidationError(field, "nickname must not contain '@'; use a short name, not an email address");
  if (!NICKNAME_RE.test(raw))
    throw new ValidationError(
      field,
      "nickname must be 1-32 chars of [a-z0-9._-] and start with [a-z0-9]",
    );
  return raw.toLowerCase();
}

export function validateMachineId(raw: unknown, field = "machine_id"): string {
  if (typeof raw !== "string")
    throw new ValidationError(field, "machine_id must be a string");
  if (!MACHINE_ID_RE.test(raw))
    throw new ValidationError(
      field,
      "machine_id must be 12 hexadecimal characters",
    );
  return raw.toLowerCase();
}

/** RFC 3339 in, UTC out. Seconds precision unless the input had milliseconds. */
export function validateTimestamp(raw: unknown, field: string): string {
  if (typeof raw !== "string")
    throw new ValidationError(field, `${field} must be an RFC 3339 timestamp`);
  if (!RFC3339_RE.test(raw))
    throw new ValidationError(field, `${field} must be an RFC 3339 timestamp`);
  const ms = Date.parse(raw);
  if (!Number.isFinite(ms))
    throw new ValidationError(field, `${field} is not a valid date`);
  return toRfc3339Utc(new Date(ms));
}

/**
 * `.000` is trimmed so output matches the timestamps documented in
 * docs/SPEC.md; the Go CLI sends whole seconds, so this is the normal path.
 */
export function toRfc3339Utc(d: Date): string {
  return d.toISOString().replace(/\.000Z$/, "Z");
}

/**
 * Server-generated stamps (`generated_at`, `added_at`, `claims[].at`) are always whole
 * seconds so responses match the shapes in docs/SPEC.md.
 */
export function nowStamp(d: Date): string {
  return new Date(Math.floor(d.getTime() / 1000) * 1000).toISOString().replace(/\.000Z$/, "Z");
}

/**
 * Caps a client timestamp at the Worker's clock. Applied to `collected_at`
 * before it is stored and before the "older than stored" check, so a machine
 * whose clock runs ahead cannot store a future snapshot that would make every
 * later, correctly-timed report look stale.
 */
export function clampToNow(ts: string, now: Date): string {
  return Date.parse(ts) > now.getTime() ? nowStamp(now) : ts;
}

function validateUsed(raw: unknown, field: string): number {
  if (typeof raw !== "number" || !Number.isFinite(raw))
    throw new ValidationError(field, `${field} must be a finite number`);
  if (raw < 0 || raw > 100)
    throw new ValidationError(field, `${field} must be between 0 and 100`);
  return Math.round(raw * 10) / 10;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function validateWindow(raw: unknown, field: string): UsageWindow {
  if (!isRecord(raw))
    throw new ValidationError(field, `${field} must be an object`);
  return {
    used: validateUsed(raw.used, `${field}.used`),
    resets_at: validateTimestamp(raw.resets_at, `${field}.resets_at`),
  };
}

function validateIdentity(raw: unknown, field: string): Identity {
  if (!isRecord(raw))
    throw new ValidationError(field, `${field} must be an object`);
  return {
    dev: validateDev(raw.dev, `${field}.dev`),
    machine_id: validateMachineId(raw.machine_id, `${field}.machine_id`),
  };
}

function validateReporter(raw: unknown): Reporter {
  return validateIdentity(raw, "reporter");
}

export function validateUsageBody(body: unknown): UsageRecord {
  if (!isRecord(body))
    throw new ValidationError("body", "body must be a JSON object");
  return {
    session: validateWindow(body.session, "session"),
    week: validateWindow(body.week, "week"),
    collected_at: validateTimestamp(body.collected_at, "collected_at"),
    reporter: validateReporter(body.reporter),
  };
}

export function validateClaimBody(body: unknown, now: Date): Claim {
  if (!isRecord(body))
    throw new ValidationError("body", "body must be a JSON object");
  return {
    dev: validateDev(body.dev),
    machine_id: validateMachineId(body.machine_id),
    at: nowStamp(now),
  };
}

export interface ShareBody {
  added_by: Identity;
  /** Absent when the client sent none; required only when the email is not shared yet. */
  nickname?: string;
}

/**
 * PUT /accounts/:email body: who is adding the email to the allowlist, and its
 * nickname. A missing, null or empty nickname is accepted here because it is
 * ignored when the email is already shared; the handler requires it on create.
 * A present but malformed one is rejected either way.
 */
export function validateShareBody(body: unknown): ShareBody {
  if (!isRecord(body))
    throw new ValidationError("body", "body must be a JSON object");
  const added_by = validateIdentity(body.added_by, "added_by");
  const raw = body.nickname;
  if (raw === undefined || raw === null || raw === "") return { added_by };
  return { added_by, nickname: validateNickname(raw) };
}

/** PUT /accounts/:email/nickname body. */
export function validateNicknameBody(body: unknown): string {
  if (!isRecord(body))
    throw new ValidationError("body", "body must be a JSON object");
  return validateNickname(body.nickname);
}

function requiredParam(params: URLSearchParams, name: string): string {
  const v = params.get(name);
  if (v === null || v === "")
    throw new ValidationError(name, `${name} query parameter is required`);
  return v;
}

/**
 * DELETE /accounts/:email/claim names exactly one holder. Both params are
 * required so a client can never drop another machine's claim by accident.
 */
export function validateHolderQuery(params: URLSearchParams): Identity {
  return {
    dev: validateDev(requiredParam(params, "dev")),
    machine_id: validateMachineId(requiredParam(params, "machine_id")),
  };
}

function bodyTooLarge(): never {
  throw new ValidationError(
    "body",
    `body must be at most ${MAX_BODY_BYTES} bytes`,
  );
}

/**
 * Reads at most MAX_BODY_BYTES. Never calls `arrayBuffer()`: a chunked body
 * carries no Content-Length, so buffering first would let a multi-MB (or
 * endless) stream into memory before the size check could reject it.
 */
async function readLimitedBody(request: Request): Promise<Uint8Array> {
  const declared = request.headers.get("content-length");
  if (declared !== null && Number(declared) > MAX_BODY_BYTES) bodyTooLarge();

  const body = request.body;
  if (body === null) return new Uint8Array(0);

  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > MAX_BODY_BYTES) {
        // Tell the client to stop sending before unwinding.
        await reader.cancel();
        bodyTooLarge();
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }

  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}

/** Guards against oversized payloads before any parsing work happens. */
export async function readJsonBody(request: Request): Promise<unknown> {
  const buf = await readLimitedBody(request);
  if (buf.byteLength === 0)
    throw new ValidationError("body", "body is required");
  try {
    return JSON.parse(new TextDecoder().decode(buf));
  } catch {
    throw new ValidationError("body", "body must be valid JSON");
  }
}

export function validateFormat(raw: string | null): "text" | "json" {
  if (raw === null || raw === "") return "text";
  if (raw === "text" || raw === "json") return raw;
  throw new ValidationError("format", "format must be 'text' or 'json'");
}

function isValidTz(tz: string): boolean {
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: tz });
    return true;
  } catch {
    return false;
  }
}

export function validateTz(raw: string | null, fallback: string): string {
  const tz = raw === null || raw === "" ? fallback : raw;
  if (!isValidTz(tz))
    throw new ValidationError("tz", "tz must be a valid IANA time zone");
  return tz;
}

/**
 * The DEFAULT_TZ var, or UTC. A bad var is an operator mistake, not the
 * client's, so it must not turn into a 400 on `tz`; it is logged and the
 * listing still renders (the Go CLI ignores `tz` and renders locally anyway).
 */
export function defaultTz(raw: string | undefined): string {
  if (raw === undefined || raw.trim() === "") return FALLBACK_TZ;
  if (isValidTz(raw)) return raw;
  console.warn(`DEFAULT_TZ is not a valid IANA time zone; using ${FALLBACK_TZ}`);
  return FALLBACK_TZ;
}

export function validateOptionalDev(raw: string | null): string | undefined {
  if (raw === null || raw === "") return undefined;
  return validateDev(raw);
}

/** Positive integer minutes from a wrangler var; falls back when unusable. */
export function ttlMinutes(raw: string | undefined, fallback: number): number {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

/**
 * KV metadata parsers. Each record is validated on its own and unknown keys are
 * dropped; an invalid record reads as null so the caller can skip just that key
 * (see src/store.ts for what each null means for the account).
 */
function parseRecord<T>(raw: unknown, parse: (v: Record<string, unknown>) => T): T | null {
  if (!isRecord(raw)) return null;
  try {
    return parse(raw);
  } catch (err) {
    if (err instanceof ValidationError) return null;
    throw err;
  }
}

/**
 * A missing nickname is normal (records from before nicknames). An unusable one
 * is dropped instead of invalidating the record: hiding the account would make
 * every CLI forget it is shared, while a missing nickname is fixed by a rename.
 * src/store.ts warns when that happens (see `storedNicknameDropped`).
 */
export function parseShareRecord(raw: unknown): ShareRecord | null {
  return parseRecord(raw, (v) => {
    const nickname = storedNickname(v.nickname);
    return {
      added_by: validateIdentity(v.added_by, "added_by"),
      added_at: validateTimestamp(v.added_at, "added_at"),
      ...(nickname !== undefined ? { nickname } : {}),
    };
  });
}

function storedNickname(raw: unknown): string | undefined {
  if (raw === undefined) return undefined;
  try {
    return validateNickname(raw);
  } catch (err) {
    if (err instanceof ValidationError) return undefined;
    throw err;
  }
}

/** True when `raw` carried a nickname that `parsed` had to drop as unusable. */
export function storedNicknameDropped(raw: unknown, parsed: ShareRecord): boolean {
  return isRecord(raw) && raw.nickname !== undefined && parsed.nickname === undefined;
}

export function parseUsageRecord(raw: unknown): UsageRecord | null {
  return parseRecord(raw, validateUsageBody);
}

export function parseClaimRecord(raw: unknown): ClaimRecord | null {
  return parseRecord(raw, (v) => ({ at: validateTimestamp(v.at, "at") }));
}
