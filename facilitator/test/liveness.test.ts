import { describe, expect, it } from "vitest";
import {
  checkLiveness,
  heartbeatMaxAgeMs,
  isTerminalOutcomeState,
  LIVENESS_SCHEMA,
  parseLivenessState,
  type LivenessOutcome,
  type LivenessState,
} from "../src/liveness.ts";

const move = (overrides: Partial<LivenessOutcome> = {}): LivenessOutcome => ({
  bid: "77b9b914366b4073d57266778da3476802c37d6a24e544b8dff12552123dbd21",
  outcome: "refund",
  eventId: "aa".repeat(32),
  auction: "1440:bb:lot",
  state: "failed",
  firstSeenAtMs: 1_000_000,
  updatedAtMs: 1_000_000,
  ...overrides,
});

const snapshot = (overrides: Partial<LivenessState> = {}): LivenessState => ({
  schema: LIVENESS_SCHEMA,
  pid: 4242,
  startedAtMs: 1_000_000,
  heartbeatAtMs: 2_000_000,
  heartbeatIntervalMs: 60_000,
  unsettledAlertAfterSec: 300,
  chainHeight: 14_503,
  commitmentsSeen: 10,
  pendingAnchors: 0,
  processing: 0,
  lastCarrierAtMs: 1_999_000,
  relays: ["wss://relay.playday.it"],
  mintUrl: "http://127.0.0.1:3338",
  counts: { witnessed: 1, published: 0, alreadySpent: 0, declined: 0, failed: 1, open: 1 },
  outstanding: [],
  ...overrides,
});

describe("liveness snapshot parsing", () => {
  it("round-trips a snapshot", () => {
    const state = snapshot({ outstanding: [move()] });
    expect(parseLivenessState(JSON.parse(JSON.stringify(state)))).toEqual(state);
  });

  it("refuses a foreign schema", () => {
    expect(parseLivenessState({ ...snapshot(), schema: "something.else" })).toBeNull();
  });

  it("refuses a truncated snapshot rather than guessing", () => {
    expect(parseLivenessState({ schema: LIVENESS_SCHEMA, pid: 1 })).toBeNull();
    expect(parseLivenessState({ ...snapshot(), outstanding: [{ bid: "x" }] })).toBeNull();
  });
});

describe("checkLiveness", () => {
  const now = 2_000_000;

  it("is healthy for a fresh heartbeat with no outstanding move", () => {
    expect(checkLiveness(snapshot(), now, () => true)).toEqual([]);
  });

  it("alarms when the pid is gone, even before the heartbeat goes stale", () => {
    const problems = checkLiveness(snapshot(), now, () => false);
    expect(problems.map((problem) => problem.kind)).toContain("process-gone");
  });

  it("alarms on a stale heartbeat at max(3 x interval, 90 s)", () => {
    expect(heartbeatMaxAgeMs(60_000)).toBe(180_000);
    const stale = snapshot({ heartbeatAtMs: now - 181_000 });
    expect(checkLiveness(stale, now, () => true).map((problem) => problem.kind)).toContain("stale-heartbeat");
    const fresh = snapshot({ heartbeatAtMs: now - 170_000 });
    expect(checkLiveness(fresh, now, () => true)).toEqual([]);
  });

  it("alarms on an outstanding 1451 older than unsettledAlertAfterSec", () => {
    const state = snapshot({ outstanding: [move({ firstSeenAtMs: now - 301_000 })] });
    const problems = checkLiveness(state, now, () => true);
    expect(problems).toHaveLength(1);
    expect(problems[0]?.kind).toBe("outstanding");
    expect(problems[0]?.message).toContain("77b9b914366b4073");
    expect(problems[0]?.message).toContain("with no 1452");
  });

  it("does not alarm for a move inside the window", () => {
    const state = snapshot({ outstanding: [move({ firstSeenAtMs: now - 120_000 })] });
    expect(checkLiveness(state, now, () => true)).toEqual([]);
  });

  it("treats published, already-spent and declined as terminal", () => {
    for (const state of ["published", "already-spent", "declined"] as const) {
      expect(isTerminalOutcomeState(state)).toBe(true);
    }
    for (const state of ["pending-anchor", "processing", "failed"] as const) {
      expect(isTerminalOutcomeState(state)).toBe(false);
    }
    const terminal = snapshot({
      outstanding: [move({ state: "declined", firstSeenAtMs: now - 10_000_000 })],
    });
    expect(checkLiveness(terminal, now, () => true)).toEqual([]);
  });
});