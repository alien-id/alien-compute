#!/usr/bin/env bash
# Run the 3 GPU compute providers on this machine against the AWS testnet.
# Relay path: the AWS gateways reach the NAT'd nodes through persistent SSH
# reverse tunnels (whitepaper §11.1: Class O nodes hold outbound tunnels).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RUN="$ROOT/demo/run"
AWS_HOST="${AWS_HOST:-15.237.243.199}"
AWS_KEY="${AWS_KEY:-$HOME/.ssh/virial-aws.pem}"
cd "$ROOT"
mkdir -p "$RUN"

echo "== stopping any local fleet processes =="
pkill -f 'bin/fleetd[ ]' 2>/dev/null || true
pkill -f 'bin/llama-serve[r]' 2>/dev/null || true
# kill a previous tunnel keeper + its ssh
[[ -f "$RUN/tunnel.pid" ]] && kill "$(cat "$RUN/tunnel.pid")" 2>/dev/null || true
pkill -f '9101:127.0.0.1:910[1]' 2>/dev/null || true
sleep 1

echo "== building =="
go build -o bin/fleetd ./cmd/fleetd

echo "== persistent reverse tunnels to $AWS_HOST (9101-9103) =="
(
  while true; do
    ssh -i "$AWS_KEY" -o BatchMode=yes -o ExitOnForwardFailure=yes \
        -o ServerAliveInterval=15 -o ServerAliveCountMax=3 \
        -N \
        -R 9101:127.0.0.1:9101 \
        -R 9102:127.0.0.1:9102 \
        -R 9103:127.0.0.1:9103 \
        ec2-user@"$AWS_HOST" 2>>"$RUN/tunnel.log" || true
    echo "$(date) tunnel dropped, reconnecting in 3s" >> "$RUN/tunnel.log"
    sleep 3
  done
) &
echo $! > "$RUN/tunnel.pid"
sleep 2

echo "== starting 3 GPU providers (permissionless join to the AWS testnet) =="
for n in a b c; do
  mkdir -p "$RUN/node-$n"
  setsid ./bin/fleetd node -config "demo/aws/node-$n.json" > "$RUN/node-$n.log" 2>&1 < /dev/null &
  echo $! > "$RUN/node-$n.pid"
done

echo "== waiting for ACTIVE status on the AWS chain =="
for i in $(seq 1 600); do
  ACTIVE=$(curl -fsS "http://$AWS_HOST:9000/v1/registry/all" 2>/dev/null \
    | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for n in (d['nodes'] or []) if n['status']=='ACTIVE'))" 2>/dev/null || echo 0)
  if [[ "$ACTIVE" == "3" ]]; then echo "   all 3 providers ACTIVE on the AWS testnet"; break; fi
  if (( i % 20 == 0 )); then echo "   ... $ACTIVE/3 ACTIVE"; fi
  sleep 1
done

echo
curl -fsS "http://$AWS_HOST:9000/v1/registry/capacity" | python3 -c "
import json,sys
d=json.load(sys.stdin)
for m in d['models']:
    print(f\" {m['name']:30s} nodes={m['node_count']} free_slots={m['free_slots']}\")"
echo
echo "Providers are serving the AWS testnet at http://$AWS_HOST:9000 (and :9001)."
