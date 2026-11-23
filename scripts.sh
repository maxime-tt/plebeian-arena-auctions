#!/usr/bin/env bash
# Arena auctions demo control: ./scripts.sh build|reset|start|stop|test
set -e; cd "$(dirname "$0")"
# Go is not on PATH in this environment.  Prepend the real toolchain only when
# `go` is not already resolvable, and never a path that does not exist (the
# retired /tmp/go/bin made every `go` invocation fail).
if ! command -v go >/dev/null 2>&1; then export PATH="$HOME/.local/go/bin:$PATH"; fi
case "$1" in
  build) (cd chain && go build -o ../bin/arena .) && (cd web && npm run build >/dev/null) && echo built ;;
  mint)  set -a; . run/mint.env; set +a; (set -m; nohup .venv-mint/bin/mint > run/mint.log 2>&1 &) ;;
  stop)  pkill -TERM -f "bin/arena gateway" || true; sleep 2; pkill -TERM -f "bin/arena node" || true ;;
  reset) $0 stop; sleep 1; ./bin/arena init -root run/chain ;;
  start) (set -m; nohup ./bin/arena gateway -root run/chain -static web/dist -addr ${HOST:-127.0.0.1}:${PORT:-8080} -dev ${DEMO_CONTROLS:+-demo-controls} > run/gateway.log 2>&1 &); sleep 4; curl -s 127.0.0.1:${PORT:-8080}/api/network | head -c 200; echo ;;
  test)  node tests/e2e.mjs "http://127.0.0.1:${PORT:-8080}" ;;
esac
