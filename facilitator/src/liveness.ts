/**
 * The facilitator's liveness contract: a fixed-cadence heartbeat into a state
 * file, and the pure check a monitor runs over it.
 *
 * The incident this exists for (2026-11-24 → 2026-11-27): the process exited
 * cleanly, `Restart=on-failure` left it dead, and nothing noticed for 2.5 days.
 * A process that is merely *running* is not evidence that money is moving, so
 * the state file names the facts that matter — the last heartbeat and every
 * kind-1451 outcome that has not reached a terminal state (a kind-1452 carrier,
 * an already-spent escrow, or a named decline).
 *
 * Pure: no I/O here. `src/index.ts` writes the snapshot; `scripts/courier-liveness.ts`
 * reads it and calls {@link checkLiveness}.
 */

export const LIVENESS_SCHEMA = "arena.facilitator.liveness.v1";

/**
 * Where one observed 1451 outcome stands.
 *
 * - `pending-anchor` — verified, but its anchored height is not in our committed
 *   view yet (re-checked on the next kind-1447).
 * - `processing` — the swap is in flight.
 * - `published` — a kind-1452 carrier was published (terminal, success).
 * - `already-spent` — the mint reports the escrow spent/pending: someone else
 *   executed it (terminal, success; the chain's own receipt is the source).
 * - `declined` — a named refusal (deadline, unreadable commitment, V4V split,
 *   missing bid/create). Retrying cannot fix it, so it never alerts (terminal).
 * - `failed` — a transient failure (mint unreachable, bad mint response, state
 *   write). Money may still be stuck; it stays outstanding and alerts.
 */
export type LivenessOutcomeState =
  | "pending-anchor"
  | "processing"
  | "published"
  | "already-spent"
  | "declined"
  | "failed";

export const TERMINAL_OUTCOME_STATES: readonly LivenessOutcomeState[] = [
  "published",
  "already-spent",
  "declined",
];

export const isTerminalOutcomeState = (state: LivenessOutcomeState): boolean =>
  TERMINAL_OUTCOME_STATES.includes(state);

/** One observed (bid, outcome) move, as the courier currently understands it. */
export interface LivenessOutcome {
  bid: string;
  outcome: "settle" | "refund";
  eventId: string;
  auction: string;
  state: LivenessOutcomeState;
  /** When the courier first witnessed this move (ms since epoch). */
  firstSeenAtMs: number;
  /** When this state was last written (ms since epoch). */
  updatedAtMs: number;
  /** The refusal / failure reason, when the state carries one. */
  detail?: string;
}

/** Terminal counts, so the state file does not grow without bound. */
export interface LivenessCounts {
  witnessed: number;
  published: number;
  alreadySpent: number;
  declined: number;
  failed: number;
  open: number;
}

/** The snapshot written on every heartbeat. */
export interface LivenessState {
  schema: typeof LIVENESS_SCHEMA;
  pid: number;
  startedAtMs: number;
  /** The heartbeat this snapshot describes. */
  heartbeatAtMs: number;
  heartbeatIntervalMs: number;
  /** An outstanding move older than this is a liveness alarm. */
  unsettledAlertAfterSec: number;
  /** Highest committed height observed from kind-1447. */
  chainHeight: number | null;
  commitmentsSeen: number;
  pendingAnchors: number;
  processing: number;
  lastCarrierAtMs: number | null;
  relays: string[];
  mintUrl: string;
  counts: LivenessCounts;
  /** Every move not in a terminal state — what a monitor must judge. */
  outstanding: LivenessOutcome[];
}

export type LivenessProblemKind =
  | "invalid-state"
  | "stale-heartbeat"
  | "process-gone"
  | "outstanding";

export interface LivenessProblem {
  kind: LivenessProblemKind;
  message: string;
}

const isFiniteNumber = (value: unknown): value is number =>
  typeof value === "number" && Number.isFinite(value);

const isOutcomeState = (value: unknown): value is LivenessOutcomeState =>
  value === "pending-anchor" ||
  value === "processing" ||
  value === "published" ||
  value === "already-spent" ||
  value === "declined" ||
  value === "failed";

/**
 * Parse a state-file value defensively. A file written by an older/newer
 * schema, or truncated by a crash, is `null` — the monitor reports it instead
 * of guessing.
 */
export function parseLivenessState(raw: unknown): LivenessState | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const o = raw as Record<string, unknown>;
  if (o.schema !== LIVENESS_SCHEMA) return null;
  if (!isFiniteNumber(o.pid) || !isFiniteNumber(o.startedAtMs) || !isFiniteNumber(o.heartbeatAtMs)) return null;
  if (!isFiniteNumber(o.heartbeatIntervalMs) || o.heartbeatIntervalMs <= 0) return null;
  if (!isFiniteNumber(o.unsettledAlertAfterSec) || o.unsettledAlertAfterSec < 0) return null;
  if (o.chainHeight !== null && !isFiniteNumber(o.chainHeight)) return null;
  if (!isFiniteNumber(o.commitmentsSeen) || !isFiniteNumber(o.pendingAnchors) || !isFiniteNumber(o.processing)) return null;
  if (o.lastCarrierAtMs !== null && !isFiniteNumber(o.lastCarrierAtMs)) return null;
  if (!Array.isArray(o.relays) || !o.relays.every((r) => typeof r === "string")) return null;
  if (typeof o.mintUrl !== "string") return null;

  const c = o.counts;
  if (c === null || typeof c !== "object" || Array.isArray(c)) return null;
  const counts = c as Record<string, unknown>;
  for (const key of ["witnessed", "published", "alreadySpent", "declined", "failed", "open"]) {
    if (!isFiniteNumber(counts[key])) return null;
  }

  if (!Array.isArray(o.outstanding)) return null;
  const outstanding: LivenessOutcome[] = [];
  for (const entry of o.outstanding) {
    if (entry === null || typeof entry !== "object" || Array.isArray(entry)) return null;
    const e = entry as Record<string, unknown>;
    if (typeof e.bid !== "string" || e.bid === "") return null;
    if (e.outcome !== "settle" && e.outcome !== "refund") return null;
    if (typeof e.eventId !== "string") return null;
    if (typeof e.auction !== "string") return null;
    if (!isOutcomeState(e.state)) return null;
    if (!isFiniteNumber(e.firstSeenAtMs) || !isFiniteNumber(e.updatedAtMs)) return null;
    outstanding.push({
      bid: e.bid,
      outcome: e.outcome,
      eventId: e.eventId,
      auction: e.auction,
      state: e.state,
      firstSeenAtMs: e.firstSeenAtMs,
      updatedAtMs: e.updatedAtMs,
      ...(typeof e.detail === "string" ? { detail: e.detail } : {}),
    });
  }

  return {
    schema: LIVENESS_SCHEMA,
    pid: o.pid,
    startedAtMs: o.startedAtMs,
    heartbeatAtMs: o.heartbeatAtMs,
    heartbeatIntervalMs: o.heartbeatIntervalMs,
    unsettledAlertAfterSec: o.unsettledAlertAfterSec,
    chainHeight: o.chainHeight,
    commitmentsSeen: o.commitmentsSeen,
    pendingAnchors: o.pendingAnchors,
    processing: o.processing,
    lastCarrierAtMs: o.lastCarrierAtMs,
    relays: o.relays as string[],
    mintUrl: o.mintUrl,
    counts: {
      witnessed: counts.witnessed as number,
      published: counts.published as number,
      alreadySpent: counts.alreadySpent as number,
      declined: counts.declined as number,
      failed: counts.failed as number,
      open: counts.open as number,
    },
    outstanding,
  };
}

/** How stale a heartbeat may be before the monitor calls the process gone. */
export const heartbeatMaxAgeMs = (heartbeatIntervalMs: number): number =>
  Math.max(3 * heartbeatIntervalMs, 90_000);

const seconds = (ms: number): number => Math.round(ms / 1000);

/**
 * Judge a snapshot.
 *
 * `processAlive` is injected so the monitor can say "the pid is gone" the
 * moment the process dies instead of waiting for the heartbeat to go stale.
 * Everything else is arithmetic over the snapshot — the same check a test can
 * run with a fabricated state.
 */
export function checkLiveness(
  state: LivenessState,
  nowMs: number,
  processAlive?: (pid: number) => boolean,
): LivenessProblem[] {
  const problems: LivenessProblem[] = [];

  if (processAlive !== undefined && !processAlive(state.pid)) {
    problems.push({
      kind: "process-gone",
      message: `courier pid ${state.pid} is not running (last heartbeat ${seconds(nowMs - state.heartbeatAtMs)}s ago)`,
    });
  }

  const ageMs = nowMs - state.heartbeatAtMs;
  const maxAgeMs = heartbeatMaxAgeMs(state.heartbeatIntervalMs);
  if (ageMs > maxAgeMs) {
    problems.push({
      kind: "stale-heartbeat",
      message: `heartbeat is ${seconds(ageMs)}s old (limit ${seconds(maxAgeMs)}s, interval ${seconds(state.heartbeatIntervalMs)}s)`,
    });
  }

  const limitMs = state.unsettledAlertAfterSec * 1000;
  for (const move of state.outstanding) {
    if (isTerminalOutcomeState(move.state)) continue;
    const openMs = nowMs - move.firstSeenAtMs;
    if (openMs <= limitMs) continue;
    const detail = move.detail === undefined ? "" : ` — ${move.detail}`;
    problems.push({
      kind: "outstanding",
      message: `outstanding 1451 bid ${move.bid.slice(0, 16)}… ${move.outcome} (${move.state}) for ${seconds(openMs)}s with no 1452${detail}`,
    });
  }

  return problems;
}