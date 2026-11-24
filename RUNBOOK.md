# Runbook — arena-settlement-facilitator

A single outbound-only process. It is **optional**: the protocol is correct with it
stopped. This runbook covers deploying, operating, and verifying it on the Plebeian Market
VPS, and states plainly what happens when it is down.

## What it can and cannot do

- It **can**: execute the committed swap at the mint within the leg's window, so the payout
  lands before `locktime`, and publish the kind-1452 carrier so the chain records the
  settlement sooner.
- It **cannot**: move funds anywhere the bidder did not pre-sign (the committee signature
  and the proofs are the authority; the courier holds no custody key), or forge anything.
- When it is **stopped**: the recipient's wallet executes the same committed template
  itself; the chain publishes its own 1443/1444 receipt once a settle commits. The only
  consequence of a stop is *latency*: if nobody executes before `locktime`, the bidder's
  refund branch becomes live too. The recipient can still execute after `locktime` (the main
  pathway stays valid; the refund pathway just becomes *also* valid), or recover later via
  NUT-09. No funds are stranded and no path lets a bidder reclaim funds already signed to
  the seller.

## Deploy (market VPS, systemd user unit)

1. Install Bun ≥ 1.1 and place the repo, e.g. `/opt/arena-settlement-facilitator`.
2. `bun install` there.
3. Create `/etc/arena-settlement-facilitator/config.json` from `config.example.json`,
   filling in the production mint URL, relays, the committee group pubkey, and the
   validator roster. **No secrets in this file.**
4. Optionally set `SF_COURIER_NSEC` in the environment file if you want a stable courier
   pubkey on the `p` tag of every 1452. Without it, an ephemeral key is generated at each
   start; that is fine — the tag carries no authority.
5. Install `systemd/arena-settlement-facilitator.service` and run:

   ```sh
   systemctl --user daemon-reload
   systemctl --user enable --now arena-settlement-facilitator.service
   journalctl --user -u arena-settlement-facilitator -f
   ```

## Network

Outbound only: WebSocket(s) to the relays, HTTPS to the mint. No inbound listener, no
public port. If it is ever given inbound work, it becomes a FIPS peer like a validator,
never a public HTTP service.

## Operating

- **Logs** are one line per decision: `drop 1451: <reason>`, `decline <bid> <outcome>:
  <reason>`, `published 1452 <id> ...`, `input already spent; nothing to do`.
- **Idempotence**: a repeated 1451 republishes the same 1452 shape from the in-process
  cache; a restart with a spent escrow logs "input already spent" and exits that bid
  (another executor — or the recipient — already moved the money; the chain's receipt is
  the primary source).
- **Deadline**: a swap is refused when `locktime - now <= clockToleranceSec`. The clock is
  the earlier of the kind-1447 chain time and the mint's `info.time`; with neither, the
  local wall clock minus `fallbackMarginSec`. It never refuses to start because the mint
  omits `info.time`.

## Verify a running instance

- Confirm it sees the committee fact:
  `journalctl --user -u arena-settlement-facilitator | grep '1452'` should show a publish
  shortly after a settle outcome commits.
- Confirm the carrier on the relay: fetch kind 1452 with the bid tag and check its `sig`
  equals the 1451's `signature` and its `proofs` unblind to the committed destinations.
- Confirm no inbound port: `ss -ltnp | grep arena` prints nothing.

## Known limitations

- A settle with a committed V4V split is declined by name (`settle with a committed V4V
  split is not supported by the courier`); the recipient wallet path still executes it.
- The single-coordinator liveness hole (if the elected validator is down, nothing is
  signed, so only the post-`locktime` refund is reachable) is a property of the **chain**
  and is documented there; this courier adds no fallback and needs none.
