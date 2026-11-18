#!/usr/bin/env bash
# Live acceptance run for docs/HANDOVER-2026-11-17-validator-workstreams.md,
# workstreams B (restart-proof ingest) and C (prompt blocks + /api/tx).
#
# WHY IT STOPS YOUR SERVICE: the validators bind FIXED ports (p2p 2665x, rpc
# 2666x, http 710x) and the acceptance suite needs 127.0.0.1:8080, so it cannot
# coexist with a running arena-chain.service. The unit is stopped first and
# restarted by the EXIT trap whatever happens.
#
# It never touches run/chain (handover §9: `arena init` regenerates the FROST
# group key, which would orphan every escrow the app has locked). The chain under
# test is initialised fresh in run/chain-e2e.
#
#   ~/work/wt-validator/scripts/acceptance-validator-workstreams.sh
#
# Logs land in ~/work/wt-validator/run/acceptance/.
set -euo pipefail

REPO=/home/admin_maxime/work/wt-validator          # the branch under test
ASSETS=/home/admin_maxime/work/arena-merged-ref    # web/dist, web/node_modules, tests/e2e.mjs
ROOT=$REPO/run/chain-e2e
LOGDIR=$REPO/run/acceptance
MINT=http://127.0.0.1:3338
# A LOCAL, EMPTY relay, deliberately: the deployment's relay.playday.it carries
# the history of every earlier run, and a fresh root has no cursor yet, so the
# observers would replay all of it before they ever see this run's events. The
# ingest path under test is identical on any relay, and an empty one makes the
# run deterministic.
RELAY_PORT=10547
RELAY="ws://127.0.0.1:$RELAY_PORT"
PORT=8095   # NOT 8080: the retired MVP demo (auctions-mvp-demo.service) owns it (handover §7.4)
NAK=$HOME/.local/bin/nak
export PATH=$HOME/.local/go/bin:$PATH

mkdir -p "$LOGDIR" "$REPO/bin"

for p in "$PORT" "$RELAY_PORT"; do
  if ss -ltn 2>/dev/null | grep -q "127.0.0.1:$p "; then
    echo "port $p is already in use; pick another one in $0" >&2
    exit 1
  fi
done

restore() {
  echo "== stopping the acceptance gateway =="
  pkill -TERM -f "arena gateway -root $ROOT" 2>/dev/null || true
  sleep 2
  pkill -TERM -f "arena node -root $ROOT" 2>/dev/null || true
  pkill -TERM -f "nak serve --port $RELAY_PORT" 2>/dev/null || true
  sleep 1
  echo "== restarting arena-chain.service =="
  systemctl --user start arena-chain.service
  systemctl --user is-active arena-chain.service || true
  echo "== logs: $LOGDIR =="
}
trap restore EXIT

echo "== stopping arena-chain.service (it owns the validator ports) =="
systemctl --user stop arena-chain.service

echo "== building the branch under test =="
(cd "$REPO/chain" && go build -o "$REPO/bin/arena" .)
cp "$ASSETS/bin/arena-frost" "$REPO/bin/arena-frost"   # chain/frost.go resolves it NEXT TO the arena binary

echo "== local relay on 127.0.0.1:$RELAY_PORT =="
nohup "$NAK" serve --port "$RELAY_PORT" > "$LOGDIR/relay.log" 2>&1 &
for _ in $(seq 1 20); do
  ss -ltn 2>/dev/null | grep -q "127.0.0.1:$RELAY_PORT " && break
  sleep 0.5
done

echo "== the acceptance suite resolves web/node_modules from the worktree =="
[ -e "$REPO/web/node_modules" ] || ln -s "$ASSETS/web/node_modules" "$REPO/web/node_modules"

echo "== fresh chain root $ROOT (never run/chain) =="
rm -rf "$ROOT"
"$REPO/bin/arena" init -root "$ROOT" -mint "$MINT"

echo "== gateway on 127.0.0.1:$PORT with demo controls (suite only, never a real-sats deployment) =="
(cd "$REPO" && set -m; nohup ./bin/arena gateway -root "$ROOT" -static "$ASSETS/web/dist" \
  -addr "127.0.0.1:$PORT" -mint "$MINT" -relay "$RELAY" -demo-controls \
  > "$LOGDIR/gateway.log" 2>&1 &)
for _ in $(seq 1 40); do
  if curl -sf "127.0.0.1:$PORT/api/network" | grep -q '"up":true'; then break; fi
  sleep 1
done
curl -s "127.0.0.1:$PORT/api/network" | head -c 300; echo

echo
echo "== B and C probes =="
node "$REPO/tests/probe-restart-ingest.mjs" "http://127.0.0.1:$PORT" 2>&1 | tee "$LOGDIR/probe-bc.log"

echo
echo "== the instant refund while the lot is still open (handover-2026-11-18 §4.1) =="
node "$REPO/tests/probe-instant-refund.mjs" "http://127.0.0.1:$PORT" "$RELAY" 2>&1 | tee "$LOGDIR/probe-refund.log"

echo
echo "== acceptance suite (its bid-commit waits are the C gate) =="
# Run the copy in THIS worktree: the suite imports ../web/src/lib/arena.js, so
# running the canonical clone's copy would exercise the canonical clone's client
# instead of the branch's.
node "$REPO/tests/e2e.mjs" "http://127.0.0.1:$PORT" 2>&1 | tee "$LOGDIR/e2e.log"

echo
echo "== the cursor each validator resumed from, and the events it re-ingested =="
# the gateway writes each validator's log to filepath.Dir(root)/vN.log
for f in "$REPO"/run/v{1,2,3,4}.log; do
  [ -f "$f" ] && { echo "--- $f"; grep -E "resuming from cursor|committed .* kind=(1440|1441)|retrying" "$f" | tail -8; }
done
