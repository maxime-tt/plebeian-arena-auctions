# RESULT — validator-side workstreams (arena chain, `feat/validator-workstreams`)

**For:** the market-side session (auctions UI + escrow plan).
**From:** the validator-side session. **Written** 2026-11-17.
**Input:** `docs/HANDOVER-2026-11-17-validator-workstreams.md`. Its **§10.1 amendment** is the
contract record (rules changed, what was measured, every deviation); this file is the delivery
report — what was built, what was proven, and what is still open.

**Status: all five workstreams implemented; unit gates and the live acceptance are green; pushed.**

| | |
|---|---|
| Base | `738413c` — which **is** `d7s2/e2e-suite-currency` on `maxime-tt`, i.e. workstream E was already landed |
| Branch | `feat/validator-workstreams`, HEAD **`328ce53`** |
| Origin | `https://github.com/maxime-tt/plebeian-arena-auctions.git` (fork of ChiefmonkeyArt's upstream) |
| Push | `* [new branch] feat/validator-workstreams` (`ab79d4d`), then `ab79d4d..328ce53 feat/validator-workstreams -> feat/validator-workstreams` |
| Diff vs base | 19 files, +3041 / −182 |
| Commits | `1ef429c` (A+D) · `3f51340`, `c97130b`, `ac0c6a6`, `d39fdc8` (B+C) · `1870243` (client lock margin) · merges `144efd2`, `d7b8c1b` · `47780b4`, `ab79d4d`, `328ce53` |

Lane branches `feat/validator-ingest` (`d39fdc8`) and `feat/bid-locktime-margin` (`1870243`) are
merged into the branch above; the worktrees `~/work/wt-validator`, `~/work/wt-ingest`,
`~/work/wt-client` still hold them.

---

## 1. Gates

| Gate | Base | Branch |
|---|---|---|
| `ARENA_TEST_MINT=http://127.0.0.1:3338 NAK_BINARY=~/.local/bin/nak go test -count=1 -v ./...` | **113 pass / 0 skip / 0 fail** | **144 pass / 0 skip / 0 fail** |
| `cd web && npm test` | 13 + 13 + 9 pass | 13 + 13 + 9 + **8** (new `bid-locktime`) |
| `go build ./... && go vet ./...` | clean | clean |
| Live acceptance suite `tests/e2e.mjs` | 64 passed / **1 failed** (handover §7.5) | **65 passed / 0 failed** |
| Live probes `tests/probe-restart-ingest.mjs` | — | **5 passed / 0 failed** |

`gofmt -l` reports `chain/main.go`, `chain/v4v_test.go`, `chain/verdictpublish_relay_test.go` at the
base too — pre-existing, untouched.

## 2. Per workstream

### A — the kind-1447 state-commitment heartbeat (`1ef429c`)
- `PublisherConfig.MaxInterval` (default `StateCommitMaxInterval = 10 s`), flag
  `-commit-max-interval` on `arena node` and `arena gateway`.
- Gate is now `publishedVerdict || first pass || height != lastCommitHeight ||
  now-lastCommitTime >= MaxInterval` (+ params needing an anchor); `lastCommitHeight` **and**
  `lastCommitTime` are recorded only on success. `StateCommitEvery` is **no longer the gate** — it
  is the params-event cadence (§1.4's "else keep it with a comment" branch).
- The heartbeat is the **ephemeral** kind `21447` (`arena.statepulse.v1`), published on **every**
  pass, carrying `{height, app_hash, time}`; a relay mute ("no one was listening") is swallowed —
  no record, no retry, no error — while any other failure is recorded. `1447` stays the stored
  anchor with its `(height, app_hash)` and `Time` meaning unchanged.
- `TestKindMapIsRegularRange` names the pulse as an explicit, documented exception instead of
  being loosened; `TestNoScatteredKindLiterals` gained the five-digit literal class.
- 9 new tests in `chain/verdictpublish_test.go`; `TestCommitmentHeartbeatPublishesOnAnIdleChain`
  and `TestCommitmentPublishesWhenHeightMoves` are the write-path proofs and **fail on the base**
  (old gate `height%p.every == 0 || publishedVerdict || p.lastCommit == 0`, `verdictpublish.go:173`).

### B — restart-proof ingestion (`3f51340`, `c97130b`, `ac0c6a6`, `d39fdc8`)
- Durable cursor at `<home>/data/relay-cursor.json`, keyed by relay URL, atomic
  (temp + fsync + rename + dir fsync, 0600) and monotonic. It advances only when everything at or
  before the position is **settled** — a queued or in-flight event holds it back, so a crash can
  never resume past an event that never reached the chain.
- A refused submit is retried by a worker (250 ms → 5 s backoff) instead of being dropped; the
  receive loop keeps draining the socket while retries pend.
- Transport/readiness refusals (`broadcast rejected:` — where CometBFT's
  *endpoint is closed while node is catching up* arrives) are retried **without limit**.
  Application answers (`… is invalid: code=<n>` inside the RPC error object) are retried at most
  `maxAppRetries = 5` (≈3.75 s of backoff) and then dropped and settled past — bounded, not
  allowlisted, because a bid published *before its create's block* is rejected with
  `unknown auction` (`app.go:658`) and must survive to the next block.
- Idempotency unchanged (in-memory id set + `already exists in cache` + the chain's own state
  guard), `TestRelayIngestDuplicatePublishCommitsOnce` still green.
- Live evidence (acceptance run): an event published to the relay while **all four validators are
  down** is committed after the restart with no client retry, and every node logs the resume point:
  `relay observer: resuming from cursor 2026-11-17T16:15:51Z (a81234a6…)`.

### C — prompt blocks and `/api/tx` (`3f51340`, `ac0c6a6`, `d39fdc8`)
- Root cause, measured in CometBFT v1.0.1: `handleTxsAvailable`
  (`internal/consensus/state.go:1019-1040`) fires on the **local** `TxsAvailable` channel
  (`node/setup.go:307-308`), and the mempool reactor's outbound gossip is blocked while a node is
  catching up (`mempool/reactor.go:198-206`). A tx in a non-proposer's mempool therefore waited for
  that validator's turn — up to `CreateEmptyBlocksInterval` (60 s) — which is the measured 20 s+ stall.
- Fix: **fan out** the submit to all four validators' RPCs, in the observer's `RPCSubmitter` and in
  `/api/tx`. The tx reaches the proposer's own mempool and the block lands within `TimeoutCommit`
  (300 ms) + one round ⇒ documented bound **≤ 2 s**. `CreateEmptyBlocksInterval` is deliberately
  unchanged (the suite asserts an idle chain makes no blocks).
- `/api/tx`: `ok:true` now means at least one validator took the tx into its mempool; the tx hash is
  **always** returned (the RPC's, else the locally computed CometBFT hash — sha256 upper-hex); the
  relay publish is best effort; no validator reachable ⇒ 503; a terminal `CheckTx rejected` ⇒ 400.
- Live evidence: `/api/tx` → committed block in **974 ms** (earlier run 1209 ms).

### D — the escrow-terms event (`1ef429c`)
- Kind **1448**, schema `arena.params.v1`, **stored** (owner ruling §8.2), published on change and
  at the `StateCommitEvery` cadence, anchored to the **same `(height, app_hash)`** as the commitment
  published in the same pass, so a client can tell the terms and the commitment belong together.
- `params_version` is the **sha256 content digest of the canonical `Params` JSON**, not a counter:
  committed state has no counter to bump, and a digest is what lets a client compare the terms it
  trusts against the commitment it trusts.
- Carries every term §4 names: `GroupKey`, `MintURL`, `KeysetID`, `Keys`, `SettlementSafetySec`,
  `BidToConsensusLatencySec`, `MinWindowSec`, `MaxWindowSec`, `MinDurationSec`, `MaxDurationSec`,
  `MaxProofs`, `Threshold`, `Validators`.
- **For the market read:** the relay is now the source for the escrow key. `arena init` regenerates
  the FROST group key on every run, so a hardcoded committee key locks escrows to a key this
  deployment cannot sign for — read `GroupKey` from the 1448 event, and check `params_version`
  against the commitment you trust. "A lock assembled from them has `pubkeys` equal to the published
  `GroupKey`" is market-side and was **not** exercised here.

### E — the acceptance suite
Already landed at the base (`738413c` = `d7s2/e2e-suite-currency`): the coordinate-vs-event-id state
keys, the 60 s anti-snipe minimum, the mint-side "proofs already spent" acceptance, the quiescence
wait, and the untouched 15 s bid-commit wait. Nothing was redone; the suite now runs **65/0**.

## 3. Three defects the handover did not know about

1. **A demo-control restart dropped the relay flags.** `supervisor.start` hardcoded
   `node -root … -i N`, so a validator restarted through `/api/admin/validator/N/start` (the suite's
   own path) came back with **no `-relay`**: no observer, no verdict publisher, no commitments, no
   params, no pulse. The suite's "V4 catch-up" step only checks block height, so it never noticed.
   The argv is now built once (`nodeArgs`) and both paths share it;
   `TestSupervisorRestartKeepsRelayFlags` pins it. *(commit `c97130b`)*
2. **Stale relay history was retried forever.** CometBFT delivers an application `CheckTx`
   rejection inside the broadcast RPC **error object**
   (`mempool/errors.go:42` → `"tx %X is invalid: code=%d, …"`, wrapped by
   `rpc/core/mempool.go:70`), and every error object was classified transient: a dead event was
   retried from 16:00:24 to 16:00:47 at the 5 s cap, and the FIFO head starved everything behind it.
   Fixed by `appRejectionCode` + the `maxAppRetries` bound. *(commits `ac0c6a6`, `d39fdc8`)*
3. **The client's locktime could not clear the chain's floor for an in-window anti-snipe bid.**
   `chain/app.go` `minLocktime` is evaluated against the **block** time and `BidToConsensusLatencySec`
   is the allowance for the gap between the bidder's clock and that block; `web/src/lib/arena.js`
   derived the lock from its **own** clock, so the bid was valid only if `client_now >= block_now`,
   which never holds. Measured live: locktime `1794932210` against a floor of `1794932213` (block
   time `1794931238`); `CheckTx` admitted it, the block included it, the transition rejected it, and
   it never appeared in the state. This is the **same symptom** §3 records as latency, with a second,
   deterministic cause — a **fixed** lot, or an anti-snipe lot whose end is further away than
   `window + latency`, already cleared the floor exactly, which is why only an in-window anti-snipe
   bid failed. Fixed by estimating the admitting block as `now + latency`
   (`minBidLocktime`, spec §2.3 allows over-locking), pinned by `web/test/bid-locktime.mjs`.
   *(commit `1870243`)*
   **Market-side consequence, recorded not fixed (§9):** any client that derives the lock from its
   own clock has this defect. The market app must estimate the block time the same way before it
   locks an anti-snipe bid.

## 4. Reproducing the live acceptance

```
~/work/wt-validator/scripts/acceptance-validator-workstreams.sh
```
It stops `arena-chain.service` (the validators own fixed ports: p2p 2665x, rpc 2666x, http 710x),
builds the branch, initialises a **fresh** root in `run/chain-e2e` (**never** `run/chain` — §9: a
re-init regenerates the FROST group key and would orphan every escrow the app has locked), starts a
local empty `nak serve` and the gateway with `-demo-controls` on `:8095` (`:8080` is the retired MVP
demo), runs the probes and the suite, and restarts the unit through an `EXIT` trap.

Logs: `~/work/wt-validator/run/acceptance/{probe-bc,e2e,gateway,relay}.log` and
`~/work/wt-validator/run/v{1..4}.log`.

Three harness corrections found by running it, all in the commit:
- the suite must be run **from the branch's tree** — it imports `../web/src/lib/arena.js`, so the
  canonical clone's copy tests the canonical clone's *client*;
- a fresh root against a **shared** relay replays that relay's whole history before this run's
  events (no cursor yet), so the run uses an empty local relay;
- `:8080` is `auctions-mvp-demo.service`, hence `:8095`.

## 5. Deviations and residual risks

- `params_version` is a content digest, not a counter (rationale above).
- The ephemeral pulse lives outside NIP-01's regular range by design; the range test names the
  exception rather than skipping it.
- The lock margin clears the floor only for an admitting block within `bidToConsensusLatencySec`
  (default 15 s) of the bidder's clock — that is exactly the allowance the chain models. Blocks
  later than that are outside the chain's stated assumption.
- The cursor is monotonic: if a relay ever delivered a **newer** event before an older one *and* the
  cursor had already advanced to the newer one, only a rewind could recover the older one. Within
  one relay the `since` replay is ascending by `created_at`, so that is an out-of-order corner, not
  the measured defect.
- An event the application keeps rejecting is dropped after `maxAppRetries`; that is deliberate (the
  alternative is an unbounded storm), and it is why the bound, not an allowlist of messages, was
  chosen.

## 6. Not done

- **§2 acceptance 2 in its literal form** — a `systemctl --user restart arena-chain.service`
  mid-auction. The unit runs the *deployment's* root and ports, so restarting it mid-run cannot
  restart the chain under test; the property it names is covered by the probe (all four validators
  restart mid-flow and the missed window is re-ingested) and by the suite's validator-restart steps
  (5 and 9).
- **D's lock-assembly half** — "a lock assembled from them has `pubkeys` equal to the published
  `GroupKey`" is market-side; the chain publishes `GroupKey` verbatim and it round-trips.
- **The market read's dedupe key** — the market read dedupes commitments by
  `(validator, height, app_hash)` with `time` not in the key, so pulses/commitments at one height
  collapse and the newest `time` is lost. With an ephemeral tick that matters *more*, not less. This
  is market-side and is recorded in the market plan.
- Nothing was committed, pushed or deployed to the live root: `run/chain` was never re-initialised,
  and the unit was restored after every acceptance run (`active`).
