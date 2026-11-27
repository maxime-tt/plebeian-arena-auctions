import { describe, expect, it } from "vitest";
import { generateSecretKey, getPublicKey } from "nostr-tools";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils.js";
import { pointFromHex, unblindSignature, verifyDLEQProof_reblind } from "@cashu/cashu-ts";
import { buildCarrier, buildCarrierEvent, SettlementRefusal, unblindToReceipts } from "../src/settle.ts";
import { outputRecipient } from "../src/sigall.ts";
import { matchSignatures, type SwapPlan } from "../src/swap.ts";
import type { CommitteeSignatureContent, MintBlindSignature } from "../src/types.ts";
import { makeMintVector, type MintVector } from "./fixtures.ts";

function planFor(v: MintVector): SwapPlan {
  return {
    outcome: "settle",
    locktime: 1,
    digest: "00".repeat(32),
    bidderSignature: "aa".repeat(64),
    committeeSignature: "bb".repeat(64),
    inputs: [],
    outputs: [{ amount: v.amount, id: v.id, B_: v.B_ }],
    openings: [{ amount: v.amount, id: v.id, secret: v.secret, r: v.rHex }],
    recipients: [v.recipient],
  };
}

function signatureFor(v: MintVector): MintBlindSignature {
  return { amount: v.amount, id: v.id, C_: v.C_, dleq: v.dleq };
}

describe("unblinding (contract §4)", () => {
  it("recovers C = C_ − r·K and the DLEQ against a known vector", () => {
    const v = makeMintVector();
    const plan = planFor(v);
    const matched = matchSignatures(plan.outputs, [signatureFor(v)]);
    const keys = { [String(v.amount)]: v.keyHex };

    const proofs = unblindToReceipts(plan, matched, keys);
    expect(proofs).toHaveLength(1);
    const proof = proofs[0]!;

    const expectedC = bytesToHex(
      unblindSignature(pointFromHex(v.C_), BigInt("0x" + v.rHex), pointFromHex(v.keyHex)).toBytes(true),
    );
    expect(proof.C).toBe(expectedC);
    expect(proof.amount).toBe(v.amount);
    expect(proof.id).toBe(v.id);
    expect(proof.dleq?.r).toBe(v.rHex);
    expect(proof.recipient).toBe(outputRecipient(v.secret));
  });

  it("produces a proof whose DLEQ verifies after reblinding", () => {
    const v = makeMintVector();
    const plan = planFor(v);
    const proofs = unblindToReceipts(plan, matchSignatures(plan.outputs, [signatureFor(v)]), {
      [String(v.amount)]: v.keyHex,
    });
    const p = proofs[0]!;
    const ok = verifyDLEQProof_reblind(
      utf8ToBytes(p.secret),
      { e: hexToBytes(p.dleq!.e), s: hexToBytes(p.dleq!.s), r: BigInt("0x" + p.dleq!.r) },
      pointFromHex(p.C),
      pointFromHex(v.keyHex),
    );
    expect(ok).toBe(true);
  });

  it("refuses when the mint gave no DLEQ", () => {
    const v = makeMintVector();
    const plan = planFor(v);
    const sig = { ...signatureFor(v), dleq: undefined };
    expect(() => unblindToReceipts(plan, matchSignatures(plan.outputs, [sig]), { [String(v.amount)]: v.keyHex })).toThrow(
      SettlementRefusal,
    );
  });

  it("refuses a partial response (fewer than the committed outputs)", () => {
    const v = makeMintVector();
    const plan = planFor(v);
    plan.outputs.push({ amount: 32, id: v.id, B_: "02" + "77".repeat(32) });
    plan.openings.push({ amount: 32, id: v.id, secret: v.secret, r: v.rHex });
    plan.recipients.push(v.recipient);
    expect(() => unblindToReceipts(plan, matchSignatures(plan.outputs, [signatureFor(v)]), { [String(v.amount)]: v.keyHex })).toThrow(
      /signed 1 of 2/,
    );
  });

  it("refuses a recipient that is not the committed destination", () => {
    const v = makeMintVector();
    const plan = planFor(v);
    plan.recipients = ["02" + "99".repeat(32)];
    expect(() => unblindToReceipts(plan, matchSignatures(plan.outputs, [signatureFor(v)]), { [String(v.amount)]: v.keyHex })).toThrow(
      /committed destination/,
    );
  });
});

describe("buildCarrier (contract §3/§5)", () => {
  const committee: CommitteeSignatureContent = {
    schema: "arena.committee_signature.v1",
    bid: "ab".repeat(32),
    auction: "1440:" + "cd".repeat(32) + ":d",
    outcome: "settle",
    height: 100,
    block_hash: "ef".repeat(32),
    app_hash: "01".repeat(32),
    digest: "02".repeat(32),
    signature: "bb".repeat(64),
    inputs: [],
    outputs: [],
  };
  const proof = {
    recipient: "02" + "aa".repeat(32),
    amount: 64,
    id: "00".padEnd(16, "a"),
    secret: "s",
    C: "02" + "11".repeat(32),
    dleq: { e: "22".repeat(32), s: "33".repeat(32), r: "44".repeat(32) },
  };

  it("builds the arena.settlement.v1 content with the committee signature", () => {
    const carrier = buildCarrier(committee, [proof]);
    expect(carrier.schema).toBe("arena.settlement.v1");
    expect(carrier.bid).toBe(committee.bid);
    expect(carrier.sig).toBe(committee.signature);
    // The chain's carrier parser is DisallowUnknownFields over `settleTx`, whose proofs are
    // Cashu proofs — so `recipient` (derivable from `secret`) must NOT ride along. Without
    // this the live chain dropped every carrier: 'json: unknown field "recipient"'.
    const { recipient: _recipient, ...chainProof } = proof;
    expect(carrier.proofs).toEqual([chainProof]);
    expect(Object.keys(carrier.proofs[0]!).sort()).toEqual(["C", "amount", "dleq", "id", "secret"]);
  });

  it("refuses an over-long proof list", () => {
    expect(() => buildCarrier(committee, Array(65).fill(proof))).toThrow(SettlementRefusal);
    expect(() => buildCarrier(committee, [])).toThrow(SettlementRefusal);
  });

  it("refuses a signature that is not 64 bytes", () => {
    expect(() => buildCarrier({ ...committee, signature: "00".repeat(32) }, [proof])).toThrow(SettlementRefusal);
  });

  it("signs a 1452 event under the courier key with the contract's tags", () => {
    const carrier = buildCarrier(committee, [proof]);
    const sk = generateSecretKey();
    const ev = buildCarrierEvent(sk, carrier, 1700);
    expect(ev.kind).toBe(1452);
    expect(ev.pubkey).toBe(getPublicKey(sk));
    expect(ev.tags).toEqual([
      ["bid", carrier.bid],
      ["outcome", carrier.outcome],
      ["p", getPublicKey(sk)],
    ]);
    expect(JSON.parse(ev.content)).toEqual(carrier);
    expect(ev.id).toMatch(/^[0-9a-f]{64}$/);
    expect(ev.sig).toMatch(/^[0-9a-f]{128}$/);
  });
});
