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
RELAY=wss://relay.playday.it
PORT=8080
export PATH=$HOME/.local/go/bin:$PATH

mkdir -p "$LOGDIR" "$REPO/bin"

restore() {
  echo "== stopping the acceptance gateway =="
  pkill -TERM -f "arena gateway -root $ROOT" 2>/dev/null || true
  sleep 2
  pkill -TERM -f "arena node -root $ROOT" 2>/dev/null || true
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
echo "== acceptance suite (its bid-commit waits are the C gate) =="
node "$ASSETS/tests/e2e.mjs" "127.0.0.1:$PORT" 2>&1 | tee "$LOGDIR/e2e.log"

echo
echo "== the cursor each validator resumed from, and the events it re-ingested =="
# the gateway writes each validator's log to filepath.Dir(root)/vN.log
for f in "$REPO"/run/v{1,2,3,4}.log; do
  [ -f "$f" ] && { echo "--- $f"; grep -E "resuming from cursor|committed .* kind=(1440|1441)|retrying" "$f" | tail -8; }
done
