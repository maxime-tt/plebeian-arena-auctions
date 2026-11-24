/**
 * Wire types for the settlement facilitator.
 *
 * These mirror, field for field, the chain's own JSON shapes
 * (`chain/app.go` BidContent/CreateContent, `chain/sigall.go`
 * sigAllOutcomeTemplates, `chain/verdict.go` ReceiptOut) and the frozen
 * wire contract (`CONTRACT-2026-11-24-settlement-facilitator.md` §2–§5).
 * A facilitator that renamed a field here would assemble a swap body the
 * mint or the chain reads differently, so nothing here is invented.
 */

/** A NIP-01 event, exactly the fields the chain's `Event` struct carries. */
export interface NostrEvent {
  id: string;
  pubkey: string;
  created_at: number;
  kind: number;
  tags: string[][];
  content: string;
  sig: string;
}

/** NUT-12 DLEQ, on the wire: lowercase hex. */
export interface Dleq {
  e: string;
  s: string;
  r?: string;
}

/**
 * An escrow proof as committed in the bid content (`chain.BidContent.proofs`).
 * `dleq` is present because the chain refuses an escrow without it; the
 * facilitator only reads `amount`/`id`/`secret`/`C` from it.
 */
export interface EscrowProof {
  id: string;
  amount: number;
  secret: string;
  C: string;
  dleq?: Dleq | null;
}

/** One output the bidder pre-authorised: its opening (secret + r) is committed. */
export interface TemplateOutput {
  amount: number;
  id: string;
  secret: string;
  r: string;
}

/** One escrow input a template spends, in committed order. */
export interface TemplateInput {
  amount: number;
  id: string;
  secret: string;
  C: string;
}

/** One exact transaction plus the bidder's signature over its layout-B digest. */
export interface OutcomeTemplate {
  inputs: TemplateInput[];
  outputs: TemplateOutput[];
  digest: string;
  bidder_signature: string;
}

/** The only two spends the escrow may ever make (settle → seller, refund → bidder). */
export interface OutcomeTemplates {
  settle: OutcomeTemplate;
  refund: OutcomeTemplate;
}

/** Kind 1441 content (the committed bid). */
export interface BidContent {
  auction: string;
  amount: number;
  wallet: string;
  name: string;
  proofs: EscrowProof[];
  outcome_templates?: OutcomeTemplates;
}

/** One committed value-for-value split row (kind 1440 content). */
export interface SplitRow {
  role: string;
  recipientPubkey: string;
  allocationBps: number;
}

/** Kind 1440 content (the auction listing); only the money fields are typed. */
export interface CreateContent {
  title?: string;
  sellerWallet: string;
  v4vSplit?: SplitRow[];
  mintUrl?: string;
  [k: string]: unknown;
}

/** Kind 1451 content — the committee signature (contract §2). */
export interface CommitteeSignatureContent {
  schema: "arena.committee_signature.v1";
  bid: string;
  auction: string;
  outcome: "settle" | "refund";
  height: number;
  block_hash: string;
  app_hash: string;
  digest: string;
  signature: string;
  inputs: { secret: string; C: string }[];
  outputs: { amount: number; B_: string }[];
}

/** Kind 1447 content — the stored state commitment (the facilitator's clock + anchor). */
export interface StateCommitmentContent {
  schema: string;
  height: number;
  app_hash: string;
  time: number;
}

/** The chain's existing ReceiptOut (contract §5): one unblinded proof + its recipient. */
export interface ReceiptOut {
  recipient: string;
  amount: number;
  id: string;
  secret: string;
  C: string;
  dleq: Dleq | null;
}

/**
 * One carried proof: the chain's own `Proof` shape — `ReceiptOut` minus the recipient.
 * The recipient is derivable from `secret`, and the chain's carrier parser
 * (`parseSettlementCarrier`, `DisallowUnknownFields`) refuses an extra field by name:
 * "bad_settlement_carrier_payload: json: unknown field \"recipient\"". So it must not ride.
 */
export type CarrierProof = Omit<ReceiptOut, "recipient">;

/** Kind 1452 content — a carried settlement transaction (contract §3). */
export interface SettlementCarrierContent {
  schema: "arena.settlement.v1";
  bid: string;
  outcome: "settle" | "refund";
  proofs: CarrierProof[];
  sig: string;
}

/** NUT-00 blinded message we send to the mint. */
export interface BlindedOutput {
  amount: number;
  id: string;
  B_: string;
}

/** NUT-00 proof we send to the mint as a swap input. */
export interface SwapInputProof {
  amount: number;
  id: string;
  secret: string;
  C: string;
  witness?: string;
}

/** NUT-00 `/v1/swap` request body (contract §4). */
export interface SwapRequestBody {
  inputs: SwapInputProof[];
  outputs: BlindedOutput[];
}

/** NUT-00 blinded signature the mint returns. */
export interface MintBlindSignature {
  amount: number;
  id: string;
  C_: string;
  dleq?: Dleq;
}

/** NUT-00 `/v1/swap` response body. */
export interface SwapResponseBody {
  signatures: MintBlindSignature[];
}

/** NUT-10 P2PK secret, parsed. `data` is the compressed key, verbatim. */
export interface P2PKLock {
  nonce: string;
  data: string;
  pubkeys: string[];
  nSigs: number;
  locktime: number;
  refund: string[];
  sigflag: string;
  tagsJson: string;
}
