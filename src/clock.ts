/**
 * The facilitator's clock and its settlement deadline.
 *
 * Policy (contract §4, ruling Q6): the chain's kind-1447 state commitments are
 * the clock the design can justify; the mint's `GET /v1/info` `time` is an
 * opportunistic extension that decides the mint's OWN locktime evaluation, so
 * it matters for `[close, locktime)` and is used when present. Take the
 * EARLIER of the two, because a swap is only ever refused for being too late —
 * erring early is the conservative side. If neither is available, fall back to
 * the local wall clock with a margin; never refuse to run because a mint omits
 * the extension.
 */

import type { StateCommitmentContent } from "./types.ts";

export interface ClockReading {
  now: number;
  source: "1447" | "mint" | "wall";
}

const isPositiveInt = (n: unknown): n is number =>
  typeof n === "number" && Number.isInteger(n) && n > 0;

/**
 * Chain time from observed kind-1447 commitments: the highest committed height
 * and its `time`. A fork (two different app hashes at one height) refuses —
 * exactly the `arenaClock.ts` policy, "refuse forks rather than average them".
 */
export function chainTimeFromCommitments(
  commitments: StateCommitmentContent[],
): { time: number; height: number } | null {
  if (commitments.length === 0) return null;
  const byHeight = new Map<number, Set<string>>();
  for (const c of commitments) {
    if (!isPositiveInt(c.height) || !isPositiveInt(c.time)) continue;
    const hashes = byHeight.get(c.height) ?? new Set<string>();
    hashes.add(c.app_hash);
    byHeight.set(c.height, hashes);
  }
  let best: { time: number; height: number } | null = null;
  for (const c of commitments) {
    if (!isPositiveInt(c.height) || !isPositiveInt(c.time)) continue;
    if ((byHeight.get(c.height)?.size ?? 0) > 1) return null; // fork
    if (best === null || c.height > best.height) best = { time: c.time, height: c.height };
  }
  return best;
}

/** The mint's `info.time` when it returns the (non-standard) extension. */
export function mintTimeFromInfo(info: { time?: unknown } | null | undefined): number | null {
  return isPositiveInt(info?.time) ? info.time : null;
}

/**
 * The time a settlement decision is made against. Earlier of the two trusted
 * clocks; local wall clock minus `fallbackMarginSec` when neither is present.
 */
export function effectiveNow(opts: {
  chainTime: number | null;
  mintTime: number | null;
  wallClockSec: number;
  fallbackMarginSec: number;
}): ClockReading {
  const { chainTime, mintTime } = opts;
  if (isPositiveInt(chainTime) && isPositiveInt(mintTime)) {
    return chainTime <= mintTime
      ? { now: chainTime, source: "1447" }
      : { now: mintTime, source: "mint" };
  }
  if (isPositiveInt(chainTime)) return { now: chainTime, source: "1447" };
  if (isPositiveInt(mintTime)) return { now: mintTime, source: "mint" };
  return { now: opts.wallClockSec - opts.fallbackMarginSec, source: "wall" };
}

/**
 * Refuse a swap the facilitator cannot finish before the leg's `locktime`.
 * After `locktime` the bidder's refund branch is live too, so starting late is
 * the one outcome the design must avoid when the funds were signed to the
 * seller. Returns the refusal reason, or null when the window is safe.
 */
export function deadlineRefusal(
  nowSec: number,
  locktimeSec: number,
  toleranceSec: number,
): string | null {
  if (!isPositiveInt(locktimeSec)) return "deadline: leg has no locktime";
  const remaining = locktimeSec - nowSec;
  if (remaining <= toleranceSec) {
    return `deadline: ${remaining}s before locktime ${locktimeSec}, tolerance ${toleranceSec}s`;
  }
  return null;
}

/** Convenience boolean form of {@link deadlineRefusal}. */
export function canFinishBefore(
  nowSec: number,
  locktimeSec: number,
  toleranceSec: number,
): boolean {
  return deadlineRefusal(nowSec, locktimeSec, toleranceSec) === null;
}
