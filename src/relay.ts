/**
 * The relay side: read kind 1451 committee signatures, verify them per contract
 * §2, and read kind 1447 state commitments (the anchor + the chain clock).
 *
 * The facilitator is a courier, so its verification is the same one the chain's
 * reader performs — digest recompute, schnorr verify against the FROST group
 * pubkey, roster signer, and an anchor read — but it never *admits* anything:
 * a 1451 it cannot verify is dropped, not carried.
 */

import type { SimplePool } from "nostr-tools";
import { hexToBytes } from "@noble/hashes/utils.js";
import { isLowerHex, schnorrVerifyDigest, sigAllDigestHex } from "./sigall.ts";
import type { CommitteeSignatureContent, NostrEvent, StateCommitmentContent } from "./types.ts";

export const KIND_COMMITTEE_SIGNATURE = 1451;
export const KIND_SETTLEMENT_CARRIER = 1452;
export const KIND_STATE_COMMITMENT = 1447;
export const COMMITTEE_SCHEMA = "arena.committee_signature.v1";
export const SETTLEMENT_SCHEMA = "arena.settlement.v1";

export const committeeFilter = { kinds: [KIND_COMMITTEE_SIGNATURE] } as const;
export const commitmentFilter = { kinds: [KIND_STATE_COMMITMENT] } as const;

function tagValue(tags: string[][], name: string): string | undefined {
  for (const t of tags) if (t.length >= 2 && t[0] === name) return t[1];
  return undefined;
}

function isLowerHexExact(s: unknown, len: number): s is string {
  return typeof s === "string" && isLowerHex(s, len);
}

export interface VerifiedCommittee {
  content: CommitteeSignatureContent;
  event: NostrEvent;
  /** The committed block height/hash the 1451 names in its tags. */
  anchor: { height: number; blockHash: string };
}

export type CommitteeVerifyResult =
  | { ok: true; value: VerifiedCommittee }
  | { ok: false; reason: string };

/** Parse a kind-1451 content string into the typed content, or return a reason. */
export function parseCommitteeContent(content: string): { content: CommitteeSignatureContent } | { error: string } {
  let raw: unknown;
  try {
    raw = JSON.parse(content);
  } catch {
    return { error: "1451 content is not JSON" };
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { error: "1451 content is not an object" };
  }
  const c = raw as Record<string, unknown>;
  if (c.schema !== COMMITTEE_SCHEMA) return { error: `1451 schema is ${String(c.schema)}` };
  if (!isLowerHexExact(c.bid, 64)) return { error: "1451 bid is not 32-byte lowercase hex" };
  if (typeof c.auction !== "string" || c.auction === "") return { error: "1451 has no auction coordinate" };
  if (c.outcome !== "settle" && c.outcome !== "refund") return { error: `1451 outcome ${String(c.outcome)}` };
  if (typeof c.height !== "number" || !Number.isInteger(c.height) || c.height <= 0) {
    return { error: "1451 height is not a positive integer" };
  }
  if (!isLowerHexExact(c.block_hash, 64)) return { error: "1451 block_hash is not 32-byte lowercase hex" };
  if (!isLowerHexExact(c.app_hash, 64)) return { error: "1451 app_hash is not 32-byte lowercase hex" };
  if (!isLowerHexExact(c.digest, 64)) return { error: "1451 digest is not 32-byte lowercase hex" };
  if (!isLowerHexExact(c.signature, 128)) return { error: "1451 signature is not 64-byte lowercase hex" };
  if (!Array.isArray(c.inputs) || c.inputs.length === 0) return { error: "1451 has no inputs" };
  if (!Array.isArray(c.outputs) || c.outputs.length === 0) return { error: "1451 has no outputs" };

  const inputs: CommitteeSignatureContent["inputs"] = [];
  for (const inRaw of c.inputs) {
    if (inRaw === null || typeof inRaw !== "object") return { error: "1451 input is not an object" };
    const i = inRaw as Record<string, unknown>;
    if (typeof i.secret !== "string" || i.secret === "") return { error: "1451 input has no secret" };
    if (typeof i.C !== "string" || i.C === "") return { error: "1451 input has no C" };
    inputs.push({ secret: i.secret, C: i.C });
  }
  const outputs: CommitteeSignatureContent["outputs"] = [];
  for (const outRaw of c.outputs) {
    if (outRaw === null || typeof outRaw !== "object") return { error: "1451 output is not an object" };
    const o = outRaw as Record<string, unknown>;
    if (typeof o.amount !== "number" || !Number.isInteger(o.amount) || o.amount <= 0) {
      return { error: "1451 output amount is not a positive integer" };
    }
    if (typeof o.B_ !== "string" || o.B_ === "") return { error: "1451 output has no B_" };
    outputs.push({ amount: o.amount, B_: o.B_ });
  }

  return {
    content: {
      schema: COMMITTEE_SCHEMA,
      bid: c.bid,
      auction: c.auction,
      outcome: c.outcome,
      height: c.height,
      block_hash: c.block_hash,
      app_hash: c.app_hash,
      digest: c.digest,
      signature: c.signature,
      inputs,
      outputs,
    },
  };
}

/**
 * Verify one kind-1451 event end to end (contract §2):
 *  - the event is a 1451, its content parses, and every content field matches
 *    its tag (bid, auction, outcome, height, hash, p);
 *  - the signer is in the roster;
 *  - `digest == sha256(concat(inputs) + concat(outputs))` recomputes;
 *  - the signature is a BIP-340 signature over that digest under the group key.
 * It does NOT check the anchor against committed state — that is
 * {@link anchorRefusal}, which needs the observed 1447 commitments.
 */
export function verifyCommitteeSignature(
  ev: NostrEvent,
  cfg: { committeeGroupPubkey: string; validatorRoster: string[] },
): CommitteeVerifyResult {
  if (ev.kind !== KIND_COMMITTEE_SIGNATURE) return { ok: false, reason: `not a 1451 (kind ${ev.kind})` };

  const parsed = parseCommitteeContent(ev.content);
  if ("error" in parsed) return { ok: false, reason: parsed.error };
  const content = parsed.content;

  const tagBid = tagValue(ev.tags, "bid");
  const tagAuction = tagValue(ev.tags, "auction");
  const tagOutcome = tagValue(ev.tags, "outcome");
  const tagHeight = tagValue(ev.tags, "height");
  const tagHash = tagValue(ev.tags, "hash");
  const tagP = tagValue(ev.tags, "p");
  if (tagBid !== content.bid) return { ok: false, reason: "1451 bid tag does not match content" };
  if (tagAuction !== content.auction) return { ok: false, reason: "1451 auction tag does not match content" };
  if (tagOutcome !== content.outcome) return { ok: false, reason: "1451 outcome tag does not match content" };
  if (tagHeight !== String(content.height)) return { ok: false, reason: "1451 height tag does not match content" };
  if (tagHash !== content.block_hash) return { ok: false, reason: "1451 hash tag does not match content" };
  if (tagP !== ev.pubkey) return { ok: false, reason: "1451 p tag is not the signer" };

  if (!cfg.validatorRoster.includes(ev.pubkey)) {
    return { ok: false, reason: `1451 signer ${ev.pubkey} is not in the roster` };
  }

  const recomputed = sigAllDigestHex(content.inputs, content.outputs);
  if (recomputed !== content.digest) {
    return { ok: false, reason: "1451 digest does not recompute from its inputs and outputs" };
  }

  if (!schnorrVerifyDigest(hexToBytes(content.digest), cfg.committeeGroupPubkey, content.signature)) {
    return { ok: false, reason: "1451 signature does not verify against the group pubkey" };
  }

  return {
    ok: true,
    value: { content, event: ev, anchor: { height: content.height, blockHash: content.block_hash } },
  };
}

/**
 * The anchor read available to a keyless courier: the 1451 must name a height
 * the chain has committed. We can prove commitment (never the Tendermint block
 * hash, which a courier has no lookup for) from observed 1447 commitments, so a
 * height beyond the highest committed one is refused as unanchored. With no
 * commitment observed yet, the signature and digest are the only authority and
 * the anchor is not refusable.
 */
export function anchorRefusal(
  content: CommitteeSignatureContent,
  commitments: StateCommitmentContent[],
): string | null {
  let max = 0;
  for (const c of commitments) if (Number.isInteger(c.height) && c.height > max) max = c.height;
  if (max === 0) return null;
  if (content.height > max) {
    return `anchor: 1451 height ${content.height} exceeds the highest committed ${max}`;
  }
  return null;
}

/** Subscribe to kind 1451 events; returns a closer. */
export function subscribeCommittee(
  pool: SimplePool,
  relays: string[],
  onEvent: (ev: NostrEvent) => void,
): { close(): void } {
  return pool.subscribeMany(relays, { kinds: [KIND_COMMITTEE_SIGNATURE] }, {
    onevent: (ev) => onEvent(ev as unknown as NostrEvent),
  });
}

/** Subscribe to kind 1447 state commitments; returns a closer. */
export function subscribeCommitments(
  pool: SimplePool,
  relays: string[],
  onEvent: (ev: NostrEvent) => void,
): { close(): void } {
  return pool.subscribeMany(relays, { kinds: [KIND_STATE_COMMITMENT] }, {
    onevent: (ev) => onEvent(ev as unknown as NostrEvent),
  });
}
