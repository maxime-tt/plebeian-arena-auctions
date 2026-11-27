import { describe, expect, it } from "vitest";
import { secp256k1 } from "@noble/curves/secp256k1.js";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils.js";
import { blindMessage, createDLEQProof, pointFromHex, verifyDLEQProof } from "@cashu/cashu-ts";
import { matchSignatures } from "../src/swap.ts";
import type { BlindedOutput, MintBlindSignature } from "../src/types.ts";

const ID = "00".padEnd(16, "a");
const sig = (amount: number, extra: Partial<MintBlindSignature> = {}): MintBlindSignature => ({
  amount,
  id: ID,
  C_: "02" + "cc".repeat(32),
  ...extra,
});

describe("matchSignatures matches by (amount, id), never by position", () => {
  const requested: BlindedOutput[] = [
    { amount: 8, id: ID, B_: "02" + "11".repeat(32) },
    { amount: 2, id: ID, B_: "02" + "22".repeat(32) },
    { amount: 4, id: ID, B_: "02" + "33".repeat(32) },
  ];

  it("associates a reordered response with the right request", () => {
    const matched = matchSignatures(requested, [sig(4), sig(8)]);
    expect(matched.map((m) => m.index)).toEqual([2, 0]);
  });

  it("skips a returned output the request did not ask for", () => {
    const matched = matchSignatures(requested, [sig(16), sig(2)]);
    expect(matched).toHaveLength(1);
    expect(matched[0]!.index).toBe(1);
  });

  it("handles a shorter response (the mint may return fewer)", () => {
    const matched = matchSignatures(requested, [sig(8)]);
    expect(matched).toHaveLength(1);
    expect(matched[0]!.index).toBe(0);
  });

  it("disambiguates two outputs sharing (amount, id) by their DLEQ", () => {
    const Apriv = secp256k1.utils.randomSecretKey();
    const keyHex = bytesToHex(secp256k1.getPublicKey(Apriv, true));
    const aScalar = BigInt("0x" + bytesToHex(Apriv));
    const r1 = BigInt("0x" + "11".repeat(32));
    const r2 = BigInt("0x" + "22".repeat(32));
    const B1 = bytesToHex(blindMessage(utf8ToBytes("secret-one"), r1).B_.toBytes(true));
    const B2 = bytesToHex(blindMessage(utf8ToBytes("secret-two"), r2).B_.toBytes(true));
    const dup: BlindedOutput[] = [
      { amount: 8, id: ID, B_: B1 },
      { amount: 8, id: ID, B_: B2 },
    ];
    // The mint's signature is over B2 only.
    const C_ = bytesToHex(pointFromHex(B2).multiply(aScalar).toBytes(true));
    const dleq = createDLEQProof(pointFromHex(B2), Apriv);
    const response: MintBlindSignature[] = [
      { amount: 8, id: ID, C_, dleq: { e: bytesToHex(dleq.e), s: bytesToHex(dleq.s) } },
    ];
    const tieBreak = (index: number, s: MintBlindSignature): boolean =>
      s.dleq !== undefined &&
      verifyDLEQProof(
        { e: hexToBytes(s.dleq.e), s: hexToBytes(s.dleq.s) },
        pointFromHex(dup[index]!.B_),
        pointFromHex(s.C_),
        pointFromHex(keyHex),
      );
    const matched = matchSignatures(dup, response, tieBreak);
    expect(matched).toHaveLength(1);
    expect(matched[0]!.index).toBe(1);
  });
});
