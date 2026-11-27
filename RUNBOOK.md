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
5. Install the units and run:

   ```sh
   install -m 0644 systemd/arena-settlement-facilitator.service \
                       systemd/arena-courier-liveness.service \
                       systemd/arena-courier-liveness.timer \
                       systemd/arena-courier-alert.service \
                       ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now arena-settlement-facilitator.service
   systemctl --user enable --now arena-courier-liveness.timer
   journalctl --user -u arena-settlement-facilitator -f
   ```

   `Restart=always` + a non-zero signalled exit are deliberate (see *Exit codes*).

## Network

Outbound only: WebSocket(s) to the relays, HTTPS to the mint. No inbound listener, no
public port. If it is ever given inbound work, it becomes a FIPS peer like a validator,
never a public HTTP service.

## Operating

- **Heartbeat**: a fixed-cadence line every `heartbeatIntervalMs` (default 60 s), plus
  the same snapshot in `SF_STATE_FILE` (default `$STATE_DIRECTORY/state.json` on a
  systemd install). The line names the chain height, the counts of published /
  already-spent / declined / failed moves, and the age of the oldest outstanding 1451.
- **Liveness monitor**: `arena-courier-liveness.timer` runs
  `scripts/courier-liveness.ts` every minute. It exits 1 and prints
  `ALERT courier-liveness [kind]: …` when the pid is gone, the heartbeat is stale, or
  an outstanding 1451 has had no 1452 for `unsettledAlertAfterSec` (default 300 s).
  A failure triggers `arena-courier-alert.service`, which appends to
  `$STATE_DIRECTORY/ALERT` — a marker that survives the journal. Check with
  `systemctl --user --failed` and `journalctl --user -u arena-courier-liveness`.
- **Logs** are one line per decision: `drop 1451: <reason>`, `decline <bid> <outcome>:
  <reason>`, `published 1452 <id> ...`, `input already spent; nothing to do`,
  `heartbeat ...`.
- **Idempotence**: a repeated 1451 republishes the same 1452 shape from the in-process
  cache; a restart with a spent escrow logs "input already spent" and exits that bid
  (another executor — or the recipient — already moved the money; the chain's receipt
  is the primary source).
- **Deadline**: a swap is refused when `locktime - now <= clockToleranceSec`. The clock is
  the earlier of the kind-1447 chain time and the mint's `info.time`; with neither, the
  local wall clock minus `fallbackMarginSec`. It never refuses to start because the mint
  omits `info.time`.

## Exit codes

| code | meaning |
|---|---|
| `0` | `SF_ONCE=1` diagnostic one-shot completed; never use it under this unit |
| `1` | fatal startup or dispatch error, or an unhandled rejection |
| `130` | SIGINT (128 + 2) |
| `143` | SIGTERM (128 + 15) |

The unit uses `Restart=always`, so both a clean exit and a signalled exit restart. This
is the fix for 2026-11-24: the process exited with status 0, `Restart=on-failure` did
not restart it, and it stayed dead for 2.5 days.

## Verify a running instance

- Confirm it is alive and the signal is real:
  `journalctl --user -u arena-settlement-facilitator | grep heartbeat` prints a line at
  the fixed cadence; `cat "$STATE_DIRECTORY/state.json"` is fresh and names the same
  pid (`systemctl --user show -p MainPID --value arena-settlement-facilitator`).
- Confirm the monitor agrees: `systemctl --user start arena-courier-liveness.service`
  then `journalctl --user -u arena-courier-liveness -n 5` ends in `courier-liveness:
  pid=… heartbeat=…s_ago open=…` with no `ALERT`.
- Confirm it sees the committee fact:
  `journalctl --user -u arena-settlement-facilitator | grep '1452'` should show a publish
  shortly after a settle outcome commits.
- Confirm the carrier on the relay: fetch kind 1452 with the bid tag and check its `sig`
  equals the 1451's `signature` and its `proofs` unblind to the committed destinations.
- Confirm no inbound port: `ss -ltnp | grep arena` prints nothing.

## Restart-on-signal check (T1 acceptance)

```sh
pid=$(systemctl --user show -p MainPID --value arena-settlement-facilitator)
kill -TERM "$pid"          # exits 143 → Restart=always brings it back
sleep 8
systemctl --user show arena-settlement-facilitator -p ActiveState -p SubState -p NRestarts -p MainPID
kill -KILL "$pid"          # SIGKILL → Restart=always brings it back
sleep 8
systemctl --user show arena-settlement-facilitator -p ActiveState -p SubState -p NRestarts -p MainPID
```

Each restart must log a fresh `facilitator watching` + `heartbeat` line and then
re-witness the outstanding 1451s (a spent escrow logs `input already spent`, a published
one republishes the carrier, a past-deadline one is `decline … deadline`).

## Known limitations

- A settle with a committed V4V split is declined by name (`settle with a committed V4V
  split is not supported by the courier`); the recipient wallet path still executes it.
- The single-coordinator liveness hole (if the elected validator is down, nothing is
  signed, so only the post-`locktime` refund is reachable) is a property of the **chain**
  and is documented there; this courier adds no fallback and needs none.
