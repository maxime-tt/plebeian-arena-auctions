import { describe, expect, it } from "vitest";
import { buildSwapBody, planSwap, type SwapPlan } from "../src/swap.ts";
import { recomputeOutputs } from "../src/sigall.ts";
import { makeSettleFixture, type Fixture } from "./fixtures.ts";

function plan(f: Fixture): SwapPlan {
  const res = planSwap({
    committee: f.committee,
    bid: f.bid,
    create: f.create,
    committeeGroupPubkey: f.keys.committeeXOnly,
  });
  if (!res.ok) throw new Error(res.reason);
  return res.plan;
}

describe("planSwap", () => {
  it("plans a swap from genuine committed facts", () => {
    const f = makeSettleFixture();
    const p = plan(f);
    expect(p.outcome).toBe("settle");
    expect(p.locktime).toBe(f.locktime);
    expect(p.digest).toBe(f.committee.digest);
    expect(p.inputs).toHaveLength(2);
    expect(p.outputs).toHaveLength(2);
  });

  it("declines when a committee output does not match the committed template", () => {
    const f = makeSettleFixture();
    const committee = structuredClone(f.committee);
    committee.outputs[0]!.B_ = "02" + "11".repeat(32);
    const res = planSwap({ committee, bid: f.bid, create: f.create, committeeGroupPubkey: f.keys.committeeXOnly });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.reason).toMatch(/output 0/);
  });

  it("declines when an output is not locked to the committed destination", () => {
    const f = makeSettleFixture();
    const bid = structuredClone(f.bid);
    // Re-lock the first settle output to a different key, and re-sign nothing:
    // the recomputed B_ and the digest will no longer line up, so it refuses.
    bid.outcome_templates!.settle.outputs[0]!.secret = bid.outcome_templates!.settle.outputs[0]!.secret.replace(
      /"data":"[0-9a-f]{66}"/,
      `"data":"03${"bb".repeat(32)}"`,
    );
    const res = planSwap({ committee: f.committee, bid, create: f.create, committeeGroupPubkey: f.keys.committeeXOnly });
    expect(res.ok).toBe(false);
  });

  it("declines a wrong bidder signature", () => {
    const f = makeSettleFixture();
    const bid = structuredClone(f.bid);
    bid.outcome_templates!.settle.bidder_signature = "00".repeat(64);
    const res = planSwap({ committee: f.committee, bid, create: f.create, committeeGroupPubkey: f.keys.committeeXOnly });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.reason).toMatch(/bidder signature/);
  });

  it("declines a wrong committee signature", () => {
    const f = makeSettleFixture();
    const committee = structuredClone(f.committee);
    committee.signature = "00".repeat(64);
    const res = planSwap({ committee, bid: f.bid, create: f.create, committeeGroupPubkey: f.keys.committeeXOnly });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.reason).toMatch(/committee signature/);
  });

  it("declines an escrow that is not the committed lock (data key)", () => {
    const f = makeSettleFixture();
    const bid = structuredClone(f.bid);
    const other = makeSettleFixture();
    // Swap in another bidder's escrow secret: the lock data no longer matches the wallet.
    bid.proofs[0]!.secret = other.bid.proofs[0]!.secret;
    const res = planSwap({ committee: f.committee, bid, create: f.create, committeeGroupPubkey: f.keys.committeeXOnly });
    expect(res.ok).toBe(false);
  });

  it("plans a settle whose committed V4V split pays the seller and a row recipient", () => {
    const splitRecipient = "02" + "33".repeat(32);
    const f = makeSettleFixture({ splitRecipient });
    const p = plan(f);
    expect(p.outcome).toBe("settle");
    expect(p.outputs).toHaveLength(2);
    // Each output keeps its own committed lock, not one key per index.
    expect(p.recipients).toHaveLength(2);
    expect(p.recipients[0]).toBe(f.create.sellerWallet);
    expect(p.recipients[1]).toBe(splitRecipient);
    const body = buildSwapBody(p);
    expect(body.outputs.map((o) => ({ amount: o.amount, B_: o.B_ }))).toEqual(recomputeOutputs(f.settleOutputs));
    const w = JSON.parse(body.inputs[0]!.witness!) as { signatures: string[] };
    expect(w.signatures).toEqual([p.bidderSignature, p.committeeSignature]);
  });

  it("declines a split row that names no recipient", () => {
    const f = makeSettleFixture({ splitRecipient: "02" + "33".repeat(32) });
    const create = structuredClone(f.create);
    create.v4vSplit = [{ role: "v4v", recipientPubkey: "", allocationBps: 5000 }];
    const res = planSwap({ committee: f.committee, bid: f.bid, create, committeeGroupPubkey: f.keys.committeeXOnly });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.reason).toMatch(/split row names no recipient/);
  });

  it("declines a template output that is not locked to an allowed destination", () => {
    const f = makeSettleFixture();
    // The committed seller payout key is not the one the outputs lock to:
    // recomputed B_ and digest still line up, so it is refused at the lock check.
    const create = { ...f.create, sellerWallet: "03" + "44".repeat(32) };
    const res = planSwap({ committee: f.committee, bid: f.bid, create, committeeGroupPubkey: f.keys.committeeXOnly });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.reason).toMatch(/not locked to an allowed committed destination/);
  });
});

describe("buildSwapBody (contract §4)", () => {
  it("puts the witness on inputs[0] only, bidder first then committee", () => {
    const f = makeSettleFixture();
    const p = plan(f);
    const body = buildSwapBody(p);
    expect(body.inputs).toHaveLength(2);
    expect(body.inputs[0]!.witness).toBeDefined();
    expect(body.inputs[1]!.witness).toBeUndefined();
    const w = JSON.parse(body.inputs[0]!.witness!) as { signatures: string[] };
    expect(w.signatures).toEqual([p.bidderSignature, p.committeeSignature]);
  });

  it("sends the committed outputs (amount, id, B_) in template order", () => {
    const f = makeSettleFixture();
    const p = plan(f);
    const body = buildSwapBody(p);
    const expected = recomputeOutputs(f.settleOutputs);
    expect(body.outputs.map((o) => ({ amount: o.amount, B_: o.B_ }))).toEqual(expected);
    expect(body.outputs.every((o) => o.id === f.settleOutputs[0]!.id)).toBe(true);
  });

  it("carries the escrow proofs as the inputs", () => {
    const f = makeSettleFixture();
    const body = buildSwapBody(plan(f));
    expect(body.inputs.map((i) => ({ amount: i.amount, id: i.id, secret: i.secret, C: i.C }))).toEqual(
      f.escrowProofs.map((i) => ({ amount: i.amount, id: i.id, secret: i.secret, C: i.C })),
    );
  });
});
