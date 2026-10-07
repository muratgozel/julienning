import { env } from "cloudflare:test";
import { afterEach, beforeEach, describe, expect, it, vi, type MockInstance } from "vitest";
import {
  A,
  ALI,
  B,
  byEmail,
  C,
  call,
  CAN,
  ControlledKV,
  getJson,
  keyNames,
  legacyShare,
  listJson,
  MURAT,
  NOT_SHARED,
  putClaim,
  putUsage,
  rawKeys,
  rename,
  seed,
  share,
  shareKey,
  shareOf,
  unshare,
  viaKv,
} from "./helpers";

const TAKEN = (nickname: string, by: string) => ({ error: "nickname is taken", nickname, by });

async function expectTaken(res: Response, nickname: string, by: string): Promise<void> {
  expect(res.status).toBe(409);
  expect(res.headers.get("content-type")).toContain("application/json");
  expect(await res.json()).toEqual(TAKEN(nickname, by));
}

async function expectField(res: Response, field: string): Promise<string> {
  expect(res.status).toBe(400);
  const body = (await res.json()) as { error: string; field: string };
  expect(body.field).toBe(field);
  return body.error;
}

/** NICK and ACCOUNT cells of each text-table row, in order. */
async function textNicks(): Promise<string[][]> {
  const text = await (await call("/accounts")).text();
  return text
    .split("\n")
    .slice(1)
    .filter((l) => /^\d/.test(l))
    .map((l) => l.split(/\s{2,}/).slice(1, 3));
}

describe("sharing with a nickname", () => {
  it("creates with the nickname lowercased into share:<email> (201)", async () => {
    expect((await share(A, MURAT, "Alpha")).status).toBe(201);
    const record = await shareOf(A);
    expect(record).toEqual({ added_by: MURAT, added_at: record.added_at, nickname: "alpha" });
    expect((await getJson(A)).nickname).toBe("alpha");
    expect(byEmail(await listJson(), A).nickname).toBe("alpha");
  });

  it("requires a nickname to share a new email, writing nothing", async () => {
    for (const nickname of [null, ""]) {
      const res = await call(`/accounts/${A}`, {
        method: "PUT",
        body: JSON.stringify(nickname === null ? { added_by: MURAT } : { added_by: MURAT, nickname }),
      });
      expect(await expectField(res, "nickname")).toBe("nickname is required to share a new account");
    }
    const explicitNull = await call(`/accounts/${A}`, {
      method: "PUT",
      body: JSON.stringify({ added_by: MURAT, nickname: null }),
    });
    await expectField(explicitNull, "nickname");
    expect(await keyNames()).toEqual([]);
  });

  it("rejects an invalid nickname with 400 on nickname, writing nothing", async () => {
    for (const bad of ["-alpha", "al pha", "x".repeat(33), "alpha@x.io", 42, "ünal"]) {
      const res = await call(`/accounts/${A}`, {
        method: "PUT",
        body: JSON.stringify({ added_by: MURAT, nickname: bad }),
      });
      await expectField(res, "nickname");
    }
    const at = await call(`/accounts/${A}`, {
      method: "PUT",
      body: JSON.stringify({ added_by: MURAT, nickname: "claude1@sixtynine.agency" }),
    });
    expect(await expectField(at, "nickname")).toMatch(/must not contain '@'/);
    expect(await keyNames()).toEqual([]);
  });

  it("validates a present nickname even when the email is already shared", async () => {
    await share(A, MURAT, "alpha");
    await expectField(await share(A, ALI, "not valid"), "nickname");
    expect((await shareOf(A)).nickname).toBe("alpha");
  });

  it("answers 409 with the holding email when another email has the nickname", async () => {
    expect((await share(A, MURAT, "alpha")).status).toBe(201);
    await expectTaken(await share(B, ALI, "alpha"), "alpha", A);
    expect(await keyNames()).toEqual([shareKey(A)]);
  });

  it("compares nicknames case-insensitively", async () => {
    expect((await share(A, MURAT, "alpha")).status).toBe(201);
    await expectTaken(await share(B, ALI, "ALPHA"), "alpha", A);
    expect((await share(B, ALI, "Beta")).status).toBe(201);
    await expectTaken(await rename(B, "Alpha"), "alpha", A);
    expect((await shareOf(B)).nickname).toBe("beta");
  });

  it("ignores the body's nickname when the email is already shared (204, nothing written)", async () => {
    await share(A, MURAT, "alpha");
    await share(B, MURAT, "beta");
    await putUsage(A);
    await putClaim(A, ALI);
    const before = await rawKeys();

    // A different nickname, one another email holds, and none at all.
    for (const nickname of ["other", "beta", null]) {
      const res = await share(A, ALI, nickname);
      expect(res.status).toBe(204);
      expect(await res.text()).toBe("");
    }
    expect(await rawKeys()).toEqual(before);
    expect((await shareOf(A)).nickname).toBe("alpha");
  });

  it("frees the nickname on unshare", async () => {
    await share(A, MURAT, "alpha");
    expect((await unshare(A)).status).toBe(204);
    expect((await share(B, ALI, "alpha")).status).toBe(201);
    expect((await share(A, MURAT, "alpha")).status).toBe(409);
  });

  it("is not blocked by an invalid share record holding the nickname", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await seed(shareKey(B), { added_by: "murat", added_at: "yesterday", nickname: "alpha" });
      expect((await share(A, MURAT, "alpha")).status).toBe(201);
      expect(warn).toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }
  });
});

describe("renaming", () => {
  it("404s with the exact not-shared body when the email is not shared", async () => {
    const res = await rename(A, "alpha");
    expect(res.status).toBe(404);
    expect(await res.json()).toEqual(NOT_SHARED);
    expect(await keyNames()).toEqual([]);
  });

  it("validates the body", async () => {
    await share(A, MURAT, "alpha");
    const cases: [string | undefined, string][] = [
      [undefined, "body"],
      ["{nope", "body"],
      ["[]", "body"],
      ["{}", "nickname"],
      [JSON.stringify({ nickname: "" }), "nickname"],
      [JSON.stringify({ nickname: "a b" }), "nickname"],
      [JSON.stringify({ nickname: "x@y.io" }), "nickname"],
    ];
    for (const [body, field] of cases) {
      const res = await call(`/accounts/${A}/nickname`, {
        method: "PUT",
        ...(body !== undefined ? { body } : {}),
      });
      await expectField(res, field);
    }
    expect((await shareOf(A)).nickname).toBe("alpha");
  });

  it("renames with 204, keeping added_by and added_at, and frees the old name", async () => {
    await share(A, ALI, "alpha");
    const before = await shareOf(A);

    const res = await rename(A, "Gamma");
    expect(res.status).toBe(204);
    expect(await res.text()).toBe("");
    expect(await shareOf(A)).toEqual({ ...before, nickname: "gamma" });
    expect((await getJson(A)).nickname).toBe("gamma");
    expect((await share(B, MURAT, "alpha")).status).toBe(201);
  });

  it("leaves usage and claims alone", async () => {
    await share(A, MURAT, "alpha");
    await putUsage(A);
    await putClaim(A, CAN);
    const others = (await rawKeys()).filter((k) => k.name !== shareKey(A));
    expect((await rename(A, "beta")).status).toBe(204);
    expect((await rawKeys()).filter((k) => k.name !== shareKey(A))).toEqual(others);
  });

  it("answers 409 with the holder and writes nothing when another email has it", async () => {
    await share(A, MURAT, "alpha");
    await share(B, MURAT, "beta");
    const before = await rawKeys();
    await expectTaken(await rename(B, "alpha"), "alpha", A);
    expect(await rawKeys()).toEqual(before);
  });

  it("answers 204 without writing or listing when the nickname is unchanged", async () => {
    await share(A, MURAT, "alpha");
    for (const same of ["alpha", "ALPHA"]) {
      const kv = new ControlledKV(env.USAGE);
      const res = await rename(A, same, viaKv(kv));
      expect(res.status).toBe(204);
      expect(kv.calls).toEqual([{ op: "getWithMetadata", key: shareKey(A) }]);
    }
  });

  it("gives a record from before nicknames its first one", async () => {
    const legacy = legacyShare(ALI);
    await seed(shareKey(A), legacy);
    expect((await rename(A, "alpha")).status).toBe(204);
    expect(await shareOf(A)).toEqual({ ...legacy, nickname: "alpha" });
  });

  it("replaces a stored nickname that is unusable", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await seed(shareKey(A), { ...legacyShare(), nickname: "Not Valid!" });
      expect((await getJson(A)).nickname).toBeNull();
      expect(warn.mock.calls.flat().join("\n")).toContain(`invalid nickname in KV key ${shareKey(A)}`);
      expect((await rename(A, "alpha")).status).toBe(204);
      expect((await getJson(A)).nickname).toBe("alpha");
    } finally {
      warn.mockRestore();
    }
  });
});

describe("records from before nicknames", () => {
  let warn: MockInstance<typeof console.warn>;

  beforeEach(async () => {
    warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    await seed(shareKey(A), legacyShare(MURAT));
    await share(B, ALI, "beta");
  });

  afterEach(() => {
    // A missing nickname is normal, not something to warn about.
    expect(warn).not.toHaveBeenCalled();
    warn.mockRestore();
  });

  it("stay shared and read as nickname null in JSON", async () => {
    const listed = byEmail(await listJson(), A);
    expect(listed.nickname).toBeNull();
    expect(listed).toHaveProperty("nickname", null);
    expect((await getJson(A)).nickname).toBeNull();
    await putUsage(A);
    expect(byEmail(await listJson(), A).session!.used).toBe(50);
  });

  it("show '-' in the text NICK column", async () => {
    expect((await textNicks()).sort()).toEqual([
      ["-", A],
      ["beta", B],
    ]);
  });

  it("keep their missing nickname when shared again (204)", async () => {
    const before = await shareOf(A);
    expect((await share(A, CAN, "alpha")).status).toBe(204);
    expect(await shareOf(A)).toEqual(before);
    expect(before).not.toHaveProperty("nickname");
    expect((await getJson(A)).nickname).toBeNull();
  });

  it("never collide with anything", async () => {
    await seed(shareKey(C), legacyShare(CAN));
    expect((await rename(A, "alpha")).status).toBe(204);
    expect((await rename(C, "gamma")).status).toBe(204);
  });
});

describe("nickname in responses", () => {
  beforeEach(async () => {
    await share(A, MURAT, "alpha");
    await putUsage(A);
  });

  it("puts nickname right after email in the list and the single account", async () => {
    const listed = (await listJson()).accounts[0]!;
    expect(Object.keys(listed).slice(0, 3)).toEqual(["rank", "email", "nickname"]);
    expect(Object.keys(await getJson(A)).slice(0, 2)).toEqual(["email", "nickname"]);
  });

  it("shows the nickname in the NICK column between # and ACCOUNT", async () => {
    const text = await (await call("/accounts")).text();
    const [header, row] = text.split("\n");
    expect(header).toMatch(/^#\s+NICK\s+ACCOUNT\s/);
    expect(row).toMatch(new RegExp(`^1\\s+alpha\\s+${A}\\s`));
  });
});
