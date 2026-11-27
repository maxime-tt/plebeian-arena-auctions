/**
 * Facilitator configuration: a file, no secrets (handover §5).
 *
 * It names only public things — the mint, the relays, the committee's FROST
 * group pubkey, the validator roster, and the timing knobs. The courier's own
 * Nostr key is NOT here: it authors nothing authoritative (kind 1452 carries no
 * roster check, no anchor; authority lives in the committee signature and the
 * proofs), so it is generated at startup or read from the environment.
 */

import { readFile } from "node:fs/promises";
import { xOnly } from "./sigall.ts";

export interface Config {
  /** The Cashu mint the arena escrows against. */
  mintUrl: string;
  /** Relays to watch for kind 1451, and to publish kind 1452 to. */
  relays: string[];
  /** The committee's FROST group pubkey (x-only hex after normalisation). */
  committeeGroupPubkey: string;
  /** Validator roster pubkeys, x-only hex; the only allowed 1451 signers. */
  validatorRoster: string[];
  /** How often to (re)check for unprocessed committee signatures, ms. */
  pollIntervalMs: number;
  /** Safety margin before `locktime`, seconds. */
  clockToleranceSec: number;
  /** Margin subtracted from the local wall clock when it is the only clock. */
  fallbackMarginSec: number;
  /** Assumed swap round-trip cost used in the deadline check, seconds. */
  estimatedSwapSeconds: number;
  /** Per-request HTTP timeout for mint and relay calls, ms. */
  requestTimeoutMs: number;
  /** Fixed cadence of the liveness heartbeat line and state-file write, ms. */
  heartbeatIntervalMs: number;
  /**
   * An observed kind-1451 outcome that has produced neither a kind-1452 nor a
   * named terminal state for this long is a liveness alarm, seconds.
   */
  unsettledAlertAfterSec: number;
  /**
   * Where the heartbeat snapshot is written. `SF_STATE_FILE` overrides it; an
   * empty string disables the state file (the log heartbeat still runs).
   */
  stateFile: string;
}

export const DEFAULT_CONFIG: Omit<Config, "mintUrl" | "relays" | "committeeGroupPubkey" | "validatorRoster"> = {
  pollIntervalMs: 5_000,
  clockToleranceSec: 30,
  fallbackMarginSec: 60,
  estimatedSwapSeconds: 10,
  requestTimeoutMs: 15_000,
  heartbeatIntervalMs: 60_000,
  unsettledAlertAfterSec: 300,
  stateFile: "",
};

export class ConfigError extends Error {
  override name = "ConfigError";
}

function isHexExact(s: unknown, len: number): s is string {
  if (typeof s !== "string" || s.length !== len) return false;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    const ok = (c >= 48 && c <= 57) || (c >= 97 && c <= 102);
    if (!ok) return false;
  }
  return true;
}

function normalizePubkey(value: unknown, field: string): string {
  if (isHexExact(value, 64)) return value;
  if (isHexExact(value, 66)) return xOnly(value);
  throw new ConfigError(`${field} must be a 32-byte x-only or 33-byte compressed lowercase-hex pubkey`);
}

function posInt(value: unknown, field: string, fallback: number): number {
  if (value === undefined) return fallback;
  if (typeof value !== "number" || !Number.isInteger(value) || value < 0) {
    throw new ConfigError(`${field} must be a non-negative integer`);
  }
  return value;
}

function clampInt(value: unknown, field: string, fallback: number, min: number, max: number): number {
  const parsed = posInt(value, field, fallback);
  if (parsed < min || parsed > max) {
    throw new ConfigError(`${field} must be between ${min} and ${max}`);
  }
  return parsed;
}

/**
 * Validate a raw config object into a Config. Throws ConfigError naming the
 * field, so a bad deployment fails at startup with a reason rather than
 * mid-swap.
 */
export function validateConfig(raw: unknown): Config {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw new ConfigError("config must be a JSON object");
  }
  const o = raw as Record<string, unknown>;

  const mintUrl = o.mintUrl;
  if (typeof mintUrl !== "string" || mintUrl.trim() === "") {
    throw new ConfigError("mintUrl is required");
  }
  let parsedUrl: URL;
  try {
    parsedUrl = new URL(mintUrl);
  } catch {
    throw new ConfigError("mintUrl must be an absolute http(s) URL");
  }
  if (parsedUrl.protocol !== "http:" && parsedUrl.protocol !== "https:") {
    throw new ConfigError("mintUrl must be http(s)");
  }

  const relays = o.relays;
  if (!Array.isArray(relays) || relays.length === 0) {
    throw new ConfigError("relays must be a non-empty array of ws(s) URLs");
  }
  for (const r of relays) {
    if (typeof r !== "string" || !/^wss?:\/\/.+/i.test(r)) {
      throw new ConfigError(`relays entry ${JSON.stringify(r)} is not a ws(s):// URL`);
    }
  }

  const committeeGroupPubkey = normalizePubkey(o.committeeGroupPubkey, "committeeGroupPubkey");

  const roster = o.validatorRoster;
  if (!Array.isArray(roster) || roster.length === 0) {
    throw new ConfigError("validatorRoster must be a non-empty array of pubkeys");
  }
  const validatorRoster = roster.map((p, i) => normalizePubkey(p, `validatorRoster[${i}]`));

  return {
    mintUrl: parsedUrl.toString().replace(/\/+$/, ""),
    relays: [...relays] as string[],
    committeeGroupPubkey,
    validatorRoster,
    pollIntervalMs: posInt(o.pollIntervalMs, "pollIntervalMs", DEFAULT_CONFIG.pollIntervalMs),
    clockToleranceSec: posInt(o.clockToleranceSec, "clockToleranceSec", DEFAULT_CONFIG.clockToleranceSec),
    fallbackMarginSec: posInt(o.fallbackMarginSec, "fallbackMarginSec", DEFAULT_CONFIG.fallbackMarginSec),
    estimatedSwapSeconds: posInt(o.estimatedSwapSeconds, "estimatedSwapSeconds", DEFAULT_CONFIG.estimatedSwapSeconds),
    requestTimeoutMs: posInt(o.requestTimeoutMs, "requestTimeoutMs", DEFAULT_CONFIG.requestTimeoutMs),
    // A zero/absurd interval would either spin or hide a dead process, so the
    // heartbeat is constrained to one second … one hour.
    heartbeatIntervalMs: clampInt(o.heartbeatIntervalMs, "heartbeatIntervalMs", DEFAULT_CONFIG.heartbeatIntervalMs, 1_000, 3_600_000),
    unsettledAlertAfterSec: posInt(o.unsettledAlertAfterSec, "unsettledAlertAfterSec", DEFAULT_CONFIG.unsettledAlertAfterSec),
    stateFile: typeof o.stateFile === "string" ? o.stateFile : DEFAULT_CONFIG.stateFile,
  };
}

/** Read and validate the config file. */
export async function loadConfig(path: string): Promise<Config> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch (err) {
    throw new ConfigError(`cannot read config ${path}: ${(err as Error).message}`);
  }
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (err) {
    throw new ConfigError(`config ${path} is not valid JSON: ${(err as Error).message}`);
  }
  return validateConfig(raw);
}
