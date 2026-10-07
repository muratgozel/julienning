import { describe, expect, it, vi } from "vitest";
import {
  canonicalEmail,
  canonicalHolder,
  clampToNow,
  defaultTz,
  MAX_BODY_BYTES,
  parseClaimRecord,
  parseShareRecord,
  parseUsageRecord,
  readJsonBody,
  storedNicknameDropped,
  ttlMinutes,
  validateClaimBody,
  validateEmail,
  validateFormat,
  validateHolderQuery,
  validateNickname,
  validateNicknameBody,
  validateShareBody,
  validateTimestamp,
  validateTz,
  validateUsageBody,
  ValidationError,
} from "../src/validate";

const NOW = new Date("2026-09-16T12:00:00Z");

function goodUsage(): unknown {
  return {
    session: { used: 50, resets_at: "2026-09-16T14:30:00Z" },
    week: { used: 28, resets_at: "2026-09-22T18:00:00Z" },
    collected_at: "2026-09-16T12:18:57Z",
    reporter: { dev: "murat", machine_id: "3fa9c2d1e07b" },
  };
}

function field(fn: () => unknown): string {
  try {
    fn();
  } catch (err) {
    if (err instanceof ValidationError) return err.field;
    throw err;
  }
  throw new Error("expected a ValidationError");
}

describe("email", () => {
  it("lowercases a valid address", () => {
    expect(validateEmail("Claude1@SixtyNine.Agency")).toBe("claude1@sixtynine.agency");
  });

  it("rejects malformed addresses and anything over 254 chars", () => {
    for (const bad of ["", "nope", "a@b", "a@b.c", "a b@c.io", "@c.io", "a@.io"])
      expect(field(() => validateEmail(bad))).toBe("email");
    expect(field(() => validateEmail(`${"a".repeat(250)}@x.io`))).toBe("email");
  });
});

describe("dev, machine_id, timestamps", () => {
  it("accepts the documented dev shape and rejects the rest", () => {
    expect(validateUsageBody(goodUsage()).reporter.dev).toBe("murat");
    for (const bad of ["", "Murat", "-murat", "a".repeat(33), "mu rat"])
      expect(
        field(() => validateUsageBody({ ...(goodUsage() as object), reporter: { dev: bad, machine_id: "3fa9c2d1e07b" } })),
      ).toBe("reporter.dev");
  });

  it("requires 12 hex chars for machine_id and normalizes case", () => {
    expect(validateClaimBody({ dev: "murat", machine_id: "3FA9C2D1E07B" }, NOW).machine_id).toBe(
      "3fa9c2d1e07b",
    );
    for (const bad of ["3fa9c2d1e07", "3fa9c2d1e07bb", "zzzzzzzzzzzz", 42])
      expect(field(() => validateClaimBody({ dev: "murat", machine_id: bad }, NOW))).toBe(
        "machine_id",
      );
  });

  it("normalizes RFC 3339 input to UTC", () => {
    expect(validateTimestamp("2026-09-16T17:30:00+03:00", "collected_at")).toBe(
      "2026-09-16T14:30:00Z",
    );
    expect(validateTimestamp("2026-09-16t14:30:00z", "collected_at")).toBe("2026-09-16T14:30:00Z");
    expect(validateTimestamp("2026-09-16T14:30:00.250Z", "collected_at")).toBe(
      "2026-09-16T14:30:00.250Z",
    );
  });

  it("rejects non-RFC-3339 timestamps", () => {
    for (const bad of ["2026-09-16 14:30:00", "2026-09-16T14:30:00", "not-a-date", 1758030000])
      expect(field(() => validateTimestamp(bad, "collected_at"))).toBe("collected_at");
  });

  it("sets claim.at from the injected now", () => {
    expect(validateClaimBody({ dev: "murat", machine_id: "3fa9c2d1e07b" }, NOW).at).toBe(
      "2026-09-16T12:00:00Z",
    );
  });
});

describe("used", () => {
  it("rounds to one decimal", () => {
    const body = goodUsage() as Record<string, unknown>;
    body.session = { used: 49.96, resets_at: "2026-09-16T14:30:00Z" };
    expect(validateUsageBody(body).session.used).toBe(50);
    body.session = { used: 49.94, resets_at: "2026-09-16T14:30:00Z" };
    expect(validateUsageBody(body).session.used).toBe(49.9);
  });

  it("rejects out-of-range and non-finite values", () => {
    for (const bad of [-1, 101, Number.NaN, Number.POSITIVE_INFINITY, "50", null]) {
      const body = goodUsage() as Record<string, unknown>;
      body.session = { used: bad, resets_at: "2026-09-16T14:30:00Z" };
      expect(field(() => validateUsageBody(body))).toBe("session.used");
    }
  });
});

describe("body handling", () => {
  function put(body: string, headers: Record<string, string> = {}): Request {
    return new Request("https://x/", { method: "PUT", body, headers });
  }

  it("rejects bodies over 4 KB", async () => {
    const big = JSON.stringify({ pad: "x".repeat(MAX_BODY_BYTES) });
    await expect(readJsonBody(put(big))).rejects.toMatchObject({ field: "body" });
  });

  it("rejects an oversized content-length before reading", async () => {
    const req = put("{}", { "content-length": String(MAX_BODY_BYTES + 1) });
    await expect(readJsonBody(req)).rejects.toMatchObject({ field: "body" });
  });

  it("aborts an oversized chunked body instead of buffering it", async () => {
    const CHUNK_BYTES = 1024;
    const CHUNKS = 4096; // 4 MB if the body were ever fully buffered.
    let pulled = 0;
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (pulled >= CHUNKS) {
          controller.close();
          return;
        }
        pulled++;
        controller.enqueue(new Uint8Array(CHUNK_BYTES).fill(0x78));
      },
    });
    // A streamed body carries no content-length, so only the running total can
    // stop it.
    const req = new Request("https://x/", {
      method: "PUT",
      body: stream,
      duplex: "half",
    } as RequestInit & { duplex: "half" });

    await expect(readJsonBody(req)).rejects.toMatchObject({ field: "body" });
    expect(pulled * CHUNK_BYTES).toBeLessThan(CHUNKS * CHUNK_BYTES / 4);
  });

  it("rejects empty and malformed JSON", async () => {
    await expect(readJsonBody(put(""))).rejects.toMatchObject({ field: "body" });
    await expect(readJsonBody(put("{not json"))).rejects.toMatchObject({ field: "body" });
  });

  it("rejects non-object bodies", () => {
    expect(field(() => validateUsageBody([1, 2]))).toBe("body");
    expect(field(() => validateUsageBody("x"))).toBe("body");
  });
});

describe("query params", () => {
  it("defaults format to text and rejects anything else", () => {
    expect(validateFormat(null)).toBe("text");
    expect(validateFormat("json")).toBe("json");
    expect(field(() => validateFormat("yaml"))).toBe("format");
  });

  it("falls back to the default tz and rejects invalid zones", () => {
    expect(validateTz(null, "Europe/Istanbul")).toBe("Europe/Istanbul");
    expect(validateTz("America/New_York", "Europe/Istanbul")).toBe("America/New_York");
    expect(field(() => validateTz("Mars/Olympus", "Europe/Istanbul"))).toBe("tz");
  });
});

describe("DEFAULT_TZ", () => {
  it("defaults to UTC when the var is missing or blank", () => {
    expect(defaultTz(undefined)).toBe("UTC");
    expect(defaultTz("")).toBe("UTC");
    expect(defaultTz("  ")).toBe("UTC");
  });

  it("uses a valid configured zone", () => {
    expect(defaultTz("Europe/Istanbul")).toBe("Europe/Istanbul");
  });

  it("logs and falls back to UTC for an invalid configured zone", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      expect(defaultTz("Mars/Olympus")).toBe("UTC");
      expect(warn).toHaveBeenCalledOnce();
    } finally {
      warn.mockRestore();
    }
  });
});

describe("share body", () => {
  it("returns added_by with machine_id lowercased", () => {
    expect(validateShareBody({ added_by: { dev: "murat", machine_id: "3FA9C2D1E07B" } })).toEqual({
      added_by: { dev: "murat", machine_id: "3fa9c2d1e07b" },
    });
  });

  it("names the failing field", () => {
    expect(field(() => validateShareBody([]))).toBe("body");
    expect(field(() => validateShareBody({}))).toBe("added_by");
    expect(field(() => validateShareBody({ added_by: "murat" }))).toBe("added_by");
    expect(field(() => validateShareBody({ added_by: { dev: "Murat", machine_id: "3fa9c2d1e07b" } }))).toBe(
      "added_by.dev",
    );
    expect(field(() => validateShareBody({ added_by: { dev: "murat" } }))).toBe("added_by.machine_id");
  });
});

describe("nickname", () => {
  it("lowercases and accepts the documented shape", () => {
    expect(validateNickname("Alpha")).toBe("alpha");
    for (const ok of ["a", "0", "claude1", "a.b_c-d", "x".repeat(32), "9lives"])
      expect(validateNickname(ok)).toBe(ok);
  });

  it("says a missing or empty nickname is required", () => {
    for (const missing of [undefined, null, ""])
      expect(() => validateNickname(missing)).toThrow("nickname is required");
  });

  it("rejects non-strings, an '@' and anything off the pattern, naming the field", () => {
    expect(() => validateNickname(42)).toThrow("nickname must be a string");
    expect(() => validateNickname("claude1@x.io")).toThrow(/must not contain '@'/);
    expect(() => validateNickname("@")).toThrow(/must not contain '@'/);
    for (const bad of ["-a", ".a", "_a", "a b", " a", "a ", "x".repeat(33), "ünal", "a/b", "a:b"])
      expect(field(() => validateNickname(bad)), bad).toBe("nickname");
  });

  it("does not let non-ASCII letters fold into ASCII ones", () => {
    // U+212A KELVIN SIGN lowercases to "k"; U+0130 to "i" plus a combining dot.
    for (const lookalike of ["\u212Aate", "\u0130van"])
      expect(field(() => validateNickname(lookalike))).toBe("nickname");
  });

  it("is optional in the share body, but validated when present", () => {
    const added_by = { dev: "murat", machine_id: "3fa9c2d1e07b" };
    for (const absent of [{ added_by }, { added_by, nickname: null }, { added_by, nickname: "" }])
      expect(validateShareBody(absent)).toEqual({ added_by });
    expect(validateShareBody({ added_by, nickname: "Alpha" })).toEqual({ added_by, nickname: "alpha" });
    expect(field(() => validateShareBody({ added_by, nickname: "a b" }))).toBe("nickname");
    expect(field(() => validateShareBody({ added_by, nickname: 7 }))).toBe("nickname");
  });

  it("is required in the rename body", () => {
    expect(validateNicknameBody({ nickname: "Beta" })).toBe("beta");
    expect(field(() => validateNicknameBody([]))).toBe("body");
    expect(field(() => validateNicknameBody({}))).toBe("nickname");
    expect(field(() => validateNicknameBody({ nickname: "" }))).toBe("nickname");
    expect(field(() => validateNicknameBody({ nickname: "x@y.io" }))).toBe("nickname");
  });
});

describe("claim holder query", () => {
  const q = (s: string) => new URLSearchParams(s);

  it("requires both dev and machine_id", () => {
    expect(validateHolderQuery(q("dev=murat&machine_id=3FA9C2D1E07B"))).toEqual({
      dev: "murat",
      machine_id: "3fa9c2d1e07b",
    });
    for (const [query, name] of [
      ["", "dev"],
      ["machine_id=3fa9c2d1e07b", "dev"],
      ["dev=&machine_id=3fa9c2d1e07b", "dev"],
      ["dev=murat", "machine_id"],
      ["dev=murat&machine_id=", "machine_id"],
      ["dev=Murat&machine_id=3fa9c2d1e07b", "dev"],
      ["dev=murat&machine_id=nope", "machine_id"],
    ] as const)
      expect(field(() => validateHolderQuery(q(query)))).toBe(name);
  });

  it("says a missing param is required", () => {
    expect(() => validateHolderQuery(q("dev=murat"))).toThrow("machine_id query parameter is required");
  });
});

describe("stored metadata", () => {
  const share = { added_by: { dev: "murat", machine_id: "3fa9c2d1e07b" }, added_at: "2026-09-16T10:00:00Z" };

  it("reads a share record, normalizing and dropping unknown keys", () => {
    expect(
      parseShareRecord({
        added_by: { dev: "murat", machine_id: "3FA9C2D1E07B", extra: 1 },
        added_at: "2026-09-16T13:00:00+03:00",
        secret: "nope",
      }),
    ).toEqual(share);
  });

  it("reads a share record's nickname, lowercased", () => {
    const record = parseShareRecord({ ...share, nickname: "Alpha" });
    expect(record).toEqual({ ...share, nickname: "alpha" });
    expect(storedNicknameDropped({ ...share, nickname: "Alpha" }, record!)).toBe(false);
  });

  it("reads a record from before nicknames without one", () => {
    const record = parseShareRecord(share);
    expect(record).toEqual(share);
    expect(record).not.toHaveProperty("nickname");
    expect(storedNicknameDropped(share, record!)).toBe(false);
  });

  it("keeps the record but drops an unusable nickname, and says so", () => {
    for (const bad of ["Bad Nick!", "a@b.io", 42, null, ""]) {
      const raw = { ...share, nickname: bad };
      const record = parseShareRecord(raw);
      expect(record, String(bad)).toEqual(share);
      expect(storedNicknameDropped(raw, record!)).toBe(true);
    }
  });

  it("rejects a share record missing or garbling either field", () => {
    for (const bad of [
      null,
      "x",
      [],
      {},
      { added_by: share.added_by },
      { added_at: share.added_at },
      { ...share, added_by: { dev: "NO", machine_id: "3fa9c2d1e07b" } },
      { ...share, added_at: "yesterday" },
    ])
      expect(parseShareRecord(bad)).toBeNull();
  });

  it("reads a usage record and rejects a partial or out-of-range one", () => {
    expect(parseUsageRecord({ ...(goodUsage() as object), extra: true })).toEqual(goodUsage());
    const { week: _week, ...partial } = goodUsage() as Record<string, unknown>;
    expect(parseUsageRecord(partial)).toBeNull();
    expect(parseUsageRecord({ ...(goodUsage() as object), session: { used: 500, resets_at: "2026-09-16T14:30:00Z" } })).toBeNull();
    expect(parseUsageRecord(null)).toBeNull();
  });

  it("reads a claim record and rejects a bad stamp", () => {
    expect(parseClaimRecord({ at: "2026-09-16T11:00:00Z", dev: "ignored" })).toEqual({ at: "2026-09-16T11:00:00Z" });
    for (const bad of [null, {}, { at: "yesterday" }, { at: 1758020400 }]) expect(parseClaimRecord(bad)).toBeNull();
  });
});

describe("canonical key parts", () => {
  it("accepts only emails in the lowercased form the Worker writes", () => {
    expect(canonicalEmail("claude1@sixtynine.agency")).toBe("claude1@sixtynine.agency");
    for (const bad of ["Claude1@sixtynine.agency", " a@x.io", "nope", ""]) expect(canonicalEmail(bad)).toBeNull();
  });

  it("accepts only valid holders with a lowercased machine id", () => {
    expect(canonicalHolder("murat", "3fa9c2d1e07b")).toEqual({ dev: "murat", machine_id: "3fa9c2d1e07b" });
    expect(canonicalHolder("murat", "3FA9C2D1E07B")).toBeNull();
    expect(canonicalHolder("Murat", "3fa9c2d1e07b")).toBeNull();
    expect(canonicalHolder("murat", "xyz")).toBeNull();
  });
});

describe("clampToNow", () => {
  it("caps a future timestamp at the Worker's clock, in whole seconds", () => {
    const now = new Date("2026-09-16T12:00:00.750Z");
    expect(clampToNow("2026-09-16T12:05:00Z", now)).toBe("2026-09-16T12:00:00Z");
    expect(clampToNow("2026-09-16T12:00:00.900Z", now)).toBe("2026-09-16T12:00:00Z");
  });

  it("leaves past and present timestamps alone", () => {
    const now = new Date("2026-09-16T12:00:00Z");
    expect(clampToNow("2026-09-16T11:59:00Z", now)).toBe("2026-09-16T11:59:00Z");
    expect(clampToNow("2026-09-16T12:00:00Z", now)).toBe("2026-09-16T12:00:00Z");
  });
});

describe("ttlMinutes", () => {
  it("falls back when the var is missing or unusable", () => {
    expect(ttlMinutes("45", 60)).toBe(45);
    expect(ttlMinutes(undefined, 60)).toBe(60);
    expect(ttlMinutes("nope", 60)).toBe(60);
    expect(ttlMinutes("0", 60)).toBe(60);
  });
});
