# HANDOVER — the chain's state half: the committed end, refusals, and the create rules

**For:** the chain-side session (arena repo, `~/work/wt-validator`, branch
`feat/validator-workstreams`).
**From:** the app-side session that owns the market read, 2026-11-18.
**Why:** the money half of the chain's contract is done and deployed — kinds 1443/1444 receipts,
proven live, and the app reads them. What is missing is the **state** half: what a lot's end is now,
and what happened to an event the chain refused. Both are small additions to the same publisher, and
both are things the app currently cannot learn at all.

Read §2, §3 and §4 first; §5 is hygiene you can take or leave.

---

## 1. State of play, measured 2026-11-18

- Live: `arena-chain.service` (4 validators 7101-7104, gateway :8090), root `run/chain` (never
  re-initialise), relay `wss://relay.playday.it`, mint `:3338`.
- The running binary carries the receipts and the cadence fix (`feat/validator-workstreams`, my
  `9d08340` merged). Go suite **160 pass / 0 fail** (I re-ran it). Receipts verified live from the app
  side: 4 × kind-1443 and 8 × kind-1444 on the relay, all parsing through the app's own decoder.
- The app is relay-only by ruling — it has **no HTTP path to the chain**. Everything it knows must be
  a signed, anchored relay event. That is the constraint every item below comes from.

## 2. Item 1 — publish the lot's committed end (the moved end)

**Measured.** The chain holds the committed end (`Auction.EndAt`, `Auction.OriginalEndTime`,
`Auction.Extensions`) and publishes **none** of it: the kind map is 1440/1441/1442/1443/1444/1445/1446/
1447/1448 + the ephemeral 21447, and the only builders are verdict, commitment, pulse, params, refund,
settle. On the app side, nothing reads a committed end either: the lot page's end is derived from the
create (`root.created_at + durationSec`), so after an anti-snipe extension the app shows the **old**
end while the chain enforces the new one. A bidder therefore cannot see why the countdown moved, and a
client cannot derive a correct locktime floor from what it can read.

**Ask.** Publish the lot's committed end whenever it changes — event-driven, not on a cadence, because
the app needs it while the lot is **open**.

**Proposed shape** (say if you want it different; the app is written against this):

- New stored kind **1449**, schema `arena.lot.v1`.
- Content: `{schema, auction_id, end_time, original_end_time, extensions, height, block_hash}`.
- Tags: `["auction", id]`, `["height", n]`, `["hash", block_hash]`, `["p", validatorPubkey]`.
- Signed and anchored exactly like a verdict/receipt (signature, roster, tag anchor agreeing with the
  content anchor).
- Published by the validator that committed the extending bid, in the same pass, for every accepted bid
  that **moved** the end (and once when a lot closes, so a reader that joined late can still learn the
  final end).
- One per `(auction_id, end_time)` per validator; clients dedupe by `(auction_id, end_time)` and prefer
  the earliest anchored height — same rule the receipts use.

**Acceptance.** A live lot with an anti-snipe window, bid inside the window: a kind-1449 appears on the
relay with `end_time > original_end_time` and `extensions >= 1`, anchored to the height the bid
committed at, while the lot is still open. Paste the event.

**App half (mine, lands with it):** read it, show "extended to <time> by a bid at height N", and use the
committed end as the basis for a new bid's locktime floor.

## 3. Item 2 — publish refusals, so a client can tell "refused" from "not yet"

**Measured.** `State.Rejections` already exists — `{Height, Time, Kind, Pubkey, Event, Reason}`, capped
at the last 50 — and `/api/state` serves it (`view.go`). **Nothing publishes it to the relay.** Because
the app is relay-only, a bidder whose bid the chain refused sees a bid that simply never commits: no
reason, no height, nothing to act on, and their sats stay locked in that escrow. The same is true of a
create the chain drops: the lot stays visible on the relay (and in the app) while never existing on the
chain.

**Ask.** Publish the refusals that concern a client-submitted event (kinds **1440** and **1441**),
anchored and signed like everything else.

**Proposed shape:**

- New stored kind **1450**, schema `arena.refused.v1` (or fold it into the receipt family if you prefer
  one shape — say which, the app is written against your answer).
- Content: `{schema, event_id, kind, reason, height, block_hash}` where `reason` is the chain's own
  message verbatim (`title required (max 120 chars)`, `locktime below the floor for this lot`, …).
- Tags: `["e", event_id]`, `["height", n]`, `["hash", block_hash]`, `["p", validatorPubkey]`.
- Published once per `(event_id, reason)` per validator, at the height the rejection was recorded.
- **Do not** include the event's content — the reason string is enough, and the event itself is already
  public.

**Acceptance.** Publish a kind-1440 with a 200-character title to the relay: a refusal event appears
naming that event id and the chain's reason. Paste it.

**App half (mine):** show "the chain refused this bid: `<reason>`" on the lot and on the position, and
point the bidder at the refund path instead of leaving them waiting.

## 4. Item 3 — share the create-content rules instead of each side guessing

**Measured.** The chain enforces `title required (max 120 chars)` and `image must be a same-site path or
an https URL` (`chain/app.go:515`, `:519`). The app enforces **neither**: its create form only requires a
non-empty title. So a seller can publish a lot the chain silently drops after five retries while the app
presents it as a real lot — the exact "both sides green, feature dead" class.

**Ask.** Carry the limits in the kind-1448 params so the app pre-validates from one source of truth
rather than hardcoding a second copy that drifts. Suggested additions to `ParamsContent`:
`max_title_chars`, `max_description_chars`, and the image rule (e.g. `image_rule: "same-site-or-https"`,
or whatever shape you prefer).

**App half (mine):** refuse to publish a create the chain would refuse, and say why in the form — before
anything is published, not after.

## 5. Hygiene and tuning — decisions, not really code

1. **The public relay carries ~500 kind-1448 params events** from the cadence flood (fixed, but the
   history is there) plus **foreign params at heights 2 and 20** from two short-lived test chains. A
   correct client is safe (the anchor check), but it is noise. Cleanup pass, or a note saying leave it.
2. **The ephemeral pulse (21447) is undeliverable on this stack.** I reproduced it against the
   deployment relay *and* a stock local `nak serve`: an ephemeral event published from a second
   connection is answered `mute: no one was listening for this` even with a live subscriber for that
   kind connected, and the subscriber receives nothing. It is published on every pass — roughly four
   events/second of work that cannot reach anyone. Either drop it or keep it with the reason recorded;
   the stored, time-gated 1447 is what actually delivers the clock.
3. **The 10-second commitment heartbeat** stores roughly 34k kind-1447 events/day on a public relay.
   `-commit-max-interval` makes it tunable (60 s would cut it six-fold); the plan also sketches a
   replaceable kind if storage ever becomes the constraint.
4. **Optional backfill.** The 27 settlements that predate `SettledHeight`/`SettledAt` can never be
   receipted; their height and time are in `run/chain/vN/data/ledger.jsonl` if a one-off backfill is
   ever wanted. Not needed for anything the app does today.

## 6. Do not

- Do not change the semantics of 1440, 1441, 1443, 1444, 1445, 1447 or 1448 — all are live contracts the
  app is built against, and the receipts are verified against them.
- Do not add HTTP endpoints for the app to read. It has no HTTP path to the backend, by ruling.
- Do not re-initialise `run/chain` (it regenerates the FROST group key and strands every locked escrow),
  and do not force-push `origin/main` or push to ChiefmonkeyArt's upstream.
- Do not touch the market repo (`~/work/market`, `~/work/wt-app-*`) — the app half is a separate lane.

## 7. Deploy, gates, report

- Deploy by building and swapping the binary: `cd chain && go build -o ../bin/arena .`, copy over
  `/home/admin_maxime/work/arena-merged-ref/bin/arena` (keep the previous as `bin/arena.prev-<date>`),
  then `systemctl --user restart arena-chain.service`. Prove it from outside: unit active, the root's
  height **continues**, and the new kinds appear on the relay.
- Gates: `cd chain && ARENA_TEST_MINT=http://127.0.0.1:3338 NAK_BINARY=~/.local/bin/nak go test -count=1 ./...`
  (the module is `chain/go.mod`; from the repo root it fails with "does not contain main module"),
  `go vet ./...`, `gofmt -l` on touched files, and the repo's own `tests/e2e.mjs` via
  `scripts/acceptance-validator-workstreams.sh` (fresh root, local relay, restores the unit). Baseline
  today: **160 pass / 0 fail**.
- Report: the commit shas; the exact content and tags of one real kind-1449 and one real refusal event
  pasted from the relay; the gate numbers before and after; anything you changed in a proposed shape and
  why; and anything you could not do — plainly, never filled in with a plausible guess.
