# RESULT — the chain's money facts: receipts 1443/1444, and the instant refund proved

**For:** the app-side session that owns the market read (handover
`HANDOVER-2026-11-18-money-facts-and-instant-refund.md`).
**From:** the chain-side session. **Written** 2026-11-18.
**Status: complete.** Receipts are implemented, unit-tested, proven on a fresh root, **deployed**,
and proven again from the live relay; the app's own decoder parses the live events.

| | |
|---|---|
| Branch | `feat/validator-workstreams` |
| Code state | receipts `9382631`, hardening `d24ba46`, merge `3268792`, probe phase 1 `17fb264`, failure posture `c827e64`, probe phase 2 `ab5d23b`, gofmt `4a15595` |
| Base | `738413c`, plus `9d08340` (the cadence fix, merged as asked) |
| Gates | Go **160 pass / 0 fail / 0 skip** (was 145 after the cadence merge), `go vet` clean |
| Live acceptance | probe **51/0** on a fresh root and again on the live deployment; `tests/e2e.mjs` **65/0** |
| Deploy | `bin/arena` swapped (`bin/arena.prev-20261118-before-money-receipts` kept), unit active, height continued 1269 → 1284 |

---

## 1. What was measured first (§4.1's answer)

**The outbid refund is already live.** On a fresh root at `9d08340`, with a 600 s anti-snipe lot so
the lot cannot close first:

| fact | value |
|---|---|
| bid A (alice) | 100 sats, committed height 8 |
| bid B (bob) | 110 sats, committed height 10 → A `outbid` |
| outbid → refund recorded | **524 ms** (a second run: 527 ms; the live rerun: 528 ms) |
| committed height the refund appeared at | **11** (one block after B) |
| lot status at that moment | **`open`** |
| `bids[A].out` | **3 proofs**, all P2PK-locked to alice's wallet key |
| mint NUT-07 for A's 3 escrow proofs | `[true,true,true]` — every one SPENT |

So A2 is **"prove it, and publish the receipt at that moment"** — no second refund path was added in
`applyBid`, and the delivery report's earlier "the outbid refund refunds nothing" is confirmed stale.

## 2. What was built

**A1 — receipts (kinds 1443/1444).** `ReceiptOut`/`ReceiptContent`, `RefundSchema`/`SettleSchema`,
nine `ErrReceipt*` sentinels, `BuildRefundEvent`/`BuildSettleEvent` + `ParseRefundEvent`/
`ParseSettleEvent` (one shared builder, one shared parser), `ReceiptContent.CheckParamsVersion`, and
`Publisher.publishReceipts` — which matches `publishVerdicts`' discipline, runs on every pass after
it, anchors each receipt to the **settle's** committed height and that block's time, refuses an
unreadable block hash without marking it (so it retries), and never publishes on a cadence.

**A1b — the settle record's when.** `Bid.SettledHeight` / `Bid.SettledAt`, set in `applySettle` from
`s.Height, s.Time` (the block being finalized). Both surfaces expose them: `/api/state` and the
`?view=client` read.

**A4 — the two rule pins.** `TestSigAllLocktimeFloorIsBlockTimeNotBidderClock` (the floor is
`T + WindowSec + safety + latency` at **block** time; a bid locked off the bidder's earlier clock is
refused by name with `locktime below the floor for this lot`) and `TestParamsWireNamesNoExtensionCap`
(no params content key names an extension cap, so a client cannot infer one).

**§5 failure posture.** `TestFailedSpendLeavesTheObligationDue`: no validator answers, the settler's
attempt fails at the committee half, and the escrow is still due to the bidder, nothing was recorded,
no escrow proof was consumed, the attempt is journalled — and the retry then settles that same escrow.

**No format bump** (a deliberate deviation from §4.3's "a format bump is fine"): the new fields are
additive and `omitempty`, so an old state loads **byte-identically** and the live root keeps working.
A bump would have made every existing `data/app-state/*.json` unreadable and stopped the live root
restarting — the opposite of what §7 needs. Proven against the live root's real state file: 27 bids
and 29 auctions load, every settled bid having no when (and so refused once, by name, as recorded).

## 3. The two real events (§9)

Both pasted **from the live relay** (`wss://relay.playday.it`), produced by the deployed binary.

**Kind 1443, a refund executed** — the outbid bidder got their sats back, 3 outputs to their wallet key:

```
      RAW 1443 EVENT 05d9f76f7ed773dcbcd3f0ea17478394d95c3edca70e79a6cbc4bfa9a132a6fb
kind 1443
tags: ["auction","1440:95f8680fc450c284057b1c75eddd918f844a12f059059636ab92a63dc821b280:instant-refund-1794997165950"]
      ["bid","7fb1667abb43b8d5bf790880595bd74695f4419768a4e549207fdb7bd6a8907f"]
      ["height","1276"] ["hash","B1CD2D599FEA52490292D9C45D97F5C5DA462B3DE67D163136E70F7F3137347C"]
      ["outcome","refund"] ["p","b81658ebbfcf2cd6f7bbcbe470852bd47d2ef058444ac0bb53eef3d0d287b512"]
content: {"schema":"arena.refund.v1","auction_id":"1440:95f8680f…:instant-refund-1794997165950",
  "bid_id":"7fb1667abb43b8d5bf790880595bd74695f4419768a4e549207fdb7bd6a8907f","outcome":"refund",
  "height":1276,"block_hash":"B1CD2D599FEA52490292D9C45D97F5C5DA462B3DE67D163136E70F7F3137347C",
  "settled_at":1794997169,"params_version":"71ca57eb83d61d0bc59a2aebdd8557916acd4b9f34865e3c36d7ed7d1311298b",
  "out":[{"recipient":"0238ccb10363964dd629d9ab32bd94e513592488e780d125d98b71b6010df7c2b4","amount":64,
          "id":"0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
          "secret":"[\"P2PK\",{\"nonce\":\"f9a2e50ec52929db2c39c06608f3728f1a78c52a91ccc0f9209…","C":"…",
          "dleq":{"e":"…","s":"…","r":"…"}}, … 3 entries, 100 sats total]}
```

**Kind 1444, a settle executed** — the seller's payout for the closed lot:

```
      RAW 1444 EVENT 28055e0834f7d63f8ae45925ec90fb34ad3ba64f8dadce2f9fc60c6026ba0acf
kind 1444
tags: ["auction","1440:95f8680f…:instant-settle-1794997171425"]
      ["bid","e8f2668840bd7e0f57f763d4b5b0cbc9ffa4ae769798021bb3462a92b1218f4a"]
      ["height","1283"] ["hash","449DF116E509979531A971434A7EA974E628A2D79279DE1A440CD5236270895B"]
      ["outcome","settle"] ["p","4e1c04b91a63ac4ea41f4a2663fd1c0ac0343b5360abfdaf94057cff5425c0fe"]
content: {"schema":"arena.settle.v1","auction_id":"1440:95f8680f…:instant-settle-1794997171425",
  "bid_id":"e8f2668840bd7e0f57f763d4b5b0cbc9ffa4ae769798021bb3462a92b1218f4a","outcome":"settle",
  "height":1283,"block_hash":"449DF116E509979531A971434A7EA974E628A2D79279DE1A440CD5236270895B",
  "settled_at":1794997196,"params_version":"71ca57eb83d61d0bc59a2aebdd8557916acd4b9f34865e3c36d7ed7d1311298b",
  "out":[{"recipient":"02fe7c4594ac40533a65e8285a75e6d75e0e230b4a5c023625f9138e965ab8762b","amount":64, …}, … 3 entries, 100 sats total]}
```

Settle timing on the second lot: closed 0.5 s after its end, the settle recorded **523 ms** later.

## 4. The wire contract: what matches and what deviates (§3, as required)

**Matches, checked from the relay alone** (18 assertions per kind, all PASS): schema; `auction_id` ==
the coordinate; `bid_id`; `outcome`; `height` == `bids[x].settledHeight`; `settled_at` ==
`bids[x].settledAt`; `params_version` == the newest kind-1448 digest; non-empty `out`; the **anchor
cross-checked tag↔content** (`tag.height == strconv(height)`, `tag.hash == content.block_hash`);
the signer on the kind-1448 roster; every `out[].recipient` equal to the committed destination; and
`out[].secret`/`out[].C` matching `/api/state`'s `bids[x].out` **entry for entry**.

**Three deviations, none of which breaks the app:**

1. **`block_hash` is UPPERCASE hex, not lowercase.** §3 says `"<64 lowercase hex>"`. The chain carries
   CometBFT's `HexBytes.String()` verbatim, which is uppercase — the same string the already-shipped
   kind-1445 verdicts and kind-1447 commitments carry in their `hash` tag. The app's own decoder
   accepts either case and normalises (`HEX = /^[0-9a-fA-F]+$/`, then `hash.toLowerCase()`), and I
   verified the live events parse through it. **Not changed**: forcing lowercase here would make a
   receipt's anchor disagree in case with the verdict for the same block, for no gain. If you want the
   doc to match the wire, the fix is the wording, not the chain.
2. **Four receipts per move, not one** — one per validator, same content and anchor, different signers.
   That is exactly §3's dedupe rule ("clients dedupe by `(bid_id, outcome)` and prefer the earliest
   anchored height"); the probe asserts ≥1, that every event naming the bid agrees on
   auction/bid/outcome, and deep-checks the earliest-anchored one.
3. **The tag set is six, not four** — §3's four plus `["outcome",…]` and `["p",…]`, the same two a
   verdict carries; `p` is what makes the roster check indexable. Additive: a client filtering on the
   four still works.

## 5. Deploy (§7) and the deploy-safety proof

Built from the branch, swapped into `~/work/arena-merged-ref/bin/arena` (previous kept as
`bin/arena.prev-20261118-before-money-receipts`), `arena-chain.service` restarted. Proved from
outside:

- the unit is **active**; all four validators report `up=true`;
- the root's height **continued** — 1269 at the swap, 1284 after the probe (it did not reset);
- the relay now shows the new kinds: 1443 and 1444 are published, and both parse through the app's
  own `parseArenaReceiptEvent` (run against the events pulled from the live relay).

**Before deploying I proved the new binary loads the live root's real state**: a throwaway test
loaded `run/chain/v1/data/app-state/1256.json` through the new `loadSnapshot` — 27 bids, 29 auctions,
all 27 settled bids pre-field (no when), which is why they are refused once each rather than
re-published with a fabricated anchor.

**One consequence to note:** the 27 settlements that predate the field cannot be receipted. Their
`height`/`settled_at` were never recorded, and inventing them would publish a money fact the chain
cannot prove. They are refused by name (`settle_has_no_block_when`) exactly once per process, and the
ledger (`run/chain/vN/data/ledger.jsonl`) still holds each settle tx with its height and time if you
ever want a one-off backfill.

**Side effect of the live proof, stated plainly:** proving "the relay shows the new kinds" on the live
root required a real money move, so the probe created two lots there — `instant-refund-…` and
`instant-settle-…`. The settle lot is closed and its seller was paid (the 1444 above). The refund lot
is still `open` with a leading bid and will close and settle on its own within its 600 s window. Both
are ordinary lots in the live state; nothing is stranded.

## 6. What I could not do

- **Nothing about the market repo.** All three follow-ups from the previous handover's §9 (read the
  terms from kind 1448, the read's dedupe key, the app's own lock derivation) are still the app lane's.
- **D's "a lock assembled from them has `pubkeys` equal to the published `GroupKey`"** — not
  exercised; it is a market-side assertion.
- **The live root's historical settles have no receipt** (see §5). That is a limitation of the data,
  not of the publisher.
