/**
 * Assemble and execute the mint swap (contract §4).
 *
 * Everything the facilitator needs is public: the create (the auction's
 * committed seller key and split), the bid (the escrow proofs and the
 * bidder's pre-signed template), and the committee signature. It verifies all
 * of it locally FIRST — the bidder's and the committee's signatures on the
 * reconstructed digest, and the outputs' destinations — and refuses, with a
 * named reason, on any mismatch. It never improvises an output.
 */

import { hexToBytes } from "@noble/hashes/utils.js";
import {
  outputBPoint,
  outputRecipient,
  parseP2PK,
  recomputeOutputs,
  schnorrVerifyDigest,
  sigAllDigestHex,
  xOnly,
} from "./sigall.ts";
import type {
  BidContent,
  BlindedOutput,
  CommitteeSignatureContent,
  CreateContent,
  MintBlindSignature,
  SwapInputProof,
  SwapRequestBody,
  TemplateOutput,
} from "./types.ts";

export interface SwapPlan {
  outcome: "settle" | "refund";
  /** The escrow lock's locktime (the settlement window's end). */
  locktime: number;
  /** The layout-B digest both signatures cover. */
  digest: string;
  bidderSignature: string;
  committeeSignature: string;
  /** Inputs with the witness on index 0 only. */
  inputs: SwapInputProof[];
  /** Blinded outputs, in committed template order. */
  outputs: BlindedOutput[];
  /** Per-output opening (secret + r) needed to unblind the mint's response. */
  openings: TemplateOutput[];
  /** Per-output committed destination (compressed), for the local check. */
  recipients: string[];
}

export type PlanResult = { ok: true; plan: SwapPlan } | { ok: false; reason: string };

const refuse = (reason: string): PlanResult => ({ ok: false, reason });

function sameHex(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}

/**
 * Verify the escrow lock is the one 2-of-2 SIG_ALL shape the contract names:
 * `data` = the bidder, `pubkeys` = the committee, `n_sigs` = 2, `sigflag`
 * SIG_ALL, and a positive `locktime`. Signer set = [data] ∪ tags.pubkeys
 * (contract amendment), so the bidder key is `data` and the committee key is
 * the group pubkey.
 */
function checkEscrowLock(
  first: { secret: string } | undefined,
  bidderWallet: string,
  committeeGroupPubkey: string,
  proofs: { secret: string }[],
): { locktime: number } | { reason: string } {
  if (!first) return { reason: "bid has no escrow proofs" };
  const lock = parseP2PK(first.secret);
  if (!lock) return { reason: "escrow secret is not a strict P2PK condition" };
  if (lock.sigflag !== "SIG_ALL") return { reason: "escrow lock is not SIG_ALL" };
  if (lock.nSigs !== 2) return { reason: `escrow lock needs exactly 2 signatures, has ${lock.nSigs}` };
  if (lock.locktime <= 0) return { reason: "escrow lock has no locktime" };
  if (lock.pubkeys.length !== 1) return { reason: "escrow lock must name exactly one committee key" };
  if (!sameHex(xOnly(lock.data), xOnly(bidderWallet))) {
    return { reason: "escrow lock data is not the bidder wallet key" };
  }
  if (!sameHex(xOnly(lock.pubkeys[0] as string), xOnly(committeeGroupPubkey))) {
    return { reason: "escrow lock pubkeys is not the committee group key" };
  }
  if (!lock.refund.some((r) => sameHex(xOnly(r), xOnly(bidderWallet)))) {
    return { reason: "escrow lock refund is not the bidder" };
  }
  for (let i = 1; i < proofs.length; i++) {
    const l = parseP2PK(proofs[i]!.secret);
    if (!l) return { reason: `escrow proof ${i} is not a strict P2PK condition` };
    if (l.data !== lock.data || l.tagsJson !== lock.tagsJson) {
      return { reason: `escrow proof ${i} does not share the first proof's condition` };
    }
  }
  return { locktime: lock.locktime };
}

/**
 * The committed destination of each template output. Returns one entry per
 * output. A settle with a committed V4V split needs the chain's denomination
 * splitter to reproduce per-row amounts, which this courier does not implement;
 * it refuses rather than guess a destination (the recipient's wallet can still
 * execute that template).
 */
function committedDestinations(
  bid: BidContent,
  create: CreateContent,
  outcome: "settle" | "refund",
  outputCount: number,
): { destinations: string[] } | { reason: string } {
  if (outcome === "refund") {
    if (!bid.wallet) return { reason: "bid committed no wallet spend key" };
    return { destinations: new Array(outputCount).fill(bid.wallet) };
  }
  if (create.v4vSplit && create.v4vSplit.length > 0) {
    return { reason: "settle with a committed V4V split is not supported by the courier" };
  }
  if (!create.sellerWallet) return { reason: "auction committed no seller payout key" };
  return { destinations: new Array(outputCount).fill(create.sellerWallet) };
}

/**
 * Verify a committee signature and its committed template against the create
 * and bid, and produce the exact swap plan — or a named refusal. No network.
 */
export function planSwap(args: {
  committee: CommitteeSignatureContent;
  bid: BidContent;
  create: CreateContent;
  committeeGroupPubkey: string;
}): PlanResult {
  const { committee, bid, create, committeeGroupPubkey } = args;
  const outcome = committee.outcome;

  const templates = bid.outcome_templates;
  if (!templates) return refuse("bid publishes no outcome templates");
  const template = outcome === "settle" ? templates.settle : templates.refund;
  if (!template) return refuse(`bid has no ${outcome} template`);

  if (template.digest && !sameHex(template.digest, committee.digest)) {
    return refuse("committee digest does not match the committed template digest");
  }

  const proofs = bid.proofs;
  if (proofs.length === 0) return refuse("bid has no escrow proofs");
  const inputs = committee.inputs;
  if (inputs.length !== proofs.length || inputs.length !== template.inputs.length) {
    return refuse("committee inputs, bid proofs and template inputs differ in count");
  }

  const lock = checkEscrowLock(proofs[0], bid.wallet, committeeGroupPubkey, proofs);
  if ("reason" in lock) return refuse(lock.reason);

  // Inputs: the same escrow, in committed order, agreed by all three sources.
  const swapInputs: SwapInputProof[] = [];
  for (let i = 0; i < proofs.length; i++) {
    const p = proofs[i]!;
    const ci = inputs[i]!;
    const ti = template.inputs[i]!;
    if (!sameHex(p.secret, ci.secret) || p.C !== ci.C) {
      return refuse(`committee input ${i} is not the committed escrow proof`);
    }
    if (!sameHex(p.secret, ti.secret) || p.C !== ti.C || p.amount !== ti.amount || p.id !== ti.id) {
      return refuse(`template input ${i} is not the committed escrow proof`);
    }
    swapInputs.push({ amount: p.amount, id: p.id, secret: p.secret, C: p.C });
  }

  // Outputs: recompute B_ from the committed openings; the committee's outputs
  // and the template's must both match, in order.
  const recomputed = recomputeOutputs(template.outputs);
  if (committee.outputs.length !== template.outputs.length) {
    return refuse("committee outputs and template outputs differ in count");
  }
  const blinded: BlindedOutput[] = [];
  for (let j = 0; j < template.outputs.length; j++) {
    const out = template.outputs[j]!;
    const co = committee.outputs[j]!;
    const b = recomputed[j]!;
    if (co.amount !== out.amount || co.B_ !== b.B_) {
      return refuse(`committee output ${j} does not match the committed template output`);
    }
    if (!out.id) return refuse(`template output ${j} names no keyset`);
    blinded.push({ amount: out.amount, id: out.id, B_: b.B_ });
  }

  // Digest: recompute and require both signatures to cover it.
  const digest = sigAllDigestHex(committee.inputs, committee.outputs);
  if (digest !== committee.digest) return refuse("digest does not recompute from the committee inputs and outputs");

  const firstLock = parseP2PK(proofs[0]!.secret)!;
  const digestBytes = hexToBytes(digest);
  if (!schnorrVerifyDigest(digestBytes, firstLock.data, template.bidder_signature)) {
    return refuse("bidder signature does not verify on the reconstructed digest");
  }
  if (!schnorrVerifyDigest(digestBytes, committeeGroupPubkey, committee.signature)) {
    return refuse("committee signature does not verify on the reconstructed digest");
  }

  // Destinations: every output pays the committed key for its index.
  const dests = committedDestinations(bid, create, outcome, template.outputs.length);
  if ("reason" in dests) return refuse(dests.reason);
  const recipients = dests.destinations;
  for (let j = 0; j < template.outputs.length; j++) {
    const recipient = outputRecipient(template.outputs[j]!.secret);
    if (recipient === null) return refuse(`template output ${j} is not a plain P2PK condition`);
    const expected = recipients[j]!;
    if (!sameHex(xOnly(recipient), xOnly(expected))) {
      return refuse(`template output ${j} is not locked to the committed destination`);
    }
  }

  // The witness rides on inputs[0] only (contract §4): bidder first, committee second.
  const witness = JSON.stringify({
    signatures: [template.bidder_signature, committee.signature],
  });
  const inputsWithWitness: SwapInputProof[] = swapInputs.map((inp, i) =>
    i === 0 ? { ...inp, witness } : inp,
  );

  return {
    ok: true,
    plan: {
      outcome,
      locktime: lock.locktime,
      digest,
      bidderSignature: template.bidder_signature,
      committeeSignature: committee.signature,
      inputs: inputsWithWitness,
      outputs: blinded,
      openings: template.outputs,
      recipients,
    },
  };
}

/** The exact `/v1/swap` request body for a plan (contract §4). */
export function buildSwapBody(plan: SwapPlan): SwapRequestBody {
  return { inputs: plan.inputs, outputs: plan.outputs };
}

export interface MatchedSignature {
  /** Index into the requested output list. */
  index: number;
  signature: MintBlindSignature;
}

/**
 * Match the mint's returned signatures to the requested outputs by
 * `(amount, id)` — NEVER by position (contract §4: the mint may return fewer,
 * or reorder). `tieBreak` disambiguates two requested outputs that share an
 * `(amount, id)`, typically by verifying the DLEQ against the candidate B_.
 */
export function matchSignatures(
  requested: BlindedOutput[],
  response: MintBlindSignature[],
  tieBreak?: (index: number, sig: MintBlindSignature) => boolean,
): MatchedSignature[] {
  const used = new Set<number>();
  const matched: MatchedSignature[] = [];
  for (const sig of response) {
    const candidates: number[] = [];
    for (let i = 0; i < requested.length; i++) {
      if (used.has(i)) continue;
      const r = requested[i]!;
      if (r.amount === sig.amount && r.id === sig.id) candidates.push(i);
    }
    if (candidates.length === 0) continue;
    let pick = candidates[0]!;
    if (candidates.length > 1 && tieBreak) {
      const verified = candidates.find((i) => tieBreak(i, sig));
      if (verified !== undefined) pick = verified;
    }
    used.add(pick);
    matched.push({ index: pick, signature: sig });
  }
  return matched;
}
