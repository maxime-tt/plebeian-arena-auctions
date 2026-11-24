# Architecture

The facilitator is a courier. It reads public facts from the relay, executes the one swap
the bidder pre-authorised, and republishes the result. Nothing it does is authoritative:
the chain re-verifies the committee signature and the proofs, so an invalid carrier is
refused at admission and a valid one is valid regardless of who carried it.

## Data flow

```
kind 1451 (committee signature) ─┐
kind 1447 (state commitments)  ──┤  relay (WebSocket, outbound)
                                 │
                                 ▼
   verifyCommitteeSignature ── digest recompute, schnorr vs group key,
                               roster signer, anchor read
                                 │
       fetchBid (1441) ──────────┤  public facts
       fetchCreate (1440) ───────┤
                                 ▼
             planSwap ── bidder sig vs P2PK data, committee sig vs group key,
                         outputs vs committed destinations, lock vs 2-of-2 SIG_ALL
                                 │
             deadlineRefusal ────┤  chain(1447) ∧ mint(info.time)
                                 ▼
             POST /v1/swap ──────┤  mint (HTTPS, outbound)
                                 ▼
             matchSignatures ────┤  by (amount, id), never position
                                 ▼
             unblindToReceipts ──┤  C = C_ − r·K, DLEQ verified
                                 ▼
             buildCarrier ───────┤  arena.settlement.v1, ReceiptOut[] shape
                                 ▼
   kind 1452 (settlement carrier) — relay (outbound); chain ingests
```

## Modules

| module | responsibility |
|---|---|
| `config.ts` | load + validate `config.json`; no secrets |
| `relay.ts` | kind-1451 parse/verify, kind-1447 reads, anchor read, subscriptions |
| `sigall.ts` | SIG_ALL message + digest, BIP-340 verify, B_ derivation, strict P2PK parser |
| `clock.ts` | chain time (1447) / mint time (`info.time`), effective-now, deadline refusal |
| `swap.ts` | `planSwap` (verify + plan), `buildSwapBody`, `matchSignatures` |
| `mint.ts` | `GET /v1/info`, `GET /v1/keys`, `POST /v1/swap` |
| `settle.ts` | unblind + DLEQ verify + `ReceiptOut`, `buildCarrier`, `buildCarrierEvent` |
| `index.ts` | the run loop: subscriptions, per-bid idempotence, publish |

## Trust boundary

- **No custody key.** The courier key signs only kind 1452, which has no roster check and
  no anchor (contract §3). It can be ephemeral.
- **Local verification first.** `planSwap` refuses, with a named reason, if the bidder
  signature, the committee signature, the digest, the lock shape, or any output
  destination does not match the committed facts. It never improvises an output.
- **Response integrity.** A returned signature is matched to a requested output by
  `(amount, id)` and, when two share a pair, by the DLEQ; the proof is only published once
  the DLEQ verifies on both the blinded and unblinded points.
- **Conservative clock.** The earlier of the two clocks, wall-clock fallback with margin,
  and a hard refusal when the window is too small to finish before `locktime`.
