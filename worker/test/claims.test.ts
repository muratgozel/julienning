import { describe, expect, it } from "vitest";
import { freshFor, liveClaims } from "../src/claims";
import type { Claim } from "../src/types";

const NOW = Date.parse("2026-09-16T12:00:00Z");
const ALI_1: Claim = { dev: "ali", machine_id: "111111111111", at: "2026-09-16T11:00:00Z" };
const ALI_2: Claim = { dev: "ali", machine_id: "222222222222", at: "2026-09-16T11:30:00Z" };
const CAN: Claim = { dev: "can", machine_id: "333333333333", at: "2026-09-16T11:45:00Z" };

describe("claim holders", () => {
  it("keeps holders younger than the TTL only", () => {
    expect(liveClaims([ALI_1, ALI_2, CAN], NOW, 30)).toEqual([CAN]);
    expect(liveClaims([ALI_1, ALI_2, CAN], NOW, 720)).toHaveLength(3);
  });

  it("treats future stamps as fresh and garbage as stale", () => {
    expect(freshFor("2026-09-16T13:00:00Z", NOW, 1)).toBe(true);
    expect(freshFor("nope", NOW, 720)).toBe(false);
    expect(freshFor(undefined, NOW, 720)).toBe(false);
    expect(freshFor("2026-09-16T11:00:00Z", NOW, 60)).toBe(false);
  });
});
