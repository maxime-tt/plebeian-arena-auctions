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
