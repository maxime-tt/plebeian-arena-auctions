/**
 * Offline fixtures: a valid create/bid/committee-signature triple and a mint
 * keypair, built from real keys with the project's own primitives. No network.
 */

import { secp256k1, schnorr } from "@noble/curves/secp256k1.js";
import { bytesToHex, hexToBytes, utf8ToBytes } from "@noble/hashes/utils.js";
import { blindMessage, createDLEQProof, pointFromHex } from "@cashu/cashu-ts";
import { recomputeOutputs, sigAllDigestHex, xOnly } from "../src/sigall.ts";
import type {
  BidContent,
  CommitteeSignatureContent,
  CreateContent,
  EscrowProof,
  NostrEvent,
  OutcomeTemplate,
  TemplateOutput,
} from "../src/types.ts";
import { KIND_COMMITTEE_SIGNATURE } from "../src/relay.ts";

const KEYSET_ID = "00".padEnd(16, "a");

const randScalar = (): Uint8Array => secp256k1.utils.randomSecretKey();
const compressedPub = (priv: Uint8Array): string => bytesToHex(secp256k1.getPublicKey(priv, true));
const randPoint = (): string => compressedPub(randScalar());

export function p2pkSecret(data: string, tags: string[][], nonce = bytesToHex(randScalar())): string {
  return JSON.stringify(["P2PK", { nonce, data, tags }]);
}

export interface Keys {
  bidderPriv: Uint8Array;
  bidderWallet: string; // compressed
  committeePriv: Uint8Array;
  committeePub: string; // compressed
  committeeXOnly: string;
  validatorPriv: Uint8Array;
  validatorPub: string; // x-only (event signer)
}

export function makeKeys(): Keys {
  const bidderPriv = randScalar();
  const committeePriv = randScalar();
  const validatorPriv = randScalar();
  const committeePub = compressedPub(committeePriv);
  return {
    bidderPriv,
    bidderWallet: compressedPub(bidderPriv),
    committeePriv,
    committeePub,
    committeeXOnly: xOnly(committeePub),
    validatorPriv,
    validatorPub: xOnly(compressedPub(validatorPriv)),
  };
}

export function escrowSecret(keys: Keys, locktime: number, nonce?: string): string {
  return p2pkSecret(
    keys.bidderWallet,
    [
      ["locktime", String(locktime)],
      ["pubkeys", keys.committeePub],
      ["n_sigs", "2"],
      ["refund", keys.bidderWallet],
      ["sigflag", "SIG_ALL"],
    ],
    nonce,
  );
}

export function destinationSecret(recipient: string, nonce = bytesToHex(randScalar())): string {
  return p2pkSecret(recipient, [["sigflag", "SIG_INPUTS"]], nonce);
}

export interface Fixture {
  keys: Keys;
  locktime: number;
  bid: BidContent;
  create: CreateContent;
  committee: CommitteeSignatureContent;
  event: NostrEvent;
  escrowProofs: EscrowProof[];
  settleOutputs: TemplateOutput[];
}

/**
 * Build a settle fixture whose digest and signatures are genuine: recomputed
 * from the committed inputs and outputs with the project's own `sigall`
 * helpers, then signed with the bidder and committee keys.
 */
export function makeSettleFixture(overrides: { locktime?: number } = {}): Fixture {
  const keys = makeKeys();
  const locktime = overrides.locktime ?? Math.floor(Date.now() / 1000) + 3600;
  const sellerWallet = randPoint();

  const escrowProofs: EscrowProof[] = [
    { id: KEYSET_ID, amount: 64, secret: escrowSecret(keys, locktime), C: randPoint() },
    { id: KEYSET_ID, amount: 32, secret: escrowSecret(keys, locktime), C: randPoint() },
  ];

  const settleOutputs: TemplateOutput[] = [
    { amount: 64, id: KEYSET_ID, secret: destinationSecret(sellerWallet), r: bytesToHex(randScalar()) },
    { amount: 32, id: KEYSET_ID, secret: destinationSecret(sellerWallet), r: bytesToHex(randScalar()) },
  ];

  const inputs = escrowProofs.map((p) => ({
    amount: p.amount,
    id: p.id,
    secret: p.secret,
    C: p.C,
  }));
  const blinded = recomputeOutputs(settleOutputs);
  const digest = sigAllDigestHex(inputs, blinded);
  const digestBytes = hexToBytes(digest);

  const bidderSig = bytesToHex(schnorr.sign(digestBytes, keys.bidderPriv));
  const committeeSig = bytesToHex(schnorr.sign(digestBytes, keys.committeePriv));

  const settle: OutcomeTemplate = {
    inputs,
    outputs: settleOutputs,
    digest,
    bidder_signature: bidderSig,
  };
  const refund: OutcomeTemplate = {
    inputs,
    outputs: settleOutputs,
    digest,
    bidder_signature: bidderSig,
  };

  const bid: BidContent = {
    auction: `1440:${keys.validatorPub}:listing-d`,
    amount: 96,
    wallet: keys.bidderWallet,
    name: "bidder",
    proofs: escrowProofs,
    outcome_templates: { settle, refund },
  };
  const create: CreateContent = { sellerWallet };

  const content: CommitteeSignatureContent = {
    schema: "arena.committee_signature.v1",
    bid: "ab".repeat(32),
    auction: bid.auction,
    outcome: "settle",
    height: 100,
    block_hash: "cd".repeat(32),
    app_hash: "ef".repeat(32),
    digest,
    signature: committeeSig,
    inputs: inputs.map((i) => ({ secret: i.secret, C: i.C })),
    outputs: blinded.map((o) => ({ amount: o.amount, B_: o.B_ })),
  };

  return {
    keys,
    locktime,
    bid,
    create,
    committee: content,
    event: committeeEvent(content, keys.validatorPub),
    escrowProofs,
    settleOutputs,
  };
}

/** A kind-1451 Nostr event carrying `content`, signed by `validatorPub`. */
export function committeeEvent(content: CommitteeSignatureContent, validatorPub: string): NostrEvent {
  return {
    id: "00".repeat(32),
    pubkey: validatorPub,
    created_at: Math.floor(Date.now() / 1000),
    kind: KIND_COMMITTEE_SIGNATURE,
    tags: [
      ["bid", content.bid],
      ["auction", content.auction],
      ["outcome", content.outcome],
      ["height", String(content.height)],
      ["hash", content.block_hash],
      ["p", validatorPub],
    ],
    content: JSON.stringify(content),
    sig: "00".repeat(64),
  };
}

export interface MintVector {
  secret: string;
  rHex: string;
  amount: number;
  id: string;
  recipient: string;
  /** keyset public key (compressed hex). */
  keyHex: string;
  /** the blinded message B_ (compressed hex). */
  B_: string;
  /** the mint's blinded signature C_ (compressed hex). */
  C_: string;
  dleq: { e: string; s: string };
}

/**
 * A NUT-12 mint vector: a keyset keypair, a blinded output, its blinded
 * signature `C_ = a·B_`, and a real DLEQ proof. Enough to prove the unblinding
 * math and DLEQ checks against a known vector.
 */
export function makeMintVector(): MintVector {
  const Apriv = randScalar();
  const keyHex = compressedPub(Apriv);
  const aScalar = BigInt("0x" + bytesToHex(Apriv));
  const secret = destinationSecret("02" + "aa".repeat(32));
  const rHex = bytesToHex(randScalar());
  const r = BigInt("0x" + rHex);
  const B_ = bytesToHex(blindMessage(utf8ToBytes(secret), r).B_.toBytes(true));
  const C_ = bytesToHex(pointFromHex(B_).multiply(aScalar).toBytes(true));
  const dleq = createDLEQProof(pointFromHex(B_), Apriv);
  return {
    secret,
    rHex,
    amount: 64,
    id: KEYSET_ID,
    recipient: "02" + "aa".repeat(32),
    keyHex,
    B_,
    C_,
    dleq: { e: bytesToHex(dleq.e), s: bytesToHex(dleq.s) },
  };
}

export { KEYSET_ID };
