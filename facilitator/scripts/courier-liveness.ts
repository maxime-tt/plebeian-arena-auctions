/**
 * The courier liveness monitor (T1, 2026-11-27).
 *
 * Reads the heartbeat snapshot `src/index.ts` writes and fails loudly when the
 * courier is not doing its job:
 *
 *  - the state file is missing or unreadable (it never started, or a schema change);
 *  - the pid recorded in the snapshot is gone (the 2026-11-24 16:34 failure);
 *  - the heartbeat is older than `max(3 × interval, 90 s)` (a hung process);
 *  - a witnessed kind-1451 has been outstanding for `unsettledAlertAfterSec`
 *    without a kind-1452 carrier or a named terminal state ("outstanding 1451s
 *    with no 1452 for N minutes").
 *
 * Run by `arena-courier-liveness.timer`. Exit 0 = healthy, 1 = alarm. It makes
 * no network calls: everything it needs is in the snapshot.
 */

import { readFile } from "node:fs/promises";
import { loadConfig } from "../src/config.ts";
import { checkLiveness, LIVENESS_SCHEMA, parseLivenessState } from "../src/liveness.ts";

async function statePath(): Promise<string> {
  const explicit = process.argv[2] ?? process.env.SF_STATE_FILE;
  if (explicit !== undefined) return explicit;
  const configPath = process.env.SF_CONFIG ?? "./config.json";
  try {
    const cfg = await loadConfig(configPath);
    return cfg.stateFile;
  } catch (err) {
    // No config is itself worth saying out loud, but the alarm below names the path.
    console.error(`courier-liveness: cannot read config ${configPath}: ${(err as Error).message}`);
    return "";
  }
}

function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    const code = typeof err === "object" && err !== null && "code" in err ? err.code : undefined;
    // ESRCH = no such process; EPERM means it exists but is not ours.
    return !(typeof code === "string" && code === "ESRCH");
  }
}

async function main(): Promise<number> {
  const path = await statePath();
  if (path === "") {
    console.error("ALERT courier-liveness [invalid-state]: no state file path (set SF_STATE_FILE or config.stateFile)");
    return 1;
  }

  let raw: string;
  try {
    raw = await readFile(path, "utf8");
  } catch (err) {
    console.error(`ALERT courier-liveness [invalid-state]: cannot read ${path}: ${(err as Error).message}`);
    return 1;
  }

  let parsedJson: unknown;
  try {
    parsedJson = JSON.parse(raw);
  } catch (err) {
    console.error(`ALERT courier-liveness [invalid-state]: ${path} is not JSON: ${(err as Error).message}`);
    return 1;
  }

  const state = parseLivenessState(parsedJson);
  if (state === null) {
    console.error(`ALERT courier-liveness [invalid-state]: ${path} is not a ${LIVENESS_SCHEMA} snapshot`);
    return 1;
  }

  const now = Date.now();
  const idleSec = Math.round((now - state.heartbeatAtMs) / 1000);
  const openSec =
    state.outstanding.length === 0
      ? 0
      : Math.round(
          (now - Math.min(...state.outstanding.map((move) => move.firstSeenAtMs))) / 1000,
        );
  console.log(
    `courier-liveness: pid=${state.pid} heartbeat=${idleSec}s_ago open=${state.counts.open}`,
    `oldest_outstanding=${openSec}s published=${state.counts.published} spent=${state.counts.alreadySpent}`,
    `declined=${state.counts.declined} failed=${state.counts.failed} chain=${state.chainHeight ?? "--"}`,
  );

  const problems = checkLiveness(state, now, processAlive);
  for (const problem of problems) {
    console.error(`ALERT courier-liveness [${problem.kind}]: ${problem.message}`);
  }
  return problems.length === 0 ? 0 : 1;
}

main()
  .then((code) => process.exit(code))
  .catch((err) => {
    console.error(`ALERT courier-liveness [invalid-state]: ${(err as Error).message}`);
    process.exit(1);
  });