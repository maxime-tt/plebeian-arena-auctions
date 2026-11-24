# Plebeian Auctions · Arena (fake sats, 4 validators)

A from-scratch, working MVP of decentralised live auctions. Four CometBFT
validators order every listing and bid. Each bid is real Cashu ecash locked to a
**2-of-2 `[bidder, committee]` `SIG_ALL`** escrow: at bid time the bidder
**pre-signs two outcome transactions** — settle to the seller, refund to itself —
so the committee can move the money **only to those two destinations**: the
bidder's honest pre-signing plus the chain's admission rule, which refuses any
other template (the mint itself has no concept of a destination — spec §1.3).
The committee is an **executor, not an author**: the elected coordinator
validator signs a committed outcome's digest and publishes it as a fact (kind 1451), and any
courier — the facilitator service, or the recipient's own wallet — carries that signature to
the mint as kind 1452. Nothing is built and no destination is chosen: the bidder's own
pre-signature fixed both. The public surface is
**relay-only**: clients publish signed Nostr events to relays and read every fact
from relays; a deployed gateway is a validator supervisor with **no HTTP surface at all**,
and a dev-only static/API mode exists for single-host runs. Fake sats only.

The normative external protocol is
[`docs/PROTOCOL.md`](docs/PROTOCOL.md); code comments that cite `spec §N` refer to
its section numbers.

## Architecture

```
browser (C Arena UI, Coco wallet + cashu-ts, nostr-tools)   ·   market app
   │  publish 1440 create / 1441 bid (signed) and lock ecash
   ▼
Nostr relay(s) — the public surface for writing and reading every fact
   ▲   │
   │   └─ every validator subscribes (-relay) and submits events to every node's mempool
   │      every validator publishes its own facts (1443–1450, 21447) under its OWN key
   │
V1..V4  CometBFT v1 + ABCI auction app + FROST share
        mesh listener 7101+i: /frost/commit · /frost/sign (bin/arena-frost, Rust)
        p2p 26656+10i · rpc 26657+10i
Nutshell 0.21 mint :3338 (FakeWallet, NUT-10 P2PK, NUT-11 SIG_ALL)
```

- Consensus: blocks need ≥3 of 4 validator signatures; the chain keeps running with one validator down.
- **Escrow lock (2-of-2):** `["P2PK",{nonce,data:<bidder>,tags:[["locktime",<n>],["pubkeys",<committee>],["n_sigs","2"],["refund",<bidder>],["sigflag","SIG_ALL"]]}]`. The committee is the FROST 3-of-4 group key (one point, never per-validator keys). Two distinct signatures move it — the bidder's and the committee's — and the bidder's half cannot be forged (Schnorr is message-bound). The **committee alone cannot move any escrow**; a **colluding bidder + committee can** send that escrow anywhere, which is not preventable (the spend happens at the mint) but is provable against the chain's own record (spec §1.3, §13).
- Bid validity (deterministic, on every validator): signed by the bidder; ≥ high + step (`MinIncrementBps`, default 200 = 2%); not the seller; DLEQ-verified proofs from the pinned keyset; unspent-on-chain; sum = amount; the 2-of-2 lock above; the bid carries its two pre-signed outcome templates and every validator re-derives each template digest and re-checks the bidder's signature at admission. A **single strict parser** (`strictTemplateOutput`) enforces the destination and the canonical NUT-11 tag set for every settle and refund output, at admission, at settle verification and before the FROST policy signs; it refuses `pubkeys`, `n_sigs` other than `1`, `refund`, `n_sigs_refund`, `locktime`, non-canonical bytes, and any output `Amount` that is not a pinned keyset denomination.
- **Lock floor is 915 s and there is no re-lock.** A bid's `locktime` must clear the end the bid itself establishes by exactly `SettlementSafetySec` (900) + `BidToConsensusLatencySec` (15); a bid that does not clear it is invalid and silently dropped.
- Two auction modes ([ADR 0009](docs/adr/0009-push-settlement-two-auction-modes.md), **chain format 6**): **fixed time** ends on the clock and never later; **anti-snipe** puts the clock back to the seller's window (60 s to 900 s) only when a bid **takes the lead**, with no cap and no hidden hard end. A bid by the current leader is a position raise and does **not** move `EndAt` or increment `Extensions`.
- Settlement (v0.6.0): **the chain never touches the mint.** When a block makes an outcome due, the elected coordinator (the smallest roster pubkey) runs a FROST round over the bid's committed template digest and publishes the committee signature (kind 1451). An executor — the facilitator service, or the recipient's own wallet — posts the swap to the mint with that signature plus the bidder's committed pre-signature, unblinds the answer, and carries it back as kind 1452; the chain re-verifies it against the committed template and records the settle transaction, and the validators publish the receipt (kind 1443/1444). Nothing builds an output and nobody chooses a destination — the bidder's pre-signature fixed both. Measured locally: FROST round 10–30 ms, mint swap about 25 ms.
- Nostr login (v0.5.0, [ADR 0010](docs/adr/0010-nostr-login-one-click-lots.md)): NIP-07 extensions and NIP-46 signers (`bunker://`, `nostrconnect://`). You bid as yourself beside the demo characters, with the same fake sats; your wallet key stays in this browser. Your name and picture come from your own relays. One-click lots list a random parody lot (Bitcoin merch "sold" by leaders of the past) in either mode.
- Fixes (v0.5.2, ADR 0011 addendum): the wallet worker now says "ready" before the page sends it anything. iOS dropped the early start-up message, so every top-up and bid failed with "wallet worker: unknown op". If the worker never gets ready, the wallet runs in the page.
- Fixes (v0.5.1, [ADR 0011](docs/adr/0011-wallet-resync-after-lost-storage.md)): a wallet whose browser store was cleared (counters back at 0, so the mint refused "outputs already signed") repairs itself with Cashu restore (NUT-09) and gets its unspent ecash back. Top-up and bot failures now show. The photo and gallery follow the next lot the moment it takes over. The outcome panel closes when the next lot starts. The demo Seller no longer bids.
- Keys: `arena init` runs a FROST (secp256k1, Taproot/BIP-340) key ceremony between the four validators; each keeps only its own share in `vN/config/frost_key.json`. Genesis carries the committee group key.
- **Read model (relay-only).** Clients reassemble state from facts, not from a snapshot service: kind 1440 (the create/root), 1449 (lot state, bid admission, create admission), 1445 (outcome/verdict) and 1447 (state commitment, the anchor). Each validator publishes its own facts under its own Nostr key, anchored to a committed height and block hash; a reader ignores an unanchored fact and checks the signer against the roster committed in the app hash. The create announces its mint URL (and, when available, its relays); a validator accepts a lot only if it trusts that mint, which requires the mint's `GET /v1/info` to advertise **NUT-10 and NUT-11**. See [`docs/KINDS.md`](docs/KINDS.md) for the full kind table.
- Rejections are silent, with one exception: a bid refused at admission publishes nothing; a seller whose lot was not admitted learns why from the anchored create refusal (kind 1450).
- Live chain (v0.4.0, ADR 0003 §1 and §4, [ADR 0007](docs/adr/0007-live-chain-push-void-bids.md); introduced in chain format 4, current format 6):
  - Blocks come only when there is something to commit, plus one a minute as a heartbeat. Each validator sends a `close` transaction when a lot is due, and block time is the proposer's clock (PBTS), so lots close on time.
  - A locked bid that lost a race is recorded as void and refunded at once.
  - Each validator archives every transaction to `vN/data/ledger.jsonl` and prunes blocks 100,000 below its newest snapshot.
- Wallet: the browser holds ecash in Coco (`@cashu/coco-core`, IndexedDB per identity); bids are Coco P2PK sends.
- Light page (v0.4.2, [ADR 0008](docs/adr/0008-page-main-thread-budget.md)): the Coco wallets and all proof maths run in a Web Worker; the page redraws ten times a second (at once after a tap or a new fact), derives each public key once and reuses its number and time formatters.

## Run

```bash
./scripts.sh build && ./serve.sh   # needs Go, Rust (cargo) and Node 20
```

Open http://127.0.0.1:8080. `serve.sh` runs the **dev-only** gateway mode (static UI + the retired read routes bound to loopback) over a local 4-node chain and the local mint; it is not a deployment shape. Other commands:

```bash
DEMO_CONTROLS=1 ./scripts.sh start && ./scripts.sh test
```

`DEMO_CONTROLS=1` turns on the validator stop/start route (`-demo-controls`), which the test needs and a real deployment must not have. The retired `LOCK_MARGIN` variable does nothing: the lock floor is `SettlementSafetySec + BidToConsensusLatencySec` (900 + 15).

`test` runs `tests/e2e.mjs`, the end-to-end acceptance suite (currently 56 checks: fixed and anti-snipe lots, the 2-of-2 lock, the pre-signed templates, pushed refunds and payouts, bids, void late bids, idle chain and on-time closes, relay facts, validator stop/catch-up, reclaim after locktime, conservation of sats). `go test ./chain/` runs the validator tests (**232** `Test*` functions across `chain/*_test.go`, including the strict-template and denomination regression tests and the fuzz seeds). `node web/test/frost-roundtrip.mjs frost/target/release/arena-frost` checks the key ceremony and signing. `cd web && npm test` runs the page checks (worker routing, remote signing, profile reader, catalogue, locktime).

## Limits (MVP)

- Fake sats, internally reviewed only ([audit](docs/SECURITY_AUDIT_2026-11-10.md)), one operator runs all four validators on localhost.
- All four FROST shares sit on one host, so that operator (with a colluding bidder, for that bidder's escrow) could move funds; real security needs the validators on separate machines run by different people.
- CometBFT p2p is not yet on the FIPS mesh; the per-node FIPS configuration the deployment needs is in [`docs/FIPS.md`](docs/FIPS.md).
- The demo characters use per-browser generated Nostr keys; logged-in users sign with their own. Chat (kind 1442) is not part of the arena's chain: the arena publishes nothing on it.
- Bids respond on tap (v0.3.4, [ADR 0006](docs/adr/0006-instant-bid-feedback.md)): the page shows your bid as sending at once, then sent, then on chain about a second later.
- App state is in memory, saved to disk every 1,000 blocks and on clean shutdown (v0.3.3, [ADR 0003](docs/adr/0003-block-growth-snapshots-pruning.md) §2); a restart replays only the blocks after the newest snapshot. State sync for new validators and state trimming (ADR 0003 §3, §5) are still proposed.

## Torii node install

Tagging `v*` runs `.github/workflows/release.yml`, which builds `plebeian-auctions-<tag>.tar.gz` with `scripts/package.sh`. The tarball holds the static `bin/arena` and `bin/arena-frost` (Rust, musl), the built front end and a private CPython 3.12 with Nutshell, installed from the hash-pinned `mint/requirements.txt` using prebuilt wheels only. Nothing is built on the VPS.

The Torii Suite installs a pinned release with `installers/install-auctions.sh` (opt-in, `INSTALL_AUCTIONS=1`). There is **no public gateway**: the four validators, their FROST endpoints and the mint bind loopback or the FIPS mesh only (see [`docs/FIPS.md`](docs/FIPS.md)), and clients reach the system through relays. A dev-only static/API mode exists for a single-host run and is never a deployed role.

Build a tarball locally:

```bash
scripts/package.sh v0.5.2 dist-release
```

Release notes for the next version: [`docs/RELEASE-v0.5.3.md`](docs/RELEASE-v0.5.3.md) (new chain root; never migrate state).

Design decisions: [ADR 0001](docs/adr/0001-frost-escrow-coco-wallet.md), [ADR 0002](docs/adr/0002-strict-locks-nutshell-021.md), [ADR 0003 (proposed)](docs/adr/0003-block-growth-snapshots-pruning.md). Protocol: [`docs/PROTOCOL.md`](docs/PROTOCOL.md). Kinds: [`docs/KINDS.md`](docs/KINDS.md). FIPS mesh: [`docs/FIPS.md`](docs/FIPS.md). Threat model: [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md).
