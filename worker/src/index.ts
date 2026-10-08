import { resolveExhaustedResetsAt } from "./exhausted";
import { formatText } from "./format";
import { rank, type RankOptions } from "./rank";
import { claimTtlSec, exhaustedTtlSec, Store } from "./store";
import type { AccountsResponse, Env } from "./types";
import {
  clampToNow,
  DEFAULT_ACTIVITY_TTL_MIN,
  DEFAULT_CLAIM_TTL_MIN,
  defaultTz,
  nowStamp,
  readJsonBody,
  ttlMinutes,
  validateClaimBody,
  validateEmail,
  validateExhaustedBody,
  validateFormat,
  validateHolderQuery,
  validateNicknameBody,
  validateOptionalDev,
  validateShareBody,
  validateTz,
  validateUsageBody,
  ValidationError,
} from "./validate";

const JSON_HEADERS = { "content-type": "application/json; charset=utf-8" };
const TEXT_HEADERS = { "content-type": "text/plain; charset=utf-8" };

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: JSON_HEADERS });
}

function errorResponse(error: string, status: number): Response {
  return json({ error }, status);
}

function noContent(): Response {
  return new Response(null, { status: 204 });
}

/**
 * The Go CLI matches this exact body (status 404, no `field`) to drop the
 * email from its local shared.json cache. Do not reword it.
 */
function notShared(): Response {
  return errorResponse("account is not shared", 404);
}

/** The Go CLI reads `by` to tell the user which account holds the nickname. */
function nicknameTaken(nickname: string, by: string): Response {
  return json({ error: "nickname is taken", nickname, by }, 409);
}

/**
 * Compares equal-length tokens in constant time so a wrong token leaks no
 * information about *where* it diverges. A length mismatch exits immediately:
 * the token's length is not a secret, and a shared bearer token is not
 * guessable byte-by-byte from timing anyway.
 */
function tokensMatch(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

function isAuthorized(request: Request, url: URL, env: Env): boolean {
  const expected = env.AUTH_TOKEN;
  if (!expected) return false;
  const header = request.headers.get("authorization");
  if (header !== null) {
    const m = /^Bearer (.+)$/.exec(header);
    return m?.[1] !== undefined && tokensMatch(m[1], expected);
  }
  // ?token= is a convenience for curl/browsers and is GET-only on purpose:
  // query strings end up in logs and browser history.
  if (request.method !== "GET") return false;
  const q = url.searchParams.get("token");
  return q !== null && tokensMatch(q, expected);
}

interface Route {
  segments: number;
  tail?: string;
  methods: Record<string, (ctx: RouteContext) => Promise<Response>>;
}

interface RouteContext {
  request: Request;
  url: URL;
  env: Env;
  store: Store;
  now: Date;
  email: string;
}

function ttls(env: Env): Pick<RankOptions, "claimTtlMin" | "activityTtlMin"> {
  return {
    claimTtlMin: ttlMinutes(env.CLAIM_TTL_MIN, DEFAULT_CLAIM_TTL_MIN),
    activityTtlMin: ttlMinutes(env.ACTIVITY_TTL_MIN, DEFAULT_ACTIVITY_TTL_MIN),
  };
}

async function handleAccounts(ctx: RouteContext): Promise<Response> {
  const format = validateFormat(ctx.url.searchParams.get("format"));
  const tz = validateTz(ctx.url.searchParams.get("tz"), defaultTz(ctx.env.DEFAULT_TZ));
  const dev = validateOptionalDev(ctx.url.searchParams.get("dev"));

  const accounts = rank(await ctx.store.listAccounts(), { now: ctx.now, dev, ...ttls(ctx.env) });
  const res: AccountsResponse = {
    generated_at: nowStamp(ctx.now),
    tz,
    accounts,
  };
  if (format === "json") return json(res);
  return new Response(formatText(res, ctx.now), { headers: TEXT_HEADERS });
}

async function handleGetAccount(ctx: RouteContext): Promise<Response> {
  const dev = validateOptionalDev(ctx.url.searchParams.get("dev"));
  const stored = await ctx.store.getAccount(ctx.email);
  if (stored === null) return notShared();
  const [account] = rank([stored], { now: ctx.now, dev, ...ttls(ctx.env) });
  // `rank` is only used here for the computed fields; a single account has no
  // meaningful position, so `rank` is dropped from the response.
  const { rank: _rank, ...rest } = account!;
  return json(rest);
}

/**
 * Share: add the email to the allowlist under a team-wide nickname. Never
 * overwrites a valid share record: when the email is already shared the body's
 * nickname is ignored (204, nothing written); renames use PUT .../nickname.
 */
async function handleShare(ctx: RouteContext): Promise<Response> {
  const body = validateShareBody(await readJsonBody(ctx.request));
  // An invalid share record reads as null, so sharing again also repairs it.
  if ((await ctx.store.getShare(ctx.email)) !== null) return noContent();
  if (body.nickname === undefined)
    throw new ValidationError("nickname", "nickname is required to share a new account");
  // Uniqueness is read-then-write (KV has no transactions or conditional
  // puts). Two requests giving different emails the same nickname at the same
  // moment can both pass this check and both succeed; KV listings lag writes
  // by up to ~60 s across Cloudflare locations, which widens that moment for
  // teammates far apart. Accepted: the listing then shows both accounts with
  // that nickname and one of them must be renamed. Same for handleRename.
  const holder = await ctx.store.nicknameHolder(body.nickname, ctx.email);
  if (holder !== null) return nicknameTaken(body.nickname, holder);
  await ctx.store.putShare(ctx.email, {
    added_by: body.added_by,
    added_at: nowStamp(ctx.now),
    nickname: body.nickname,
  });
  return new Response(null, { status: 201 });
}

/**
 * Rename: rewrites `share:<email>` with the new nickname, keeping added_by and
 * added_at. Also how records shared before nicknames existed get their first.
 */
async function handleRename(ctx: RouteContext): Promise<Response> {
  const nickname = validateNicknameBody(await readJsonBody(ctx.request));
  const share = await ctx.store.getShare(ctx.email);
  if (share === null) return notShared();
  if (share.nickname === nickname) return noContent();
  // Same read-then-write uniqueness race as handleShare.
  const holder = await ctx.store.nicknameHolder(nickname, ctx.email);
  if (holder !== null) return nicknameTaken(nickname, holder);
  // The one read-modify-write in the layout. Two renames of the same email:
  // the last write wins. A rename racing an unshare of the same email can put
  // the share key back after unshare deleted it (old added_by/added_at, new
  // nickname, no usage or claims): the account reappears and has to be
  // unshared again. Both need two people acting on one account at once.
  await ctx.store.putShare(ctx.email, { ...share, nickname });
  return noContent();
}

/** Unshare: the share key, the usage key and every claim and exhausted key go together. */
async function handleUnshare(ctx: RouteContext): Promise<Response> {
  // Deleted unconditionally rather than after a read: a lagging KV read must
  // never leave an email shared after the user asked to remove it.
  await ctx.store.unshare(ctx.email);
  return noContent();
}

/** Minimum age before a usage report re-stamps its reporter's claim. */
const CLAIM_REFRESH_MIN = 60;

async function handlePutUsage(ctx: RouteContext): Promise<Response> {
  const body = validateUsageBody(await readJsonBody(ctx.request));
  const report = { ...body, collected_at: clampToNow(body.collected_at, ctx.now) };
  const [share, current, claimAt] = await Promise.all([
    ctx.store.getShare(ctx.email),
    ctx.store.getUsage(ctx.email),
    ctx.store.claimAt(ctx.email, report.reporter),
  ]);
  if (share === null) return notShared();
  // A machine with a lagging clock must not clobber a newer snapshot. Dropping
  // the write silently (204) keeps the PUT idempotent; the CLI has nothing to
  // do about it either way. Equal timestamps still write (last writer wins).
  // The stored side is clamped too, so nothing stored can sit in the future.
  // The reporter's claim is not refreshed either: nothing is written.
  if (current !== null &&
      Date.parse(clampToNow(current.collected_at, ctx.now)) > Date.parse(report.collected_at))
    return noContent();
  await Promise.all([
    ctx.store.putUsage(ctx.email, report),
    // Reporting proves the reporter's session is alive, so its own claim key
    // (and only that one) is re-stamped; a reporter without a claim gets none.
    // Only claims older than CLAIM_REFRESH_MIN are re-stamped: refreshing on
    // every report would double KV writes and push a five-person team past
    // the free tier, while the claim TTL (12 h) only needs an hourly refresh.
    // Not atomic with the existence check: if this holder's own DELETE claim
    // lands in between, its claim is re-created and lives until the next
    // release or the claim TTL. No other holder can be affected.
    claimAt !== null && ctx.now.getTime() - Date.parse(claimAt) >= CLAIM_REFRESH_MIN * 60_000
      ? ctx.store.putClaim(ctx.email, report.reporter, nowStamp(ctx.now))
      : undefined,
  ]);
  return noContent();
}

async function handlePutClaim(ctx: RouteContext): Promise<Response> {
  const claim = validateClaimBody(await readJsonBody(ctx.request), ctx.now);
  if ((await ctx.store.getShare(ctx.email)) === null) return notShared();
  await ctx.store.putClaim(ctx.email, claim, claim.at);
  return noContent();
}

async function handleDeleteClaim(ctx: RouteContext): Promise<Response> {
  const holder = validateHolderQuery(ctx.url.searchParams);
  // Unconditional and without a share check: a read first could see a stale
  // "absent" (KV caches misses) and leave the holder claimed until the TTL.
  await ctx.store.deleteClaim(ctx.email, holder);
  return noContent();
}

/**
 * Exhausted: Claude Code refused this reporter a request for a limit (the
 * CLI's StopFailure hook, matcher `rate_limit`). Writes only the reporter's
 * own `exhausted:` key; the usage record is read, never written, to fill in an
 * unknown reset (see src/exhausted.ts for how the reset is resolved and how
 * long the record counts).
 */
async function handlePutExhausted(ctx: RouteContext): Promise<Response> {
  const body = validateExhaustedBody(await readJsonBody(ctx.request));
  const [share, usage] = await Promise.all([
    ctx.store.getShare(ctx.email),
    ctx.store.getUsage(ctx.email),
  ]);
  if (share === null) return notShared();
  const resets_at = resolveExhaustedResetsAt(body.window, body.resets_at, usage, ctx.now);
  await ctx.store.putExhausted(
    ctx.email,
    body.reporter,
    { window: body.window, resets_at, at: nowStamp(ctx.now) },
    exhaustedTtlSec(body.window, resets_at, ctx.now),
  );
  return noContent();
}

/**
 * Every route but /healthz needs the token (Bearer; `?token=` on GET only).
 *
 *   GET    /accounts                    ranked listing (?format, ?tz, ?dev)
 *   GET    /accounts/:email             one account (?dev); 404 not shared
 *   PUT    /accounts/:email             share {added_by, nickname}: 201 new, 204 existed, 409 nickname taken
 *   DELETE /accounts/:email             unshare (idempotent)
 *   PUT    /accounts/:email/nickname    rename {nickname}: 204, 404 not shared, 409 taken
 *   PUT    /accounts/:email/usage       usage report {session, week, collected_at, reporter}: 204, 404 not shared
 *   PUT    /accounts/:email/claim       claim {dev, machine_id}: 204, 404 not shared
 *   DELETE /accounts/:email/claim       release ?dev=&machine_id= (idempotent, no share check)
 *   PUT    /accounts/:email/exhausted   refusal {window, resets_at?, reporter}: 204, 404 not shared
 *
 * Bodies are validated before the share check, so a bad body is a 400 even
 * for an email that is not shared.
 */
const ROUTES: Route[] = [
  { segments: 1, methods: { GET: handleAccounts } },
  { segments: 2, methods: { GET: handleGetAccount, PUT: handleShare, DELETE: handleUnshare } },
  { segments: 3, tail: "nickname", methods: { PUT: handleRename } },
  { segments: 3, tail: "usage", methods: { PUT: handlePutUsage } },
  { segments: 3, tail: "claim", methods: { PUT: handlePutClaim, DELETE: handleDeleteClaim } },
  { segments: 3, tail: "exhausted", methods: { PUT: handlePutExhausted } },
];

async function route(request: Request, url: URL, env: Env): Promise<Response> {
  const segments = url.pathname.split("/").filter((s) => s.length > 0);

  if (segments.length === 1 && segments[0] === "healthz") {
    if (request.method !== "GET")
      return methodNotAllowed(["GET"]);
    return new Response("ok", { headers: TEXT_HEADERS });
  }

  if (!isAuthorized(request, url, env))
    return errorResponse("unauthorized", 401);

  if (segments[0] !== "accounts") return errorResponse("not found", 404);

  const match = ROUTES.find(
    (r) =>
      r.segments === segments.length &&
      (r.tail === undefined || r.tail === segments[2]),
  );
  if (match === undefined) return errorResponse("not found", 404);

  const handler = match.methods[request.method];
  if (handler === undefined) return methodNotAllowed(Object.keys(match.methods));

  const email = segments.length >= 2 ? validateEmail(decodePathEmail(segments[1]!)) : "";
  const store = new Store(env.USAGE, { claimTtlSec: claimTtlSec(ttls(env).claimTtlMin) });
  return handler({ request, url, env, store, now: new Date(), email });
}

function decodePathEmail(raw: string): string {
  try {
    return decodeURIComponent(raw);
  } catch {
    throw new ValidationError("email", "email is not a valid address");
  }
}

function methodNotAllowed(allowed: string[]): Response {
  return new Response(JSON.stringify({ error: "method not allowed" }), {
    status: 405,
    headers: { ...JSON_HEADERS, allow: allowed.join(", ") },
  });
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    try {
      return await route(request, new URL(request.url), env);
    } catch (err) {
      if (err instanceof ValidationError)
        return json({ error: err.message, field: err.field }, 400);
      // Never surface stack traces or KV internals to clients.
      console.error("unhandled error", err);
      return errorResponse("internal error", 500);
    }
  },
} satisfies ExportedHandler<Env>;
