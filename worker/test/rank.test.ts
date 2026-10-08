import { describe, expect, it } from "vitest";
import { rank } from "../src/rank";
import type { Claim, ExhaustedReport, StoredAccount, WindowName } from "../src/types";

const NOW = new Date("2026-09-16T12:00:00Z");
const BASE = { now: NOW, claimTtlMin: 720, activityTtlMin: 15 };

function at(minutesFromNow: number): string {
  return new Date(NOW.getTime() + minutesFromNow * 60_000).toISOString().replace(/\.000Z$/, "Z");
}

function account(email: string, extra: Partial<StoredAccount> = {}): StoredAccount {
  return { email, claims: [], ...extra };
}

function holder(dev: string, agoMin: number, machine_id = "aabbccddeeff"): Claim {
  return { dev, machine_id, at: at(-agoMin) };
}

function refusal(
  window: WindowName,
  resetsInMin: number | null,
  opts: { agoMin?: number; dev?: string; machine_id?: string } = {},
): ExhaustedReport {
  return {
    dev: opts.dev ?? "zoe",
    machine_id: opts.machine_id ?? "aabbccddeeff",
    window,
    resets_at: resetsInMin === null ? null : at(resetsInMin),
    at: at(-(opts.agoMin ?? 10)),
  };
}

function reported(
  email: string,
  session: number,
  week: number,
  opts: { sessionResets?: number; weekResets?: number; dev?: string; agoMin?: number } = {},
): StoredAccount {
  return {
    email,
    session: { used: session, resets_at: at(opts.sessionResets ?? 300) },
    week: { used: week, resets_at: at(opts.weekResets ?? 4000) },
    collected_at: at(-(opts.agoMin ?? 120)),
    reporter: { dev: opts.dev ?? "zoe", machine_id: "aabbccddeeff" },
    claims: [],
  };
}

describe("effective usage", () => {
  it("zeroes a window whose reset has passed and flags reset_passed", () => {
    const [a] = rank([reported("a@x.io", 80, 60, { sessionResets: -10 })], BASE);
    expect(a!.session).toEqual({
      used: 80,
      effective: 0,
      resets_at: at(-10),
      reset_passed: true,
    });
    expect(a!.week!.reset_passed).toBe(false);
    expect(a!.week!.effective).toBe(60);
  });

  it("treats a reset exactly at now as passed", () => {
    const [a] = rank([reported("a@x.io", 80, 60, { sessionResets: 0 })], BASE);
    expect(a!.session!.reset_passed).toBe(true);
    expect(a!.session!.effective).toBe(0);
  });

  it("omits windows that were never reported", () => {
    const [a] = rank([account("a@x.io")], BASE);
    expect(a!.session).toBeUndefined();
    expect(a!.week).toBeUndefined();
    expect(a!.state).toBe("free");
    expect(a!.busy_by).toEqual([]);
    expect(a!.claims).toEqual([]);
  });
});

describe("state and busy_by", () => {
  it("marks fresh activity by another dev as in_use", () => {
    const [a] = rank([reported("a@x.io", 10, 10, { dev: "ali", agoMin: 5 })], {
      ...BASE,
      dev: "murat",
    });
    expect(a!.state).toBe("in_use");
    expect(a!.busy_by).toEqual(["ali"]);
  });

  it("expires in_use once activity is older than ACTIVITY_TTL_MIN", () => {
    const acc = reported("a@x.io", 10, 10, { dev: "ali", agoMin: 14 });
    expect(rank([acc], { ...BASE, dev: "murat" })[0]!.state).toBe("in_use");
    const stale = reported("a@x.io", 10, 10, { dev: "ali", agoMin: 16 });
    const [a] = rank([stale], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("free");
    expect(a!.busy_by).toEqual([]);
  });

  it("does not treat the querying dev's own activity as busy", () => {
    const acc = reported("a@x.io", 10, 10, { dev: "murat", agoMin: 2 });
    expect(rank([acc], { ...BASE, dev: "murat" })[0]!.state).toBe("free");
    expect(rank([acc], { ...BASE, dev: "ali" })[0]!.state).toBe("in_use");
  });

  it("counts every active dev as other when the query has no dev", () => {
    const acc = reported("a@x.io", 10, 10, { dev: "murat", agoMin: 2 });
    const [a] = rank([acc], BASE);
    expect(a!.state).toBe("in_use");
    expect(a!.busy_by).toEqual(["murat"]);
  });

  it("marks a fresh claim by another dev as claimed", () => {
    const acc = account("a@x.io", { claims: [holder("ali", 30)] });
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("claimed");
    expect(a!.busy_by).toEqual(["ali"]);
    expect(a!.claims).toEqual([holder("ali", 30)]);
  });

  it("ignores a holder older than CLAIM_TTL_MIN and drops it from claims", () => {
    const fresh = account("a@x.io", { claims: [holder("ali", 719)] });
    expect(rank([fresh], { ...BASE, dev: "murat" })[0]!.state).toBe("claimed");

    const acc = account("a@x.io", { claims: [holder("ali", 721), holder("can", 5)] });
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("claimed");
    expect(a!.busy_by).toEqual(["can"]);
    expect(a!.claims).toEqual([holder("can", 5)]);

    const expired = account("a@x.io", { claims: [holder("ali", 721)] });
    const [b] = rank([expired], { ...BASE, dev: "murat" });
    expect(b!.state).toBe("free");
    expect(b!.busy_by).toEqual([]);
    expect(b!.claims).toEqual([]);
  });

  it("lists every other holder once, sorted, and keeps the querying dev's own claims", () => {
    const acc = account("a@x.io", {
      claims: [
        holder("zoe", 3),
        holder("murat", 2),
        holder("ali", 1, "111111111111"),
        holder("ali", 4, "222222222222"),
      ],
    });
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("claimed");
    expect(a!.busy_by).toEqual(["ali", "zoe"]);
    expect(a!.claims).toHaveLength(4);
  });

  it("is free when only the querying dev holds it, from any machine", () => {
    const acc = account("a@x.io", {
      claims: [holder("murat", 2, "111111111111"), holder("murat", 9, "222222222222")],
    });
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("free");
    expect(a!.busy_by).toEqual([]);
  });

  it("gives each dev the other one in busy_by when two devs hold it", () => {
    const acc = account("a@x.io", { claims: [holder("murat", 2), holder("ali", 1)] });
    expect(rank([acc], { ...BASE, dev: "murat" })[0]!.busy_by).toEqual(["ali"]);
    expect(rank([acc], { ...BASE, dev: "ali" })[0]!.busy_by).toEqual(["murat"]);
    expect(rank([acc], { ...BASE, dev: "can" })[0]!.busy_by).toEqual(["ali", "murat"]);
    expect(rank([acc], BASE)[0]!.busy_by).toEqual(["ali", "murat"]);
  });

  it("prefers in_use over claimed and merges the reporter into busy_by", () => {
    const acc: StoredAccount = {
      ...reported("a@x.io", 10, 10, { dev: "ece", agoMin: 1 }),
      claims: [holder("ali", 5), holder("ece", 5)],
    };
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("in_use");
    expect(a!.busy_by).toEqual(["ali", "ece"]);
  });

  it("is claimed, not in_use, when only the querying dev is reporting", () => {
    const acc: StoredAccount = {
      ...reported("a@x.io", 10, 10, { dev: "murat", agoMin: 1 }),
      claims: [holder("ali", 5)],
    };
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("claimed");
    expect(a!.busy_by).toEqual(["ali"]);
  });

  it("passes added_by and added_at through", () => {
    const acc = account("a@x.io", {
      added_by: { dev: "murat", machine_id: "3fa9c2d1e07b" },
      added_at: at(-60),
    });
    const [a] = rank([acc], BASE);
    expect(a!.added_by).toEqual({ dev: "murat", machine_id: "3fa9c2d1e07b" });
    expect(a!.added_at).toBe(at(-60));
  });
});

describe("ordering", () => {
  it("applies every tiebreaker in order", () => {
    const accounts: StoredAccount[] = [
      // Busy: lowest usage of all, still last.
      { ...reported("busy@x.io", 0, 0), claims: [holder("ali", 5)] },
      account("unknown@x.io"),
      reported("d@x.io", 20, 10),
      reported("c@x.io", 10, 20),
      reported("e@x.io", 10, 10, { sessionResets: 360 }),
      reported("b@x.io", 10, 10),
      reported("a@x.io", 10, 10),
    ];
    const ranked = rank(accounts, { ...BASE, dev: "murat" });
    expect(ranked.map((r) => r.email)).toEqual([
      "a@x.io",
      "b@x.io",
      "e@x.io",
      "c@x.io",
      "d@x.io",
      "unknown@x.io",
      "busy@x.io",
    ]);
    expect(ranked.map((r) => r.rank)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  it("ranks a passed reset ahead of a low but live one", () => {
    const ranked = rank(
      [reported("live@x.io", 5, 5), reported("spent@x.io", 99, 99, { sessionResets: -1, weekResets: -1 })],
      BASE,
    );
    expect(ranked[0]!.email).toBe("spent@x.io");
  });

  it("puts claim-only records in the unknown-usage band", () => {
    const ranked = rank(
      [
        account("claimonly@x.io", { claims: [holder("murat", 1)] }),
        reported("busy@x.io", 99, 99, { dev: "ali", agoMin: 1 }),
        reported("used@x.io", 99, 99),
      ],
      { ...BASE, dev: "murat" },
    );
    expect(ranked.map((r) => r.email)).toEqual(["used@x.io", "claimonly@x.io", "busy@x.io"]);
  });

  it("is stable across both perspectives of the same data", () => {
    const accounts = [
      reported("a@x.io", 50, 50, { dev: "murat", agoMin: 1 }),
      reported("b@x.io", 90, 90),
    ];
    expect(rank(accounts, { ...BASE, dev: "murat" }).map((r) => r.email)).toEqual([
      "a@x.io",
      "b@x.io",
    ]);
    expect(rank(accounts, { ...BASE, dev: "ali" }).map((r) => r.email)).toEqual([
      "b@x.io",
      "a@x.io",
    ]);
  });
});

describe("exhaustion", () => {
  it("sinks an account with its week at 100% below a 4%/13% one, despite 0% session", () => {
    const ranked = rank([reported("weekout@x.io", 0, 100), reported("ok@x.io", 4, 13)], BASE);
    expect(ranked.map((r) => r.email)).toEqual(["ok@x.io", "weekout@x.io"]);
    expect(ranked[0]).toMatchObject({ exhausted: false, exhausted_until: null, state: "free" });
    expect(ranked[1]).toMatchObject({
      rank: 2,
      exhausted: true,
      exhausted_until: at(4000),
      state: "exhausted",
    });
  });

  it("sinks an account with its session at 100% below a 99%/99% one", () => {
    const ranked = rank([reported("sessout@x.io", 100, 0), reported("ok@x.io", 99, 99)], BASE);
    expect(ranked.map((r) => r.email)).toEqual(["ok@x.io", "sessout@x.io"]);
    expect(ranked[1]).toMatchObject({ exhausted: true, exhausted_until: at(300), state: "exhausted" });
  });

  it("treats 99.9% as usable", () => {
    const [a] = rank([reported("a@x.io", 99.9, 99.9)], BASE);
    expect(a).toMatchObject({ exhausted: false, exhausted_until: null, state: "free" });
  });

  it("waits on the later reset when both windows are at 100%", () => {
    const weekLater = rank([reported("a@x.io", 100, 100, { sessionResets: 300, weekResets: 4000 })], BASE);
    expect(weekLater[0]!.exhausted_until).toBe(at(4000));
    // A week window about to roll over can reset before the session does.
    const sessionLater = rank([reported("a@x.io", 100, 100, { sessionResets: 300, weekResets: 60 })], BASE);
    expect(sessionLater[0]!.exhausted_until).toBe(at(300));
  });

  it("takes exhausted_until only from the windows at 100%", () => {
    const weekOnly = rank([reported("a@x.io", 99, 100, { sessionResets: 9000, weekResets: 4000 })], BASE);
    expect(weekOnly[0]!.exhausted_until).toBe(at(4000));
    const sessionOnly = rank([reported("a@x.io", 100, 99, { sessionResets: 300, weekResets: 4000 })], BASE);
    expect(sessionOnly[0]!.exhausted_until).toBe(at(300));
  });

  it("is no longer exhausted once the window at 100% has reset", () => {
    const passed = rank([reported("a@x.io", 100, 100, { sessionResets: -1, weekResets: -1 })], BASE);
    expect(passed[0]).toMatchObject({ exhausted: false, exhausted_until: null, state: "free" });
    const atNow = rank([reported("a@x.io", 100, 10, { sessionResets: 0 })], BASE);
    expect(atNow[0]).toMatchObject({ exhausted: false, exhausted_until: null });
  });

  it("stays exhausted on the window that has not reset yet", () => {
    const weekLeft = rank([reported("a@x.io", 100, 100, { sessionResets: -1, weekResets: 4000 })], BASE);
    expect(weekLeft[0]).toMatchObject({ exhausted: true, exhausted_until: at(4000) });
    const sessionLeft = rank([reported("a@x.io", 100, 100, { sessionResets: 300, weekResets: -1 })], BASE);
    expect(sessionLeft[0]).toMatchObject({ exhausted: true, exhausted_until: at(300) });
  });

  it("takes precedence over in_use and claimed, keeping busy_by", () => {
    const inUse = rank([reported("a@x.io", 100, 10, { dev: "ali", agoMin: 1 })], { ...BASE, dev: "murat" });
    expect(inUse[0]).toMatchObject({ state: "exhausted", busy_by: ["ali"] });
    const claimed = rank([{ ...reported("a@x.io", 10, 100), claims: [holder("can", 5), holder("ali", 9)] }], {
      ...BASE,
      dev: "murat",
    });
    expect(claimed[0]).toMatchObject({ state: "exhausted", busy_by: ["ali", "can"] });
    expect(claimed[0]!.claims).toHaveLength(2);
  });

  it("orders below busy and unknown accounts, then by exhausted_until, then the usual tiebreakers", () => {
    const accounts: StoredAccount[] = [
      reported("late@x.io", 0, 100, { weekResets: 4000 }),
      { ...reported("soonbusy@x.io", 100, 0, { sessionResets: 300 }), claims: [holder("ali", 5)] },
      reported("soonhigh@x.io", 100, 50, { sessionResets: 300 }),
      reported("soonlow-b@x.io", 100, 20, { sessionResets: 300 }),
      reported("soonlow-a@x.io", 100, 20, { sessionResets: 300 }),
      { ...reported("busy@x.io", 90, 90), claims: [holder("ali", 5)] },
      account("unknown@x.io"),
    ];
    const ranked = rank(accounts, { ...BASE, dev: "murat" });
    expect(ranked.map((r) => r.email)).toEqual([
      "unknown@x.io",
      "busy@x.io",
      "soonlow-a@x.io",
      "soonlow-b@x.io",
      "soonhigh@x.io",
      "soonbusy@x.io",
      "late@x.io",
    ]);
    expect(ranked.map((r) => r.rank)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  it("orders exhausted accounts with the same exhausted_until by effective session, then session reset", () => {
    const ranked = rank(
      [
        reported("s30@x.io", 30, 100, { weekResets: 4000 }),
        reported("s10w100@x.io", 10, 100, { weekResets: 4000 }),
        reported("s10w100b@x.io", 10, 100, { weekResets: 4000, sessionResets: 200 }),
      ],
      BASE,
    );
    // Same session %, same week %: the earlier session reset wins.
    expect(ranked.map((r) => r.email)).toEqual(["s10w100b@x.io", "s10w100@x.io", "s30@x.io"]);
  });

  it("always sets both fields, also for never-reported accounts", () => {
    const [a] = rank([account("a@x.io")], BASE);
    expect(a).toHaveProperty("exhausted", false);
    expect(a).toHaveProperty("exhausted_until", null);
    const json = JSON.parse(JSON.stringify(a)) as Record<string, unknown>;
    expect(typeof json.exhausted).toBe("boolean");
    expect(json.exhausted_until).toBeNull();
    const [b] = rank([reported("b@x.io", 100, 0)], BASE);
    expect(typeof (JSON.parse(JSON.stringify(b)) as Record<string, unknown>).exhausted_until).toBe("string");
  });
});

describe("exhaustion from refusal records", () => {
  const exhaustedFields = (a: StoredAccount) => {
    const [r] = rank([a], BASE);
    return { state: r!.state, exhausted: r!.exhausted, until: r!.exhausted_until, window: r!.exhausted_window };
  };

  it("marks an account exhausted whatever its usage says", () => {
    // Claude Code keeps reporting the refused window's stale numbers.
    const acc = { ...reported("a@x.io", 3, 4, { sessionResets: 90 }), exhausted: [refusal("session", 90)] };
    expect(exhaustedFields(acc)).toEqual({ state: "exhausted", exhausted: true, until: at(90), window: "session" });
    const noUsage = account("b@x.io", { exhausted: [refusal("week", 3000)] });
    expect(exhaustedFields(noUsage)).toEqual({ state: "exhausted", exhausted: true, until: at(3000), window: "week" });
  });

  it("sets exhausted_window null when usable, and treats a missing list as none", () => {
    const [a] = rank([reported("a@x.io", 3, 4)], BASE);
    expect(a).toHaveProperty("exhausted_window", null);
    expect(exhaustedFields(account("b@x.io", { exhausted: [] }))).toMatchObject({ exhausted: false, window: null });
  });

  it("ignores a record whose reset has passed, exactly at now included", () => {
    for (const resets of [-1, 0])
      expect(exhaustedFields(account("a@x.io", { exhausted: [refusal("week", resets)] })).exhausted).toBe(false);
  });

  it("counts a record with an unknown reset for one window length after it was reported", () => {
    const live = account("a@x.io", { exhausted: [refusal("session", null, { agoMin: 299 })] });
    expect(exhaustedFields(live)).toEqual({ state: "exhausted", exhausted: true, until: null, window: "session" });
    const lapsed = account("a@x.io", { exhausted: [refusal("session", null, { agoMin: 300 })] });
    expect(exhaustedFields(lapsed).exhausted).toBe(false);
    const week = account("a@x.io", { exhausted: [refusal("week", null, { agoMin: 6 * 24 * 60 })] });
    expect(exhaustedFields(week)).toMatchObject({ exhausted: true, window: "week" });
  });

  it("is not cleared by usage under 100% on the same window, even with a slightly later reset", () => {
    for (const usageReset of [90, 91, 90 + 59]) {
      const acc = { ...reported("a@x.io", 12, 30, { sessionResets: usageReset }), exhausted: [refusal("session", 90)] };
      expect(exhaustedFields(acc), String(usageReset)).toMatchObject({ exhausted: true, until: at(90) });
    }
  });

  it("is superseded once the same-named usage window resets later than the refused one can", () => {
    // A new session window: it resets at least most of five hours after the refused one.
    const known = { ...reported("a@x.io", 12, 30, { sessionResets: 90 + 300 }), exhausted: [refusal("session", 90)] };
    expect(exhaustedFields(known)).toMatchObject({ state: "free", exhausted: false, until: null, window: null });
    // Unknown reset reported 3 h ago: the refused window resets within 2 h; a
    // usage window resetting in 4 h started after the refusal.
    const unknown = {
      ...reported("a@x.io", 12, 30, { sessionResets: 240 }),
      exhausted: [refusal("session", null, { agoMin: 180 })],
    };
    expect(exhaustedFields(unknown).exhausted).toBe(false);
    // ...while one resetting in 2.5 h may still be the refused window.
    const same = {
      ...reported("a@x.io", 12, 30, { sessionResets: 150 }),
      exhausted: [refusal("session", null, { agoMin: 180 })],
    };
    expect(exhaustedFields(same).exhausted).toBe(true);
  });

  it("is not superseded by the other window", () => {
    const acc = {
      ...reported("a@x.io", 12, 30, { sessionResets: 60, weekResets: 9000 }),
      exhausted: [refusal("session", 90)],
    };
    expect(exhaustedFields(acc)).toMatchObject({ exhausted: true, window: "session" });
  });

  it("waits on the latest known reset among windows at 100% and records", () => {
    const windowLater = {
      ...reported("a@x.io", 10, 100, { sessionResets: 90, weekResets: 4000 }),
      exhausted: [refusal("session", 90)],
    };
    expect(exhaustedFields(windowLater)).toMatchObject({ until: at(4000), window: "week" });
    const recordLater = {
      ...reported("a@x.io", 100, 10, { sessionResets: 120, weekResets: 3000 }),
      exhausted: [refusal("week", 3000)],
    };
    expect(exhaustedFields(recordLater)).toMatchObject({ until: at(3000), window: "week" });
    const twoRecords = account("a@x.io", {
      exhausted: [refusal("session", 120, { dev: "ali" }), refusal("session", 200, { dev: "can" })],
    });
    expect(exhaustedFields(twoRecords)).toMatchObject({ until: at(200), window: "session" });
  });

  it("prefers week on a tie", () => {
    const tie = {
      ...reported("a@x.io", 100, 10, { sessionResets: 120, weekResets: 120 }),
      exhausted: [refusal("week", 120)],
    };
    expect(exhaustedFields(tie)).toMatchObject({ until: at(120), window: "week" });
    const tieRecords = account("a@x.io", { exhausted: [refusal("week", 120, { dev: "ali" }), refusal("session", 120)] });
    expect(exhaustedFields(tieRecords)).toMatchObject({ until: at(120), window: "week" });
  });

  it("uses a known reset over an unknown one, and names week when none is known", () => {
    const mixed = account("a@x.io", { exhausted: [refusal("week", null, { dev: "ali" }), refusal("session", 100)] });
    expect(exhaustedFields(mixed)).toMatchObject({ until: at(100), window: "session" });
    const withWindow = { ...reported("a@x.io", 100, 10, { sessionResets: 120 }), exhausted: [refusal("week", null)] };
    expect(exhaustedFields(withWindow)).toMatchObject({ until: at(120), window: "session" });
    const both = account("a@x.io", { exhausted: [refusal("session", null, { dev: "ali" }), refusal("week", null)] });
    expect(exhaustedFields(both)).toMatchObject({ exhausted: true, until: null, window: "week" });
    const sessionOnly = account("a@x.io", { exhausted: [refusal("session", null)] });
    expect(exhaustedFields(sessionOnly)).toMatchObject({ until: null, window: "session" });
  });

  it("keeps busy_by and the usual states' signals on a refused account", () => {
    const acc = {
      ...reported("a@x.io", 10, 10, { dev: "ali", agoMin: 1, sessionResets: 60 }),
      claims: [holder("can", 5)],
      exhausted: [refusal("session", 60)],
    };
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a).toMatchObject({ state: "exhausted", busy_by: ["ali", "can"] });
  });

  it("sorts refused accounts last, by exhausted_until, unknown after known", () => {
    const ranked = rank(
      [
        account("unknown-reset@x.io", { exhausted: [refusal("week", null)] }),
        account("late@x.io", { exhausted: [refusal("week", 3000)] }),
        { ...reported("soon@x.io", 0, 0, { sessionResets: 30 }), exhausted: [refusal("session", 30)] },
        reported("window@x.io", 100, 0, { sessionResets: 60 }),
        { ...reported("busy@x.io", 90, 90), claims: [holder("ali", 5)] },
        account("never@x.io"),
      ],
      { ...BASE, dev: "murat" },
    );
    expect(ranked.map((r) => r.email)).toEqual([
      "never@x.io",
      "busy@x.io",
      "soon@x.io",
      "window@x.io",
      "late@x.io",
      "unknown-reset@x.io",
    ]);
  });
});
