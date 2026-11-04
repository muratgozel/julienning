import { describe, expect, it } from "vitest";
import { rank } from "../src/rank";
import type { Claim, ExhaustedReport, ProbeRecord, StoredAccount } from "../src/types";

const NOW = new Date("2026-09-16T12:00:00Z");
const BASE = { now: NOW, claimTtlMin: 720, activityTtlMin: 15 };
const DAY_MIN = 24 * 60;
const WEEK_MIN = 7 * DAY_MIN;

function at(minutesFromNow: number): string {
  return new Date(NOW.getTime() + minutesFromNow * 60_000).toISOString().replace(/\.000Z$/, "Z");
}

function account(email: string, extra: Partial<StoredAccount> = {}): StoredAccount {
  return { email, claims: [], ...extra };
}

function holder(dev: string, agoMin: number, machine_id = "aabbccddeeff"): Claim {
  return { dev, machine_id, at: at(-agoMin) };
}

/** A refusal of the week, the only window. */
function refusal(
  resetsInMin: number | null,
  opts: { agoMin?: number; dev?: string; machine_id?: string } = {},
): ExhaustedReport {
  return {
    dev: opts.dev ?? "zoe",
    machine_id: opts.machine_id ?? "aabbccddeeff",
    window: "week",
    resets_at: resetsInMin === null ? null : at(resetsInMin),
    at: at(-(opts.agoMin ?? 10)),
  };
}

function reported(
  email: string,
  week: number,
  opts: { weekResets?: number; dev?: string; agoMin?: number } = {},
): StoredAccount {
  return {
    email,
    week: { used: week, resets_at: at(opts.weekResets ?? 4000) },
    collected_at: at(-(opts.agoMin ?? 120)),
    reporter: { dev: opts.dev ?? "zoe", machine_id: "aabbccddeeff" },
    claims: [],
  };
}

describe("effective usage", () => {
  it("zeroes the week once its reset has passed and flags reset_passed", () => {
    const [a] = rank([reported("a@x.io", 80, { weekResets: -10 })], BASE);
    expect(a!.week).toEqual({
      used: 80,
      effective: 0,
      resets_at: at(-10),
      reset_passed: true,
    });
    const [b] = rank([reported("b@x.io", 60)], BASE);
    expect(b!.week!.reset_passed).toBe(false);
    expect(b!.week!.effective).toBe(60);
  });

  it("treats a reset exactly at now as passed", () => {
    const [a] = rank([reported("a@x.io", 80, { weekResets: 0 })], BASE);
    expect(a!.week!.reset_passed).toBe(true);
    expect(a!.week!.effective).toBe(0);
  });

  it("omits the week when it was never reported", () => {
    const [a] = rank([account("a@x.io")], BASE);
    expect(a!.week).toBeUndefined();
    expect(a!.state).toBe("free");
    expect(a!.busy_by).toEqual([]);
    expect(a!.claims).toEqual([]);
  });

  it("never lists a session window, also for an account that still carries one", () => {
    // Claude dropped the five-hour window; nothing may bring it back into the listing.
    const legacy = { ...reported("a@x.io", 10), session: { used: 99, resets_at: at(60) } } as StoredAccount;
    const [a] = rank([legacy], BASE);
    expect(a).not.toHaveProperty("session");
    expect(a!.week!.used).toBe(10);
  });
});

describe("state and busy_by", () => {
  it("marks fresh activity by another dev as in_use", () => {
    const [a] = rank([reported("a@x.io", 10, { dev: "ali", agoMin: 5 })], {
      ...BASE,
      dev: "murat",
    });
    expect(a!.state).toBe("in_use");
    expect(a!.busy_by).toEqual(["ali"]);
  });

  it("expires in_use once activity is older than ACTIVITY_TTL_MIN", () => {
    const acc = reported("a@x.io", 10, { dev: "ali", agoMin: 14 });
    expect(rank([acc], { ...BASE, dev: "murat" })[0]!.state).toBe("in_use");
    const stale = reported("a@x.io", 10, { dev: "ali", agoMin: 16 });
    const [a] = rank([stale], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("free");
    expect(a!.busy_by).toEqual([]);
  });

  it("does not treat the querying dev's own activity as busy", () => {
    const acc = reported("a@x.io", 10, { dev: "murat", agoMin: 2 });
    expect(rank([acc], { ...BASE, dev: "murat" })[0]!.state).toBe("free");
    expect(rank([acc], { ...BASE, dev: "ali" })[0]!.state).toBe("in_use");
  });

  it("counts every active dev as other when the query has no dev", () => {
    const acc = reported("a@x.io", 10, { dev: "murat", agoMin: 2 });
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
      ...reported("a@x.io", 10, { dev: "ece", agoMin: 1 }),
      claims: [holder("ali", 5), holder("ece", 5)],
    };
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a!.state).toBe("in_use");
    expect(a!.busy_by).toEqual(["ali", "ece"]);
  });

  it("is claimed, not in_use, when only the querying dev is reporting", () => {
    const acc: StoredAccount = {
      ...reported("a@x.io", 10, { dev: "murat", agoMin: 1 }),
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
      { ...reported("busy@x.io", 0), claims: [holder("ali", 5)] },
      account("unknown@x.io"),
      reported("d@x.io", 20),
      reported("c@x.io", 10, { weekResets: 5000 }),
      reported("b@x.io", 10),
      reported("a@x.io", 10),
    ];
    const ranked = rank(accounts, { ...BASE, dev: "murat" });
    expect(ranked.map((r) => r.email)).toEqual([
      "a@x.io",
      "b@x.io",
      "c@x.io",
      "d@x.io",
      "unknown@x.io",
      "busy@x.io",
    ]);
    expect(ranked.map((r) => r.rank)).toEqual([1, 2, 3, 4, 5, 6]);
  });

  it("breaks a tie on week % with the earlier week reset, whose quota expires first", () => {
    const ranked = rank(
      [
        reported("late@x.io", 30, { weekResets: 6000 }),
        reported("soon@x.io", 30, { weekResets: 1000 }),
        reported("lower@x.io", 29, { weekResets: 9000 }),
      ],
      BASE,
    );
    expect(ranked.map((r) => r.email)).toEqual(["lower@x.io", "soon@x.io", "late@x.io"]);
  });

  it("ranks a passed reset ahead of a low but live one", () => {
    const ranked = rank([reported("live@x.io", 5), reported("spent@x.io", 99, { weekResets: -1 })], BASE);
    expect(ranked[0]!.email).toBe("spent@x.io");
  });

  it("puts claim-only records in the unknown-usage band", () => {
    const ranked = rank(
      [
        account("claimonly@x.io", { claims: [holder("murat", 1)] }),
        reported("busy@x.io", 99, { dev: "ali", agoMin: 1 }),
        reported("used@x.io", 99),
      ],
      { ...BASE, dev: "murat" },
    );
    expect(ranked.map((r) => r.email)).toEqual(["used@x.io", "claimonly@x.io", "busy@x.io"]);
  });

  it("is stable across both perspectives of the same data", () => {
    const accounts = [reported("a@x.io", 50, { dev: "murat", agoMin: 1 }), reported("b@x.io", 90)];
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
  it("sinks an account with its week at 100% below a 99% one", () => {
    const ranked = rank([reported("weekout@x.io", 100), reported("ok@x.io", 99)], BASE);
    expect(ranked.map((r) => r.email)).toEqual(["ok@x.io", "weekout@x.io"]);
    expect(ranked[0]).toMatchObject({ exhausted: false, exhausted_until: null, exhausted_window: null, state: "free" });
    expect(ranked[1]).toMatchObject({
      rank: 2,
      exhausted: true,
      exhausted_until: at(4000),
      exhausted_window: "week",
      state: "exhausted",
    });
  });

  it("treats 99.9% as usable", () => {
    const [a] = rank([reported("a@x.io", 99.9)], BASE);
    expect(a).toMatchObject({ exhausted: false, exhausted_until: null, state: "free" });
  });

  it("is no longer exhausted once the week has reset", () => {
    const passed = rank([reported("a@x.io", 100, { weekResets: -1 })], BASE);
    expect(passed[0]).toMatchObject({ exhausted: false, exhausted_until: null, state: "free" });
    const atNow = rank([reported("a@x.io", 100, { weekResets: 0 })], BASE);
    expect(atNow[0]).toMatchObject({ exhausted: false, exhausted_until: null });
  });

  it("takes precedence over in_use and claimed, keeping busy_by", () => {
    const inUse = rank([reported("a@x.io", 100, { dev: "ali", agoMin: 1 })], { ...BASE, dev: "murat" });
    expect(inUse[0]).toMatchObject({ state: "exhausted", busy_by: ["ali"] });
    const claimed = rank([{ ...reported("a@x.io", 100), claims: [holder("can", 5), holder("ali", 9)] }], {
      ...BASE,
      dev: "murat",
    });
    expect(claimed[0]).toMatchObject({ state: "exhausted", busy_by: ["ali", "can"] });
    expect(claimed[0]!.claims).toHaveLength(2);
  });

  it("orders below busy and unknown accounts, then by exhausted_until, then the usual tiebreakers", () => {
    const accounts: StoredAccount[] = [
      reported("late@x.io", 100, { weekResets: 4000 }),
      { ...reported("soonbusy@x.io", 100, { weekResets: 300 }), claims: [holder("ali", 5)] },
      reported("soon-b@x.io", 100, { weekResets: 300 }),
      reported("soon-a@x.io", 100, { weekResets: 300 }),
      // Refused at 20%: same exhausted_until, lower week %.
      { ...reported("soonlow@x.io", 20, { weekResets: 300 }), exhausted: [refusal(300)] },
      { ...reported("busy@x.io", 90), claims: [holder("ali", 5)] },
      account("unknown@x.io"),
    ];
    const ranked = rank(accounts, { ...BASE, dev: "murat" });
    expect(ranked.map((r) => r.email)).toEqual([
      "unknown@x.io",
      "busy@x.io",
      "soonlow@x.io",
      "soon-a@x.io",
      "soon-b@x.io",
      "soonbusy@x.io",
      "late@x.io",
    ]);
    expect(ranked.map((r) => r.rank)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  it("always sets the exhausted fields, also for never-reported accounts", () => {
    const [a] = rank([account("a@x.io")], BASE);
    expect(a).toHaveProperty("exhausted", false);
    expect(a).toHaveProperty("exhausted_until", null);
    expect(a).toHaveProperty("exhausted_window", null);
    const json = JSON.parse(JSON.stringify(a)) as Record<string, unknown>;
    expect(typeof json.exhausted).toBe("boolean");
    expect(json.exhausted_until).toBeNull();
    const [b] = rank([reported("b@x.io", 100)], BASE);
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
    const acc = { ...reported("a@x.io", 4, { weekResets: 90 }), exhausted: [refusal(90)] };
    expect(exhaustedFields(acc)).toEqual({ state: "exhausted", exhausted: true, until: at(90), window: "week" });
    const noUsage = account("b@x.io", { exhausted: [refusal(3000)] });
    expect(exhaustedFields(noUsage)).toEqual({ state: "exhausted", exhausted: true, until: at(3000), window: "week" });
  });

  it("sets exhausted_window null when usable, and treats a missing list as none", () => {
    const [a] = rank([reported("a@x.io", 4)], BASE);
    expect(a).toHaveProperty("exhausted_window", null);
    expect(exhaustedFields(account("b@x.io", { exhausted: [] }))).toMatchObject({ exhausted: false, window: null });
  });

  it("ignores a record whose reset has passed, exactly at now included", () => {
    for (const resets of [-1, 0])
      expect(exhaustedFields(account("a@x.io", { exhausted: [refusal(resets)] })).exhausted).toBe(false);
  });

  it("counts a record with an unknown reset for one week after it was reported", () => {
    const live = account("a@x.io", { exhausted: [refusal(null, { agoMin: WEEK_MIN - 1 })] });
    expect(exhaustedFields(live)).toEqual({ state: "exhausted", exhausted: true, until: null, window: "week" });
    const lapsed = account("a@x.io", { exhausted: [refusal(null, { agoMin: WEEK_MIN })] });
    expect(exhaustedFields(lapsed).exhausted).toBe(false);
  });

  it("is not cleared by usage under 100%, even with a slightly later reset", () => {
    for (const usageReset of [90, 91, 90 + 59]) {
      const acc = { ...reported("a@x.io", 30, { weekResets: usageReset }), exhausted: [refusal(90)] };
      expect(exhaustedFields(acc), String(usageReset)).toMatchObject({ exhausted: true, until: at(90) });
    }
  });

  it("is superseded once the week resets later than the refused one can", () => {
    // A new week: it resets a week after the refused one.
    const known = { ...reported("a@x.io", 30, { weekResets: 90 + WEEK_MIN }), exhausted: [refusal(90)] };
    expect(exhaustedFields(known)).toMatchObject({ state: "free", exhausted: false, until: null, window: null });
    // Unknown reset reported 3 days ago: the refused week resets within 4 days;
    // a week resetting in 5 days started after the refusal.
    const unknown = {
      ...reported("a@x.io", 30, { weekResets: 5 * DAY_MIN }),
      exhausted: [refusal(null, { agoMin: 3 * DAY_MIN })],
    };
    expect(exhaustedFields(unknown).exhausted).toBe(false);
    // ...while one resetting in 3.5 days may still be the refused week.
    const same = {
      ...reported("a@x.io", 30, { weekResets: 3.5 * DAY_MIN }),
      exhausted: [refusal(null, { agoMin: 3 * DAY_MIN })],
    };
    expect(exhaustedFields(same).exhausted).toBe(true);
  });

  it("waits on the latest known reset among the week at 100% and records", () => {
    const recordLater = { ...reported("a@x.io", 100, { weekResets: 3000 }), exhausted: [refusal(3030)] };
    expect(exhaustedFields(recordLater)).toMatchObject({ until: at(3030), window: "week" });
    const windowLater = { ...reported("a@x.io", 100, { weekResets: 3030 }), exhausted: [refusal(3000)] };
    expect(exhaustedFields(windowLater)).toMatchObject({ until: at(3030), window: "week" });
    const twoRecords = account("a@x.io", {
      exhausted: [refusal(120, { dev: "ali" }), refusal(200, { dev: "can" })],
    });
    expect(exhaustedFields(twoRecords)).toMatchObject({ until: at(200), window: "week" });
  });

  it("uses a known reset over an unknown one, and names the week when none is known", () => {
    const mixed = account("a@x.io", { exhausted: [refusal(null, { dev: "ali" }), refusal(100)] });
    expect(exhaustedFields(mixed)).toMatchObject({ until: at(100), window: "week" });
    const withWindow = { ...reported("a@x.io", 100, { weekResets: 120 }), exhausted: [refusal(null)] };
    expect(exhaustedFields(withWindow)).toMatchObject({ until: at(120), window: "week" });
    const unknown = account("a@x.io", { exhausted: [refusal(null, { dev: "ali" }), refusal(null)] });
    expect(exhaustedFields(unknown)).toMatchObject({ exhausted: true, until: null, window: "week" });
  });

  it("keeps busy_by and the usual states' signals on a refused account", () => {
    const acc = {
      ...reported("a@x.io", 10, { dev: "ali", agoMin: 1, weekResets: 60 }),
      claims: [holder("can", 5)],
      exhausted: [refusal(60)],
    };
    const [a] = rank([acc], { ...BASE, dev: "murat" });
    expect(a).toMatchObject({ state: "exhausted", busy_by: ["ali", "can"] });
  });

  it("sorts refused accounts last, by exhausted_until, unknown after known", () => {
    const ranked = rank(
      [
        account("unknown-reset@x.io", { exhausted: [refusal(null)] }),
        account("late@x.io", { exhausted: [refusal(3000)] }),
        { ...reported("soon@x.io", 0, { weekResets: 30 }), exhausted: [refusal(30)] },
        reported("window@x.io", 100, { weekResets: 60 }),
        { ...reported("busy@x.io", 90), claims: [holder("ali", 5)] },
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

describe("probe records", () => {
  function probed(
    _email: string,
    week: number,
    opts: { agoMin?: number; weekResets?: number | null; dev?: string } = {},
  ): ProbeRecord {
    return {
      week: { used: week, resets_at: opts.weekResets === null ? null : at(opts.weekResets ?? 4000) },
      collected_at: at(-(opts.agoMin ?? 5)),
      reporter: { dev: opts.dev ?? "murat", machine_id: "3fa9c2d1e07b" },
    };
  }

  it("takes the numbers from the newer record, the probe on a tie", () => {
    const newerProbe = { ...reported("a@x.io", 80, { agoMin: 30 }), probe: probed("a@x.io", 20, { agoMin: 5 }) };
    const newerStatus = { ...reported("b@x.io", 80, { agoMin: 5 }), probe: probed("b@x.io", 20, { agoMin: 30 }) };
    const tie = { ...reported("c@x.io", 80, { agoMin: 5 }), probe: probed("c@x.io", 20, { agoMin: 5 }) };
    const byMail = new Map(rank([newerProbe, newerStatus, tie], BASE).map((a) => [a.email, a]));
    expect(byMail.get("a@x.io")).toMatchObject({ source: "probe", week: { used: 20 }, numbers_at: at(-5), collected_at: at(-30) });
    expect(byMail.get("b@x.io")).toMatchObject({ source: "statusline", week: { used: 80 }, numbers_at: at(-5), collected_at: at(-5) });
    expect(byMail.get("c@x.io")).toMatchObject({ source: "probe", week: { used: 20 } });
  });

  it("ranks on the probe's numbers", () => {
    const stale = { ...reported("a@x.io", 5, { agoMin: 600 }), probe: probed("a@x.io", 90) };
    const other = reported("b@x.io", 50, { agoMin: 600 });
    expect(rank([stale, other], BASE).map((a) => a.email)).toEqual(["b@x.io", "a@x.io"]);
  });

  it("works with only a probe: known usage, no activity, no collected_at/reporter", () => {
    const [a] = rank([account("a@x.io", { probe: probed("a@x.io", 4, { dev: "ali" }) })], { ...BASE, dev: "murat" });
    expect(a).toMatchObject({ source: "probe", state: "free", busy_by: [], numbers_at: at(-5) });
    expect(a!.collected_at).toBeUndefined();
    expect(a!.reporter).toBeUndefined();
    const unknown = account("b@x.io");
    expect(rank([unknown, account("a@x.io", { probe: probed("a@x.io", 4) })], BASE).map((x) => x.email))
      .toEqual(["a@x.io", "b@x.io"]);
  });

  it("keeps activity, collected_at and reporter from the status line under a newer probe", () => {
    const a = { ...reported("a@x.io", 50, { agoMin: 3, dev: "ali" }), probe: probed("a@x.io", 60, { agoMin: 1 }) };
    const [r] = rank([a], { ...BASE, dev: "murat" });
    expect(r).toMatchObject({
      state: "in_use", busy_by: ["ali"], source: "probe", week: { used: 60 }, numbers_at: at(-1),
      collected_at: at(-3), reporter: { dev: "ali", machine_id: "aabbccddeeff" },
    });
  });

  it("lets a later probe saying the week is not running end a refusal", () => {
    const later = account("a@x.io", {
      probe: probed("a@x.io", 0, { weekResets: null, agoMin: 2 }),
      exhausted: [refusal(null, { agoMin: 30 })],
    });
    expect(rank([later], BASE)[0]!.exhausted).toBe(false);
  });

  it("treats a week that is not running as 0% with no reset: never passed, never blocking, sorted after real resets", () => {
    const idle = account("a@x.io", { probe: probed("a@x.io", 0, { weekResets: null }) });
    const used = account("b@x.io", { probe: probed("b@x.io", 0, { weekResets: 60 }) });
    const ranked = rank([idle, used], BASE);
    expect(ranked.map((a) => a.email)).toEqual(["b@x.io", "a@x.io"]);
    expect(ranked[1]!.week).toEqual({ used: 0, effective: 0, resets_at: null, reset_passed: false });
    expect(ranked[1]!.exhausted).toBe(false);
  });

  it("is not ended by a week that was not running before the refusal", () => {
    const a = account("a@x.io", {
      probe: probed("a@x.io", 0, { weekResets: null, agoMin: 30 }),
      exhausted: [refusal(600, { agoMin: 10 })],
    });
    expect(rank([a], BASE)[0]).toMatchObject({ exhausted: true, exhausted_window: "week" });
  });
});
