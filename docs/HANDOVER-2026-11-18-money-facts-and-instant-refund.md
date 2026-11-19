# HANDOVER — the chain's money facts: publish 1443/1444 receipts, and prove the instant refund

**For:** an omp session working the arena chain (`~/work/wt-validator`, branch
`feat/validator-workstreams`, plus `fix/params-cadence-edge` to merge in).
**From:** the app-side session that owns the market read, on 2026-11-18.
**Why:** the app can now read every *claim* the chain makes (creates, bids, verdicts, commitments,
params) but not one *receipt*: nothing publishes kind 1443 (refund) or 1444 (settle), so the app can
show "outcome not verified" and can never show "refunded" or "paid", and its wallet can never claim the
ecash the validators mint. This increment closes that, and is the gate on the app's order-management
work.

Read this whole file before touching code. §3 is a wire contract the app is being written against.

---

## 1. State of play, measured 2026-11-18

- Deployed and live: `arena-chain.service`, 4 validators on `127.0.0.1:7101-7104`, gateway `:8090`,
  root `run/chain` (never re-initialise it), relay `wss://relay.playday.it`, mint `:3338`.
- Running binary: `fix/params-cadence-edge` @ `9d08340` (it carries the params-cadence fix below).
- Live state right now: 27 bids — 13 `won/settle`, 10 `lost/refund`, 2 `void/refund`, 2 `leading`;
  179 kind-1445 verdicts and 177+ kind-1447 commitments on the relay.
- **So close-time settlement and close/void refunds already work** (`settler.go` + `settle.go`, driven by
  `dueMoves`/`outcomeDue`). What does not exist is any way for a client to *see* them.
- The validator delivery report (`docs/VALIDATOR-WORKSTREAMS-2026-11-17-RESULT.md`) says the outbid
  refund "refunds nothing". That was measured on an older ref: `outcomeDue` today accepts
  `outbid | lost | void` as refundable and the settler sweeps on every block, so the **mechanism may
  already be live**. §4.1 tells you how to settle that question by measurement before building anything.

## 2. What to build, in order

1. **Receipts (A1)** — publish kind 1443/1444 per validator, anchored exactly like a verdict, on every
   executed money move, carrying the minted outputs so a client can claim them from the relay alone.
2. **The settle record's when (A1b)** — `applySettle` sets `Bid.Settled` but no height or time, so nothing
   can say *when* money moved. Add the settling height and block time to committed state (a format bump
   is fine — this is a dev chain; state is disposable) and carry them in the receipt.
3. **Prove the instant refund (A2)** — see §4.1. If it already works, the work is the probe plus the
   receipt; if it does not, implement it in `applyBid`'s leader change with the failure posture in §5.
4. **Pin the two rules (A4)** — a test that an extending bid's locktime floor comes from
   `now + WindowSec` at **block** time (`State.minLocktime`), and a test that the anti-snipe extension is
   **uncapped** (`MaxTotalExtension` must not exist anywhere the app can infer it).
5. **Merge the cadence fix (A5)** — `fix/params-cadence-edge` (`9d08340`) makes the kind-1448 cadence
   edge-triggered: `height % every == 0` is a level, and while the chain idled at height 500 every
   validator re-published the same params event every second (measured: 61 events per validator at one
   height, ~4/s of public-relay storage). It carries
   `TestParamsCadenceDoesNotRepublishWhileIdleAtAMultipleOfEvery`, which fails on the old code (6
   publishes while idle at height 100) and passes on the fix. Merge it, do not rebuild it.

## 3. THE WIRE CONTRACT — kinds 1443 and 1444

The app's decoder is written to this shape. **If you need to change any field name, type or meaning,
say so in your report before you merge** — the two halves are useless if they disagree, and this is
exactly the seam where both sides can be green and the feature still dead.

**Kind 1443** = a refund executed; **kind 1444** = a settle (payout) executed. Both are **stored,
regular** kinds (already in `chain/nostr.go`'s map: `KindRefund = 1443`, `KindSettle = 1444`).

Content (snake_case, mirroring `ParamsContent`/`VerdictContent`):

```json
{
  "schema": "arena.refund.v1",            // or "arena.settle.v1"
  "auction_id": "1440:<seller>:<d>",      // the coordinate, exactly as state keys it
  "bid_id": "<the 1441 event id whose escrow moved>",
  "outcome": "refund",                    // or "settle"
  "height": 1201,                         // the committed height the settle landed at
  "block_hash": "<64 lowercase hex>",
  "settled_at": 1794999999,               // the block TIME of that height
  "params_version": "<the 1448 digest in force>",
  "out": [
    {
      "recipient": "<compressed 33-byte wallet pubkey this output is P2PK-locked to>",
      "amount": 1234,
      "id": "<keyset id>",
      "secret": "<the output's secret>",
      "C": "<the output's C>",
      "dleq": {"e": "…", "s": "…", "r": "…"}   // null when the mint gave none
    }
  ]
}
```

Tags (same discipline as a verdict — the anchor is **mandatory** and cross-checked against the content):

```
["auction", "<auction_id>"], ["bid", "<bid_id>"], ["height", "<n>"], ["hash", "<block_hash>"]
```

Rules the app relies on:

- **One output entry per committed destination.** A V4V settle pays several recipients from one escrow;
  each output is locked to ITS committed recipient, so `out` is the list, not a single proof.
- **`out` may be published.** Those proofs are P2PK-locked to `recipient`; spending needs that key, so
  the secret is not a bearer instrument here. It is the only relay-only path for the app's claim
  watcher — without it the app would need the chain's HTTP state, and the app has just deleted its
  last client→backend HTTP path on purpose.
- **Sign + roster:** signed by the executing validator's verdict key, roster-checkable exactly like
  1445 (`ParseVerdict`'s pattern). Another validator may publish its own receipt for the same move —
  clients dedupe by `(bid_id, outcome)` and prefer the **earliest** anchored height.
- **Idempotent:** at most one receipt per `(bid_id, outcome)` per validator per height. A restart may
  re-publish at a later height (the `published` map is in memory); that is acceptable, see the dedupe
  rule above. Do **not** publish on a cadence — this is a receipt, not a heartbeat.
- **No secrets beyond the locked proofs:** do not put the bidder's or seller's private anything in the
  event, and do not add a second signature scheme.

## 4. Where the code goes

### 4.1 First: measure the instant refund (do this before writing A2)

`State.outcomeDue` (`chain/settle.go:76`) already returns the bidder's wallet for a bid whose status is
`outbid`, and `settler.run`/`poke` sweeps on every block — so the refund may already fire while the lot
is still open. Measure it:

1. Create a lot with a 10-minute duration and an anti-snipe window (`-demo-controls` gateway on a
   **fresh root in a separate directory**, never `run/chain`).
2. Bid A (funded), then bid B at a higher amount, and **do not close the lot**.
3. Poll `/api/state` and the mint: within a block or two, A's bid must show `settled: "refund"` with a
   non-empty `out`, and the mint must show A's original proofs spent. Record the height and wall time.
4. If it does: A2 is "prove it, and publish the receipt at that moment" — do not add a second refund
   path. If it does not: implement it in `applyBid`'s leader change (`chain/app.go`, the
   `s.Bids[au.HighBid].Status = "outbid"` line) by making the refund due at that height, and keep the
   same routine for `void` and `lost` (one routine, three triggers).

### 4.2 The publishing hook

Mirror `publishVerdicts` (`chain/verdictpublish.go:308`): scan committed state for bids with
`Settled != ""` that have no receipt published for `(bid, settled)` yet, require the committed block
hash at the current height (`p.blockHash(height)`), refuse to publish unanchored, and record the publish
so a transient relay failure retries. Add `BuildRefundEvent`/`BuildSettleEvent` +
`ParseRefundEvent`/`ParseSettleEvent` in `chain/verdict.go` beside their verdict siblings, with the same
signature/schema/anchor/roster refusals named as sentinel errors.

### 4.3 The settle record's when (A1b)

`applySettle` (`chain/settle.go:257`) sets `bid.Settled, bid.Out`. Add `SettledHeight` and `SettledAt`
(block time) to `Bid`, set them there, and carry them in the receipt as `height`/`settled_at`. If the
state shape changes, bump the chain format and wipe the dev root — **never** migrate (spec §12).

## 5. Failure posture (do not skip this)

- **A leader change must never lose someone's sats.** If the refund spend cannot complete in the block
  that outbids them, the obligation must stay visible and be retried (the settler's `dueMoves` already
  does this — prove it by making the spend fail deliberately and showing the obligation survives).
- **Never refund a winning bid before its locktime** (spec §4): `outcomeDue("settle")` requires a closed
  lot and `au.Winner == bid.ID`; keep that true.
- **No new pull path.** The settler is an executor, not an author: it may only submit a pre-signed
  template and record the result, and it holds no key.

## 6. Tests and acceptance

- Unit: a receipt is built with the contract's fields and tags, an off-roster signer is refused, an
  unanchored content or a tag/content anchor mismatch is refused by name, a params-version mismatch is
  refused, and a receipt is published exactly once per `(bid, outcome)` across repeated passes.
- The settlement/refund cases you already have (`TestPushRefundSettles`, `TestPushSettleRefusesForgeries`)
  must stay green; the new receipt publication rides on them.
- A4's two pinning tests.
- **Live acceptance, and this is the evidence to bring back:** on the demo root, A funds and bids, B
  outbids, and within a block or two: A's bid is `settled: "refund"`, the mint shows A's proofs spent,
  **and a kind-1443 event is on the relay** whose content names A as the recipient and whose anchor
  matches the height it landed at. Then close the lot and show the kind-1444 for the seller's payout.
  Paste the two real events.

Gates: `cd chain && ARENA_TEST_MINT=http://127.0.0.1:3338 NAK_BINARY=~/.local/bin/nak go test -count=1 ./...`
(the module is `chain/go.mod`; from the repo root it fails with "does not contain main module"),
`go vet ./...`, and the repo's own `tests/e2e.mjs` via
`scripts/acceptance-validator-workstreams.sh` (it uses a fresh root and a local relay; it stops and
restores `arena-chain.service`, so say when you run it). Today's baseline is **144 pass / 0 fail** on
`feat/validator-workstreams`, and **145** once the cadence fix (with its one new test) is merged.

## 7. Deploy rules (the live chain)

- Deploy by building and swapping the binary: `cd chain && go build -o ../bin/arena .`, copy it over
  `/home/admin_maxime/work/arena-merged-ref/bin/arena` (keep the previous as `bin/arena.prev-<date>`),
  then `systemctl --user restart arena-chain.service`. Rebuild `web/dist` only if you touched the client.
- **Never `arena init` on `run/chain`.** It regenerates the FROST group key and strands every escrow the
  app has locked. A fresh root goes in its own directory (`run/chain-e2e`).
- Prove the deploy from outside: the unit is active, the root's height **continues** (does not reset to
  single digits), and the relay shows the new kinds.

## 8. Do not

- Do not change the kind map's existing entries, the 1440/1441 wire fields, or the verdict semantics —
  all four are live contracts the app is built against.
- Do not add HTTP endpoints for the app to read. It has no HTTP path to the backend any more, by ruling.
- Do not touch the market repo (`~/work/*/wt-app-*`, `~/work/market`) — the app half is a separate lane.
- Do not re-init, force-push `origin/main`, or push to ChiefmonkeyArt's upstream.

## 9. Report back

A checklist: what you measured first (§4.1's answer, with the height and timing), the commit shas, the
exact content and tags of **one real refund receipt and one real settle receipt** pasted from the relay,
the gate numbers before and after, anything you changed in the wire contract and why, and anything you
could not do — plainly, named, never filled in with a plausible guess.
