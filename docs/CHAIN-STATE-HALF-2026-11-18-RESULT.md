# RESULT — the chain's state half: the committed end (1449), refusals (1450), and the create rules

Implements `HANDOVER-2026-11-18-chain-state-half.md` on the arena repo,
`~/work/wt-validator`, branch `feat/validator-workstreams` (pushed to `maxime-tt`).
The market repo was never touched.

Both new kinds are **live on the deployment** and both real events are pasted from
`wss://relay.playday.it` in §4. One thing in the handover's model turned out to be
wrong and one piece of it could not be delivered as asked: §5 and §6 say so plainly.

## 1. Commits

| | |
|---|---|
| `2333e83` | kinds 1449 + 1450: types, builders, parsers, `publishLots`, `publishRefusals` (worktree `~/work/wt-lotstate`) |
| `79a71f0` | the create-content rules published in kind 1448 (worktree `~/work/wt-createrules`) |
| `d6a1568` | admit a create the mempool would refuse, so the chain can record and publish the reason |
| `ad1de47`, `85b939d`, `f665f86` | merges into `feat/validator-workstreams` |
| `7dd9f64` | the live probe, `tests/probe-state-half.mjs` |

## 2. Gates — before and after

Baseline on the branch at `8fe917c`: **160 pass / 0 fail / 0 skip**.
After this work: **178 pass / 0 fail / 0 skip** (+18: 8 for the two new kinds, 5 for the
create rules, 5 for the admission change). Per-slice: `79a71f0` 165, `2333e83` 168,
`d6a1568` 173, merged 178.

Command, from `chain/` (the module is `chain/go.mod`):
`ARENA_TEST_MINT=http://127.0.0.1:3338 NAK_BINARY=~/.local/bin/nak go test -count=1 ./...`
`go vet ./...` clean. `gofmt -l` names only the two files already unformatted at `8fe917c`
(`v4v_test.go`, `verdictpublish_relay_test.go`), untouched here.

`tests/probe-state-half.mjs`: **16 pass / 0 fail** live (part 1 alone: 17 pass, before
the admission change required a probe-side expectation change — see §5).

## 3. Deploy (§7)

Built from the merged branch and swapped by binary, no `arena init`:

- previous kept as `bin/arena.prev-20261118-before-state-half`
- deployed `bin/arena` sha256 `460bd5c9863a4ed4` (the first swap in this increment,
  before the admission change, was `52f2ce0429d70c42`)
- `systemctl --user restart arena-chain.service` → unit `active`
- the root's height **continued**: 1730 → 1731 at the first swap, 1745 at the second; no reset
- the relay carries both new kinds from the deployed binary: fresh kind-1449s and
  kind-1450s with `created_at` after the restart, 4 of each (one per validator)

## 4. The two real events

### Kind 1449 — the moved end, published while the lot was still open

From a live anti-snipe lot (`duration 120 s, window 100 s`). A bid landed inside the
window, moved the end, and the validator that committed the bid published this in the
same pass. `/api/state` showed the lot `open` at the moment this event was readable.

```
RAW 1449 EVENT aae151a246cbead8e76a1496b1a6911fbb4c227a308b4b4ea7cbe31faf19563f
{
  "kind": 1449,
  "id": "aae151a246cbead8e76a1496b1a6911fbb4c227a308b4b4ea7cbe31faf19563f",
  "pubkey": "4e1c04b91a63ac4ea41f4a2663fd1c0ac0343b5360abfdaf94057cff5425c0fe",
  "created_at": 1795021527,
  "tags": [
    ["auction", "1440:3e936db1aad4706d79b20a486c1f2a73aaadf0f3af4b909e7835464ec9b441dd:lot1449-1795021494772"],
    ["height", "1734"],
    ["hash", "A8E25D2044C0BE17413F822A4E867AF5B4332F599C760DC6DA180F604CB9542F"],
    ["p", "4e1c04b91a63ac4ea41f4a2663fd1c0ac0343b5360abfdaf94057cff5425c0fe"]
  ],
  "content": "{\"schema\":\"arena.lot.v1\",\"auction_id\":\"1440:3e936db1aad4706d79b20a486c1f2a73aaadf0f3af4b909e7835464ec9b441dd:lot1449-1795021494772\",\"end_time\":1795021627,\"original_end_time\":1795021615,\"extensions\":1,\"height\":1734,\"block_hash\":\"A8E25D2044C0BE17413F822A4E867AF5B4332F599C760DC6DA180F604CB9542F\"}",
  "sig": "01c43d18a2a30c1de0d161f85640f928b0174065849340c9647e1f7cf159101b1eb8351cf7b595ddec3713c993750126150f6a245b33b49c48a9f6a8771bd9c5"
}
```

Read the numbers: `end_time 1795021627 > original_end_time 1795021615` — the end moved
by exactly the 12 s the bid had left under the 100 s window; `extensions 1`; the anchor
`height 1734` is **the bid's own committed height**, which `/api/state` reported as the
committing block for that bid before the event was read back.

`content` is left as the relay frames it — a JSON string, not an object.

### Kind 1450 — a refusal, naming the event and the chain's own reason

A kind-1440 create with a 200-character title, published to the relay. The chain refuses
it, records the refusal in the block that carries it, and the publisher states it:

```
RAW 1450 EVENT c0797a1f73934a38741f3feb5c98c4b19c04f19183291a636a08650855619dfc
{
  "kind": 1450,
  "id": "c0797a1f73934a38741f3feb5c98c4b19c04f19183291a636a08650855619dfc",
  "pubkey": "b81658ebbfcf2cd6f7bbcbe470852bd47d2ef058444ac0bb53eef3d0d287b512",
  "created_at": 1795021902,
  "tags": [
    ["e", "00189699a970057ed792605801d420fcae40403518ae7998d4e7eced0605f376"],
    ["height", "1749"],
    ["hash", "0210E491EA9750BC2673D5D5AB3CFAD2ADABAB56DFF0BF095FF2BC5FA9BA8FB6"],
    ["p", "b81658ebbfcf2cd6f7bbcbe470852bd47d2ef058444ac0bb53eef3d0d287b512"]
  ],
  "content": "{\"schema\":\"arena.refused.v1\",\"event_id\":\"00189699a970057ed792605801d420fcae40403518ae7998d4e7eced0605f376\",\"kind\":1440,\"reason\":\"title required (max 120 chars)\",\"height\":1749,\"block_hash\":\"0210E491EA9750BC2673D5D5AB3CFAD2ADABAB56DFF0BF095FF2BC5FA9BA8FB6\"}",
  "sig": "319c4b3db0d99cc47da14a7334754b8c1152f05f628739f3c0bbc6c77238a3778414b3b84ddeee735fdb15b33c7a42904db0129946d7239d18a9354c7bd1716b"
}
```

`/api/state` records the same refusal — the same event id, the same reason, `kind 1440`,
at height 1749 — which is what makes the anchor checkable against the chain rather than
merely against the relay. No auction for that create exists on the chain.

Four validators publish one event each per fact, exactly as the receipts do; a client
dedupes kind-1449 by `(auction_id, end_time)` and kind-1450 by `(event_id, reason)`,
keeping the earliest anchored height. Six tags on the 1449 and three on the 1450 are the
shapes your handover proposed — the tag order is `auction,height,hash,p` and `e,height,hash,p`.

## 5. What I changed in the proposed shapes, and why

1. **`Auction.OriginalEndTime` does not exist.** The handover says the chain holds it; it
   does not. The original end is `CreatedAt + DurationSec` — the create set
   `end := now + DurationSec` with `CreatedAt: now`, and no supersession or extension ever
   moves either — so it is derived by a method rather than added as a field, which keeps
   the state format and every existing snapshot byte-identical.
2. **`extensions` is the per-bid ordinal, not the lot's current count.** A lot can be
   extended more than once, and a reader that joined late needs the ordinal that goes with
   the end the event names, not the lot's final count. Two `omitempty` fields on `Bid`
   (`EstablishedEnd`, `ExtensionOrdinal`) carry it, set where `applyBid` already moves the
   end. No format bump: an old snapshot re-encodes byte-identically and loads.
3. **Refusals now have to reach a block — and previously they could not.** This is the
   handover's one wrong premise. `State.Rejections` is only written in `FinalizeBlock`, so
   an event the mempool refuses in `CheckTx` never becomes a rejection at all: measured
   live, a 200-character-title create was answered `code=2` at admission, never entered a
   block, and `/api/state` still showed the same three old rejections afterwards. Since the
   app has no HTTP path to the chain, that meant the 1450 could never be published for the
   very case the acceptance names. `CheckTx`'s create branch now **admits** a create that
   its dry run refuses, with the log `will be decided at delivery: <reason>`; the decision
   is deterministic and made again at delivery, where it is recorded and anchored. The dry
   run mutates nothing and creates are one per lot, so the block-space cost is bounded.
   **The bid branch is deliberately left strict**, because `applyBid` verifies escrow
   cryptography (DLEQ, SIG_ALL) and admitting unverified bids would multiply that work
   across every validator — see §6 for what that costs.
4. **Params carry the create rules as code, not as committed terms.** The fields
   (`max_title_chars`, `max_description_chars`, `max_image_chars`, `max_category_chars`,
   `max_name_chars`, `image_rule`, `start_price_min/max`, `increment_min/max`) come from the
   same constants `applyCreate` enforces, and they are on `ParamsContent` only — never on
   `Params`. That is deliberate: `paramsVersion` digests `Params`, so **no published
   `params_version` moves**. Proved live after the deploy: the relay's newest kind-1448
   still carries `71ca57eb83d61d0bc59a2aebdd8557916acd4b9f34865e3c36d7ed7d1311298b`, the same
   value as before this change, now alongside `max_title_chars: 120` and
   `image_rule: "same-site-or-https"`. The rule is a NAME, not a pattern: it tells the app
   which rule to implement (`safeImage`'s: empty, `https://…`, `./…` or `/…` with a single
   slash, and a fixed URI-safe byte set), it does not parametrise it.

## 6. What I could not do — plainly

**A bid refused in the mempool still gets no 1450.** This is the remaining half of the
refusal feature, and it is measured, not inferred: the chain's three historical kind-1441
rejections (heights 25, 85, 1275) are all refusals that happened at *delivery*, and every
kind-1450 on the relay today comes from a rejection in `State.Rejections`. A bid refused by
`CheckTx` — the locktime-below-the-floor case the handover itself anticipates, which is what
a stale client produces — never reaches a block, so there is nothing to anchor a 1450 to.

Two ways to close it, both of which change live behaviour and are therefore yours to pick:

- **Relax bid admission at the escrow boundary.** `applyBid` verifies the escrow first and
  raises the refusals a client acts on (locktime floor, amount, closed) after that. Typing
  the escrow-verification failures (`errors.As` on a dedicated type, messages unchanged) and
  admitting only the post-escrow refusals would cost nothing in verification work — the
  expensive part has already passed — while letting those refusals reach a block. It is the
  smaller change, and it is still a change to the admission policy of the escrow path.
- **Publish a validator's own refusal observation.** The reason is a deterministic function
  of committed state plus the public event, so a validator could sign "at committed height H
  (hash X) I refuse event E because R" without the event ever being in a block. It needs a
  side channel from `CheckTx` to the publisher (the publisher reads committed snapshots
  only, by design) and gives the app a new verification story.

I did neither: the first edits the admission path of the escrow machinery, which is exactly
the kind of change to ask before making on a live chain, and the second is a new mechanism
rather than the shape you proposed. Everything else in §3 of the handover is implemented.

Also not done, and not fixable:

- **`State.Rejections` caps at 50.** If more than 50 refusals are recorded between two
  publisher passes, the oldest are gone before they can be published. That is the chain's
  own bound, inherited rather than introduced.
- **The refusal dedupe map is in memory**, so a process restart republishes the current
  rejections once — the same behaviour the existing verdict map has, and bounded by the
  same 50.
- **The live probe leaves state behind.** It creates a real lot on whatever root it runs
  against; that lot closes and settles, and the escrow goes to an identity the probe
  discards at exit. Run it against a demo root unless the point is to prove the deployment.
  `PROBE_PART2_ONLY=1` skips the lot half.

## 7. §5 hygiene — decisions, with the measurements I took

1. **Relay noise: leave it.** The newest 500 kind-1448 events show the flood tail exactly
   where the cadence fix left it — 226 events at height 1000 and 223 at height 1100, against
   **4 per recent height** (one per validator) — so the fix is visible in the history. The
   foreign test-chain events at heights 2 and 20 are older than the 500-event window the
   relay returns per query, so I could not enumerate them without paginating; they are
   harmless anyway, because a client checks the anchor and the `params_version`. Deleting
   stored events from a public relay is not something the chain should do.
2. **The pulse (21447) delivers nothing on this stack — keep it, recorded.** I reproduced
   the handover's finding first-hand: subscribed to kind 21447 on the deployment relay for
   25 s while the publisher emitted roughly 4/s, the relay forwarded **0** events, and a
   `-l 10` query shows it stores **0**. So the ephemeral pulse is wasted work in the strict
   sense. I left the code alone because removing a kind is a contract change, and because
   the clock is delivered by what does work: the stored 1447 heartbeat. Dropping it is one
   call site in `Step` if you want it gone.
3. **The commitment heartbeat: measured, not changed.** 256 kind-1447 events in the last
   600 s — 4 validators × 60 ticks at the 10 s interval, ≈ 37k/day, which matches your
   ~34k estimate. `-commit-max-interval` is the lever and 60 s would cut it about six-fold,
   but that is the deployment's clock resolution and the app reads the 1447 anchor for time,
   so it is a tuning decision rather than a bug to fix here.
4. **The pre-`SettledHeight` backfill: declined.** Nothing the app does needs it, and the
   receipts for those settles can never be anchored, so a backfill would add state for no
   reader.

## 8. Cross-check for your app half

Everything your §2–§4 app halves need is on the wire and was read back from the relay in
this increment: the moved end with the bid's anchor height (§2), the refusal with the
verbatim reason, its kind and its anchor height (§3), and the create rules in the params
event with `params_version` unchanged (§4). `probe-state-half.mjs` is the reader to copy
from — it re-checks each contract from the relay alone, including the tag-vs-content
cross-check a relay could otherwise hide.
