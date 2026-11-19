# RESULT — the chain's advertised mint origin (`params.mintUrl`), 2026-11-19

Brief: make the chain name the browser-reachable mint origin so the anchored `arena.params.v1`
(kind 1448) and `/api/state` report `https://mint.playday.it` instead of `http://127.0.0.1:3338`,
and deploy that to the local test chain.

## 0. Bottom line

**Blocked, by the brief's own rules, and the blocker is structural — not a missing flag.**

`mintUrl` lives in `Params`, `Params` is a field of the **hashed** app state, and the app hash of
every block is signed by the validators. It is written **only** at genesis (`arena init`) and read
**only** at `InitChain`; nothing at runtime — no tx kind, no `-mint` on `arena node`, no environment
variable — can change it. The only way to change it is a **new genesis**, i.e. a chain reset, which
the brief forbids ("NEVER run `arena init`", "do not wipe state, do not reset the chain DB"). So the
value is unchanged and nothing was deployed; §3–§6 below are the measured facts and the recommended
alternative. §7 gives the exact, keys-preserving reset an operator can authorise in one step.

The chain's **own** mint endpoint is a *different* value (the `-mint` flag, used by the gateway's
`/mint/` proxy and the settler). The code separates the two, and that separation is why the
recommendation is: **keep the chain's own calls on loopback, and fix the advertised origin** — which
in this deployment can only be done app-side today, or by a reset.

## 1. The mechanism, with citations

### 1.1 Where the value lives and where it is written

- Field: `Params.MintURL string \`json:"mintUrl"\`` — `chain/app.go:25`.
- Written **only** by genesis: `arena init` reads `-mint` (`chain/node.go:120`), pins the mint's
  active sat keyset, and builds the params:
  `params := Params{MintURL: *mint, ...}` — `chain/node.go:197`; that value becomes
  `types.GenesisDoc{..., AppState: ab}` — `chain/node.go:217-218`, saved as
  `vN/config/genesis.json` — `chain/node.go:222`. No other production assignment to `MintURL`
  exists anywhere in `chain/` (checked across the tree).
- Read **only** at chain start: `InitChain` unmarshals `req.AppStateBytes` into `a.st.Params` —
  `chain/app.go:284-286`. On every later restart the state (and therefore `Params`) comes from the
  app-state snapshot, not genesis: `chain/node.go:266-272` (`loadSnapshot` + `restore`),
  `chain/snapshot.go:282-287` (`restore`).

### 1.2 It is inside the app hash

- `Params` is a field of the hashed `State` — `chain/app.go:164-176`;
  `func (s *State) canonical()` marshals it (only `Height`/`Time` are zeroed) — `chain/app.go:180-186`;
  `hash()` is sha256 of those bytes — `chain/app.go:188-191`.
- Verified numerically against the live root's own snapshot
  (`run/chain/v1/data/app-state/3000.json`, `state.height/time = 0`, i.e. the canonical bytes):

  ```
  committed appHash                     : d8dbab51d60185b3213855e368fe6a6abcdbc5daa8a0ed4c626626c1605e8659
  sha256(state as stored)               : d8dbab51d60185b3213855e368fe6a6abcdbc5daa8a0ed4c626626c1605e8659  (equal)
  sha256(state, mintUrl=mint.playday.it): e4696ee8e83d7bd5a0354ce4ac20d2dc44baf7aa6e6fa87a8da9085542101e3b  (differs)
  ```

  The invariant is asserted in-repo too: `chain/live_test.go:352`
  ("app hash must be sha256 of the canonical bytes").

### 1.3 The `-mint` flag is a different thing (this is the important separation)

- `arena node` has **no** mint flag at all — `chain/node.go:244-247` (`-root`, `-i`,
  `-snapshot-every`, `-retain-blocks`, `-relay`, `-commit-max-interval`).
- `arena gateway -mint` is used for exactly two things, neither of which touches `Params`:
  the `/mint/` reverse proxy (`chain/main.go:177-179`) and the settler's mint endpoint
  (`chain/main.go:346` → `settler.mint`, `chain/settler.go:85`, used for the actual swap POST at
  `chain/settler.go:307`).
- So: **the flag is the chain's own endpoint; `params.mintUrl` is what a client is told to use.**

### 1.4 What re-publishes the anchored kind-1448

- `Publisher.Step` — `chain/verdictpublish.go:194-287`. `needParams` is edge-triggered on the content
  digest plus a height cadence: `digest := paramsVersion(state.Params)` —
  `chain/verdictpublish.go:235-238`; the event is built from the **committed** state:
  `BuildParamsEvent(p.key, state.Params, height, appHashHex, now)` — `chain/verdictpublish.go:263`.
- The anchor is `(height, app_hash)`, carried in both the tags and the content:
  `{"height", h}`, `{"hash", appHashHex}`, `{"p", pubkey}` — `chain/verdict.go:615-619`; content
  `height` + `app_hash` — `chain/verdict.go:597`. `mint_url` is content — `chain/verdict.go:598`,
  field `chain/verdict.go:294`.
- `params_version` digests the whole `Params` — `chain/verdict.go:342` (`paramsVersion`). A client
  that trusts the commitment is meant to check the params it reads against it (§4 of the
  2026-11-17 handover: "the params in the event must be the ones committed in the app hash it
  names").

### 1.5 What validates existing escrows / receipts against the old value

**Nothing.** The mint identity a bid is checked against is the **keyset pin**, not the URL:

- `s.Params.KeysetID` + `s.Params.Keys[amount]` — `chain/app.go:713,716` (bid admission) and
  `chain/settle.go:180-181` (settle verification);
- the FROST group key — `chain/app.go:666`, `chain/settle.go:230`.

`MintURL` has no reader in the chain's own money paths; its only readers are the params event
(§1.4), `ClientView.Params` (`chain/view.go:143`) and the full-state `/state`/`/api/state` handler
(`chain/node.go:318-321`, proxied by `chain/main.go:184-218`). So changing it would invalidate
nothing — it is purely the advertised origin. The blocker is purely the hash binding.

### 1.6 Nothing can update it at runtime

- The ingest transports only two kinds: `RelayKinds = []int{KindCreate, KindBid}` —
  `chain/relayobserve.go:41-42`.
- The tx kinds are create/bid (`decodeTx`, `chain/app.go:366-380`) plus the special unsigned
  `close` / `settle` txs (`chain/app.go:836-838`, `chain/app.go:917-931`). There is no params tx.
- The only environment variables the chain reads are `ARENA_FROST`, `ARENA_FROST_SERVE`
  (`chain/frost.go:31,46`) and `ARENA_PUSH` (`chain/main.go:345`); `ARENA_TEST_MINT` /
  `NAK_BINARY` are test-only (`chain/sigall_mint_test.go:41`, `chain/relayobserve_test.go:54`).
- `arena init -if-needed` returns early and changes nothing when the FORMAT marker matches —
  `chain/node.go:123-130`; otherwise it wipes.

## 2. Why an edited genesis cannot be adopted by the running chain

Two independent walls, either of which is fatal:

1. **The snapshot is authoritative.** On restart the validator restores the committed state from
   `data/app-state/<h>.json` (`chain/node.go:266-272`) — which carries the old value — and refuses
   any snapshot whose app hash disagrees with the chain's own record
   (`chain/snapshot.go:270-275`). An edited `app-state` file is refused: the validator stops with
   the reason rather than replaying.
2. **Deleting the snapshot makes replay itself panic.** With the snapshot gone the app reports
   height 0, so CometBFT calls `InitChain` with the edited genesis and then replays the block store
   (`cometbft@v1.0.1 internal/consensus/replay.go:303-324`). Each replayed app hash is compared with
   the next block header's `AppHash` and the mismatch is a **panic**, not an error:
   `assertAppHashEqualsOneFromBlock` — `internal/consensus/replay.go:484-486` and `:529-537`.
   The headers' `AppHash` fields are covered by validator signatures, so they cannot be rewritten
   either.

So the value is immutable for the lifetime of a genesis. That is not a configuration gap; it is the
protocol as designed (the terms are deliberately committed).

One trap worth recording, because "just edit `genesis.json`" is the obvious first idea and it is
**not** inert: the gateway reads `v1/config/genesis.json` live at every start for the release
coordinator's params (`chain/frost.go:320-337`, `params: g.AppState`). Today that copy of
`mintUrl` is unused (the settler dials `settler.mint`, from the `-mint` flag — §1.3, and the keyset
pin is what the money paths read), but a hand-edit there would still be a live, silent divergence
between the genesis on disk and the params the app hash commits to — so don't: it changes nothing
about the anchors and only risks the two disagreeing.

## 3. The public origin is reachable from the chain's own client path — measured

`https://mint.playday.it` → Caddy → `127.0.0.1:3338`, same mint DB, same keyset as the committed one.

```
$ curl -s https://mint.playday.it/v1/keysets
{"keysets":[{"id":"0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1",
 "unit":"sat","active":true,"input_fee_ppk":0,"final_expiry":null}]}

$ curl -o /dev/null -w '%{http_code}' -X OPTIONS https://mint.playday.it/v1/swap \
    -H 'Origin: https://market-e2e.playday.it' -H 'Access-Control-Request-Method: POST'
204
```

and the chain's **own escrow client** ran against it: the whole suite was run with
`ARENA_TEST_MINT=https://mint.playday.it`, which is the mint the escrow tests actually mint, lock
and spend against (`chain/sigall_mint_test.go`, `chain/v4v_mint_test.go` — the mint's own NUT-11
verification is the judge). See §6 for the counts; the public round-trip is exercised end to end,
not merely probed.

**Recommendation: keep the chain's own calls on loopback** (`-mint http://127.0.0.1:3338`). The
mint is on this host, so the public path adds DNS + Caddy + TLS + an external dependency to
settlement for no benefit — a Caddy or DNS blip would stall refunds and payouts. The two values are
already separate in the code (§1.3); the one that needs to change is the *advertised* one, which
today cannot change without a reset. Until then the only non-destructive fix is app-side: the app
must treat `http://127.0.0.1:3338` and `https://mint.playday.it` as the same mint identity (same
pubkey, same keyset) — they are — and use the public origin for browser-initiated calls.

## 4. The deployment was not touched

No binary change was needed for the outcome above, so nothing was swapped or restarted: restarting
the unit to deploy a no-op would have risked the running chain for nothing. The unit still runs

```
ExecStart=/home/admin_maxime/work/arena-merged-ref/bin/arena gateway -root /home/admin_maxime/work/arena-merged-ref/run/chain \
  -static /home/admin_maxime/work/arena-merged-ref/web/dist -addr 127.0.0.1:8090 \
  -mint http://127.0.0.1:3338 -relay wss://relay.playday.it
```

(height 3105 at 2026-11-19 14:07 UTC, 4 validators up, root untouched — `bin/arena` sha256
`460bd5c9863a4ed4…` unchanged, no `bin/arena.prev-*` created).

## 5. The "before" state (the value that is still published)

```
$ curl -s http://127.0.0.1:8090/api/state      → height 3105, params.mintUrl "http://127.0.0.1:3338"
                                                 params.keysetId 0102b930f4dc2d6a8cb29b56e30d3e16436337859f54b77849ad1a1297be2880a1
```

Newest anchored kind-1448 on `wss://relay.playday.it` (one of four, one per validator, same anchor):

```
id   7e4b4a6711a777ee3c2babca7347521c79c57745de290856f14fae45e100f91d
tags [["height","3100"],["hash","dbda136796ac2d0c6e4fee72a502820785a9f1f0874933200d63d4679822ac49"],["p","4e1c04b9…"]]
     content height=3100 app_hash=dbda1367…  params_version=71ca57eb…  mint_url=http://127.0.0.1:3338
```

Tags and content agree on the anchor `(3100, dbda1367…)`; the content's `params_version`
(`71ca57eb…`, first seen in the 2026-11-19 defect note) is the digest of the committed params, i.e.
of the loopback value.

## 6. Suite

`~/work/wt-validator/chain`,
`ARENA_TEST_MINT=https://mint.playday.it NAK_BINARY=$HOME/.local/bin/nak go test -count=1 -v ./...`

```
--- PASS: 178        --- FAIL: 0        --- SKIP: 0
ok  	arenachain	17.581s
```

Documented baseline at `998c7b6`: 178 pass / 0 fail / 0 skip — **the same count, and nothing was
skipped**, which matters here: the escrow tests only run when a mint answers
(`chain/sigall_mint_test.go:109`, `chain/v4v_mint_test.go`), and they passed against the public
origin, through DNS + Caddy + TLS:

```
--- PASS: TestMintAcceptsSettleTwoOfTwo (0.61s)
--- PASS: TestMintAcceptsRefundAfterLocktime (0.51s)
--- PASS: TestMintRefusesCommitteeAlone (0.46s)
--- PASS: TestMintRefusesRedirectedOutputs (0.46s)
--- PASS: TestMintAcceptsV4VSplitSettle (0.51s)
```

So the same run is §3's proof: these mint, lock and spend real escrows at
`https://mint.playday.it`, and the mint's own NUT-11 verification is the judge. Raw log:
`~/.hermes/profiles/maxime/cache/scratch/mintorigin/suite-public.log`.

## 7. The only route that would change the value — for an operator to authorise

A new genesis. The sanctioned in-repo command is `./scripts.sh reset` / `arena init`, but a plain
`arena init` regenerates the FROST group key and every validator/verdict key (`chain/node.go:186-212`)
and wipes the root (`chain/node.go:150-151`): every escrow the app has locked becomes unspendable.
If a reset is authorised, do it **keys-preserving** so no escrow is cryptographically stranded, and
accept the state loss:

```bash
set -euo pipefail
R=~/work/arena-merged-ref; NEW=$R/run/chain-mintorigin
systemctl --user stop arena-chain.service                      # 0. stop first: never move a live root
# 1. new genesis = the current one with ONLY app_state.mintUrl changed (same groupKey,
#    frostPublic, validators roster, keyset, validators[] — so the retained key files still match)
python3 - "$R/run/chain" "$NEW" <<'PY'
import json,os,shutil,sys
src,new=sys.argv[1],sys.argv[2]
for i in (1,2,3,4):
    h=os.path.join(new,f'v{i}')
    os.makedirs(os.path.join(h,'config'),exist_ok=True); os.makedirs(os.path.join(h,'data'),exist_ok=True)
    g=json.load(open(os.path.join(src,f'v{i}/config/genesis.json')))
    g['app_state']['mintUrl']='https://mint.playday.it'
    json.dump(g,open(os.path.join(h,'config/genesis.json'),'w'))
    for f in ('frost_key.json','verdict_key.json','priv_validator_key.json','node_key.json'):
        shutil.copy2(os.path.join(src,f'v{i}/config',f),os.path.join(h,'config',f))
    shutil.copy2(os.path.join(src,f'v{i}/peers.txt'),os.path.join(h,'peers.txt'))
open(os.path.join(new,'FORMAT'),'w').write('6\n')
PY
# 2. keep the old root, point the unit at the new one, restart
mv $R/run/chain $R/run/chain-pregmintorigin-$(date +%Y%m%d)
sed -i "s#-root $R/run/chain #-root $NEW #" ~/.config/systemd/user/arena-chain.service
systemctl --user daemon-reload && systemctl --user start arena-chain.service
```

Cost, stated plainly: the chain restarts at height 0 with an empty history — every auction and bid
record is gone, and with it the committed outcome templates, so the escrows locked on the current
chain (including the two the 2026-11-19 defect note already found unsettleable) can no longer be
settled or refunded from the chain. The FROST group key, the keyset and the verdict roster are
preserved by the script above, so those escrows remain *signable* — the loss is the chain's record
of them, not the key.

Rollback: the old root is kept as `run/chain-pregmintorigin-<date>`; point `-root` back and restart.

## 8. The durable fix (not implemented — it needs an owner decision)

The protocol conflates two different things in one hashed term:

- a **committed escrow term** that a client must be able to verify against the app hash (the FROST
  group key, the keyset, the windows, the roster) — correctly genesis-bound; and
- the **mint origin a client should dial**, which is a transport/display value, identical for both
  identities of the same mint, and the one thing that must be able to change when a deployment gets
  a public hostname.

Options, in order of preference:

1. **App-side identity mapping** (interim, chosen while this stands): the app treats the committed
   loopback origin and the public origin as the same mint (same pubkey/keyset) and uses the public
   one for browser calls. No chain change; no security loss, because the keyset pin is what actually
   binds escrow value.
2. **Publish the dial-able origin beside the committed terms** — e.g. a new content field on the
   kind-1448, explicitly *not* part of `params_version` — so clients can discover it without a
   reset. This weakens the §4 consistency rule (the field is unverifiable), so it needs an owner
   ruling and a documented trust rule (a client must still lock value to the *committed* keyset).
3. **Accept a reset** whenever the advertised origin must change (§7), and keep the value in genesis.
