# Kinds the facilitator touches

Only two kinds are written or read for the settlement itself; kind 1447 is read only as a
clock and anchor hint. Field names, tags, and byte order are frozen by
`CONTRACT-2026-11-24-settlement-facilitator.md` §2–§5 — change them there first.

| Kind | Name | Who signs | Anchor | Facilitator's role |
|---|---|---|---|---|
| **1451** | `committee_signature` (`arena.committee_signature.v1`) | the elected coordinator validator, under its own verdict key | `height` + `hash` (block hash) of a committed block | **reader** — verify and act |
| **1452** | `settlement_carrier` (`arena.settlement.v1`) | any key (the courier) | none; the chain's `settle:` transaction is the acceptance | **writer** — publish the result |
| 1447 | `state_commitment` (`arena.state.v1`) | each validator, own key | its own height | **reader** — chain clock + anchor bound |
| 1440 | `create` | the seller | — | **reader** — the committed seller payout key |
| 1441 | `bid` | the bidder | — | **reader** — escrow proofs + committed templates |

## 1451 — what is verified

Tags `bid`, `auction`, `outcome`, `height`, `hash`, `p`. Content carries the digest, the
aggregate signature, and the ordered SIG_ALL aggregation inputs (`inputs: [{secret, C}]`,
`outputs: [{amount, B_}]`).

`verifyCommitteeSignature` accepts a 1451 only when:

1. every content field equals its tag (`bid`, `auction`, `outcome`, `height`, `hash`), and
   `p == pubkey`;
2. the signer is in the roster;
3. `digest == sha256(concat(secret_i || C_i) || concat(str(amount_j) || B__j))` recomputes;
4. the signature is a valid BIP-340 signature over `digest` under the committee group key.

`anchorRefusal` then refuses a 1451 whose `height` is above the highest committed height
seen in kind-1447 commitments.

## 1452 — what is published

Tags `bid`, `outcome`, `p` (the courier). No anchor. Content is exactly the chain's
`settleTx` payload: `arena.settlement.v1`, `bid`, `outcome`, `proofs` (the unblinded
`ReceiptOut` list), and `sig` (the committee signature). Authority is entirely `sig` +
`proofs`, which the chain re-verifies; no roster check applies to the courier.
