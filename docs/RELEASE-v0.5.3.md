# Release v0.5.3 — relay-only facts, 2-of-2 escrow, no re-lock

`VERSION` is bumped to `v0.5.3` by the integrator; this note records what
changed and what an operator must do.

## What changed

- **Escrow destination is enforced by one strict parser.** A settle or refund
  template output is now validated by `strictTemplateOutput` (`chain/sigall.go`)
  in admission, in settle verification and in the FROST signing policy. It
  refuses any output whose NUT-11 tags could hand a second signer the spend —
  `pubkeys`, `n_sigs` other than `1`, `refund`, `n_sigs_refund`, `locktime` — and
  refuses non-canonical bytes (uppercase hex, nonce/data reordering, whitespace,
  extra JSON fields). Before this fix the deployed mint could read a crafted
  output differently from the chain; two parsers, one committed byte string, and
  the same bytes were admitted then unsettleable. The honest wallet shape
  (`web/src/lib/arena.js` — `["P2PK",{nonce,data,tags:[["sigflag","SIG_INPUTS"]]}]`)
  is accepted exactly.
- **Non-denomination template outputs are refused** at admission and again before
  signing: an output `Amount` that is not a key of `Params.Keys` can never settle
  at the mint.
- **The escrow is the 2-of-2 `[bidder, committee]` `SIG_ALL` lock**, with the
  bidder's pre-signed settle and refund templates committed in the bid. The
  committee alone cannot move the escrow; a colluding bidder + committee can, and
  that residual is accepted, provable and not preventable (spec §1.3, §13).
- **Timing: the lock floor is 915 s and there is no re-lock.** A bid's locktime
  must clear `SettlementSafetySec (900) + BidToConsensusLatencySec (15)` past the
  end the bid itself establishes. A bid by the current leader is a position raise
  and **never** moves `EndAt` or increments `Extensions`; only a lead change
  extends an anti-snipe lot, and there is no cap on extensions and no re-lock.
  The old 24 h `LockMarginSec` margin is retired as a lock driver.
- **The public surface is relay-only.** Clients submit events to relays and read
  facts (1440, 1445, 1447, 1449) from relays; there is no public `/api/*`, no
  `/mint/*` proxy, no `/api/release` and no `/api/settle`. HTTP is mesh-internal
  (deployed): each validator serves only what its peers need — `/frost/commit`,
  `/frost/sign` — on its FIPS-mesh listener, and a **dev-only** static/API mode
  remains for a single-host run. The create (kind 1440) announces its mint and its
  relays; a validator accepts a lot only if it trusts the mint, and trust
  requires the mint's `GET /v1/info` to advertise **NUT-10 and NUT-11**.
- **Rejections are silent, except an anchored create refusal** (kind 1450): a bid
  refused at admission publishes nothing; a seller whose lot was not admitted
  learns why from the anchored 1450.
- **Chain format is `"6"`** (`chain/node.go`). Adding `outcome_templates` to `Bid`
  and `mode`/`windowSec`/split fields to `Auction` changed committed state, so
  this release is a **wipe**, not an upgrade. The retired `-lock-margin` flag is
  accepted and does nothing.

## Operator action

1. **Create a fresh chain root**: run `arena init` on a **new** root directory.
   **Never migrate state in place** — there is no migration and no format
   promotion; an old root is simply not read.
2. **Never run `arena init` against a root that holds escrows.** Init regenerates
   the FROST group key, which orphans every proof locked under the old group key
   (and under the old escrow shape). Init is for a fresh root only.
3. Point the validators at the new root and re-fund the mint wallets. The demo
   chain's archived lots are history, not state.
