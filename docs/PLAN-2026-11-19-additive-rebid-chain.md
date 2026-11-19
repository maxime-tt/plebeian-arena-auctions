# PLAN — the chain implements the additive rebid chain (bids are deltas on a position)

Status: **plan only. Nothing implemented, nothing committed, nothing deployed, no
`arena init`, no relay or app-tree touched.** Every claim below is read from the two trees
as they stand now: the chain at `~/work/wt-validator` (`feat/validator-workstreams` @
`42af5c5`) and the **deployed** app tree at `~/work/wt-e2e` (detached `7fb0e52ac`, which
`docs/HANDOVER-2026-11-19-auctions-all-work-and-the-denomination-defect.md:42-54` names as
the deployed app tree). Line numbers are as of those commits.

---

## 0. Bottom line

The app's arena lane bids in **deltas on a position**; the chain scores bids in
**totals**. Every component except `chain/` already implements the additive model:

| component | what a bid's `amount` means | what the floor compares | cite |
|---|---|---|---|
| app doctrine | a **delta** added to the bidder's position | `own + delta` vs the floor | `ui/auction/arena/bid-rules.ts:155-165`, `platform/web/auctions/relay/arenaBidPublish.ts:53-58` |
| app floor | — | `top + min_increment`; **the leader only needs to reach the top** | `ui/auction/arena/bid-rules.ts:196-228` (`:211-216`), `:301-302` |
| app ladder | publishes `target − own` | the same check, re-run on the exact delta | `ui/auctions/arena-lot/arena-lot-view.tsx:104-133` (`:130`), `:601-607`, `platform/web/auctions/relay/AuctionBidHost.tsx:254-261`, `:307-312` |
| app read | sums deltas per bidder into positions | `max` per bidder = the top | `platform/web/auctions/relay/arenaAuctionRead.ts:1306-1324` |
| **chain** | **the bid's whole escrow** (`proofs total == Amount`, `chain/app.go:744-746`) | **`Amount` alone vs `HighAmount + MinIncrement`** | `chain/app.go:758-773` |
| **chain** | **`HighAmount` = the last accepted delta** | — | `chain/app.go:807` |
| **chain** | **the previous high bid becomes `outbid`, always** — including the leader's own previous leg | — | `chain/app.go:792-794` |
| **chain** | **one winner bid settles; every other bid is `lost`** | — | `chain/app.go:816-834`, `chain/settle.go:90-94` |

The consequence measured live on lot `1440:becb0c53…:612a25ed-…` (top 522, increment 10) is
exactly this table: deltas of 100 and 1 were admitted then marked `void`
(`chain/app.go:787-790`) because `100 < 522+10`, while the bidder's true position was 0; and
a typed 600 was accepted as the top. The app had computed that bidder's position from *all*
their historical deltas, including legs the chain had already outbid and refunded.

**The fix is small and does not change the state schema**: four sites in `chain/app.go`,
one in `chain/settle.go`, two guards, and not one new field. There is **no chain reset**; there is a
coordinated binary swap of the four validators. The app's follow-on work is on its **read**
and its **settlement display** (§6), not on bidding: it already publishes exactly what the
fixed chain accepts.

---

## 1. What exists today, with citations

### 1.1 The chain: one bid, one escrow, one winner

- **The bid's `Amount` is its escrow total.** `BidContent` (`chain/app.go:312-322`) carries
  `Amount int64` and the proofs; `applyBid` refuses `sum != b.Amount` by name
  (`chain/app.go:744-746`, "proofs total %d but bid is %d"). There is no cumulative field
  and no `prev_bid`/chain-linkage anywhere in the chain's content model.
- **Admission is cryptographic before it is economic** (`chain/app.go:677-757`): DLEQ per
  proof (`chain/app.go:700-746`, the call at `:724`), one shared 2-of-2 SIG_ALL lock
  (`checkLock`, `chain/app.go:662-671` → `checkSigAllLock`, `chain/sigall.go:462`),
  then the bidder's two pre-signed outcome templates (`verifySigAllBidTemplates`,
  `chain/app.go:755`, `chain/sigall.go:327-392`). Those templates are bound to **this
  bid's own proofs and this bid's own amount**: inputs must be exactly the committed proof
  secrets in order (`chain/sigall.go:353-361`) and outputs must sum to `content.Amount`
  (`:362-392`).
- **The floor is a total, not a position** (`chain/app.go:758-773`):
  ```go
  min := au.StartPrice
  if au.HighBid != "" { min = au.HighAmount + au.MinIncrement }
  switch {
  case closed/ended:                 late = "auction closed"
  case ev.Pubkey == au.Seller:       late = "seller cannot bid on own lot"
  case au.HighBid != "" && s.Bids[au.HighBid].Bidder == ev.Pubkey:
                                     late = "you are already the highest bidder"
  case b.Amount < min:               late = "bid must be at least %d sats"
  }
  ```
  The third case makes a leader's *raise* impossible; the fourth compares a **delta** to a
  **total**. The `late` family is stored, marked `void`, and appended to `au.Voids`
  (`chain/app.go:787-790`); its proofs are marked used (`chain/app.go:784-786`) so it can
  never be replayed, and it is refundable at once (`chain/settle.go:84-89`).
  `lateBid` is a typed error (`chain/app.go:355-360`) so `CheckTx` answers `late:` and
  `FinalizeBlock` answers `void:` (`chain/app.go:876-883`, `:944-950`).
- **Acceptance overwrites the top with the delta** (`chain/app.go:792-808`):
  ```go
  if au.HighBid != "" { s.Bids[au.HighBid].Status = "outbid" }   // exactly one bid
  ... anti-snipe extension ...
  au.HighBid, au.HighAmount = ev.ID, b.Amount                    // assignment
  au.Bids = append(au.Bids, ev.ID)
  ```
  `Auction.HighBid`/`HighAmount` are state fields (`chain/app.go:74-75`), so they are inside
  the app hash (`State` at `:166-176`, `canonical()` at `:184-189`, `hash()` at `:191-194`).
  `Auction.Bids` holds accepted bids only; void bids live in `Auction.Voids`
  (`chain/app.go:79`, `:787-790`).
- **Close declares one winner bid and loses everything else** (`chain/app.go:816-834`):
  ```go
  au.Winner = au.HighBid
  s.Bids[au.HighBid].Status = "won"
  for _, bid := range au.Bids { if bid != au.Winner { s.Bids[bid].Status = "lost" } }
  ```
- **Settlement is per bid and pushes itself** (`chain/settle.go:1-24`):
  `outcomeDue` (`:76-97`) allows `refund` for `outbid|lost|void` and `settle` only when
  `au.Status == "closed" && au.Winner == bid.ID`; `settlementOutputs` (`:103-116`) reads the
  bid's own committed template; `verifySettle` (`:145-233`) re-derives its outputs from that
  template, re-checks DLEQ, re-checks each output's P2PK recipient, and verifies the FROST
  group signature over `sigAllMessage(bid.Proofs, outs)`; `applySettle` (`:257-268`) records
  `Settled/Out/SettledHeight/SettledAt` on the bid. `dueMoves` (`:284-308`) lists one move
  per unsettled bid, oldest first, stopping at the window (`:278-281`, `7d`). The settler
  drives it per bid id with a journal (`chain/settler.go:161-216`, `:245-293`); FROST signs
  per bid (`SignReq` `chain/node.go:446-452`, `releasePolicy` `:464-489`, commit `:493-524`,
  share `:529-560`). **Nothing in this path ever names more than one bid at a time.**
- **Receipts are already per (bid, outcome)**: `publishReceipts` walks every settled bid and
  publishes one kind 1443/1444 anchored to that bid's settle height
  (`chain/verdictpublish.go:385-411`, content at `:462-466`; `ReceiptContent`
  `chain/verdict.go:228-247`). Several settles on one lot therefore produce several
  receipts **today** — no format change needed.
- **The verdict names one bid, with no amount**: `DeriveVerdict` returns
  `BidID: au.Winner` (`chain/verdict.go:191-200`); `VerdictContent` has no amount
  (`:203-212`).
- **V4V splits are per bid**: the settle template must reproduce the committed split of
  *this bid's amount* (`chain/sigall.go:365-374` → `settleMatchesSplit`,
  `chain/v4v.go:223-261`), `splitSats` rounds each row on that amount
  (`chain/v4v.go:194-210`), and a bid whose amount cannot represent a row is **refused at
  admission** (`chain/v4v.go:229-235`).
- **The state half publishes the whole `Auction`** (so `highBid`/`highAmount` are on the
  wire) plus one `BidView` per accepted bid carrying `amount` and `status`
  (`chain/view.go:34-48`, `:113-171`). `/api/state` is the JSON of that view
  (`chain/main.go:183`, `chain/node.go:310`).

### 1.2 The app: deltas, positions, and a read that cannot see status

- **Doctrine.** "Bids are **deltas** added to a bidder's position, not the resulting total"
  (`ui/auction/arena/bid-rules.ts:155-165`); "`amount` is the DELTA … the read adapter sums
  bid amounts per bidder into positions" (`platform/web/auctions/relay/arenaBidPublish.ts:53-58`).
- **Floor.** `arenaBidFloor` computes `top + step` for a challenger, and **`top` itself for a
  bidder whose `ownPosition >= top`** — "requiring top + increment from the current leader
  would make them outbid themselves by a step" (`ui/auction/arena/bid-rules.ts:211-216`);
  `checkArenaBid` compares `(ownPosition ?? 0) + amount` to that minimum (`:301-302`), with
  `ownPosition` from `model.positions` (`:274-276`).
- **The ladder publishes deltas.** `ArenaLadderStep` carries both a `target` position and a
  `delta`, `delta: Math.max(1, target - own)` (`ui/auctions/arena-lot/arena-lot-view.tsx:104-133`,
  `:126-131`), and the swipe submits `String(next.delta)` (`:601-607`). The host re-runs the
  same check on the exact raw delta (`platform/web/auctions/relay/AuctionBidHost.tsx:307-312`)
  and pre-fills the box with `check.minimum` (`:254-261`) — i.e. the typed number is a
  delta and the box shows the *floor*, the number the position must reach, not what this
  bid adds.
- **Funding proves the delta.** The escrow proof sum must equal the bid's amount
  (`platform/web/auctions/relay/arenaBidPublish.ts:337-342`), and each bid's settle template
  is built against **its own amount** (`arenaBidFunding.ts:454-470`, refusal names at
  `:221-224`).
- **The read cannot see chain status.** `arenaAuctionRead.ts:1306-1324` sums **every** bid's
  amount per bidder, including void and outbid legs, and the top is the maximum sum
  (`:1319-1324`). Void bids are invisible to it: they are recorded only as a ledger line and
  a `void:` tx result (`chain/app.go:944-950`) — they are **not** `Rejection`s, so they
  produce **no kind 1450** (`chain/verdictpublish.go:635-680` publishes `s.Rejections`
  only, appended at `chain/app.go:956`). The comment at `arenaAuctionRead.ts:1360-1371`
  ("the chain records a bid it refused at delivery (the late family …) as a 1450") is
  therefore **wrong today** — worth a separate app-side correction.
- **Surfaces consume one receipt.** `settleReceipt` is *the first* settle receipt of the lot
  (`arenaAuctionRead.ts:1330-1344`), rendered as one line "paid X sats to the seller"
  (`auctionSurfacesRead.ts:216-231`, `:302`, `ui/auction/arena/model.ts:524-533`), and the
  purchases projection picks **one** bid and **one** settle receipt while reporting
  `amount: view.positions[buyer]` (`platform/web/orders/arenaPurchases.ts:77-105`).
- **A second, contradictory floor model exists** in `platform/web/auctions/catalogue/bidMath.ts`
  (`:96-104`: `minNextPosition = leaderPosition + max(1, minIncrement)` for *everyone*,
  so the leader's `minDelta` is a full increment, contradicting
  `bid-rules.ts:211-216`; `:114-122` then throws for a 1-sat leader raise). It has **no
  production callers** — only its own tests — so it is dead doctrine today and must not be
  treated as the app's rule. (Cleanup candidate, not part of this fix.)
- **The legacy per-leg settlement machinery.** `platform/web/auctions/auctionPublishSettlement.ts:392-479`
  walks a chain and requires **one kind-1025 path release per leg** (`:397-427`), derives
  each leg's contribution as `legAmount = cumulative − runningCumulative` where a leg's
  `amount` is **cumulative** (`:451-457`), and refuses unless the sum equals the winning
  amount (`:475-479`); `AUCTIONS.md:503-536` states the rule, `:629-641` requires one
  release per leg, `:692-704` requires one `payout` tag per leg with
  `Σ payout == final_amount`. **This is the retired path-release protocol** (kind 1023/1024/1025),
  not the arena chain: the arena chain has no kind-1025, no seller redemption and no
  cumulative amount. Its invariants are the analogues of `docs/adr/ADR-0003-…md:337-344`
  (R2.6: `delta = Σ proofs`) and `:402-405` (S4.9/S4.10), not the letter of those rows.

---

## 2. The minimal change set

Five sites. No new struct field, no new JSON key, no new event kind, no wire change.

### 2.1 Positions: derived, never stored

The floor needs exactly two numbers, and both already exist:

- **the top** = `au.HighAmount` (redefined below), and
- **the caller's own position**, which is either `au.HighAmount` (the caller is the leader)
  or **`0`** (the caller is not).

That is not an approximation. Whenever a bidder takes the lead their earlier legs are
`outbid` — refundable and, by `chain/settle.go:84-89`, actively refunded — so the only
money they still hold on this lot is the legs of the chain that leads. A bidder who takes
the lead with a new delta therefore starts from 0, and the leader's position is the sum of
their leading legs, which is precisely `au.HighAmount` once §2.4 lands. This is the same
rule the app's `platform/web/auctions/catalogue/bidMath.ts:45-50` states ("a displaced
bidder's position is 0, because the chain returned their money").

**Therefore: no `Positions` map, no `Bid.Position` field, no chain-linkage tag.** (Adding
`Bid.Position` would be possible — `omitempty` would keep old snapshots byte-identical, the
`SettledHeight` precedent at `chain/app.go:132-139` — but it buys nothing the two existing
numbers do not already give, and every added field is a schema version to defend.)

### 2.2 `chain/app.go:758-773` — the floor becomes a position floor

Replace the `min`/`switch` block with:

```go
// A bid's Amount is a DELTA added to the bidder's position (app doctrine:
// ui/auction/arena/bid-rules.ts:155-165; the escrow IS this delta, see
// :744-746). The top is the leader's POSITION, and every other bidder's
// position on this lot is ZERO: their earlier legs were outbid and are
// refundable, so the money they still hold here is nothing.
own := int64(0)
if au.HighBid != "" && s.Bids[au.HighBid].Bidder == ev.Pubkey {
    own = au.HighAmount // the leader's position IS the top
}
min := au.StartPrice // first bid: the position must reach the opening price
if au.HighBid != "" {
    min = au.HighAmount + au.MinIncrement
    if own > 0 {
        min = own // a leader improving their own standing need only reach the top
    }
}
switch {
case au.Status != "open" || (timed && now >= au.EndAt):
    late = &lateBid{"auction closed"}
case ev.Pubkey == au.Seller:
    late = &lateBid{"seller cannot bid on own lot"}
case b.Amount > math.MaxInt64-own:
    return errors.New("position overflows") // Byzantine guard, see §2.7
case own+b.Amount < min:
    late = &lateBid{fmt.Sprintf(
        "bid must add at least %d sats: your position would reach %d, the floor is %d",
        min-own, own+b.Amount, min)}
}
```

The `"you are already the highest bidder"` rule is **deleted**. It exists only to force
progress under replacement semantics; under additive semantics every positive delta
(`b.Amount >= 1`, guaranteed by the proof-sum rule) strictly raises the leader's position.

`late` therefore covers, from now on: **auction closed, seller, below the position floor**
— three cases instead of four, and no case ever fires for a leader's raise.

### 2.3 `chain/app.go:792-794` — only a *changing* lead outbids a chain

```go
if au.HighBid != "" && s.Bids[au.HighBid].Bidder != ev.Pubkey {
    // The lead changed hands: the whole previous position is outbid, every
    // leg of it, because each leg's escrow is a separate delta the bidder
    // must get back. A leader RAISING their own position keeps every leg
    // leading: those legs are the position, and they are paid at settle.
    for _, id := range au.Bids {
        if s.Bids[id].Status == "leading" {
            s.Bids[id].Status = "outbid"
        }
    }
}
```

This runs before the new bid is appended (`:808`), so it cannot touch it.

### 2.4 `chain/app.go:807` — `HighAmount` is the position, `HighBid` the leg that set it

```go
au.HighBid, au.HighAmount = ev.ID, own+b.Amount
```

**Meaning, fixed for every reader of `Auction.HighBid`/`HighAmount` (state, `/api/state`,
verdicts, scripts):**

- `HighAmount` = **the leader's position** — the sum of the amounts of every bid by the
  leader on this lot with status `leading` (before close) / `won` (after). Monotone
  non-decreasing over the life of the lot (every accepted bid adds ≥ 1).
- `HighBid` = **the leg that established that position** — the leader's newest accepted bid.
  It is the same id `closeDue` promotes to `Winner` and `DeriveVerdict` names.

### 2.5 `chain/app.go:816-834` — close promotes every leading leg of the winner's chain

```go
if au.HighBid != "" {
    au.Winner = au.HighBid
}
for _, id := range au.Bids {
    if s.Bids[id].Status == "leading" {
        s.Bids[id].Status = "won" // every leg of the winning position
    } else {
        s.Bids[id].Status = "lost" // every outbid leg; void bids are not in au.Bids
    }
}
```

Keying on the status rather than on the bidder is deliberate: a bidder's *outbid* legs
(from a losing chain they later re-entered from) are still refundable and must not be
promoted to `won`.

### 2.6 `chain/settle.go:90-94` — settle is due for any leg of the winning position

```go
case "settle":
    if au.Status != "closed" || au.Winner == "" || bid.Status != "won" {
        return "", errors.New("only the winning chain of a closed auction pays out")
    }
    return au.SellerWallet, nil
```

Single-leg lots behave exactly as before (their winner's status is `won`). The refund arm
(`chain/settle.go:84-89`) is unchanged and already refuses `leading`/`won`.

Nothing else moves: `settlementOutputs` (`:103-116`), `verifySettle` (`:145-233`),
`applySettle` (`:257-268`), `dueMoves` (`:284-308`), `releasePolicy`
(`chain/node.go:464-489`) and the settler (`chain/settler.go:161-293`) are already
per-bid, so a winning chain of *N* legs becomes *N* independent moves with no code change.

### 2.7 Two guards the change makes necessary

1. **Overflow**: `own + b.Amount` must not wrap. `b.Amount` is bounded by the proof-sum
   check (`chain/app.go:739-746`), but `own` accumulates, so refuse
   `b.Amount > math.MaxInt64-own` before comparing (`math` is already imported,
   `chain/app.go:14`).
2. **The invariant, stated where the money is**: after any accepted bid,
   `au.HighAmount == Σ { bid.Amount : bid.Bidder == leader, bid.Status == "leading" }`
   and, after close, `au.HighAmount == Σ { bid.Amount : bid.Status == "won" }`. This is the
   chain's form of **S4.9** (`docs/adr/ADR-0003-…md:403` "Payout sum == final_amount (for
   rebid chains)"): the sum of every settled leg's amount equals the position the lot
   closed at. It is assertable in unit tests (§5) and is the sentence to put in
   `applyBid`/`closeDue` comments.

### 2.8 What deliberately does **not** change

- `BidContent` (`chain/app.go:312-322`): no `prev_bid`, no `cumulative`. The chain does not
  need linkage — the position is a status question, not a graph walk — and adding a tag
  would create an admission rule whose absence ADR-0003's R2.x table
  (`docs/adr/ADR-0003-…md:337-344`) explicitly says must not condemn a bid.
- `verifySigAllBidTemplates` (`chain/sigall.go:327-392`): the template is still bound to
  this bid's proofs and this bid's amount. That is what makes per-leg settlement the only
  admissible shape (§3).
- `maxVoids` (`chain/app.go:362-364`) and the void path (`:774-776`, `:787-790`): a void
  delta is still recorded, still consumes its proof-Ys, still refundable.
- The client view schema (`chain/view.go:34-48`): `BidView` already carries `amount` and
  `status`; `Auction` already carries `highBid`/`highAmount`. Only values change.

---

## 3. Settlement over multiple legs (the hard part)

### 3.1 What the chain must spend

**Each leg's own escrow, one SIG_ALL swap per leg.** Not one combined swap, and not a
single "final template", for a reason that is structural rather than stylistic:

- A bid's template is admitted only if its inputs are **exactly that bid's proof secrets in
  order** (`chain/sigall.go:353-361`) and its outputs sum to **that bid's amount**
  (`:362-392`). A template naming two legs' proofs belongs to no bid, so no admission rule
  would have validated it.
- The only key that can authorise it is the bidder's wallet spend key, verified against
  the template digest of the bid that carries it (`verifySigAllBidderSignature`,
  `chain/sigall.go:292-306`). There is no event in the protocol by which a bidder
  authorises a spend of *several* of their bids at once.
- Introducing one would be a new client-authored event, a new admission path, and a new
  authorisation surface — the opposite of the 2-of-2 design's whole claim
  (`~/work/wt-poc-vocab/MERGED-PROTOCOL-SPEC.md` §0/§4 — the owner-ruled merged spec:
  the committee may move money only to the two destinations the bidder pre-signed, each
  escrow separately).

So: a winner holding legs `L1…Ln` produces **n settle txs**, each
`settle:<json>` spending `Li.Proofs` into `Li`'s committed settle outputs, each signed by
FROST over `sigAllMessage(Li.Proofs, outs_i)` (`chain/settle.go:229-232`).

### 3.2 What it pays

Each leg pays **its own delta** to the committed destination(s):

- no V4V split → the whole `Li.Amount` to `au.SellerWallet` (`chain/settle.go:90-94`,
  `:206-212`);
- with a split → that leg's committed rows plus the seller remainder, computed on
  `Li.Amount` (`chain/settle.go:161-166` → `splitSats`, `chain/v4v.go:194-210`).

The seller's total is `Σ Li.Amount = au.HighAmount` = the winning position. Receipts carry
the per-leg outputs (one kind 1444 per leg, `chain/verdictpublish.go:385-411`), so a client
that sums the lot's settle receipts gets the final amount. **That is S4.9 satisfied by
construction**, and S4.10 ("all chain legs have corresponding 1025") has no 1025 analogue
here; its form is "every `won` leg has exactly one settle tx", enforced by
`outcomeDue` + `dueMoves` listing each unsettled won leg.

### 3.3 Does the app's per-leg kind-1025 machinery match?

**No — and it must not be press-ganged into matching.** `platform/web/auctions/auctionPublishSettlement.ts`
is the **seller-driven path-release protocol**: the seller collects kind-1025 releases,
walks `prev_bid`, and publishes kind 1024 (`AUCTIONS.md:629-641`, `:692-704`). The arena
chain has no kind 1025, no seller signature, no `prev_bid`, and no cumulative
`amount`; its settlement is the validators' own push (`chain/settle.go:1-24`), and the app
never participates. Its `legAmount = cumulative − runningCumulative` arithmetic
(`:451-457`) is the *other* protocol's semantics and would silently mis-price an arena
delta (a 100-delta leg after a 500-delta leg is 100, not 600−500).

What the arena app **does** need from that lesson is the shape: one redemption per leg,
and the displayed total is the sum of the legs (§6).

### 3.4 Operational shape of a multi-leg settlement

- `dueMoves` lists all due moves for one lot in `(height, id)` order
  (`chain/settle.go:301-306`); the settler runs up to 4 swaps at once
  (`chain/settler.go:41`, `:177-215`) with a per-bid journal (`:93-112`) and per-bid
  backoff (`:186-210`). Nothing is auction-level serialised, so a winner's legs settle
  concurrently. **There is no cross-leg atomicity**: leg 1 can land while leg 2's swap is
  refused. That is a client-visible state ("paid 300 of 500") that the surfaces must be
  able to say; see Q4.
- The window is per bid (`chain/settle.go:278-281`, `:291`): a leg left unsettled after
  7 days drops off the due list and falls back to the bidder's locktime refund. With
  several legs the window is per leg, so a partial abandonment is possible.

---

## 4. Consensus, deploy, rollback

### 4.1 State shape and the app hash

- **The JSON schema does not change.** No field is added, removed or renamed, so the
  canonical encoder (`chain/app.go:184-189`) produces the same bytes for the same values,
  snapshots still re-encode byte-identically (the `omitempty` requirement documented at
  `chain/app.go:132-139`), and `loadSnapshot`/`restore` (`chain/snapshot.go:282-287`)
  is untouched.
- **The values change**, so the app hash changes for every block that carries a rebid or a
  close under the new rules. `HighAmount` becomes a sum; a leader's `leading` legs become
  several; `closeDue` produces several `won`. All four validators must therefore be on the
  same binary.
- **Which half of the state this touches:** the bid half is `/api/state` only, plus the
  per-leg receipts. The Nostr state half carries none of it — 1447 is a hash commitment,
  1448 the params, 1449 the lot's committed end, 1450 a refusal
  (`chain/nostr.go:44-54`, `chain/lotevents.go:1-22`, `chain/verdictpublish.go:504-680`) —
  so **no event kind, tag set or content schema changes**; only `ClientView` values and the
  set of 1443/1444 receipts do.

### 4.2 Is a chain reset required? **No.**

- Existing state remains interpretable *and* money-consistent. Under the old rules a
  leader's earlier leg is always `outbid` (`chain/app.go:792-794`), which is exactly the
  status the new rules exclude from a position, so a legacy leader's position under the new
  rule (`own = HighAmount`) matches what the chain will actually pay them from: their last
  leg. Their `outbid` legs are already refundable and, where the settler has run, already
  refunded (`chain/settle.go:84-89`) — a leg whose refund is still pending is refunded
  later and is not counted either way, so no money is lost or paid twice.
- Lots that are already closed and settled keep their recorded outcomes; the change only
  governs future blocks.
- The observed defective lot (`1440:becb0c53…:612a25ed-…`, high 522) keeps its record: its
  void deltas are refundable and its settled 600 is settled. A reset is the *only* way to
  erase that history, and it is not needed to make the code correct.

### 4.3 If a reset were nevertheless authorised

It would be coherent only with the relay purge measured in
`docs/MINT-ORIGIN-2026-11-19-RESULT.md:288-417`: the readers are newest-wins for params
(1448) and the clock (1447) and earliest-wins for lot state/refusals/receipts
(1449/1450/1443/1444) (`:309-317`), so old events keep winning until purged; the minimum is
(1) stop the unit, (2) a keys-preserving genesis, root **outside** `run/` so the settle
journal is not inherited (`:346-349`), (3) purge kinds 1447/1448/1449/1450/1445/1443/1444 —
NIP-09 kind-5s signed by the four validator keys, or a relay-store wipe (`:363-386`),
(4) decide 1440/1441 (`:339-345`), (5) restart and verify a fresh 1448 (`:412-417`).
**This plan does not need any of it.**

### 4.4 Deploy

- Build `bin/arena` from `~/work/wt-validator` (`scripts.sh:5` `build`), keep the previous
  binary as `bin/arena.prev-2026-11-19-before-additive-rebid` beside the existing
  `arena.prev-*` set in `~/work/arena-merged-ref/bin/`, swap, then restart the **one** unit
  that runs all four validators plus the gateway
  (`docs/HANDOVER-2026-11-19-….md:48-54`; the gateway spawns `procs [4]` from the same
  binary, `chain/main.go:47`). A single restart is what keeps the four in lockstep; the
  per-validator demo controls (`chain/main.go:415-434`) must not be used to run a mixed
  set, because a mixed set stalls consensus on the first rebid block.
- **No `arena init`.** Never; it regenerates the FROST group key and strands every escrow
  (`docs/HANDOVER-2026-11-19-….md:182-193`).
- Proof from outside: height continues (no reset), the root is intact, `/api/state` shows
  `highAmount` equal to the sum of the leader's `leading` amounts, and a live leader rebid
  is admitted (not void).
- **Sequencing constraint (blocking):** the app's settle/refund templates must be
  denomination-decomposed before this lands. A delta is an arbitrary integer, and the
  defect in `docs/HANDOVER-2026-11-19-….md:113-178` means a template output that is not a
  mint denomination is refused by the mint forever. Additive deltas make small,
  non-denominational legs the *normal* case, so landing this chain change first would
  manufacture stuck escrows at a higher rate. Order: app template decomposition →
  chain additive bids. The chain-side companions in that handover (§4 items 5–7:
  refuse non-denomination template outputs at admission, log the mint's refusal, surface a
  counted failure) also belong in the same deploy window, but they are not this plan.

### 4.5 Rollback

Restore `bin/arena.prev-2026-11-19-before-additive-rebid` and restart the unit.

- **Format-safe**: the state has no new fields, so the old binary loads it.
- **Not semantics-safe**: any multi-leg winner closed under the new code has several bids
  with status `won`; the old `outcomeDue` (`chain/settle.go:90-94`) settles only
  `au.Winner`, so the *other* legs stop settling at once and stay locked until their
  locktime refund. A rollback should be treated as an incident, not a quiet undo, and the
  affected legs listed before the swap.

---

## 5. Tests required before any deploy

Baseline gate (from `docs/HANDOVER-2026-11-19-….md:197-201` and
`docs/CHAIN-STATE-HALF-2026-11-18-RESULT.md:28-31`): `cd chain && ARENA_TEST_MINT=… \
NAK_BINARY=… go test -count=1 ./...`, `go vet ./...`, `gofmt -l`.

### 5.1 Unit, in `chain/*_test.go` (new)

Helpers to reuse: `liveApp`/`bidTx`/`finalize` (`chain/live_test.go:163-238`),
`mintMove`/`testSettler`/`outputsOf` (`chain/settle_test.go:55-89`).

1. **`TestLeaderRebidIsAccepted`** — A bids 500, A bids 10: both `leading`,
   `HighAmount == 510`, `HighBid == second id`, and the second escrow's proof sum is 10.
   This is the case the chain refuses today (`chain/app.go:769-770`).
2. **`TestLeaderRebidOfOneSatIsAccepted`** — the ladder's `Next` delta for the leader
   (`arena-lot-view.tsx:130`) is 1; it must be admitted, so the app's button is not a lie.
3. **`TestHighAmountIsPositionNotDelta`** — after 500 then 10, `HighAmount == 510`
   (today `chain/app.go:807` leaves 10).
4. **`TestPositionFloorRefusesSmallDelta`** — X holds an `outbid` 500, A leads 510;
   X's 1-delta is `void` with a message naming the required **delta** (20) and the floor
   (520); X's position stays 0. Covers the live defect exactly.
5. **`TestPositionFloorAdmitsReachingDelta`** — the same X with a 20-delta is accepted and
   becomes the leader with position 20, **not** 520 (their old 500 is refunded, not
   counted).
6. **`TestLeadChangeOutbidsEveryLeg`** — A 300 + A 200 (`leading`, `HighAmount 500`);
   B 510: both A legs `outbid`, `dueMoves` lists **two** refunds to A, B position 510.
7. **`TestLeaderRaiseKeepsExistingLegsLeading`** — A 500, A 10: no refund is due for
   either leg, and a later B bid outbids **both** A legs.
8. **`TestCloseMarksEveryWinningLegWon`** — A 300 + A 200, close: two bids `won`, none
   `lost`, `Winner == au.HighBid`, `HighAmount == 500`.
9. **`TestCloseDoesNotPromoteRefundedLegs`** — A 500 (outbid, refunded), B 520, A 30 (void):
   at close only B is `won`; A's 500 is `lost`, never `won`.
10. **`TestMultiLegWinnerSettlesEveryLeg`** — A 300 + A 200, close, then `mintMove` and
    apply a settle tx for **each** leg: both `Settled == "settle"`, both to
    `au.SellerWallet`, `Σ st.Proofs amounts == 500 == HighAmount`; a third settle for a
    `lost` leg is refused; re-applying either is refused.
11. **`TestSettleRequiresWonStatus`** — a `leading` bid of an open lot, an `outbid` bid and
    a `void` bid all refuse `outcome:"settle"` (`chain/settle.go:90-94`).
12. **`TestPartialChainSettlementIsVisible`** — settle leg 1 only: `dueMoves` still lists
    leg 2; the lot's settled total is 300 of 500 (the state a surface must be able to say).
13. **`TestV4VMultiLegSettlementSumsToPosition`** — with a committed split, A 300 + A 200:
    each leg's settle pays the split of *its own* amount, the seller takes each leg's
    remainder, and the settled line items sum to exactly 500 (the position); each recipient
    receives `Σ_legs floor(leg·bps/10000)`, which may fall short of
    `floor(500·bps/10000)` by at most `legs − 1` sats (the ruling under Q3 decides whether
    that is accepted). A delta below the representability floor is **refused at
    admission** (`chain/v4v.go:229-235`), not void — assert the exact refusal so the app
    can pre-check it.
14. **`TestHighAmountMonotoneAndInvariant`** — for a random-ish sequence of accepted bids,
    `HighAmount` never decreases and always equals `Σ` leading amounts; the same after close
    over `won` bids.
15. **`TestOverflowPositionRefused`** — `own` near `MaxInt64` with a large delta is refused
    by name, not wrapped.
16. **`TestSnapshotRoundTripAfterRebid`** — a state with several `leading` legs and a
    multi-leg close re-encodes byte-identically (`chain/snapshot_test.go` discipline;
    `chain/app.go:132-139`), and `TestEmptyBlocksKeepAppHash`
    (`chain/live_test.go:336`) still passes.

### 5.2 Unit, existing tests that must change (and how)

- `TestLateBidIsVoidAndRefundable` (`chain/live_test.go:240-294`): the exact string
  `"late: bid must be at least 210"` becomes the delta message; the rest of the assertions
  (void recorded, proofs used, refund policy passes, view scoping) stay.
- `TestPushSettleAndRefund` (`chain/settle_test.go:243-277`): `len(items) != 1` stays valid
  for a single-leg lot; add the multi-leg case rather than changing it.
- `TestPushSettleRefusesForgeries` (`:195-241`): `bad["leading bid"] = refund` and
  `bad["settle of an open lot"]` stay; add "settle of an outbid leg".
- `tests/e2e.mjs:133-148`: the regex `/at least 120/` must become the new message, and the
  `highBid` assertions (`:143`) stay valid (a single-leg lot).
- `scripts/auctions/live-lifecycle.test.ts:428-431`, `:605-610` (in `~/work/wt-e2e`) read
  `highBid` from `/api/state`; they keep passing for single-bidder lots, and a multi-leg
  assertion should be added there (app lane).

### 5.3 Live probes (the suite already runs these; add one)

- `tests/e2e.mjs` — the full walk; must pass with the new message.
- `tests/probe-instant-refund.mjs` — leader change → refund of the whole previous
  position; must additionally assert **both** legs of a two-leg leader are refunded after a
  challenger takes the lead.
- `tests/probe-state-half.mjs` — 1449/1450 unaffected, run as a regression.
- `tests/probe-restart-ingest.mjs` — unaffected, run as a regression.
- **New `tests/probe-additive-rebid.mjs`** — on a live open lot: (1) leg 1, (2) a rebid by
  the same bidder is **admitted, not void** and `/api/state` shows `highAmount == Σ` its
  leading amounts; (3) a challenger takes the lead; (4) the first bidder's 1-delta is void
  with the delta message; (5) a delta that reaches the floor is admitted with position ==
  the delta; (6) the lot is closed and the probe polls `/api/state` + the relay until every
  `won` leg is settled, then asserts `Σ` settle receipts' amounts `==` `highAmount` at close
  and that each receipt is anchored (`chain/verdictpublish.go:385-411`). Wire it into
  `scripts/acceptance-validator-workstreams.sh:90-102` beside the other probes.

---

## 6. App-side implications

The chain change alone makes the app *publish the right thing*; it does not make the app
*display the right thing*, because the app's arena read cannot see chain status.

1. **Positions must stop counting dead legs.** `arenaAuctionRead.ts:1306-1324` sums every
   delta of every bid, void and outbid included; `bid-rules.ts:274-276` feeds that sum into
   the floor. Required rule: a bidder's position is `0` unless they are the top, and a
   leg whose refund receipt (`1443`) has been read subtracts from its bidder's sum (the
   read already indexes `refundForBid`, `arenaAuctionRead.ts:1333-1341`). Without this the
   app keeps today's disagreement: the defect note's own reading of the live bids is that
   the bidder's earlier 500 (outbid, refunded) was still counted as their position, so
   `target − own` produced the 100 and 1 deltas the chain voided.
   **Alternative (needs a product ruling):** read the chain's `/api/state`, which has the
   authoritative `highAmount` and per-bid `status` (`chain/view.go:34-48`, `:113-171`) —
   but as deployed the gateway listens on `127.0.0.1:8090`
   (`docs/HANDOVER-2026-11-19-….md:48-54`) while the app is browser-served, so the
   relay-only rule is the deployable one.
2. **The lot-level settle line must aggregate legs.** `settleReceipt` is the first settle
   receipt (`arenaAuctionRead.ts:1343`), labelled as the whole payout
   (`ui/auction/arena/model.ts:524-533`); with *N* legs it will state leg 1's amount as the
   lot's. Sum the lot's `1444`s (and say "N legs" when > 1), or show per-leg rows.
3. **The purchase row must aggregate legs.** `arenaPurchases.ts:77-105` picks one bid and
   one settle receipt while reporting the buyer's summed `positions` — after the fix a
   multi-leg win reads "amount 500, paid 300". Sum the buyer's settle receipts for the lot
   and key "won" on **any** of the buyer's bids being named by a settle receipt, not on the
   verdict's single `bid_id`.
4. **Correct the read's claim about 1450.** `arenaAuctionRead.ts:1360-1371` says a void
   ("late family") bid is recorded as a kind 1450. It is not: `applyBid`'s late path appends
   a ledger line and returns a `lateBid`, and only `Rejection`s are published
   (`chain/app.go:944-950`, `:956`, `chain/verdictpublish.go:635-680`). A void bid is
   learnable only from `/api/state` or from its eventual `1443`.
5. **Disambiguate the two floor doctrines.** `bidMath.ts:96-104`/`:114-122` gives the
   leader a full increment, contradicting `bid-rules.ts:211-216`; it has no production
   callers, so delete it or align it — it must not become the rule by accident.
6. **Pre-check the V4V representability limit** if Q3 rules that splits are paid per leg:
   a delta too small for a committed row is refused **at admission** by
   `chain/v4v.go:229-235`, before any void, so the app should refuse it before locking
   (its funding layer already refuses the same shape, `arenaBidFunding.ts:221-224`).
7. **Explicitly none:** `platform/web/auctions/auctionPublishSettlement.ts` and the
   kind-1025 flow need **no** change for this fix — that protocol is not what the arena
   chain runs (§3.3). `AUCTIONS.md` §4.2.1's cumulative wording is legacy-only and should be
   marked as such in that protocol's section, not ported to the arena lane.

---

## 7. Open questions — for the user's ruling, not assumptions

**Q1. Which meaning binds a bid's `amount` on the arena chain — the delta, or the
cumulative commitment of `AUCTIONS.md:503-536`?**
The live app publishes deltas (`arenaBidPublish.ts:53-58`), and the chain's admission rule
*forces* deltas (`proofs total == Amount`, `chain/app.go:744-746`). §4.2.1 of AUCTIONS.md
says a leg's `amount` is cumulative while the lock is the delta — i.e. the two protocols
disagree about the same field name. This plan implements the delta (and treats the escrow
as exactly the delta) on the grounds that the app and the chain already do. **Please
confirm that the arena 1441 `amount` is a delta and that §4.2.1's cumulative wording is
legacy-only**, otherwise the change set is different (a new content field plus an app
change).

**Q2. Does a rebid lock only the delta?** (The app locks only the delta today:
`arenaBidPublish.ts:337-342`, and each leg keeps its own escrow and template.) If a rebid
were instead meant to lock the *new total* while being scored as a position, the chain's
`sum == Amount` rule and every template would have to change. Confirm "delta only", and
with it the capital-efficiency claim of `AUCTIONS.md:527-531`.

**Q3. How should a winner's multiple escrows pay a committed V4V split?** Each leg's
template can only pay the split of *its own* amount (`chain/sigall.go:365-374`,
`chain/v4v.go:194-210`), which rounds per leg — so recipients can receive up to
`(legs − 1)` sats less than their basis-point share of the position, and a delta smaller
than the representability floor is **refused at admission** (`chain/v4v.go:229-235`), which
makes the leader's 1-sat raise impossible on a V4V lot. Accept per-leg rounding plus the
representability floor (minimal), or require the split to be paid only from the final leg
(a different template rule and a different set of refusals)?

**Q4. Is partial settlement acceptable and how is it stated?** Legs settle independently
(`chain/settler.go:177-215`); one can land while another is refused. Should a lot be
considered "paid" only when `Σ settled == HighAmount`, and what may a surface say in
between ("paid 300 of 500")? The chain can report the state; the wording is a product
ruling.

**Q5. Is the set of 1444 receipts the final amount, or must a signed total exist on the
wire?** The verdict carries `bid_id` and an outcome but **no amount**
(`chain/verdict.go:203-212`), so today a client can only infer the final amount by summing
receipts. Minimal: keep it that way (receipts are the money facts). Alternative: extend
kind 1445's content with the settled total and the leg list — a wire + schema change this
plan does not include.

**Q6. May several bids carry `status: "leading"`/`"won"` on the wire?** The state half
publishes `BidView.status` verbatim (`chain/view.go:34-48`) and `/api/state` copies the
`Auction` (`:146-166`). If any existing client (the live scripts at
`~/work/wt-e2e/scripts/auctions/live-lifecycle.test.ts:428-431`, `:605-610`, or the app)
assumes exactly one `leading`/`won` bid, this fix changes that contract *without a schema
change*. Confirm the assumption is "several is legal", or say what must be added so a
client can tell.

**Q7. Is `au.Winner` still the lead's newest leg, or should it name a chain root?** This
plan keeps `Winner = HighBid` (unchanged meaning: the leg that set the top, which is what
`DeriveVerdict` names). The consequence is that the verdict's `bid_id` is the *last* leg
while several receipts carry money. Confirm, or specify a different naming (e.g. add
`winner_bidder` to the verdict — a wire change).

**Q8. App-side: may the floor be advisory only?** If the app keeps computing positions from
relay deltas it will sometimes disagree with the chain (void legs are unobservable until
refunded, §6-1). Either the app subtracts refunded legs and treats every non-top bidder as
0, or the app must read chain state. Which is acceptable for the deployed (browser-served,
relay-only) app?

---

## 8. Non-goals (deliberately not in this change)

- No `prev_bid`/chain-linkage tag, no `Bid.Position` field, no new kind, no wire-content
  change (§2.8, Q5, Q7).
- No chain reset, no relay purge, no `arena init` (§4.2–4.4).
- Not fixing the app's denomination/template defect or the chain-side admission/logging
  companions from `docs/HANDOVER-2026-11-19-….md:113-178` — they are prerequisites and
  parallel work, not this fix.
- Not repairing the already-recorded defective lot (`1440:…:612a25ed`). Its void legs are
  refundable (subject to the denomination defect) and its settled leg stands.
- Not porting `AUCTIONS.md` §4.2.1's cumulative model into the arena lane (Q1).