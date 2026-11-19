# RESULT — the chain implements the additive rebid chain (R1-A, R2-A, R3, R4)

Date: 2026-11-19. Tree: `~/work/wt-validator` (`feat/validator-workstreams`), commit
`828cdf2` (local, not pushed). Deployed binary: `~/work/arena-merged-ref/bin/arena`,
sha256 `df28bb51bad947fa6fc29fcef50d17b9c169cc812f30f529fdda13aac1d8199d`.
Plan of record: `docs/PLAN-2026-11-19-additive-rebid-chain.md` (§2 change set, §5 tests),
with the owner's rulings R1 = A, R2 = A, R3 and R4 folded in.

## 0. Bottom line

A bid's `amount` is the **delta** it locks now (`proofs total == Amount`, unchanged). The
chain now tracks **positions**: it sums each bidder's live legs, the floor compares the
resulting position against `top + min_increment`, `HighAmount` is the leader's position,
`HighBid` is the leg that established it, the lead changing hands outbids **every** leg of
the dethroned chain, close promotes **every** leading leg to `won`, and settlement walks
**every** won leg (one SIG_ALL swap per leg, each leg's own template; the paid total is
the position). A bid that is economically late **on arrival** is refused at admission
(R3) and never committed. The chain publishes the per-bid **admission fact** (R4).

No state-schema change, no new `Bid` field, no reset, no `arena init`.

## 1. Change sites

| # | site | what changed |
|---|---|---|
| 1 | `chain/app.go:758-798` (`applyBid`) | the floor is a **position** floor: `own` is `HighAmount` for the leader and `0` for everyone else; `min` is `StartPrice` for the first bid, `HighAmount+MinIncrement` for a challenger, `own` for the leader's own raise; the overflow guard returns `position overflows`; the message states the required **delta**, the resulting position and the floor. The `"you are already the highest bidder"` case is **deleted** (R2-A). |
| 2 | `chain/app.go:817-829` | the sweep runs **only when the lead changes hands** and outbids **every** leg whose status is `leading`. |
| 3 | `chain/app.go:842-846` | `au.HighBid, au.HighAmount = ev.ID, own+b.Amount` — position, not delta; invariant stated in the comment. |
| 4 | `chain/app.go:855-879` (`closeDue`) | every `leading` leg → `won`, everything else → `lost`, keyed on the **status**, never on the bidder. |
| 5 | `chain/app.go:883-899` (`positionOf`) | one place that answers "the position this bid belongs to" (only for `leading`/`won`), used by the publisher. |
| 6 | `chain/app.go:920-952` (`CheckTx`, bid branch) | **R3**: `applyBid(..., apply=false, timed=true)` with the wall clock; a `lateBid` is answered `Code 2` with the reason (refuse), so it is never committed. Cryptography is untouched; `void` remains the delivery-time answer (see §3). |
| 7 | `chain/settle.go:90-100` (`outcomeDue`) | settle is due for **any** `won` leg of a closed lot (`bid.Status == "won"`), not only `au.Winner`. |
| 8 | `chain/lotevents.go:58`, `:233-326` | **R4**: `BidAdmitSchema = "arena.bidadmit.v1"`, `BidAdmitContent`, `check()`, `BuildBidAdmitEvent`, `ParseBidAdmitEvent`, five named refusals. Kind 1449 now carries two schemas (`arena.lot.v1` and `arena.bidadmit.v1`); each parser refuses the other's shape **by name**. |
| 9 | `chain/verdictpublish.go:125`, `:580-632` (`publishLots`) | **R4**: a second loop in the lot-state publisher publishes the admission fact for every admitted bid that is still `leading` or `won`, anchored at **that bid's own committed height**, deduped per bid id. |
| 10 | `chain/nostr.go:53` | the `KindLot` comment names the admission shape. |
| 11 | `chain/main.go:588-596` | the gateway's `late:` trim is kept, with a comment saying why: since R3 no current validator answers that way, but a mixed-version set during the swap can, and a client must not be told "rejected" about a bid an old validator really did commit as void. |

`chain/sigall.go`, `chain/settle.go`'s settle application (`settlementOutputs`,
`verifySettle`, `applySettle`), `dueMoves`, `releasePolicy` and the settler needed **no**
change: they are already per-bid, so N legs became N independent moves.

## 2. R4 — the exact event shape

Kind **1449** (the lot-state kind), content schema **`arena.bidadmit.v1`**:

```json
{"schema":"arena.bidadmit.v1","auction_id":"1440:<seller>:<d>","bid_id":"<64 hex>",
 "bidder":"<64 hex x-only>","amount":5,"position":105,"high_bid":"<64 hex>",
 "height":3450,"block_hash":"<64 hex>"}
```

Tags: `["e", bid_id]`, `["auction", auction_id]`, `["height", "3450"]`,
`["hash", block_hash]`, `["p", <signer>]`. The anchor is **mandatory** and the parser
cross-checks the tags against the content (same discipline as kinds 1443/1444/1449/1450),
the event is signed under the validator's own key, and `p` must be on the kind-1448 roster.

- `amount` — **this leg's delta** (what it locked).
- `position` — the position the bid **belongs to**: the sum of its bidder's live legs
  (`leading`, or `won` after close), which for the leg that set the position is exactly the
  resulting position. It is never below `amount`.
- `high_bid` — the leg that set that position (the id `closeDue` names `Winner`).

Why a second schema on 1449 and not a new kind: the ruling put the fact in the lot-state
publication the chain already maintains on change; the *schema* is what a reader dispatches
on, so one kind keeps one anchor discipline while the schema says which fact it is. Two
consequences, both deliberate and tested:

1. **A bid is announced once.** Dedupe is per bid id; a bid first seen `leading` is not
   re-announced when it becomes `won`.
2. **Nothing is published about a dead leg.** An `outbid`/`lost` leg's position is no longer
   a committed fact (its money is on its way back), so the publisher states nothing about
   it — see `State.positionOf`. An app that needs to know an immediately-outbid bid was
   accepted reads `/api/state` (the bid exists, with its status) — the same answer as before.

The one honest imprecision: a leg that was alone for a moment can carry the position *at
publication* (100) rather than the position *at its admission* (100 too, in that case) —
visible live in §5: `leg 1: position 100 … (100 or 105)`, `leg 2: position 105`. The app can
tell them apart with `high_bid == bid_id`.

## 3. R3 — which cases refuse, which still `void`, and why

**Refused at admission** (`CheckTx` answers `Code 2` with the chain's own reason on the
submit; `/api/tx` returns it as `error`):

| case | why it can never become valid |
|---|---|
| below the resulting-position floor | the floor only rises (`HighAmount` is monotone non-decreasing) |
| closed lot (`Status != "open"`) | a closed lot stays closed |
| the lot's end already passed by this validator's clock | the delivering block closes the lot before it applies the bid |
| the seller bidding on their own lot | the seller never changes |
| past the lot's void cap | the cap never falls |

The `"you are already the highest bidder"` case is **gone** — a leader's positive delta
always strictly raises the top (R2-A), so there is nothing to refuse.

**Still `void` at delivery** (`applyBid` unchanged in this respect, `Code 0` + `void:`):

- a bid that **passed admission and was overtaken** before its block — another bid took the
  lead and raised the floor in between. This is the race `void` exists for, and it is now
  the *only* way an honest bid becomes `void` (unit test
  `TestLateBidIsVoidAndRefundable`, rewritten to that shape);
- a bid that only a **Byzantine proposer** could have included (admission is a mempool
  filter, not consensus: `applyBid` re-decides everything).

Why the wall clock is safe in `CheckTx`: `minLocktime` is monotone non-decreasing in `now`,
so using the validator's wall clock (which is never earlier than the block's time) can only
make `CheckTx` **stricter** about the locktime floor than delivery, never looser. `CheckTx`
is a mempool filter, so non-determinism there is not a consensus risk — the same reasoning
`checkClose` already relies on.

A refused bid leaves **no trace**: no `Bids` entry, no `UsedY` entry, no `Voids` entry. Its
escrow therefore stays locked and unspent at the mint until its own locktime (the bidder
reclaims it alone). Live evidence in §5-3.

## 4. Tests

Suite: **201 pass / 0 fail** (baseline at `42af5c5`: **182 pass / 0 fail**), `go vet ./...`
clean, `gofmt -l` clean. Gate command:
`cd chain && ARENA_TEST_MINT=http://127.0.0.1:3338 NAK_BINARY=~/.local/bin/nak go test -count=1 ./...`.

19 new tests in `chain/rebid_test.go`:

`TestLeaderRebidIsAccepted`, `TestLeaderRebidOfOneSatIsAccepted`,
`TestHighAmountIsPositionNotDelta`, `TestPositionFloorAdmitsReachingDelta`,
`TestLeadChangeOutbidsEveryLeg`, `TestLeaderRaiseKeepsExistingLegsLeading`,
`TestCloseMarksEveryWinningLegWon`, `TestCloseDoesNotPromoteRefundedLegs`,
`TestMultiLegWinnerSettlesEveryLeg`, `TestSettleRequiresWonStatus`,
`TestPartialChainSettlementIsVisible`, `TestV4VMultiLegSettlementSumsToPosition`,
`TestHighAmountMonotoneAndInvariant`, `TestOverflowPositionRefused`,
`TestSnapshotRoundTripAfterRebid`, `TestBelowFloorBidIsRefusedAtAdmission` (R3),
`TestClosedLotAndSellerBidsRefusedAtAdmission` (R3), `TestBidAdmissionFactIsPublished` (R4),
`TestBidAdmitEventRefusals` (R4).

Updated, no assertion weakened:

- `TestLateBidIsVoidAndRefundable` (`chain/live_test.go`) — rewritten to the **race** shape
  (both bids clear admission, one block, the second is overtaken → `void`); every other
  assertion (void recorded, proofs used, refund policy, view scoping) kept, and the 210
  message is the delta message.
- `TestPushSettleRefusesForgeries` (`chain/settle_test.go`) — added
  `bad["settle of an outbid leg"]`.
- `TestLotEventForAnExtendingBid`, `TestLotEventForAClosedLotWithoutExtension`,
  `TestLotRefusedWhenAnchorHashUnknown` (`chain/lotevents_test.go`) — count **lot-shape**
  1449 events through a new `lotShapeEvents` helper, because the admission facts ride the
  same kind. The assertions themselves are unchanged.
- Fixtures (`live_test.go`, `v4v_test.go`, `sigall_test.go`, `settle_test.go`): the template
  output secrets are now scoped to the bid's nonce (`templates(..., tag)` /
  `mkOut(..., tag)`). Before, two legs of the same bidder to the same recipient shared an
  output secret, so the *test fixture* — not the chain — refused the second settle with
  "proof already used". A multi-leg chain needs distinct per-leg output secrets, which is
  also what a real wallet does.
- `liveApp` split into `liveApp` + `liveAppSeller` so a test can sign an event **as the
  lot's seller** (needed for the R3 seller case).

Live probes:

- **new `tests/probe-additive-rebid.mjs`** — the two-leg chain, the admission fact from the
  relay, the below-floor refusal, and per-leg settlement with the receipts' sum. Wired into
  `scripts/acceptance-validator-workstreams.sh`.
- `tests/probe-instant-refund.mjs` — added the `[R1-A]` section: a leader raises, a
  challenger takes the lead, **both** legs are outbid, refunded, SPENT at the mint and
  receipted (kind 1443 each).
- `tests/e2e.mjs` — the under-increment block now proves the **R3 refusal** (nothing
  committed, no void, escrow left locked) instead of the old void + instant refund; the
  escrow is recorded as stranded so the reclaim/conservation steps stay exact.

**Fresh-root acceptance run** (`scripts/acceptance-validator-workstreams.sh`: it stops the
deployed unit, initialises `run/chain-e2e` — never `run/chain` — runs the probes and the
walk, then restarts the unit): `probe-restart-ingest` **5 passed / 0 failed**,
`probe-instant-refund` **93 / 0**, `probe-additive-rebid` **55 / 0**, the `e2e` walk
**64 passed / 4 failed** — and all four are the **pre-existing** create-refusal assertions of
§7-5, unrelated to this change. Conservation in that run: `seller 360 + alice 640 + bob 1000
+ carol 885 + 115 awaiting locktime = 3000 (3000 minted)`, every escrow the lots recorded
settled at the mint and on chain.

## 5. Deploy and live proof

Deploy (discipline in `docs/HANDOVER-2026-11-19-….md:182-193`): built
`~/work/wt-validator/bin/arena`, kept the previous binary as
`~/work/arena-merged-ref/bin/arena.prev-2026-11-19-before-additive-rebid`
(sha256 `7de9d160995c5993…`), stopped `arena-chain.service`, swapped, started. **No
`arena init`, no reset, no relay/mint/app-tree change.** (The in-place `cp` first failed
`Text file busy`, so the unit was stopped for the swap — the one restart is what keeps the
four validators in lockstep.)

Proof from outside, before → after:

| check | evidence |
|---|---|
| height continues | pre `height 3433` → post `height 3433` (idle chain), then the probes took it to **3482** |
| root intact | the app hash at 3433 is **identical** before and after: `7025a6462b2cd425785fea951cbb1503c5cbb096fba9b3d459f075910acf0d30`; `params.groupKey` `03be47f12cc26f63…` and `keysetId` `0102b930f4dc…` unchanged |
| four validators agree | `/api/network`: 4 up, all at one height, one app hash |
| schema strings present | deployed `bin/arena` carries `arena.{bidadmit,lot,params,refund,refused,settle,statecommit,statepulse,verdict}.v1`; the previous binary carries **no** `arena.bidadmit.v1` |

Live (deployed gateway `127.0.0.1:8090`, relay `wss://relay.playday.it`),
`node tests/probe-additive-rebid.mjs` — **55 passed, 0 failed**:

1. **Two-leg rebid chain (R1-A/R2-A).** leg 1 = 100 (bid height 3449), the leader's own
   raise leg 2 = 5 (bid height 3450): `highAmount 105 == 100 + 5`, `highBid` = leg 2, both
   legs `leading`, leg 2's escrow is **5** sats (the delta), `Σ leading legs 105 ==
   highAmount`.
2. **Admission fact (R4) on the relay.** one `arena.bidadmit.v1` event per admitted bid;
   `amount` = 100 / 5, `position` = 100 / 105, `high_bid` names the leading leg, anchored at
   **the bid's own** height (3448 and 3450 — the lot's create was at 3445), `e`/`auction`/
   `height`/`hash` tags agree with the content, `p` names the signer, signer on the roster.
3. **Below-floor refusal (R3).** a forced 114-sat lock submitted at a floor of 115 was
   answered by the chain, verbatim:
   `bid must add at least 115 sats: your position would reach 114, the floor is 115`
   (`code=2`, at admission). `/api/state`: **no** bid of that bidder, **empty** void list,
   the lot still at 105; the mint's `checkstate` for the 4 escrow proofs: `[false,false,false,false]`
   (untouched); the wallet still shows them locked.
4. **Settlement over every won leg.** the lot closed with the newest leg as winner, **2**
   won legs summing to 105, both settled, every settled output P2PK-locked to the committed
   seller wallet, the seller's wallet received **105**, and **one kind-1444 receipt per
   won leg** whose `out` amounts sum to **105 == highAmount**.

`node tests/probe-instant-refund.mjs` — **93 passed, 0 failed**; the new `[R1-A]` section:
a leader raises (position 105), a challenger takes the lead, both legs are `outbid` and
recorded `settled:refund` 1102 ms later while the lot is still OPEN, both legs' escrow
proofs are SPENT at the mint (3 + 2), and each leg has its own kind-1443 receipt.

## 6. Q3–Q8 status

- **Q3 (V4V per leg).** Implemented per leg — each leg's settle pays the committed split of
  *its own* amount, the seller takes each leg's remainder, and a delta the split cannot
  represent is **refused at admission** by `settleMatchesSplit` (`chain/v4v.go:226-231`),
  not void. `TestV4VMultiLegSettlementSumsToPosition` proves it (recipient 75+50=125,
  line items sum to 500 = the position; a 3-sat delta refused with "rounds to 0 sats").
  Whether recipients may lose up to `legs−1` sats of rounding remains a **product ruling**,
  not a code gap. The leader's 1-sat raise is impossible on a V4V lot whose smallest row
  rounds to 0 at that delta — that follows from the representability rule, not from R2-A.
- **Q4 (partial settlement).** Visible and expressed by the chain without change:
  `TestPartialChainSettlementIsVisible` shows "paid 300 of 500" with the second leg still
  due. The wording a surface uses stays a product decision.
- **Q5 (is the set of 1444 receipts the final amount?).** Unchanged: yes, receipts are the
  money facts; the verdict still carries no amount (no wire change made). Not decided by me.
- **Q6 (may several bids carry `leading`/`won`?).** Yes — that is now the normal state, and
  it is what `/api/state` and `BidView.status` publish. No schema change. Any client that
  assumed exactly one is now wrong; that is an **app-side** item.
- **Q7 (is `au.Winner` still the lead's newest leg?).** Kept as is: `Winner = HighBid`, the
  leg that set the position, which survives close unchanged (`closeDue`). Several won legs
  exist and each has a receipt; the verdict still names one `bid_id`.
- **Q8 (may the app's floor be advisory?).** Untouched — app-side. Implemented chain-side so
  the app's two options both work; the chain now answers the app's real floor and refuses at
  admission, so a stale app sees the refusal instead of a void.

## 7. Findings and follow-ups (not done, deliberately)

1. **The plan's §5.1 items 4–5 contradict its own §2.2.** They are written as if a
   displaced bidder's position counted their *outbid* legs (a required "delta (20)" and a
   "position 20, not 520"). §2.2 — restated by R1-A — makes a non-leader's position `0`, so
   a challenger's delta must reach `top + increment`. I implemented §2.2 and wrote the tests
   to it; `TestPositionFloorAdmitsReachingDelta` and `TestBelowFloorBidIsRefusedAtAdmission`
   state the numbers explicitly.
2. **A mixed-version set will stall** on the first rebid block (the plan's warning stands).
   The gateway's `late:` answer is kept only for that window.
3. **The denomination constraint from `docs/HANDOVER-2026-11-19-….md:113-178` still blocks
   the *ordering* the plan asked for**: additive deltas make small, non-denominational legs
   normal, and an app template that emits a non-denomination amount can never settle. My
   probe lots use only representable amounts (100 = 64+32+4, 5 = 4+1), so they settled;
   that is not a general guarantee. The chain-side companions in that handover (§4 items
   5–7) are still outstanding and were not in this ticket.
4. **App-side follow-ups unchanged** (plan §6): positions must stop counting dead legs, the
   lot-level settle line and the purchase row must aggregate legs, the read's claim that a
   void bid produces a kind 1450 is wrong, `bidMath.ts`'s second floor doctrine must be
   deleted or aligned, and `web/src/lib/arena.js:288` still refuses a leader's own raise
   client-side (`lockForBid`) — the live probes must pass `force: true` for it.
5. **The e2e's four create-refusal assertions are PRE-EXISTING red**, and I did not weaken
   them: `anti-snipe window below the minimum refused`, `unknown auction mode refused`,
   `invalid lot rejected`, `lot with an unsafe image path rejected`. Cause: since `d6a1568`
   ("chain: admit refused creates so kind 1450 can name the reason", 2026-11-18) the create
   branch deliberately admits a refused create at `CheckTx` so the chain can anchor a
   kind-1450 to it, so `createAuction` RESOLVES and `expectReject` reports "was accepted".
   Measured live against the deployed chain with all four payloads: each answers
   `{"ok":true,...}` (the create is refused at delivery, and the 1450 carries the reason —
   `probe-state-half.mjs` proves that half). The assertions predate `d6a1568` (`3c7a55e`,
   2026-11-13); they need to be re-expressed as "read the kind-1450", not as "the submit
   throws", which is a separate change.
6. **Pre-existing gate finding, now fixed:** `chain/v4v_test.go` and
   `chain/verdictpublish_relay_test.go` were **already** unformatted at `42af5c5` (a stray
   blank line each), so `gofmt -l` was dirty on the baseline. Both are now formatted (one
   whitespace line each) because I had to edit the former anyway.
7. Not touched: the already-recorded defective lot `1440:becb0c53…:612a25ed-…`, the relay,
   the mint, and every app tree.
8. **Rollback format-safety re-measured** after the swap: the state's key sets are identical
   before and after (`{'auctions','bids','height','order','params','rejections','time',
   'txCount','usedY'}`; a `Bid` carries the same 13 keys), so the previous binary can load
   new snapshots. The *semantics* are not reversible (a multi-leg winner's other legs would
   stop settling), as the plan's §4.5 already says.