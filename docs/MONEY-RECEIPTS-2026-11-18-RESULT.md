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
The only editorial change is that `content` is shown as an expanded JSON object for readability —
re-compacting it reproduces the relay's `content` string byte for byte (checked), and `id`, `tags`,
`pubkey`, `created_at` and `sig` are verbatim. Four validators publish one receipt each for a move, so
other ids exist for these same two bids with identical content and anchor.

**Kind 1443, a refund executed** — the outbid bidder got their sats back, 3 outputs to their wallet key:

```
      RAW 1443 EVENT 05d9f76f7ed773dcbcd3f0ea17478394d95c3edca70e79a6cbc4bfa9a132a6fb
{
  "kind": 1443,
  "id": "05d9f76f7ed773dcbcd3f0ea17478394d95c3edca70e79a6cbc4bfa9a132a6fb",
  "pubkey": "b81658ebbfcf2cd6f7bbcbe470852bd47d2ef058444ac0bb53eef3d0d287b512",
  "created_at": 1794997170,
  "tags": [
    ["auction", "1440:95f8680fc450c284057b1c75eddd918f844a12f059059636ab92a63dc821b280:instant-refund-1794997165950"],
    ["bid", "7fb1667abb43b8d5bf790880595bd74695f4419768a4e549207fdb7bd6a8907f"],
    ["height", "1276"],
    ["hash", "B1CD2D599FEA52490292D9C45D97F5C5DA462B3DE67D163136E70F7F3137347C"],
    ["outcome", "refund"],
    ["p", "b81658ebbfcf2cd6f7bbcbe470852bd47d2ef058444ac0bb53eef3d0d287b512"]
  ],
  "content": {
    "schema": "arena.refund.v1",
    "auction_id": "1440:95f8680fc450c284057b1c75eddd918f844a12f059059636ab92a63dc821b280:instant-refund-1794997165950",
    "bid_id": "7fb1667abb43b8d5bf790880595bd74695f4419768a4e549207fdb7bd6a8907f",
    "outcome": "refund",
    "height": 1276,
    "block_hash": "B1CD2D599FEA52490292D9C45D97F5C5DA462B3DE67D163136E70F7F3137347C",
    "settled_at": 1794997169,
    "params_version": "71ca57eb83d61d0bc59a2aebdd8557916acd4b9f34865e3c36d7ed7d1311298b",
    "out": [
      {"recipient": "0238ccb10363964dd629d9ab32bd94e513592488e780d125d98b71b6010df7c2b4", "amount": 64,
       "id": "0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
       "secret": "[\"P2PK\",{\"nonce\":\"f9a2e50ec52929db2c39c06608f3728f1a78c52a91ccc0f9209d90d4602ae9a6\",\"data\":\"0238ccb10363964dd629d9ab32bd94e513592488e780d125d98b71b6010df7c2b4\",\"tags\":[[\"sigflag\",\"SIG_INPUTS\"]]}]",
       "C": "035bb3eededd00a0552ac10820ffbbb87a4215298f82b7225657cab492373ee34c",
       "dleq": {"e": "77d675f3c0ee507a59a7d27bd3fa1f7bb5a3b4b197dbb71bb538f2082c92a5fe",
                "s": "7d5b12fa0279eb79742ad042b9e800c6b378ece9c4ec623921bfdcdd8cea3ad1",
                "r": "141d515b56e6a01b9ac0546c6b0c3112053faffc08192b6f6fac6daea448ce54"}},
      {"recipient": "0238ccb10363964dd629d9ab32bd94e513592488e780d125d98b71b6010df7c2b4", "amount": 32,
       "id": "0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
       "secret": "[\"P2PK\",{\"nonce\":\"fdabaa7dbdfeec85a075737bd15cc3e8372b0dfc0f140e9028196d8a6ac7ff32\",\"data\":\"0238ccb10363964dd629d9ab32bd94e513592488e780d125d98b71b6010df7c2b4\",\"tags\":[[\"sigflag\",\"SIG_INPUTS\"]]}]",
       "C": "0208d15f072aebb09107bac01371303627d554b979478c7be89c9136181d8d3e69",
       "dleq": {"e": "664187d178c00d8104227e34e712ecddf808cd266bc80f2ffabd47d707ba951b",
                "s": "8cac76e39b8ae3bcefeb6314c6250e955e22952445cf64b8edec89adcbc31400",
                "r": "b616268fa10766e0fd5625d5293c72589fc7629a42caf6a8f3162aeb8c0ed4bf"}},
      {"recipient": "0238ccb10363964dd629d9ab32bd94e513592488e780d125d98b71b6010df7c2b4", "amount": 4,
       "id": "0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
       "secret": "[\"P2PK\",{\"nonce\":\"0232c03360df31653ca515f8f0295dec610227566a3888339d249e870c101286\",\"data\":\"0238ccb10363964dd629d9ab32bd94e513592488e780d125d98b71b6010df7c2b4\",\"tags\":[[\"sigflag\",\"SIG_INPUTS\"]]}]",
       "C": "03de9674ea55d191f5e9df06030397c0dde5e8943a0bf3a05e83afa0709b10c00c",
       "dleq": {"e": "604acf1c4a173c91e89d5130d699207bd889428257588692ec91606d3c587826",
                "s": "c547a8828408bd97778f7aa7d75cb32604468c9126739fbb14eaeaf96866bd93",
                "r": "68a52aad7b632156a505057b0a08c41c0902dc7d5823e99ecaed6c39daf523fc"}}
    ]
  },
  "sig": "08ca8691f33ea9fae91279809b1d6857ed54c2bfbc17ba09d69f2fcde209415e5c7cf04af75989a0ab4391900c266d7301db84907fd7e7310415978791dd3943"
}
```

**Kind 1444, a settle executed** — the seller's payout for the closed lot:

```
      RAW 1444 EVENT dfe501d303564fdae67e3f0c613cb32ad315a080800990793ffbdb0dfe4015e2
{
  "kind": 1444,
  "id": "dfe501d303564fdae67e3f0c613cb32ad315a080800990793ffbdb0dfe4015e2",
  "pubkey": "1e17774d0db6fbffc4488b607d4b9e3c8295212fd1c556d54ee68df9d7e00e38",
  "created_at": 1794997197,
  "tags": [
    ["auction", "1440:95f8680fc450c284057b1c75eddd918f844a12f059059636ab92a63dc821b280:instant-settle-1794997171425"],
    ["bid", "e8f2668840bd7e0f57f763d4b5b0cbc9ffa4ae769798021bb3462a92b1218f4a"],
    ["height", "1283"],
    ["hash", "449DF116E509979531A971434A7EA974E628A2D79279DE1A440CD5236270895B"],
    ["outcome", "settle"],
    ["p", "1e17774d0db6fbffc4488b607d4b9e3c8295212fd1c556d54ee68df9d7e00e38"]
  ],
  "content": {
    "schema": "arena.settle.v1",
    "auction_id": "1440:95f8680fc450c284057b1c75eddd918f844a12f059059636ab92a63dc821b280:instant-settle-1794997171425",
    "bid_id": "e8f2668840bd7e0f57f763d4b5b0cbc9ffa4ae769798021bb3462a92b1218f4a",
    "outcome": "settle",
    "height": 1283,
    "block_hash": "449DF116E509979531A971434A7EA974E628A2D79279DE1A440CD5236270895B",
    "settled_at": 1794997196,
    "params_version": "71ca57eb83d61d0bc59a2aebdd8557916acd4b9f34865e3c36d7ed7d1311298b",
    "out": [
      {"recipient": "02fe7c4594ac40533a65e8285a75e6d75e0e230b4a5c023625f9138e965ab8762b", "amount": 64,
       "id": "0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
       "secret": "[\"P2PK\",{\"nonce\":\"dc87de775f0ef8ce8365de08a0adb93b785364e8eaef75e86335da3ca087e22d\",\"data\":\"02fe7c4594ac40533a65e8285a75e6d75e0e230b4a5c023625f9138e965ab8762b\",\"tags\":[[\"sigflag\",\"SIG_INPUTS\"]]}]",
       "C": "03c61b1e5484609e275e9abe4d0b7adb62a890b81b24b52abdea2d77c55d83bcb5",
       "dleq": {"e": "807f00a0b5a36b9c3478a1aab74849bc627224f54d791d6da230dd297b684e18",
                "s": "16d3e040ed249e087c84e07615e2bc836cdf72828a98688b164c914ec022d2df",
                "r": "216ebe3e458e631322a43b91dc47d459288156b7e79486cbaab2d2ac4d3d5613"}},
      {"recipient": "02fe7c4594ac40533a65e8285a75e6d75e0e230b4a5c023625f9138e965ab8762b", "amount": 32,
       "id": "0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
       "secret": "[\"P2PK\",{\"nonce\":\"8f9be209e05b0e733e4e26fa9062d5143b8299831545eeefffbf5cc8f1de69d5\",\"data\":\"02fe7c4594ac40533a65e8285a75e6d75e0e230b4a5c023625f9138e965ab8762b\",\"tags\":[[\"sigflag\",\"SIG_INPUTS\"]]}]",
       "C": "02a47db5af4c5317eb96fcb1017882b8df21b0ba04b0a02fc3e15bc3c8802e2453",
       "dleq": {"e": "e328295500d282e7865ea731b70ca46093c9b9fb6d151ab2caff6dde439b68df",
                "s": "783fbf8173299aef8139913d47875341e60505e7fb800d5d021c9132ae02c65f",
                "r": "6bb2a8ecdf5cf9f2a7e2902f1fadcd7bf5e150152c512e260d7eb4bcad1b344d"}},
      {"recipient": "02fe7c4594ac40533a65e8285a75e6d75e0e230b4a5c023625f9138e965ab8762b", "amount": 4,
       "id": "0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
       "secret": "[\"P2PK\",{\"nonce\":\"5cfb722ddbacc5087283a717d01ef13b7fc8e762eff86bdb4cf9c4456ce0ca68\",\"data\":\"02fe7c4594ac40533a65e8285a75e6d75e0e230b4a5c023625f9138e965ab8762b\",\"tags\":[[\"sigflag\",\"SIG_INPUTS\"]]}]",
       "C": "025c40e02d8ebb0239f3a922880ca9ff47f6544c8f83a4568110da9bc8c227b4c6",
       "dleq": {"e": "b9039b604dd4dcb549edfe1ec1eb9a6d6b510032a29c02c01902447a85e4de34",
                "s": "293438e0b00e8015ce03cb4c9f1c55d290ea0b4b5dc4f191d92da6e448c6a345",
                "r": "e9d2104b3708316465284c9063bd783b6a15f72f932e42f2662d8ac178325835"}}
    ]
  },
  "sig": "3cf73b0a05226b74a6300bea8de1a0a40af6b7a2453574db94cd345f9c6aa4485e40851b172578ad2986eb97deec11b7f5c9e80eafb4315303b41d2186a21920"
}
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
is still `open` with a leading bid of 110 sats, which is bob's escrow; the chain blocks on demand but
still advances when idle (measured: one block per 30–90 s), so it closes at the first block after its
end and the settler then moves that escrow. Both are ordinary lots in the live state; nothing is
stranded by this increment.

## 6. What I could not do

- **Nothing about the market repo.** All three follow-ups from the previous handover's §9 (read the
  terms from kind 1448, the read's dedupe key, the app's own lock derivation) are still the app lane's.
- **D's "a lock assembled from them has `pubkeys` equal to the published `GroupKey`"** — not
  exercised; it is a market-side assertion.
- **The live root's historical settles have no receipt** (see §5). That is a limitation of the data,
  not of the publisher.
