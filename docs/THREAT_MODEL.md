# Threat model: Plebeian Auctions arena (v0.5.3, relay-only public surface)

Status: draft, 2026-11-10; updated 2026-11-23 for the 2-of-2 escrow and the
retired public HTTP surface (v0.5.3, chain format 6). Fake sats only. This
document defines what must hold before real sats are used, and what an auditor
should test.

## What we protect

| Asset | Why it matters |
|---|---|
| Escrowed bids (Cashu proofs locked to the 2-of-2 `[bidder, committee]` `SIG_ALL` lock) | Theft or loss of bidders' and sellers' sats |
| The four FROST key shares (`chain/vN/config/frost_key.json`) | Any 3 give the committee half that, with a colluding bidder, can move any escrow |
| Wallet keys and Nostr keys in each browser | Control of a user's refunds, payouts and identity |
| Mint signing key (`/etc/torii/auctions/mint.env`) | Can create or refuse ecash |
| Agreed auction state (CometBFT chain) | Decides who won, who is refunded, who is paid |
| Availability of validators and the settler | Refunds and payouts need 3 of 4 validators (and a settler) online |

## Security goals

1. The committee can move an escrow only to a destination the **bidder**
   pre-signed: the settle output (to the seller's committed payout key, or the
   committed V4V split) and the refund output (to the bidder's own wallet key).
   The chain refuses at admission any template not locked to those keys.
2. The **committee alone cannot move any escrow.** A spend needs two distinct
   signatures over the SIG_ALL message; the committee holds only its half and
   cannot forge the bidder's (Schnorr is message-bound; no combine, no reuse).
3. A winning bid is payable to the seller even with nobody online: the settler
   executes the bidder's pre-signed settle template. **The committee alone can
   never divert it.** A **colluding bidder + committee can** send the escrow
   anywhere — the spend happens at the mint, so the chain cannot prevent it. That
   residual is accepted: there is no fund-loss victim (the seller never had
   custody), the templates are **public** so a seller can verify the committed
   destinations before shipping, and a diversion is **detectable and
   attributable** against the chain's own record. Not preventable; provable
   (spec §1.3, §13).
4. Every validator applies the same bid rules; a bid that breaks them never enters the state.
5. If the validators fail, bidders recover their sats without anyone's help:
   after the locktime the NUT-11 `refund` key lets the bidder reclaim alone.

## Trust boundaries

There is **no public gateway and no inbound public HTTP**. The public surface is
the Nostr relays and the mint.

```
browser / market app ──Nostr WebSocket──► relay(s)  (1440→1450 facts; client writes 1440/1441)
        │                                              ▲
        │  wallets talk to the mint directly            │ each validator publishes its own facts
        ▼                                              │ (1443–1450, 21447) under its own key
   Nutshell mint :3338 (FakeWallet)                    │
                                                       │
        ┌──────────────────────────────────────────────┴───────────┐
        │  FIPS mesh — peers only, FROST port opened, default drop  │
        ▼                                                           ▼
   V1..V4 (CometBFT + ABCI + /frost/commit · /frost/sign)      bin/arena-frost (Rust, frost-secp256k1-tr 3.0.0)
```

The validators' HTTP listener (`7101+i`, serving `/frost/commit` and
`/frost/sign`) is reachable only from whitelisted mesh peers (FIPS, see
`docs/FIPS.md`). A **dev-only** static/API mode serves the built UI and the old
read routes for a single-host run; it is never a deployed role.

Today all boxes run on one host under one operator. The intended trust model
(each validator run by a different person on a different machine) is not
deployed yet.

## Actors

| Actor | Wants to | Prevented by | Gap today |
|---|---|---|---|
| Anonymous visitor | Disrupt auctions, steal sats | 2-of-2 escrow lock; auth signatures checked by each validator | No public HTTP surface remains; the relay is the only public door. Can flood chat and the relay. (F1 retired with the gateway.) |
| Bidder | Bid without paying; get refunded while winning; reuse proofs | Bid needs DLEQ-valid proofs from the pinned keyset, summing to the amount, locked to the 2-of-2 `[bidder, committee]` `SIG_ALL` lock, with pre-signed settle and refund templates checked at admission; proofs recorded in `UsedY`; refunds only for `outbid`/`lost`/`void` (a void bid never leads or wins) | A **colluding bidder + committee** can send their own escrow anywhere (goal 3). A winner can reclaim after locktime if the seller was not paid (F2). |
| Seller | Inflate price; keep sats without delivering | Seller's Nostr key cannot bid on its own lot | Shill bids from a second key (F6). No delivery or dispute step: payout releases at close (F5). |
| One validator (malicious or compromised) | Sign alone; censor; corrupt state | 3-of-4 FROST; BFT consensus needs 3 of 4 votes; each validator re-checks policy and re-derives the exact template digest before signing | Cannot move an escrow alone (needs the bidder's half). Can delay bids during its proposer turns. |
| Committee (≥3 colluding validators) | Divert an escrow | Cannot forge the bidder's `SIG_ALL` signature, so cannot move a bidder's escrow without that bidder | With a colluding **bidder** they can send that bidder's escrow anywhere (goal 3, provable). |
| Settler / executor | Build outputs or choose a destination | It has no key and no build authority: it submits a **pre-signed** outcome template and collects one FROST co-signature; validators re-derive the digest | Can refuse to execute (bidder falls back to the locktime refund). |
| Host operator (one machine, all shares) | Anything | Nothing on a single host | **Holds all 4 shares, mint key and the process** (F3). |
| Mint operator | Inflate, refuse swaps, track users | Validators verify DLEQ against a keyset pinned in genesis | Custodial by design (F4); the mint sees group key and refund keys. |
| Supply-chain attacker | Ship a malicious release | Exact-pinned npm deps, `Cargo.lock`, `go.sum`, hash-pinned Python wheels; Suite pins tag + SHA-256 in a reviewed PR | Releases are built by GitHub Actions and not signed or reproducible (F9). |
| Browser attacker (XSS, malicious extension, shared device) | Steal keys | Strict CSP (only own-origin and hashed scripts), no third-party requests | Keys live in localStorage/IndexedDB; no NIP-07; no tested recovery (F7). |

## Findings

Severity is for real sats; with fake sats nothing is at financial risk.

### F1 (retired): unauthenticated validator stop/start on a public gateway

The public HTTP surface is gone in v0.5.3 (handover §6.1): `chain/main.go` no
longer serves `/api/admin/*`, `/api/state`, `/api/bid`, `/api/tx`, `/api/settle`,
`/api/events`, `/api/chat`, `/api/network`, `/api/publish` or `/mint/` in a
deployed role. The validators' HTTP listener serves only the FROST endpoints and
is reachable only from whitelisted mesh peers (FIPS, `docs/FIPS.md`); a dev-only
static/API mode remains for a single-host run and must never be deployed as the
public face. The finding is closed by removing the route, not by gating it.

### F2 High: winning bid can be reclaimed by the bidder after locktime

v0.5.3: the lock floor is **915 s** (`SettlementSafetySec` 900 +
`BidToConsensusLatencySec` 15, spec §2.1), **not days, and there is no re-lock.**

A bid's locktime must clear the end the bid itself establishes by exactly
`safety + latency`; a bid that does not is invalid and silently dropped. The
pre-signed settle template pays the seller at close with **nobody online** — the
settler executes it, it does not need the seller's browser. So the design no
longer depends on a long margin or on a re-lock:

- Goal 3 holds while the settler lands the settle inside the 900 s safety window:
  the winner cannot reclaim before locktime, and the sale completes.
- The accepted residual: if settlement does **not** land within that window, the
  winner reclaims via the locktime refund path, the seller is unpaid, and the
  parties re-arrange. Money is safe and nothing is stuck; the sale is lost
  (spec §5, §13.2). There is **no re-lock** to extend the window, by owner
  ruling — do not reintroduce one.

The earlier framing (a days-long margin and a re-lock move, ADR 0009) is
**superseded** by the 2-of-2 model: `docs/PROTOCOL.md` §2 and §5.

### F3 High: one host holds every key

`arena init` runs the key ceremony in one process and writes all four shares to one disk, next to the mint key and the process. The operator, or anyone who compromises the host, can (with a colluding bidder, for that bidder's escrow) move funds and mint ecash. The FROST threshold gives no protection here.

Fix: one validator per machine and per person; a real distributed key ceremony over authenticated channels; the mint run separately from the validators.

### F4 High (for real sats): custodial mint

Cashu is custodial: the mint can inflate, refuse swaps or disappear. The FakeWallet mint also gives unlimited free sats to anyone who can reach it. The mint learns every lock's committee key and refund key, linking a bidder's bids.

Fix: a Lightning-backed mint operated by a party the team trusts, independently of the validators; plan for keyset rotation (the keyset is pinned in genesis).

### F5 Medium: escrow protects payment, not delivery

The winning bid is released to the seller at close. A seller who never ships keeps the sats.

Fix: a delivery/confirmation window or a dispute path (e.g. release after buyer confirmation or a timeout, with validator arbitration rules).

### F6 Medium: shill bidding and bid censorship

A seller can bid from a second Nostr key to raise the price. A malicious proposer can delay a late bid during its turn; the anti-snipe window (60–900 s) limits but does not remove this.

Fix: reputation or identity linking (out of scope for the escrow), and longer soft-close windows for high-value lots.

### F7 Medium: browser key custody

Nostr and wallet keys are generated and stored in the browser. Losing browser data loses unsettled sats; any script running on the origin could read them.

Fix: NIP-07 signer for identity; tested seed backup and restore for the Coco wallet; keep the strict CSP.

### F8 Low: resource exhaustion

*(The HTTP-based limits below describe the retired public surface; the deployed
system has no public HTTP. The relay-facing resource questions — chat flood,
relay connection limits — remain.)*

v0.3.0: HTTP timeouts on every server, at most 4 open signing sessions per escrow.

- Each validator holds at most 256 open signing sessions for one minute. Only authorised requests create them, but one bidder repeating valid release requests can fill them and delay others' releases.
- Chat accepts any validly signed event (300-message ring, 280 chars), so it can be flooded.
- Mitigated in part by the nginx rate limit (zone `torii_auctions`).

Fix: per-bid session limits, per-key chat limits.

### F9 Low: release integrity

Releases are built in CI and pinned by checksum in a Suite PR, which is the only review point. A compromised GitHub account could publish a different tarball for a new tag.

Fix: reproducible builds checked by a second builder, and signed release checksums.

### F10 Info: privacy

All auctions, bids, amounts, bidder wallet keys and Nostr keys are public through `/api/state`. v0.3.1 adds `/api/state?view=client` and `/api/bid?id=` (ADR 0004). They expose nothing the full state did not already, and the `bidders` parameter only tells the gateway which public keys a browser holds, which it already learns when they sign events. Gzip is safe here: these responses carry no secrets, so compression side channels (BREACH) do not apply. Intended for an auction, but bidders are linkable across lots.

v0.3.2 (ADR 0005) lets one path, `/mint/v1/ws`, upgrade to a WebSocket that is passed straight to the mint. Every other upgrade attempt gets a 400. A held socket costs a goroutine and two file descriptors and is cut after 10 minutes. Nginx rate-limits new upgrades like any other request, but this is not a cap on open sockets per client: on real sats, add a per-IP connection limit (`limit_conn`) in front.

v0.3.3 (ADR 0003 §2) writes state snapshots to `vN/data/app-state/<height>.json` (0600, directory 0700). Anyone who can write there can already rewrite the block store, so this adds no new attacker, but a snapshot must never become a quiet way to change state. On start a validator loads only the newest file and refuses to run unless it is a regular file under 256 MiB, hashes to its stored app hash, re-encodes to the same bytes, and matches the chain's own record (the next block header's app hash, or the saved CometBFT state at the tip). A refusal never falls back to an older snapshot or to genesis on its own; the operator deletes the file. Snapshots contain only public state (the same data as `/api/state`), no keys.

v0.3.4 (ADR 0006) changes only the page. A one-tap bid (Next, Jump, Big move, the sticky Bid button, swipe) is priced against the view read just before the lock, so it can be up to two increments above the amount the button showed; anything higher is refused before any sats are locked, and the toast states the amount actually bid. A tap made while a bid is in flight is held (latest one wins) and dropped if you are leading when the first lands, so taps cannot stack bids. The demo bots in your own browser wait 5 s after your tap, which removes the most common cause of a stale-amount rejection (and its sats locked until locktime). Nothing changes on the chain or the gateway.

v0.4.0 (ADR 0003 §1 and §4, ADR 0007, chain format 4) changes the chain:

- **Void bids.** A fully locked bid that lost a race is recorded as `void` and refundable at once, instead of staying locked until locktime. The escrow checks run first, so a forged or badly locked bid is still refused and leaves no trace. A void bid marks its proofs used and never leads. Spam costs a real mint lock per bid, and each lot keeps at most 256 void bids (then late bids are refused as before). Nginx rate limits apply.
- **Block time is the proposer's clock (PBTS).** Each honest validator prevotes a proposal only if its time is within 505 ms ahead of, or 2 s + 505 ms behind, its own clock. A dishonest proposer can therefore backdate a block by about 2.5 s, which could let a bid in just after the end (CometBFT widens the window by 10% for each failed round). Before v0.4.0, BFT time let a block after an idle minute carry a time up to a minute old. A validator whose clock drifts beyond the window stops voting for others' proposals, so separate servers must run NTP.
- **Close transactions** (`close:<lot>:<endAt>:<seconds>`) are refused unless that lot is open, still ends at that time, the end has passed on the validator's own clock, and the transaction is under 30 s old. They change nothing in FinalizeBlock: the close itself runs from block time. The public `/api/tx` accepts only JSON events, so only a validator's own loopback RPC can submit one.
- **The app hash leaves out height and time.** Both are still in the header of every block, which the commit signs, and snapshots are checked against the header's app hash as before.
- **Pruning and the ledger archive.** Blocks more than `-retain-blocks` (100,000) below the newest snapshot are deleted. Every transaction is first written to `vN/data/ledger.jsonl` (0600) and synced. The archive is public data. It is now the history record, so it belongs in backups.
- **`/api/events`** (Server-Sent Events) is capped at 512 open streams in total. Each stream lasts at most 15 min, and each write must finish within 10 s. One client can still take every stream; others then fall back to polling, so nothing breaks. On real sats, add a per-IP `limit_conn` in front.

## Already in place

- Escrows are the 2-of-2 `[bidder, committee]` `SIG_ALL` lock with the bidder's
  pre-signed settle and refund templates committed in the bid; the strict parser
  (`strictTemplateOutput`, `chain/sigall.go`) refuses any other destination, tag
  set or non-canonical bytes, at admission, at settle verification and before
  the FROST policy signs.
- Non-denomination template outputs (an `Amount` not a key of `Params.Keys`) are
  refused at admission and again before signing.
- DLEQ verification against the genesis-pinned keyset; duplicate proofs and proofs used in an earlier bid rejected.
- The settlement policy is checked independently by each validator from its own
  committed state against the exact pre-signed template digest; a validator will
  not co-sign a template it would not admit (`releasePolicy`) and cannot build
  outputs the bidder did not sign.
- FROST nonces are single-use, deleted before signing, expire after a minute; a validator signs only the message it committed to (`handleShare`).
- The coordinator verifies the aggregated signature before returning it; the mint rejects forged signatures (tested).
- Validators publish their own facts (1443–1450, 21447) under their **own** keys,
  each anchored to a committed height and block hash; readers check the signer
  against the roster committed in the app hash.
- The HTTP listener binds the FIPS mesh, not a public address; key files are 0600.
- Strict CSP, noindex, no framing on the dev-only static surface; no third-party requests.
- The FROST roundtrip test and the end-to-end acceptance script cover the honest
  wallet path, the 2-of-2 lock, the pre-signed templates and the relay facts.

## Before real sats

- [ ] F1: closed by removing the public HTTP surface; verify no dev-only mode is deployed publicly.
- [ ] F2: settlement must land inside the 900 s safety window with nobody online; there is **no** re-lock (do not add one).
- [ ] F3: validators on separate machines and operators; distributed key ceremony.
- [ ] F4: Lightning-backed mint run independently.
- [ ] F5: delivery or dispute rule decided.
- [ ] F7: NIP-07 and wallet backup/restore.
- [ ] Fuzzing of bid and template checks; tests for a misbehaving validator.
- [ ] Independent audit of the FROST flow, escrow rules and validator code, against this document and `docs/PROTOCOL.md`.
- [ ] Small per-auction caps and a bug bounty for the first real-sats trial.
- [ ] NTP on every validator server (PBTS).
