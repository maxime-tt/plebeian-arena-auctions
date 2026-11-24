/**
 * Unblind the mint's response and publish the settlement carrier (contract
 * §3–§5).
 *
 * The facilitator receives `C_` (blinded signatures) from the mint. Because
 * each committed template output's blinding factor `r` is public (it rides in
 * the 1441 content), the facilitator unblinds with `C = C_ − r·K`, verifies the
 * DLEQ (NUT-12) both on the blinded and the unblinded point, derives each
 * output's recipient from its P2PK secret exactly as the chain's
 * `receiptOutputs` does, and publishes ONE kind-1452 carrier under its own key.
 * The carrier's authority is the committee signature plus the proofs; the chain
 * re-verifies both, so no roster check applies to the courier.
 */

import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils.js";
import { finalizeEvent, getPublicKey } from "nostr-tools";
import { pointFromHex, unblindSignature, verifyDLEQProof, verifyDLEQProof_reblind } from "@cashu/cashu-ts";
import { outputRecipient, xOnly, isLowerHex } from "./sigall.ts";
import type { MatchedSignature, SwapPlan } from "./swap.ts";
import { KIND_SETTLEMENT_CARRIER, SETTLEMENT_SCHEMA } from "./relay.ts";
import type {
  CommitteeSignatureContent,
  NostrEvent,
  ReceiptOut,
  SettlementCarrierContent,
} from "./types.ts";

export class SettlementRefusal extends Error {
  override name = "SettlementRefusal";
}

function sameHex(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}

/**
 * Unblind every matched signature into the `ReceiptOut` shape the chain and the
 * app share (contract §5), verifying the DLEQ at each step. The whole batch is
 * refused if any output cannot be produced — a partial receipt would not match
 * the committed template, and the chain's `parseSettle` requires output-for-
 * output equality.
 */
export function unblindToReceipts(
  plan: SwapPlan,
  matched: MatchedSignature[],
  keys: Record<string, string>,
): ReceiptOut[] {
  if (matched.length !== plan.outputs.length) {
    throw new SettlementRefusal(
      `mint signed ${matched.length} of ${plan.outputs.length} committed outputs`,
    );
  }
  const seen = new Set<number>();
  const proofs: ReceiptOut[] = [];
  for (const m of matched) {
    if (seen.has(m.index)) throw new SettlementRefusal(`output ${m.index} was signed twice`);
    seen.add(m.index);
    proofs.push(unblindOne(plan, m, keys));
  }
  return proofs;
}

function unblindOne(plan: SwapPlan, m: MatchedSignature, keys: Record<string, string>): ReceiptOut {
  const opening = plan.openings[m.index];
  const requested = plan.outputs[m.index];
  if (!opening || !requested) throw new SettlementRefusal(`matched output ${m.index} is out of range`);

  const keyHex = keys[String(opening.amount)];
  if (!keyHex) {
    throw new SettlementRefusal(`no mint key for denomination ${opening.amount}`);
  }
  let A;
  let B_;
  let C_;
  try {
    A = pointFromHex(keyHex);
    B_ = pointFromHex(requested.B_);
    C_ = pointFromHex(m.signature.C_);
  } catch (err) {
    throw new SettlementRefusal(`bad point in mint response for output ${m.index}: ${(err as Error).message}`);
  }

  const dleq = m.signature.dleq;
  if (!dleq || !dleq.e || !dleq.s) {
    throw new SettlementRefusal(`mint returned no DLEQ for output ${m.index}; refusing to publish`);
  }
  const dleqBytes = { e: hexToBytes(dleq.e), s: hexToBytes(dleq.s) };
  if (!verifyDLEQProof(dleqBytes, B_, C_, A)) {
    throw new SettlementRefusal(`DLEQ does not verify on the blinded signature for output ${m.index}`);
  }

  const rHex = opening.r;
  const r = BigInt("0x" + rHex);
  const C = unblindSignature(C_, r, A);
  if (!verifyDLEQProof_reblind(utf8ToBytes(opening.secret), { ...dleqBytes, r }, C, A)) {
    throw new SettlementRefusal(`DLEQ does not verify on the unblinded proof for output ${m.index}`);
  }

  const recipient = outputRecipient(opening.secret);
  if (recipient === null) {
    throw new SettlementRefusal(`output ${m.index} is not a plain P2PK condition`);
  }
  const expected = plan.recipients[m.index];
  if (expected !== undefined && !sameHex(xOnly(recipient), xOnly(expected))) {
    throw new SettlementRefusal(`output ${m.index} does not pay the committed destination`);
  }

  return {
    recipient,
    amount: opening.amount,
    id: opening.id,
    secret: opening.secret,
    C: bytesToHex(C.toBytes(true)),
    dleq: { e: dleq.e, s: dleq.s, r: rHex },
  };
}

/**
 * Build the kind-1452 content from the committee signature and the unblinded
 * proofs, mirroring `parseSettle`'s guards: a 32-byte bid, a 64-byte signature,
 * an outcome in {settle, refund}, and 1..64 proofs.
 */
export function buildCarrier(
  committee: CommitteeSignatureContent,
  proofs: ReceiptOut[],
): SettlementCarrierContent {
  if (!isLowerHex(committee.bid, 64)) {
    throw new SettlementRefusal("carrier bid is not 32-byte lowercase hex");
  }
  if (!isLowerHex(committee.signature, 128)) {
    throw new SettlementRefusal("carrier signature is not 64-byte lowercase hex");
  }
  if (committee.outcome !== "settle" && committee.outcome !== "refund") {
    throw new SettlementRefusal(`carrier outcome ${String(committee.outcome)}`);
  }
  if (proofs.length < 1 || proofs.length > 64) {
    throw new SettlementRefusal(`carrier must carry 1..64 proofs, got ${proofs.length}`);
  }
  return {
    schema: SETTLEMENT_SCHEMA,
    bid: committee.bid,
    outcome: committee.outcome,
    proofs: proofs.map(({ id, amount, secret, C, dleq }) => ({ id, amount, secret, C, dleq })),
    sig: committee.signature,
  };
}

/**
 * Build the signed kind-1452 event: tags `["bid", …]`, `["outcome", …]`,
 * `["p", <courier>]`, no anchor. Signed under `sk`, the facilitator's own key.
 */
export function buildCarrierEvent(
  sk: Uint8Array,
  carrier: SettlementCarrierContent,
  createdAt: number,
): NostrEvent {
  const pubkey = courierPubkey(sk);
  const event = finalizeEvent(
    {
      kind: KIND_SETTLEMENT_CARRIER,
      created_at: createdAt,
      tags: [
        ["bid", carrier.bid],
        ["outcome", carrier.outcome],
        ["p", pubkey],
      ],
      content: JSON.stringify(carrier),
    },
    sk,
  );
  return event as unknown as NostrEvent;
}

/** x-only pubkey for a secret key (the `p` tag value). */
export function courierPubkey(sk: Uint8Array): string {
  return getPublicKey(sk);
}
