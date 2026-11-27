import { describe, expect, it } from "vitest";
import {
  canFinishBefore,
  chainTimeFromCommitments,
  deadlineRefusal,
  effectiveNow,
  mintTimeFromInfo,
} from "../src/clock.ts";
import type { StateCommitmentContent } from "../src/types.ts";

const commit = (height: number, time: number, appHash = "aa".repeat(32)): StateCommitmentContent => ({
  schema: "arena.state.v1",
  height,
  app_hash: appHash,
  time,
});

describe("chain time from kind-1447 commitments", () => {
  it("takes the highest committed height and its time", () => {
    expect(chainTimeFromCommitments([commit(10, 1000), commit(12, 1200), commit(11, 1100)])).toEqual({
      time: 1200,
      height: 12,
    });
  });

  it("refuses a fork rather than averaging", () => {
    expect(chainTimeFromCommitments([commit(12, 1200, "aa".repeat(32)), commit(12, 1300, "bb".repeat(32))])).toBeNull();
  });

  it("returns null with no usable commitment", () => {
    expect(chainTimeFromCommitments([])).toBeNull();
  });
});

describe("mint time", () => {
  it("reads the optional info.time extension", () => {
    expect(mintTimeFromInfo({ time: 1700 })).toBe(1700);
    expect(mintTimeFromInfo({})).toBeNull();
    expect(mintTimeFromInfo(null)).toBeNull();
  });
});

describe("effectiveNow takes the earlier trusted clock", () => {
  it("picks the earlier of chain and mint time", () => {
    expect(effectiveNow({ chainTime: 1000, mintTime: 900, wallClockSec: 5000, fallbackMarginSec: 60 })).toEqual({
      now: 900,
      source: "mint",
    });
    expect(effectiveNow({ chainTime: 800, mintTime: 900, wallClockSec: 5000, fallbackMarginSec: 60 })).toEqual({
      now: 800,
      source: "1447",
    });
  });

  it("falls back to the local wall clock with a margin when neither is present", () => {
    expect(effectiveNow({ chainTime: null, mintTime: null, wallClockSec: 5000, fallbackMarginSec: 60 })).toEqual({
      now: 4940,
      source: "wall",
    });
  });

  it("never refuses to start because time is absent", () => {
    const r = effectiveNow({ chainTime: null, mintTime: null, wallClockSec: 100, fallbackMarginSec: 0 });
    expect(r.now).toBe(100);
    expect(r.source).toBe("wall");
  });
});

describe("deadline refusal", () => {
  it("refuses a swap that cannot finish before locktime", () => {
    expect(deadlineRefusal(1000, 1010, 30)).toMatch(/deadline/);
    expect(canFinishBefore(1000, 1010, 30)).toBe(false);
  });

  it("allows a swap with room to spare", () => {
    expect(deadlineRefusal(1000, 2000, 30)).toBeNull();
    expect(canFinishBefore(1000, 2000, 30)).toBe(true);
  });

  it("refuses a leg with no locktime", () => {
    expect(deadlineRefusal(1000, 0, 30)).toMatch(/no locktime/);
  });
});
