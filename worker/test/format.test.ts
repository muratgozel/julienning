import { describe, expect, it } from "vitest";
import { formatResetTime, formatStamp, formatText } from "../src/format";
import { rank } from "../src/rank";
import type { AccountsResponse, StoredAccount } from "../src/types";

/** 2026-10-31T12:00:00Z sits two days before the US DST change on 2026-11-01. */
const NOW = new Date("2026-10-31T12:00:00Z");

const FIXTURE: StoredAccount[] = [
  {
    email: "claude1@sixtynine.agency",
    nickname: "alpha",
    session: { used: 50, resets_at: "2026-10-31T18:00:00Z" },
    week: { used: 28, resets_at: "2026-11-02T18:00:00Z" },
    collected_at: "2026-10-31T11:58:00Z",
    reporter: { dev: "murat", machine_id: "3fa9c2d1e07b" },
    claims: [],
  },
  {
    email: "claude2@sixtynine.agency",
    session: { used: 91.5, resets_at: "2026-10-31T10:00:00Z" },
    week: { used: 12, resets_at: "2026-12-01T18:00:00Z" },
    collected_at: "2026-10-31T11:20:00Z",
    reporter: { dev: "ali", machine_id: "aabbccddeeff" },
    claims: [{ dev: "ali", machine_id: "aabbccddeeff", at: "2026-10-31T11:57:00Z" }],
  },
];

function render(tz: string, accounts = FIXTURE, dev: string | undefined = "murat"): string {
  const res: AccountsResponse = {
    generated_at: NOW.toISOString(),
    tz,
    accounts: rank(accounts, { now: NOW, dev, claimTtlMin: 720, activityTtlMin: 15 }),
  };
  return formatText(res, NOW);
}

describe("reset time rules", () => {
  it("renders today as HH:MM", () => {
    expect(formatResetTime("2026-10-31T18:00:00Z", "Europe/Istanbul", NOW)).toBe("21:00");
  });

  it("renders the next six days as Day HH:MM", () => {
    expect(formatResetTime("2026-11-02T18:00:00Z", "Europe/Istanbul", NOW)).toBe("Mon 21:00");
    expect(formatResetTime("2026-11-06T18:00:00Z", "Europe/Istanbul", NOW)).toBe("Fri 21:00");
  });

  it("renders anything further out as a full date", () => {
    expect(formatResetTime("2026-11-07T18:00:00Z", "Europe/Istanbul", NOW)).toBe("2026-11-07 21:00");
  });

  it("keeps midnight at 00, not 24", () => {
    // 21:00Z is 00:00 the next day in Istanbul (+03).
    expect(formatResetTime("2026-10-31T21:00:00Z", "Europe/Istanbul", NOW)).toBe("Sun 00:00");
    expect(formatResetTime("2026-10-31T04:00:00Z", "America/New_York", NOW)).toBe("00:00");
  });

  it("uses the tz calendar day, not UTC's", () => {
    // 22:30Z on Oct 31 is already Nov 1 in Istanbul (+03).
    expect(formatResetTime("2026-10-31T22:30:00Z", "Europe/Istanbul", NOW)).toBe("Sun 01:30");
  });
});

describe("DST handling", () => {
  it("keeps Europe/Istanbul at +03 on both sides of the US change", () => {
    // Same wall-clock hour before and after 2026-11-01: Istanbul has no DST.
    expect(formatResetTime("2026-10-31T18:00:00Z", "Europe/Istanbul", NOW)).toBe("21:00");
    expect(formatResetTime("2026-11-02T18:00:00Z", "Europe/Istanbul", NOW)).toBe("Mon 21:00");
    expect(formatStamp(NOW, "Europe/Istanbul")).toBe("2026-10-31 15:00");
  });

  it("shifts America/New_York from EDT to EST across 2026-11-01", () => {
    // 18:00Z is 14:00 EDT on Oct 31 and 13:00 EST on Nov 2.
    expect(formatResetTime("2026-10-31T18:00:00Z", "America/New_York", NOW)).toBe("14:00");
    expect(formatResetTime("2026-11-02T18:00:00Z", "America/New_York", NOW)).toBe("Mon 13:00");
    expect(formatStamp(NOW, "America/New_York")).toBe("2026-10-31 08:00");
  });
});

describe("text table", () => {
  it("aligns columns and renders Europe/Istanbul times", () => {
    expect(render("Europe/Istanbul")).toBe(
      [
        "#  NICK   ACCOUNT                   SESSION      WEEK                    STATE           UPDATED",
        "1  alpha  claude1@sixtynine.agency  50% → 21:00  28% → Mon 21:00         free            2m ago",
        "2  -      claude2@sixtynine.agency  0% → reset   12% → 2026-12-01 21:00  claimed by ali  40m ago",
        "",
        "generated 2026-10-31 15:00 (Europe/Istanbul)",
        "",
      ].join("\n"),
    );
  });

  it("aligns columns and renders America/New_York times across the DST boundary", () => {
    expect(render("America/New_York")).toBe(
      [
        "#  NICK   ACCOUNT                   SESSION      WEEK                    STATE           UPDATED",
        "1  alpha  claude1@sixtynine.agency  50% → 14:00  28% → Mon 13:00         free            2m ago",
        "2  -      claude2@sixtynine.agency  0% → reset   12% → 2026-12-01 13:00  claimed by ali  40m ago",
        "",
        "generated 2026-10-31 08:00 (America/New_York)",
        "",
      ].join("\n"),
    );
  });

  it("keeps every column at the same offset on every row", () => {
    const lines = render("Europe/Istanbul").split("\n");
    const header = lines[0]!;
    const starts = [...header.matchAll(/(?<=^| {2})\S/g)].map((m) => m.index);
    for (const row of lines.slice(1, 3)) {
      for (const start of starts) expect(row[start]).not.toBe(" ");
    }
  });

  it("shows a passed reset as reset with 0%", () => {
    const line = render("Europe/Istanbul").split("\n")[2]!;
    expect(line).toContain("0% → reset");
    expect(line).not.toContain("91.5%");
  });

  it("shows in use by <dev> from the other dev's perspective", () => {
    const out = render("Europe/Istanbul", FIXTURE, "ali");
    expect(out).toContain("in use by murat (2m)");
    expect(out).toContain("free");
  });

  it("lists every other holder of a claimed account, without an age", () => {
    const out = render("Europe/Istanbul", [
      {
        email: "shared@x.io",
        claims: [
          { dev: "can", machine_id: "111111111111", at: "2026-10-31T11:00:00Z" },
          { dev: "ali", machine_id: "222222222222", at: "2026-10-31T11:30:00Z" },
          { dev: "murat", machine_id: "3fa9c2d1e07b", at: "2026-10-31T11:59:00Z" },
        ],
      },
    ]);
    expect(out.split("\n")[1]).toContain("claimed by ali, can");
    expect(out).not.toContain("murat");
  });

  it("names every other dev on an in-use account with the report age", () => {
    const out = render("Europe/Istanbul", [
      {
        ...FIXTURE[0]!,
        collected_at: "2026-10-31T11:48:00Z",
        reporter: { dev: "can", machine_id: "111111111111" },
        claims: [{ dev: "ali", machine_id: "222222222222", at: "2026-10-31T11:30:00Z" }],
      },
    ]);
    expect(out.split("\n")[1]).toContain("in use by ali, can (12m)");
  });

  it("shows one decimal only when the percentage has one", () => {
    const out = render("Europe/Istanbul", [
      { ...FIXTURE[0]!, session: { used: 91.5, resets_at: "2026-10-31T18:00:00Z" } },
    ]);
    expect(out).toContain("91.5% → 21:00");
  });

  it("renders windowless and never-reported accounts", () => {
    const out = render("Europe/Istanbul", [{ email: "fresh@x.io", claims: [] }]);
    expect(out.split("\n")[1]).toBe("1  -     fresh@x.io  -        -     free   never");
  });

  it("says so when nothing has been reported", () => {
    expect(formatText({ generated_at: NOW.toISOString(), tz: "UTC", accounts: [] }, NOW)).toBe(
      "no accounts reported yet\n",
    );
  });
});
