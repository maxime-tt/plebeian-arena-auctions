# arena-settlement-facilitator

An **optional settlement courier** for the Plebeian Arena auction escrow.

It watches the relay for a published **committee signature** (Nostr kind **1451**),
assembles the Cashu swap from public facts, POSTs it to the mint (contract §4), and
publishes the unblinded result as a **settlement carrier** (kind **1452**) so the chain can
record the settlement and recipients can claim. It holds no custody key, signs nothing
authoritative, and decides nothing: authority lives entirely in the committee signature and
the proofs the mint returns.

Stopping it changes *when* the money moves, not *whether* it can — the recipient's wallet
can execute the same committed template itself, and the chain publishes its own receipt
once a settle commits. See `RUNBOOK.md`.

## What it implements

- **CONTRACT-2026-11-24-settlement-facilitator.md §2** — read and verify kind 1451
  (`arena.committee_signature.v1`): digest recompute, schnorr verify against the group
  pubkey, roster signer, anchor read.
- **§4** — the mint `/v1/swap` request: escrow inputs (witness on `inputs[0]` only, bidder
  then committee), the committed template's `(amount, B_)` outputs, response matched by
  `(amount, id)` and unblinded with the public `r`, DLEQ-verified.
- **§3/§5** — the kind 1452 `arena.settlement.v1` carrier, whose `proofs` are the same
  `ReceiptOut` shape the chain publishes.

The escrow lock is the one 2-of-2 SIG_ALL condition contract §4 names: `data` = the bidder,
`pubkeys` = the committee group key, `n_sigs` = 2, `sigflags` SIG_ALL, `refund` = the
bidder. The bidder signature is verified against the P2PK secret's `data` field; the
committee signature against the configured group pubkey.

## Install

Requires [Bun](https://bun.sh) ≥ 1.1.

```sh
bun install
cp config.example.json config.json   # then edit
```

## Configure

`config.json` is a file with **no secrets**: the mint, the relays, the committee group
pubkey, the validator roster, and the timing knobs. See `config.example.json`. The only
optional environment variable is `SF_COURIER_NSEC` (an `nsec` the courier signs 1452 with);
without it the facilitator generates an ephemeral key at startup. The courier key authors
nothing authoritative — kind 1452 carries no roster check and no anchor — so an ephemeral
key is fine.

| field | meaning |
|---|---|
| `mintUrl` | the Cashu mint the arena escrows against |
| `relays` | relays to watch (1451/1447) and publish (1452) to |
| `committeeGroupPubkey` | the FROST group pubkey (x-only or compressed hex) |
| `validatorRoster` | the only allowed 1451 signers |
| `pollIntervalMs` | how often to re-check (subscription reconnect cadence) |
| `clockToleranceSec` | safety margin before a leg's `locktime` |
| `fallbackMarginSec` | margin applied when the local wall clock is the only clock |
| `estimatedSwapSeconds` | assumed swap round-trip cost |
| `requestTimeoutMs` | per-request HTTP timeout |

## Run

```sh
bun start                      # bun src/index.ts
bun start /etc/arena-settlement-facilitator/config.json
SF_CONFIG=./config.json bun start
```

Outbound only: the relay (WebSocket) and the mint (HTTPS). **No inbound listener.** It is
safe to run on the market VPS without opening anything.

## Test and typecheck

No network; all cryptography is checked against the chain's frozen golden vectors.

```sh
bun test          # vitest run
bun run typecheck # tsc --noEmit
```

## Limitations

- **V4V settle splits** require the chain's denomination splitter to reproduce each row's
  amount; the courier does not implement it and **declines** such a settle rather than guess
  a destination. The recipient's wallet can still execute that template.
- The anchor read proves the 1451 names a *committed* height (from observed kind-1447
  commitments) but not the Tendermint block hash, which a keyless courier has no lookup
  for. The committee signature and digest are the authority; the anchor bounds the height.

## Layout

```
src/config.ts   config load + validation (no secrets)
src/relay.ts    kind 1451 verification + kind 1447 reads
src/sigall.ts   the SIG_ALL digest, BIP-340 verify, B_ derivation, P2PK parser
src/clock.ts    chain/mint clock and the locktime deadline
src/swap.ts     plan + verify + build the /v1/swap body; match the response
src/mint.ts     GET /v1/info, GET /v1/keys, POST /v1/swap
src/settle.ts   unblind + DLEQ verify + the 1452 carrier
src/index.ts    the run loop
```
