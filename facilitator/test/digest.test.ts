import { describe, expect, it } from "vitest";
import { outputBPoint, sigAllDigestHex, sigAllMessage } from "../src/sigall.ts";

// The chain's frozen cross-language golden vector (chain/sigall.go,
// chain/sigall_test.go). A drift here is a facilitator that disagrees with the
// mint and the validators, so it is pinned byte for byte.
const GOLDEN_INPUTS = [
  { secret: "sigall-golden-secret-0001", C: "02" + "cc".repeat(32) },
  { secret: "sigall-golden-secret-0002", C: "03" + "dd".repeat(32) },
];
const GOLDEN_OUTPUTS = [
  { amount: 1, B_: "02" + "aa".repeat(32) },
  { amount: 2, B_: "03" + "bb".repeat(32) },
];
const GOLDEN_DIGEST = "d3403843d4f3ead4a341f02476b14f140699d346054435fd38d27c294130bec3";

const GOLDEN_POINT_SECRET =
  `["P2PK",{"nonce":"${"00".repeat(16)}","data":"02${"aa".repeat(32)}","tags":[["sigflag","SIG_INPUTS"]]}]`;
const GOLDEN_POINT_R = "11".repeat(32);
const GOLDEN_OUTPUT_POINT = "0336bf38b85ee80f36950f9a05f22c0f36576118dcb30b5300b5a9f73bd41af17e";

describe("SIG_ALL digest (layout B)", () => {
  it("matches the chain's golden digest", () => {
    expect(sigAllDigestHex(GOLDEN_INPUTS, GOLDEN_OUTPUTS)).toBe(GOLDEN_DIGEST);
  });

  it("builds the message as secret||C then amount||B_, no separators", () => {
    expect(sigAllMessage(GOLDEN_INPUTS, GOLDEN_OUTPUTS)).toBe(
      "sigall-golden-secret-0001" + "02" + "cc".repeat(32) +
        "sigall-golden-secret-0002" + "03" + "dd".repeat(32) +
        "1" + "02" + "aa".repeat(32) +
        "2" + "03" + "bb".repeat(32),
    );
  });

  it("rejects colliding layouts (input C spliced into the output list)", () => {
    const other = sigAllDigestHex(GOLDEN_INPUTS, GOLDEN_OUTPUTS.slice(1));
    expect(other).not.toBe(GOLDEN_DIGEST);
  });
});

describe("B_ derivation", () => {
  it("matches the chain's golden output point", () => {
    expect(outputBPoint(GOLDEN_POINT_SECRET, GOLDEN_POINT_R)).toBe(GOLDEN_OUTPUT_POINT);
  });

  it("is deterministic and differs with r", () => {
    const a = outputBPoint(GOLDEN_POINT_SECRET, GOLDEN_POINT_R);
    const b = outputBPoint(GOLDEN_POINT_SECRET, "22".repeat(32));
    expect(a).toBe(GOLDEN_OUTPUT_POINT);
    expect(b).not.toBe(a);
  });
});
