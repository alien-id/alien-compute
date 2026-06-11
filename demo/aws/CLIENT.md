# Running a Fleet compute client (third computer)

The Fleet testnet is live with its control plane on AWS:

| What | Where |
|---|---|
| Gateway entrypoints | `http://15.237.243.199:9000` and `http://15.237.243.199:9001` (eu-west-3) |
| Compute providers | 3 GPU nodes (RTX 4090s): gemma4 (Gemma 4 12B), gemma-4-26b-a4b-it, gemma-4-e4b-it |
| Payment | testnet FLEET — the client claims 50 FLEET from the faucet automatically |

You only need **one** entrypoint URL: the client discovers all gateways via
peer exchange (`/v1/net/peers`), races pings, and fails over automatically.

## 1. Get the `fleet-proxy` binary

Pick the build for your OS from the AWS node (same key as for SSH):

```bash
# Linux x86_64
scp -i ~/.ssh/virial-aws.pem ec2-user@15.237.243.199:fleet/dist/fleet-proxy-linux-amd64 ./fleet-proxy
# macOS (Apple Silicon)
scp -i ~/.ssh/virial-aws.pem ec2-user@15.237.243.199:fleet/dist/fleet-proxy-darwin-arm64 ./fleet-proxy
# Windows
scp -i .ssh/virial-aws.pem ec2-user@15.237.243.199:fleet/dist/fleet-proxy-windows-amd64.exe fleet-proxy.exe

chmod +x ./fleet-proxy   # linux/macos
```

(Or build from source: `go build ./cmd/fleet-proxy` in the fleet repo.)

## 2. Start the local proxy

```bash
./fleet-proxy -gateway http://15.237.243.199:9000
```

First run prints:
- your generated session key (your wallet identity),
- `faucet granted 50 FLEET`,
- `payment channel … open, deposit 45 FLEET`,
- `OpenAI-compatible API on http://127.0.0.1:8080/v1`.

State (keys, channel, alias pins) persists in `~/.fleet-proxy`.

## 3. Use it like any OpenAI endpoint

```bash
# what can the network serve right now (models, metadata, capacity, measured perf)
curl http://127.0.0.1:8080/v1/models
curl http://127.0.0.1:8080/fleet/capacity

# chat (default model gemma4 = Gemma 4 12B; it is a reasoning model —
# give it max_tokens ≥ 200)
curl http://127.0.0.1:8080/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "gemma4",
  "messages": [{"role": "user", "content": "Hello from the third computer!"}],
  "max_tokens": 300
}'

# streaming works too: add "stream": true
```

Python (any OpenAI SDK):

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="not-needed")
r = client.chat.completions.create(
    model="gemma4",
    messages=[{"role": "user", "content": "What model are you?"}],
    max_tokens=300,
)
print(r.choices[0].message.content)
```

## 4. See what you paid

```bash
curl http://127.0.0.1:8080/fleet/status     # your wallet, channel, verified receipts
curl http://15.237.243.199:9000/v1/ledger/stats   # chain view: burns, provider earnings
```

Every request is end-to-end encrypted (HPKE) to the serving GPU node — the
AWS gateway relays ciphertext only — and is paid for with a signed voucher;
the node's signed receipt is verified and countersigned by your proxy before
it settles on the testnet chain.

## Troubleshooting

- `fleet-proxy` needs outbound TCP to `15.237.243.199:9000-9001`. No inbound ports.
- A second machine claiming the faucet uses its own session key → its own 50 FLEET.
- If you ever see `all gateways unreachable`, check `curl http://15.237.243.199:9000/healthz`.
