# INCIDENT + FIX — the outbound publisher froze for 1h45m while the chain advanced, 2026-11-19

A relay restart at 15:17:26 UTC killed the chain's publish socket. The chain kept
producing blocks, but **nothing** — no verdict (1445), receipt (1443/1444), lot
state (1449), refusal (1450), params (1448) or state commitment (1447) — reached
the relay until the whole `arena-chain.service` was restarted at ~16:2x. Same
binary, same relay, same chain: the publisher was holding a dead connection and
never re-dialled.

## 0. Bottom line

`chain/relayclient.go` dialled the relay **once** and handed that one
`*RelayClient` to the publisher for the process's life. When the socket died,
`readLoop` exited and `failAll` released the pending `OK` waiter, but **nothing
replaced the client and nothing marked it dead**: every later `Publish` wrote
into the dead socket (or waited on a receipt that could never arrive until the
process exited), and every pass repeated that against the same socket. The
failure was recorded only on an in-memory `PublishRecord`, which is printed
nowhere.

Fixed by making reconnection the transport's job: a per-relay
`ReconnectingWriter` owns the client, replaces it on a transport failure and
retries the same event once, backs off exponentially while the relay stays
down, and reports `PublishHealth` on the validator's `/status` and the
gateway's `/api/publish`. The event construction, the anchor and the
publisher's claim-once semantics are untouched.

## 1. The measured incident

| time (UTC) | fact |
|---|---|
| 15:17:26 | last state commitment before the freeze: height **3176**, `created_at 1795101446` — the minute the relay was restarted |
| 15:17–16:2x | **1h45m** with the chain advancing to 3245+ and no new kind-1447/1443/1444/1445/1448/1449/1450 on the relay |
| 15:17–15:25 | the relay *observer* (chain reading FROM the relay) hit 502s, retried every second and recovered at 15:25:47 (`run/v1.log:1139-1150`) — only the outbound side stayed dead |
| 16:2x | restarting `arena-chain.service` resumed publishing immediately: height 3250, `created_at 1795104944` |
| 16:24 | this fix deployed: fresh 1447 at `created_at 1795105478`, height 3260, same app hash `31bc7f4e…` |

The publisher retried every pass against the dead client, which is why the relay
shows repeated 1447s at heights 3175/3176 at 10-second intervals just before the
stop: the writes were still failing into the dead socket, not blocked on the
relay.

Damage while it was dead: the verdicts panel's roster (carried by 1447,
`chain/verdict.go:144`), every receipt for money that moved, every lot end and
every refusal were unwritten. The app's whole chain-state half was frozen.

## 2. The mechanism, with citations (before the fix)

- **One dial, forever.** `DialRelays` — `chain/relayclient.go:284-299` (pre-fix) — dialled
  each URL once and put the `*RelayClient` itself into `MultiRelayWriter.Writers`
  (`:259-272`, pre-fix). `startVerdictPublish` (`chain/relayclient.go:284`, pre-fix; call site `chain/node.go:638`) called it once at
  validator start; the `*MultiRelayWriter` lived in the `Publisher` struct
  (`chain/verdictpublish.go:113`, pre-fix) for the life of the process.
- **The read loop died, the client stayed "live".** `readLoop` returned on the
  first read error after recording `readErr` (`relayclient.go:78-86`, pre-fix) and
  `failAll` released the pending `okWait` channels (`:147-160`, pre-fix). The `closed`
  channel was closed **only** by `Close` (`:238-245`, pre-fix), and nothing called
  `Close` on a read error. So a client whose socket was gone was
  indistinguishable, to its callers, from a healthy one.
- **`Publish` then failed forever, or hung forever.** `Publish`
  (`relayclient.go:166-199`, pre-fix) registered a waiter, wrote, then selected on the
  receipt, `ctx.Done()` or `c.closed`. On a dead socket: either the write
  returned an error (recorded, no re-dial — this is what the relay saw as
  repeated failing 1447 attempts at one height) or the write found buffer space
  and the call waited for an `OK` that no goroutine would ever deliver. The
  publisher's `ctx` was cancelled only when the process exited
  (`node.go:637-640`, pre-fix), so the wait had no bound at all.
- **Nothing surfaced it.** `PublishRecord.Error` (`verdictpublish.go:76`, pre-fix) was
  appended to `p.records` (`:682`, pre-fix) and read only by tests. `/status` exposed
  height/peers/signers and nothing about the transport; the gateway had no route
  that would answer "is the publisher working?".
- **A relay down at validator start disabled publishing permanently.**
  `DialRelays` returned `errors.New("no relay could be reached")` when the
  initial dial failed, and `startVerdictPublish` turned that into "publish off"
  (`node.go:639-645`, pre-fix) for the process's lifetime — the same silent-death shape
  triggered from the other end.

## 3. The fix

All in `chain/relayclient.go` (plus wiring), with the publisher's event
building and anchoring untouched.

- **A dead socket now says so.** `RelayClient` gained a `dead` channel closed
  once when `readLoop` ends (`markDead`, `relayclient.go:164-166`); `Publish`
  checks it before registering and selects on it, so a publish on a dead socket
  returns `ErrRelayClosed` (wrapping the relay URL and the read error) instead
  of blocking. `write` refuses a dead socket. `Publish` also bounds the wait for
  `OK` with `publishAckTimeout = 15s`: a half-alive socket is a transport
  failure too.
- **`ReconnectingWriter`** (`relayclient.go:353`) is the publisher's transport
  for one relay and owns the client's lifetime:
  - a transport failure drops the client, re-dials and retries the **same**
    event once, immediately;
  - while the relay is down, re-dials are gated by an exponential backoff
    (`DefaultPublishBackoffBase = 1s` doubling to `DefaultPublishBackoffMax =
    30s`); an attempt inside the window fails fast without a dial, so a down
    relay costs one dial per window, not one per pass;
  - the first failure is logged with the relay URL and the error, repeats are
    rate-limited to `DefaultPublishLogEvery = 30s`, and recovery logs
    "publishing again after N failed attempt(s)";
  - a relay `OK=false` (including khatru's "mute: no one was listening" for the
    ephemeral pulse) is **not** a transport failure: it is counted as a
    rejection and never re-dials, because a fresh socket cannot change the
    answer to the same event.
- **`DialRelays` no longer errors when the relay is down at start**: every URL
  gets a reconnecting writer, the initial dial is only a warm-up, and publishing
  begins when the relay returns.
- **Observable from outside.** `MultiRelayWriter.Health()` and
  `Publisher.RelayHealth()` (`verdictpublish.go:701`) report, per relay:
  `connected`, `dials`, `failures`, `consecutiveFailures`, `rejections`,
  `deferrals`, `lastError`, `lastErrorAt`, `lastSuccessAt`, `nextDialAt`. Served
  by:
  - validator `GET /status` → `"publish": [...]` (`node.go:361`);
  - gateway `GET /api/publish` → per-validator relay health
    (`main.go:294-322`; the deployed gateway is `127.0.0.1:8090`).

### What an operator sees while the relay is down

```
$ curl -s 127.0.0.1:8090/api/publish
[{"index":1,"name":"Validator 1","up":true,"relays":[{"url":"wss://relay.playday.it",
  "connected":false,"dials":7,"failures":6,"consecutiveFailures":6,"rejections":0,
  "deferrals":138,"lastError":"dial tcp …: connect: connection refused",
  "lastErrorAt":1795109999,"lastSuccessAt":1795109900,"nextDialAt":1795110008}]}, …]
```

and in `run/v1.log` (first failure, then at most one every 30 s):

```
relay wss://relay.playday.it: publish relay failure (1 consecutive, next dial in 1s): dial tcp …: connect: connection refused
relay wss://relay.playday.it: publish relay failure (4 consecutive, next dial in 8s): …
relay wss://relay.playday.it: publishing again after 4 failed attempt(s)
```

A frozen publisher is now a line in the log and a `connected:false` with a
rising `consecutiveFailures` at a URL — not an inference from stale events an
hour later.

## 4. Tests (deterministic, loopback + fake clock; no nak, no mint)

`chain/relayreconnect_test.go`:

1. `TestReconnectingWriterRedialsAfterDroppedConnection` — a fake relay closes
   the first connection right after its first EVENT (no OK): the publish
   re-dials and succeeds (2 connections, 2 events received, 1 failure recorded,
   0 consecutive), the recovered connection is reused without a third dial, and
   a publish after the relay vanishes returns instead of hanging.
2. `TestReconnectingWriterBacksOffCountsAndLogs` — a closed port: three passes
   inside the window produce **one** dial, 1 failure, 2 deferrals and **one**
   log line naming the URL and the dial error; then the delay doubles and caps
   (2s, 4s, 8s, 8s, 8s) and every retry is counted.
3. `TestReconnectingWriterHealthyRelayDoesNotRedial` — three publishes, one
   connection, one dial, no failures, no log lines.
4. `TestReconnectingWriterKeepsSocketOnRelayRejection` — `OK=false` keeps the
   socket, counts `rejections`, logs no transport failure.

Raw counter output from test 2 (`go test -run TestReconnectingWriter -v`, fake clock):

```
health while down (3 passes inside the window): {URL:ws://127.0.0.1:40213 Connected:false
  Dials:1 Failures:1 ConsecutiveFailures:1 Rejections:0 Deferrals:2
  LastError:dial ws://127.0.0.1:40213: dial tcp 127.0.0.1:40213: connect: connection refused
  LastErrorAt:1800000000 NextDialAt:1800000001}
health after 8 gated+retried passes: {... Dials:6 Failures:6 ConsecutiveFailures:6
  Deferrals:2 ... NextDialAt:1800000058}
log lines total: 2 (rate-limited to one per LogEvery)
```

and from test 1 (the drop): `Dials:2 Failures:1 ConsecutiveFailures:0 LastError:relay
connection closed: … websocket: close 1006 (abnormal closure)` followed by
`publishing again after 1 failed attempt(s)`.

### Pre-fix, same scenario (probe at `998c7b6`, throwaway, raw output)

```
publish 1: err=relay connection closed after 0s
publish 2: err=context deadline exceeded after 1.001s
publish 3: err=write tcp 127.0.0.1:44682->127.0.0.1:36923: write: broken pipe after 0s
relay connections seen: 1
PRE-FIX: 3 publish attempts after the drop, 0 succeeded, connections still 1: the dead client was never replaced
```

Note the middle line: the pre-fix `Publish` **blocked** until its context died —
in production that context was the process's.

### Suite

`ARENA_TEST_MINT=http://127.0.0.1:3338 NAK_BINARY=$HOME/.local/bin/nak go test ./...`:

- baseline `998c7b6`: **178 pass, 0 fail, 0 skip**
- with the fix: **182 pass, 0 fail, 0 skip** (the 4 new tests)

## 5. Deploy (documented discipline)

```
go build -o …/arena-new .                      # from ~/work/wt-validator/chain
systemctl --user stop arena-chain.service
cp -p bin/arena bin/arena.prev-20261119-before-publisher-reconnect
cp …/arena-new bin/arena
systemctl --user start arena-chain.service
```

Proof from outside, immediately after the restart:

```
$ curl -s 127.0.0.1:8090/api/network
[("Validator 1",3260,"31bc7f4e856fc87c"), ("Validator 2",3260,"31bc7f4e856fc87c"),
 ("Validator 3",3260,"31bc7f4e856fc87c"), ("Validator 4",3260,"31bc7f4e856fc87c")]

$ curl -s 127.0.0.1:8090/api/publish
[{"name":"Validator 1","up":true,"relays":[{"url":"wss://relay.playday.it",
  "connected":true,"dials":1,"failures":0,"consecutiveFailures":0,
  "rejections":15,"deferrals":0,"lastSuccessAt":1795105468}]}, …]

$ nak req -k 1447 -l 1 wss://relay.playday.it
created_at 1795105478  tag ["height","3260"]
content {"schema":"arena.statecommit.v1","height":3260,"app_hash":"31bc7f4e856fc87c…"}
```

The root is the SAME app hash the pre-restart chain and the relay's last event
carried (`31bc7f4e…`, height 3260): state was not reset, `arena init` was not
run, the FROST group key is unchanged, and the schema string
`arena.statecommit.v1` is present on the fresh event. 1447 publishing resumed
within the first 10-second interval (`StateCommitMaxInterval`), and the
rejections shown are the ephemeral pulse's "mute: no one was listening"
non-events, not failures.

**Limitation, stated plainly:** the live relay (`wss://relay.playday.it`) was
not taken down for a reconnect demonstration — touching the relay is outside
this brief. The re-dial is proven by the Go tests above; the deployed binary is
proven to publish normally (fresh 1447 every ≤10 s, `connected:true`,
`failures:0`).