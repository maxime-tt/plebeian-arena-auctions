# Nostr kinds used by the auctions arena

Every kind is defined once in `chain/nostr.go` (`AuctionKinds`, kinds 1440–1452
plus the ephemeral pulse 21447); this table is derived from that map and from
the builders/parsers in `chain/verdict.go`, `chain/lotevents.go` and
`chain/verdictpublish.go`. Nothing here is guessed.

The 1440–1452 series are all **regular** kinds (NIP-01 `1000 ≤ n < 10000`): khatru
(the relay this deployment runs) refuses an ephemeral event with no live
subscription and does not buffer it, so a fact a client must be able to read
later cannot live in the ephemeral range. The one deliberate exception is the
**state pulse**, kind 21447, which is a heartbeat and gates nothing.

"Who signs" is the key that produces the signature the reader checks. Validator
facts (1443–1450, 21447) are each signed under **that validator's own key**
(`Params.Validators`, committed in the app hash), never a shared publisher key;
the `p` tag carries the signer pubkey and readers check it against the roster.

| Kind | Name | Who signs | What anchors it | Which client reads it |
|---|---|---|---|---|
| **1440** | create / root | the **seller's** Nostr key (client-submitted to a relay) | no anchor on the event itself; the chain records its acceptance as a kind-1449 **create-admit** anchored at the accepting block, and the event id becomes the auction root id `a = 1440:<seller>:<d>` | browser UI (`web/`) and the market app on the relays |
| **1441** | bid | the **bidder's** Nostr key (client-submitted to a relay) | no anchor on the event itself; the chain's acceptance is a kind-1449 **bid-admit** anchored at the accepting block | browser UI and the market app on the relays |
| **1442** | chat | the **user's** Nostr key | none | not read by the arena (his kind; the arena publishes nothing on it) |
| **1443** | refund receipt | each **validator**, under its own key | mandatory `height` + `block_hash` of the **SETTLE's** committed block, cross-checked against the content | relay-only clients claiming the refunded ecash |
| **1444** | settle receipt | each **validator**, under its own key | mandatory `height` + `block_hash` of the **SETTLE's** committed block, cross-checked against the content | relay-only clients claiming the paid-out ecash |
| **1445** | outcome / verdict | each **validator**, under its own key | mandatory `height` + `block_hash` of the block that decided the auction | relay-only clients resolving who won |
| **1446** | funding abort | *(reserved)* | *(reserved)* | *(reserved)* — the outcome vocabulary reserves `cancelled`/`aborted` for this path; no builder or publisher exists yet |
| **1447** | state commitment | each **validator**, under its own key | the anchor **is** the fact: `height` + `app_hash`, plus the wall-clock `time` | every reader: it proves the chain of time a client binds a 1443/1444/1445/1449/1450 fact to |
| **1448** | params | each **validator**, under its own key | `height` + `app_hash` of the same pass as the commitment | clients reading the live escrow terms (mint, keyset, roster, `settlementSafetySec`, `bidToConsensusLatencySec`, window bounds) |
| **1449** | lot / bid-admit / create-admit | each **validator**, under its own key | mandatory `height` + `block_hash`; three schemas told apart by the content `schema` string: `arena.lot.v1` (a lot's committed end), `arena.bidadmit.v1` (the chain accepted a bid), `arena.createadmit.v1` (the chain accepted a lot) | relay-only clients: lot end, bid confirmation, and which auctions are followed |
| **1450** | refusal | each **validator**, under its own key | mandatory `height` + `block_hash` of the rejecting block | a client whose 1440/1441 the chain dropped (reason published **verbatim**) |
| **1451** | committee signature | the **elected coordinator validator**, under its own verdict key | mandatory `height` + `hash` of a **committed** block, cross-checked against the content; the digest is the committed SIG_ALL message | executors: the facilitator and the recipient's wallet |
| **1452** | settlement carrier | **any** key (the courier: facilitator, recipient wallet, observer) | **none**; the chain's acceptance is the `settle:` transaction it commits | the chain (ingest) and the app (a claim candidate) |
| **21447** | state pulse (**ephemeral**) | each **validator**, under its own key | `height` + `app_hash`; **ephemeral**, relayed to live subscribers only and never stored | live subscribers placing the chain's clock between commitments |

Notes:

- Kinds **1443** and **1444** are the receipt family: one event per (bid, outcome),
  never on a cadence, carrying the mint's DLEQ-proved outputs the recipient needs
  to claim. Kinds **1445** and **1447–1450** are the claim/state family: a claim is
  published only after the block that decides it is committed, anchored to that
  committed block's hash (`chain/verdictpublish.go`, `Publisher.Step`).
- Kinds **1445, 1449** are append-only in effect: a lifecycle change is a new
  event, never a replacement, so the audit trail survives.
- Kind **1442** is kept mapped only so a chat event stays recognisable by name;
  the arena neither publishes nor applies it (`chain/nostr.go`).
