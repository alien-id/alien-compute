#!/usr/bin/env bash
# Launch the Phase-0 Fleet testnet:
#   1 chain-sim (stands in for the decentralized L2 of whitepaper §14)
# + 2 independent gateway PoPs (stateless relays — any number may run)
# + 3 permissionless Class O nodes pinned to GPUs 0 / 1,2 / 3,
#   joining through different gateways.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN="$ROOT/demo/run"
cd "$ROOT"
mkdir -p "$RUN"

echo "== building binaries =="
go build -o bin/fleetd ./cmd/fleetd
go build -o bin/fleet-proxy ./cmd/fleet-proxy

echo "== stopping any previous testnet =="
# [r] avoids the pattern matching wrapper shells that quote this script
pkill -f 'bin/fleetd[ ]' 2>/dev/null || true
pkill -f 'bin/fleet-prox[y]' 2>/dev/null || true
pkill -f 'bin/llama-serve[r]' 2>/dev/null || true
sleep 1

if [[ "${FRESH:-0}" == "1" ]]; then
  echo "== FRESH=1: wiping ledger + node state =="
  rm -rf "$RUN"/ledger.json "$RUN"/node-* ~/.fleet-proxy-demo
fi

echo "== starting chain-sim (testnet stand-in for the L2 rollup) =="
./bin/fleetd chain \
  -listen 127.0.0.1:9500 \
  -state "$RUN/ledger.json" \
  -shadow 5s \
  -dev-faucet \
  -node-bond 0.05 \
  > "$RUN/chain.log" 2>&1 &
echo $! > "$RUN/chain.pid"
until curl -fsS http://127.0.0.1:9500/healthz >/dev/null 2>&1; do sleep 0.2; done
echo "   chain up: http://127.0.0.1:9500"

echo "== starting 2 independent gateway PoPs =="
./bin/fleetd gateway -name gw-east -listen 127.0.0.1:9000 -chain http://127.0.0.1:9500 \
  > "$RUN/gateway-1.log" 2>&1 &
echo $! > "$RUN/gateway-1.pid"
./bin/fleetd gateway -name gw-west -listen 127.0.0.1:9001 -chain http://127.0.0.1:9500 \
  > "$RUN/gateway-2.log" 2>&1 &
echo $! > "$RUN/gateway-2.pid"
until curl -fsS http://127.0.0.1:9000/healthz >/dev/null 2>&1; do sleep 0.2; done
until curl -fsS http://127.0.0.1:9001/healthz >/dev/null 2>&1; do sleep 0.2; done
echo "   gateways up: http://127.0.0.1:9000 (gw-east), http://127.0.0.1:9001 (gw-west)"

echo "== starting 3 nodes (permissionless join) =="
for n in a b c; do
  mkdir -p "$RUN/node-$n"
  ./bin/fleetd node -config "demo/node-$n.json" > "$RUN/node-$n.log" 2>&1 &
  echo $! > "$RUN/node-$n.pid"
done

echo "== waiting for nodes to load models, register, and pass SHADOW =="
for i in $(seq 1 600); do
  ACTIVE=$(curl -fsS http://127.0.0.1:9000/v1/registry/all 2>/dev/null \
    | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for n in (d['nodes'] or []) if n['status']=='ACTIVE'))" 2>/dev/null || echo 0)
  if [[ "$ACTIVE" == "3" ]]; then
    echo "   all 3 nodes ACTIVE"
    break
  fi
  if (( i % 20 == 0 )); then echo "   ... $ACTIVE/3 ACTIVE"; fi
  sleep 1
done

echo
echo "== registry =="
curl -fsS http://127.0.0.1:9000/v1/registry/all | python3 -m json.tool | grep -E '"(nik|status|gpus|aliases|bundle_hash)"' || true
echo
echo "== GPU pinning =="
nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory --format=csv || true
echo
echo "Testnet is up. Logs in $RUN/. Next: demo/demo.sh"
