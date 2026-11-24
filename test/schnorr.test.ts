import { describe, expect, it } from "vitest";
import { schnorr } from "@noble/curves/secp256k1.js";
import { hexToBytes } from "@noble/hashes/utils.js";
import { schnorrVerifyDigest } from "../src/sigall.ts";
import {
  anchorRefusal,
  parseCommitteeContent,
  verifyCommitteeSignature,
} from "../src/relay.ts";
import { committeeEvent, makeSettleFixture, type Fixture } from "./fixtures.ts";
import type { StateCommitmentContent } from "../src/types.ts";

describe("schnorr verification", () => {
  it("verifies a BIP-340 signature over a digest under the group key", () => {
    const f = makeSettleFixture();
    const digest = hexToBytes(f.committee.digest);
    // The fixture signed with the committee key; verified against its x-only form.
    expect(schnorrVerifyDigest(digest, f.keys.committeeXOnly, f.committee.signature)).toBe(true);
    expect(schnorrVerifyDigest(digest, f.keys.committeePub, f.committee.signature)).toBe(true);
  });

  it("refuses a wrong key, a wrong digest, and a mangled signature", () => {
    const f = makeSettleFixture();
    const digest = hexToBytes(f.committee.digest);
    const other = makeSettleFixture();
    expect(schnorrVerifyDigest(digest, other.keys.committeePub, f.committee.signature)).toBe(false);
    expect(schnorrVerifyDigest(hexToBytes("00".repeat(32)), f.keys.committeePub, f.committee.signature)).toBe(false);
    const bad = "00" + f.committee.signature.slice(2);
    expect(schnorrVerifyDigest(digest, f.keys.committeePub, bad)).toBe(false);
  });

  it("signs a 1451 with the validator key, not the group key (two distinct roles)", () => {
    const f = makeSettleFixture();
    const digest = hexToBytes(f.committee.digest);
    // A signature by the event-signing validator key is NOT the committee signature.
    expect(schnorrVerifyDigest(digest, f.keys.validatorPub, f.committee.signature)).toBe(false);
    expect(schnorr.verify(hexToBytes(f.committee.signature.slice(0, 128)), digest, hexToBytes(f.keys.committeeXOnly))).toBe(true);
  });
});

describe("verifyCommitteeSignature (kind 1451)", () => {
  const cfg = (f: Fixture) => ({
    committeeGroupPubkey: f.keys.committeeXOnly,
    validatorRoster: [f.keys.validatorPub],
  });

  it("accepts a well-formed, roster-signed, group-signed 1451", () => {
    const f = makeSettleFixture();
    const res = verifyCommitteeSignature(f.event, cfg(f));
    expect(res.ok).toBe(true);
    if (res.ok) {
      expect(res.value.content.digest).toBe(f.committee.digest);
      expect(res.value.anchor).toEqual({ height: 100, blockHash: f.committee.block_hash });
    }
  });

  it("refuses a signer outside the roster", () => {
    const f = makeSettleFixture();
    const res = verifyCommitteeSignature(f.event, { committeeGroupPubkey: f.keys.committeeXOnly, validatorRoster: [] });
    expect(res.ok).toBe(false);
  });

  it("refuses a digest that does not recompute from the content", () => {
    const f = makeSettleFixture();
    const tampered = { ...f.committee, digest: "11".repeat(32) };
    const res = verifyCommitteeSignature(committeeEvent(tampered, f.keys.validatorPub), cfg(f));
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.reason).toMatch(/digest/);
  });

  it("refuses a tag that disagrees with the content", () => {
    const f = makeSettleFixture();
    const ev = committeeEvent(f.committee, f.keys.validatorPub);
    ev.tags = ev.tags.map((t) => (t[0] === "height" ? ["height", "999"] : t));
    const res = verifyCommitteeSignature(ev, cfg(f));
    expect(res.ok).toBe(false);
  });

  it("refuses a non-1451 event and bare/garbage content", () => {
    const f = makeSettleFixture();
    expect(verifyCommitteeSignature({ ...f.event, kind: 1443 }, cfg(f)).ok).toBe(false);
    expect(parseCommitteeContent("not json")).toHaveProperty("error");
    expect(parseCommitteeContent("{}")).toHaveProperty("error");
  });
});

describe("anchorRefusal", () => {
  const commitment = (height: number): StateCommitmentContent => ({
    schema: "arena.state.v1",
    height,
    app_hash: "aa".repeat(32),
    time: 1000,
  });

  it("refuses a 1451 anchored above the highest committed height", () => {
    const f = makeSettleFixture();
    expect(anchorRefusal(f.committee, [commitment(99)])).toMatch(/exceeds/);
    expect(anchorRefusal(f.committee, [commitment(100)])).toBeNull();
    expect(anchorRefusal(f.committee, [])).toBeNull();
  });
});
