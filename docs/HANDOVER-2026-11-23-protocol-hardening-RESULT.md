# RESULT — auctions protocol hardening, increment 1

**Written:** 2026-11-23. **Implements:** `HANDOVER-2026-11-23-protocol-hardening.md`
(§3–§6; §8 untouched). **Branch:** `d7s2/relay-only-and-timing`.
**Commit:** `d0d59a2` (pushed to `origin` `main`; fast-forward from `651a967`).
**VERSION:** `v0.5.3`. Chain format stays `"6"` — no format bump, no migration.

All measured evidence below was produced in this worktree
(`/home/admin_maxime/work/wt-validator`).

---

## 1. Increment 0 — the critical escrow defect

**Defect.** Admission (`verifySigAllBidTemplates` → `sigAllOutputRecipient`)
validated only the `data` field of a settle/refund template output and ignored
its NUT-11 tags. Nutshell 0.21 builds the signer set as `[data] +
tags["pubkeys"]` with `n_sigs`, so an output carrying
`tags:[["pubkeys",<bidder>],["n_sigs","1"]]` was spendable by the bidder alone,
while the strict settle-time parser refused it. Two parsers, one committed byte
string.

**Fix.** `chain/sigall.go` has ONE destination parser,
`strictTemplateOutput(secret, expectedKey)`. It requires the canonical
`["P2PK",{nonce,data,tags}]` shape: 64-char lowercase-hex nonce, compressed
`data` equal to the expected key in x-only form, no `pubkeys`/`refund`/
`n_sigs_refund`/`locktime`, `n_sigs` only `"1"`, `sigflag` only `SIG_INPUTS`,
no duplicate keys, and a byte-for-byte canonical re-marshal. It is called from:

- `verifySigAllBidTemplates` (consensus admission), every settle and refund
  output, including the V4V split branch;
- `recipientMatches` (settle verification) — now just the same call;
- `settlementOutputs` (the FROST signing policy), so a validator cannot sign a
  template it would not admit.

`sigAllAmountPinned` refuses an output `Amount` that is not a key of
`Params.Keys`, at admission and again before signing.

**V4V (deviation from “parser fix”).** `splitSats` used to emit one
arbitrary-amount output per split row (e.g. 33/33/34), which can never settle at
a power-of-two keyset. Under the global denomination rule those templates are
refused; to keep the feature settleable, `splitSats` now decomposes each row's
share and the seller remainder into pinned denominations (`splitAmount`), row
order and recipients preserved. The per-output JSON schema, the escrow lock
shape and the settler are unchanged.

**Evidence.**

- `TestStrictTemplateOutputRefusedEverywhere` (13 subtests): crafted
  `pubkeys`+`n_sigs=1`, `pubkeys`, `locktime`, `refund`, `n_sigs_refund`,
  duplicate `data`, uppercase nonce, extra field, missing `tags`, whitespace,
  `n_sigs=2`, escrow relock — each refused at admission, at settle verification
  and at signing; the honest wallet shape accepted.
- `TestStrictTemplateOutputNonDenominationRefused`,
  `TestSigAllSplitShareNotRepresentableRefused`.
- `TestSigAllAdmissionRefusesAuditExtraPubkeyProbe` — the audit probe
  (`verification-go-admission-test.go`) is now a permanent regression and no
  longer admits.
- Mint probe `verification-mint-extrakey-probe.py` re-run against the live
  `~/.mint-venv`: unchanged, still `RESULT: the deployed mint ACCEPTS it -> True`.
  It documents the mint, not us; the chain now refuses that shape.

## 2. Increment 1 — timing

`chain/app.go`: an anti-snipe lot extends **only when a bid changes the lead**
(`takesLead` / `bidExtends`). A leader's self-raise leaves `EndAt`,
`Extensions`, `Extended`, `EstablishedEnd`, `ExtensionOrdinal` untouched. The
extension decision is resolved **before** the locktime floor, which is derived
from the end the bid must survive (`establishedEnd`/`minLocktime` now take the
resolved `extends` as input). No cap on extensions; no re-lock.

Tests: `TestSelfRaiseInsideWindowDoesNotExtend`,
`TestChallengerInsideWindowExtends`, `TestExtendingBidMustClearNewEnd`,
`TestAntiSnipeUncappedByChallengers`, and the state-walking tripwire
`TestClosedWonLegsClearSettlementSafety`.

**Known limit of the invariant.** `Locktime > ClosedAt + SettlementSafetySec`
holds while the closing block is within `BidToConsensusLatencySec` of `EndAt`.
`closeDue` sets `ClosedAt = now`; a stall longer than 15 s can make the
inequality false. The code does not assert more than this; a future change
should decide whether `ClosedAt` needs a floor.

## 3. Increment 2 — the public HTTP surface is retired

`chain/gateway.go` builds the route table as a pure function (`gatewayMux`).

- **Deployed role (default):** only `POST /internal/settle` on the mesh
  listener; no static files; every retired path 404s. Proven by
  `chain/gateway_routes_test.go` (`TestDeployedGatewayServesOnlyInternalSettle`).
- **`-dev`:** keeps the retired public routes and the static UI, on loopback
  only. It is the repository's single-host dev/demo role.
- `-mesh-host` (gateway + node) binds each node's internal listener to one
  interface and is threaded through `supervisor.nodeArgs` and every internal
  URL. `0.0.0.0`, `::`, `[::]`, empty host and a missing port are refused.
- The FIPS ACL is the only boundary on the mesh listener; no second auth scheme
  (see `docs/FIPS.md`). Hub/settler/coordinator wiring is intact (§8.6).
- `scripts.sh`: real Go on PATH (`$HOME/.local/go/bin`), the retired
  `-lock-margin` dropped from `reset`, `start` passes `-dev`.
  `serve.sh`: loopback + `-dev`. Acceptance script: `-dev`. No `0.0.0.0`
  remains in scripts or server binds.

**Fact path.** `chain/factcheck.go`: one roster predicate
(`factSignerAllowed`) wired into all ten fact parsers, each keeping its named
`Err*NotInRoster`; one block-hash seam `BlockHashLookup` (default refuses with
`ErrAnchorLookupUnavailable`, §8.7 undecided), `BindFactAnchor` and
`CheckFactAtCommitment` for height/hash binding. Covered by
`chain/factcheck_test.go` for all nine fact shapes (wrong signer, wrong anchor,
no lookup, wrong commitment height).

**Create announces mint + relays.** `CreateContent` gains `mintUrl` and
`relays`. A root create must name `Params.MintURL` (trailing-slash normalised)
or it is refused by name; a supersession may omit it but not name a different
one; relays are validated (`ws://`/`wss://` with a host, ≤ 8, no duplicates).
Nothing new is stored on `Auction`; the encoding is unchanged. Refused creates
keep the anchored kind-1450 path.

**Mint trust.** `chain/mintcaps.go` `requireMintCapabilities` GETs
`<mint>/v1/info` (5 s) and requires NUT-10 and NUT-11. It is fatal at
`arena init` and at node start against the committed `Params.MintURL`. Not
called from consensus (that stays a deterministic equality check).

## 4. Documentation and version

- `README.md` — 2-of-2 escrow, 915 s floor, no re-lock, format `"6"`, relay-only
  public surface, dev-only static/API mode, test count 232.
- `docs/THREAT_MODEL.md` — trust boundaries without a public gateway; goal 3
  against the 2-of-2 model; F2 rewritten (915 s, no re-lock).
- `docs/adr/0009-…md` — re-lock move, `Relocks` counter and `/api/release`
  marked **Superseded** by name.
- `docs/PROTOCOL.md` — the external spec copied verbatim (header notes that
  code comments citing `spec §N` refer to it).
- `docs/KINDS.md` — every kind 1440–1450, 21447: signer, anchor, reader.
- `docs/FIPS.md` — per-node `peers.allow` + `arena-frost.nft`, derivation and
  verification via `fipsctl`, `fips-allow-peer` flow, port map.
- `docs/RELEASE-v0.5.3.md` — operator action.
- `VERSION` and `web/package.json` → `v0.5.3`.

## 5. Measurements

| Command | Result |
|---|---|
| `go build ./... && go vet ./...` (in `chain/`) | clean |
| `go test ./... -count=1` | `ok arenachain` |
| `go test ./... -count=1 -race` | `ok arenachain 85.2s` |
| `cd web && npm test` | 13/13, 9/9, 8/8 |
| `scripts/acceptance-validator-workstreams.sh` (fresh `run/chain-e2e`) | **EXIT=0** — probes 5/0, 93/0, 55/0; e2e **68/0** |
| `gofmt -l`, `git diff --check` | clean |

Acceptance log: `run/acceptance/handover-run4.log`. The script stops and
restores `arena-chain.service`; the service was active after every run.

## 6. Deliberately blocked (§8)

Untouched: the settler stays in the gateway; no facilitator; no settlement
kind; no change to the published settlement payload; no `locktime` clock
assumption; `won` stays as-is; the coordinator stays internal; the block-hash
transport is a refusing seam only.

## 7. Local deployment (2026-11-23 21:39 UTC)

- Binary built from `d0d59a2` and installed at
  `/home/admin_maxime/work/arena-merged-ref/bin/arena` (previous:
  `bin/arena.pre-hardening-20261123-213844`).
- Web built from the branch and installed at
  `/home/admin_maxime/work/arena-merged-ref/web/dist` (previous:
  `web/dist.pre-hardening-20261123-213844`).
- `~/.config/systemd/user/arena-chain.service` gained `-dev` (with a comment
  that this is the local single-host test role; a deployed role drops it).
- `run/chain` was NOT re-initialised. `FORMAT` is still `6`; no new
  `chain-archive` entry. All four validators up, same height (`9443` at the time
  of check), `/api/network` and `/api/state` answering, new `web/dist` served.
- Legacy escrows in the live root carry non-denomination outcome templates
  (amounts 72, 80). The settler now refuses to sign them by name
  (`amount N is not a pinned keyset denomination`) instead of failing later at
  the mint. Those escrows were never settleable through their committed
  template; the refusal is the defect made visible, not a new regression.

## 8. Contradictions found (report, not silently worked around)

1. **§4.3 vs the V4V split.** A global denomination rule and a one-output-per-row
   split cannot both hold. Resolved by decomposing split shares; this is the
   only change beyond “parser fix”. The per-output schema is unchanged.
2. **The §5.3 invariant is conditional** on the close block landing within
   `BidToConsensusLatencySec` of `EndAt` (see §2).
3. **The web UI is not relay-only.** The deployed role has no public HTTP, but
   the repository's own `web/` client, `tests/e2e.mjs` and the acceptance script
   still use the `-dev` HTTP routes. No relay-fact client was built here; “the
   UI works against relay data” is not delivered by this change.
4. **The JS harness was stale against `651a967`** (the 2 % raise commit):
   e2e/probe floor literals, the invalid-create assertions (creates are admitted
   at CheckTx by design so the 1450 can be anchored), the no-escrow bid identity
   and the client's `lockForBid` floor all still assumed `minIncrement`. Fixed
   to the committed rule; without this the acceptance script could not pass.
5. **Deployed unit is a local test role.** `-dev` was added so the local
   market-e2e backend keeps its UI and `/api/*`. A real deployment must not
   pass `-dev`, and then the browser UI needs the relay-only client from (3).
