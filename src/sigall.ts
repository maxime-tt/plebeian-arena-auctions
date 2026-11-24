/**
 * The NUT-11 SIG_ALL message, BIP-340 verification, the B_ derivation, and the
 * strict P2PK reader — the cryptography the facilitator must agree on,
 * byte for byte, with the mint and the chain.
 *
 * The digest here is LAYOUT B, what Nutshell >= 0.20.3 signs and what
 * `chain/sigall.go` and `chain/node.go` implement. The golden vector in
 * `test/digest.test.ts` is the chain's own frozen vector, so a drift here is
 * caught before it ever reaches a mint.
 */

import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils.js";
import { schnorr } from "@noble/curves/secp256k1.js";
import { blindMessage } from "@cashu/cashu-ts";
import type { P2PKLock } from "./types.ts";

export interface SigAllInput {
  secret: string;
  C: string;
}

export interface SigAllOutput {
  amount: number;
  B_: string;
}

/**
 * The exact string Nutshell 0.21's `sigall_message_to_sign` builds: for every
 * input its secret then its C; then for every output its amount as a decimal
 * string then its blinded point B_. No separators.
 */
export function sigAllMessage(inputs: SigAllInput[], outputs: SigAllOutput[]): string {
  let msg = "";
  for (const i of inputs) msg += i.secret + i.C;
  for (const o of outputs) msg += String(o.amount) + o.B_;
  return msg;
}

/** sha256 of the SIG_ALL message (the 32-byte value the signatures cover). */
export function sigAllDigest(inputs: SigAllInput[], outputs: SigAllOutput[]): Uint8Array {
  return sha256(utf8ToBytes(sigAllMessage(inputs, outputs)));
}

/** The same digest as lowercase hex (the wire form). */
export function sigAllDigestHex(inputs: SigAllInput[], outputs: SigAllOutput[]): string {
  return bytesToHex(sigAllDigest(inputs, outputs));
}

/**
 * BIP-340 verify of a 32-byte digest against a compressed (02/03-prefixed) or
 * x-only public key. Mirrors `chain/schnorrVerifyHash`: a 33-byte key is
 * reduced to its x-only form before verification.
 */
export function schnorrVerifyDigest(digest: Uint8Array, pubHex: string, sigHex: string): boolean {
  try {
    const pb = hexToBytes(stripHex(pubHex));
    const xonly = pb.length === 33 ? pb.slice(1) : pb;
    const sig = hexToBytes(stripHex(sigHex));
    return schnorr.verify(sig, digest, xonly);
  } catch {
    return false;
  }
}

/**
 * B_ = hash_to_curve(secret) + r·G, exactly the point the mint signs. Uses
 * cashu-ts's `blindMessage` so the derivation is not re-implemented here
 * (contract non-goal: no arena-specific swap/unblind math).
 */
export function outputBPoint(secret: string, rHex: string): string {
  const r = BigInt("0x" + stripHex(rHex));
  return bytesToHex(blindMessage(utf8ToBytes(secret), r).B_.toBytes(true));
}

/** Recompute every template output's B_ from its committed (secret, r). */
export function recomputeOutputs(
  outputs: { amount: number; secret: string; r: string }[],
): SigAllOutput[] {
  return outputs.map((o) => ({ amount: o.amount, B_: outputBPoint(o.secret, o.r) }));
}

// --- hex helpers -----------------------------------------------------------

export function isLowerHex(s: string, len: number): boolean {
  if (s.length !== len) return false;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    const ok = (c >= 48 && c <= 57) || (c >= 97 && c <= 102);
    if (!ok) return false;
  }
  return true;
}

function stripHex(s: string): string {
  return s.startsWith("0x") ? s.slice(2) : s;
}

// --- P2PK (NUT-10 / NUT-11) ------------------------------------------------

const ALLOWED_BODY_KEYS: Record<string, true> = { nonce: true, data: true, tags: true };

/**
 * Parse a NUT-11 P2PK secret, strictly enough that the facilitator and the
 * mint cannot read it differently. Refuses: non-`["P2PK", {...}]` shapes,
 * unknown/duplicate body keys, a missing nonce/data, a `data` that is not a
 * lowercase compressed pubkey, duplicate tags, and unknown tags. `data` is
 * returned verbatim (compressed), matching `chain/cashu.go parseP2PK`.
 */
export function parseP2PK(secret: string): P2PKLock | null {
  let arr: unknown;
  try {
    arr = JSON.parse(secret);
  } catch {
    return null;
  }
  if (!Array.isArray(arr) || arr.length !== 2) return null;
  const [kind, body] = arr as [unknown, unknown];
  if (kind !== "P2PK") return null;
  if (body === null || typeof body !== "object" || Array.isArray(body)) return null;
  const obj = body as Record<string, unknown>;
  for (const k of Object.keys(obj)) if (!ALLOWED_BODY_KEYS[k]) return null;
  const nonce = obj.nonce;
  const data = obj.data;
  if (typeof nonce !== "string" || nonce === "") return null;
  if (typeof data !== "string" || !isLowerHex(data, 66)) return null;

  const lock: P2PKLock = {
    nonce,
    data,
    pubkeys: [],
    nSigs: 1,
    locktime: 0,
    refund: [],
    sigflag: "",
    tagsJson: "[]",
  };

  const tags = obj.tags;
  if (tags !== undefined) {
    if (!Array.isArray(tags)) return null;
    const seen = new Set<string>();
    for (const t of tags) {
      if (!Array.isArray(t) || t.length < 1 || typeof t[0] !== "string") return null;
      const name = t[0] as string;
      if (seen.has(name)) return null;
      seen.add(name);
      const vals = t.slice(1);
      for (const v of vals) if (typeof v !== "string") return null;
      switch (name) {
        case "pubkeys":
          for (const v of vals) lock.pubkeys.push(v.toLowerCase());
          break;
        case "n_sigs": {
          if (vals.length !== 1) return null;
          const n = Number(vals[0]);
          if (!Number.isInteger(n)) return null;
          lock.nSigs = n;
          break;
        }
        case "locktime": {
          if (vals.length !== 1) return null;
          const n = Number(vals[0]);
          if (!Number.isInteger(n) || String(n) !== vals[0]) return null;
          lock.locktime = n;
          break;
        }
        case "refund":
          for (const v of vals) {
            if (!isLowerHex(v, 66)) return null;
            lock.refund.push(v);
          }
          break;
        case "sigflag":
          if (vals.length !== 1) return null;
          lock.sigflag = vals[0] as string;
          break;
        case "n_sigs_refund":
          if (vals.length !== 1 || vals[0] !== "1") return null;
          break;
        default:
          return null;
      }
    }
    lock.tagsJson = JSON.stringify(tags);
  }
  return lock;
}

/** The x-only (BIP-340) form of a compressed key: drop the 02/03 prefix. */
export function xOnly(compressedHex: string): string {
  const h = stripHex(compressedHex);
  return h.length === 66 ? h.slice(2) : h;
}

/** Recipient key a template output pays: `parseP2PK(secret).data`, verbatim. */
export function outputRecipient(secret: string): string | null {
  const lock = parseP2PK(secret);
  return lock ? lock.data : null;
}
