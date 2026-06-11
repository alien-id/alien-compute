#!/usr/bin/env bash
# End-to-end client demo: fleet-proxy (OpenAI API) -> gateway relay ->
# GPU node, paid per request with testnet FLEET through a payment channel.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN="$ROOT/demo/run"
cd "$ROOT"

PROXY=http://127.0.0.1:8080
AGENT_ID_CLI="${AGENT_ID_CLI:-}"

echo "== starting fleet-proxy (local OpenAI-compatible proxy) =="
pkill -f 'bin/fleet-prox[y]' 2>/dev/null || true
sleep 0.5
EXTRA=()
if [[ -n "$AGENT_ID_CLI" ]]; then EXTRA+=(-agent-id-cli "$AGENT_ID_CLI"); fi
./bin/fleet-proxy \
  -gateway http://127.0.0.1:9000 \
  -listen 127.0.0.1:8080 \
  -default-model gemma4 \
  -state-dir "$HOME/.fleet-proxy-demo" \
  "${EXTRA[@]}" \
  > "$RUN/proxy.log" 2>&1 &
echo $! > "$RUN/proxy.pid"
until curl -fsS $PROXY/healthz >/dev/null 2>&1; do sleep 0.2; done
sed -n '1,5p' "$RUN/proxy.log"

echo
echo "== 1. model list (OpenAI /v1/models) =="
curl -fsS $PROXY/v1/models | python3 -m json.tool

echo
echo "== 1b. network capacity query (exact models, metadata, slots, measured perf) =="
curl -fsS $PROXY/fleet/capacity | python3 -m json.tool

echo
echo "== 2. chat completion, default model (gemma4 = gemma-4-12B on node-a, GPU 0) =="
curl -fsS $PROXY/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "gemma4",
  "messages": [{"role":"user","content":"In one sentence: what is a payment channel?"}],
  "max_tokens": 400
}' | python3 -m json.tool

echo
echo "== 3. streaming chat completion (SSE) =="
(curl -fsSN $PROXY/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "gemma4",
  "messages": [{"role":"user","content":"Count from 1 to 5, words only."}],
  "max_tokens": 300, "stream": true
}' || true) | tail -6

echo
echo "== 4. a different model on the 2-GPU node (gemma-4-26b-a4b-it, GPUs 1+2) =="
curl -fsS $PROXY/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "gemma-4-26b-a4b-it",
  "messages": [{"role":"user","content":"Reply with exactly: FLEET-26B-OK"}],
  "max_tokens": 300
}' | python3 -m json.tool

echo
echo "== 5. third model (gemma-4-e4b-it on node-c, GPU 3) =="
curl -fsS $PROXY/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "gemma-4-e4b-it",
  "messages": [{"role":"user","content":"Reply with exactly: FLEET-E4B-OK"}],
  "max_tokens": 300
}' | python3 -m json.tool

echo
echo "== 6. proxy wallet / receipts (client view) =="
curl -fsS $PROXY/fleet/status | python3 -m json.tool

echo
echo "== 7. chain-sim ledger (burns, tips, bonds, dual-signed receipts) =="
curl -fsS http://127.0.0.1:9000/v1/ledger/stats | python3 -m json.tool

echo
echo "== 8. GPU pinning during the run =="
nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory --format=csv || true

echo
echo "=================================================================="
echo "==                FINAL REPORT — Fleet testnet                  =="
echo "=================================================================="
"$(dirname "$0")/report.sh"

echo
echo "Client-side view: curl http://127.0.0.1:8080/fleet/status"
