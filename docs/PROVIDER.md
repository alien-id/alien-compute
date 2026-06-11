# Connecting a compute provider to the Fleet testnet

Anyone with a GPU can join the live testnet and earn testnet FLEET —
joining is **permissionless** (whitepaper §8.1): no account, no approval,
no capital. Your bond is *earned-escrow*: the first 0.05 testnet-FLEET of
tips is held as your bond, everything after that is spendable.

The live testnet control plane:

| | |
|---|---|
| Gateway entrypoints | `http://15.237.243.199:9000`, `http://15.237.243.199:9001` |
| Dashboard | https://fleet.lethe.gg |

## 0. What you need

- A 64-bit Linux machine with an NVIDIA GPU (≥ 8 GB VRAM; `cuda-consumer`
  profile, §5.2) — or several GPUs, one node process per model.
- CUDA toolkit + driver, Go ≥ 1.22, cmake (see [BUILD.md](BUILD.md)).
- Outbound internet. Inbound reachability is needed for the relay path —
  see step 3 for the NAT options.

## 1. Build

```bash
git clone <this repo> fleet && cd fleet
make build                    # bin/fleetd
make llama CUDA_ARCH=89       # llama.cpp backend (set your GPU arch: 86=30xx, 89=40xx, 90=H100)
make models                   # or drop your own GGUF into models/
```

## 2. Write your node config

`my-node.json`:

```jsonc
{
  "name": "my-node",
  "gateway": "http://15.237.243.199:9000,http://15.237.243.199:9001",
  "listen": "0.0.0.0:9101",
  "public_addr": "http://<YOUR-REACHABLE-ADDR>:9101",   // see step 3
  "data_dir": "node-data",                // your NIK (identity + earnings key) persists here
  "llama_bin": "third_party/llama.cpp/build/bin/llama-server",
  "llama_port": 9201,
  "gpus": "0",                            // CUDA_VISIBLE_DEVICES; "0,1" = 2-GPU layer split
  "profile": "cuda-consumer",
  "n_parallel": 2,
  "nek_rotate": "1h",
  "model": {
    "path": "models/gemma-4-12B-it-Q4_K_M.gguf",
    "aliases": ["gemma4", "gemma-4-12b-it"],
    "ctx_len": 16384,
    "tip_in_ktok": 1000000,               // YOUR price: nano-FLEET per kTok input
    "tip_out_ktok": 5000000               //                              output
  }
}
```

Notes:

- **Model choice is yours.** Serve an existing bundle (alias must match the
  bundle already registered — alias↔hash binding is first-come, AX5) or
  bring a brand-new model with new aliases. Scarce models earn you all the
  traffic for them.
- **Pricing is yours.** The tip is what you earn per kTok; the burned base
  fee is network-wide. Routing weighs score²/price, so overpricing costs
  you traffic.
- **Identity**: `node-data/nik.seed` *is* your node — its earnings, score,
  and reputation. Back it up; don't share it.

## 3. Reachability (the relay path)

Gateways relay encrypted requests **to** `public_addr`. Three options:

| Your situation | `public_addr` |
|---|---|
| Public IP / port-forward on your router | `http://<your-ip>:9101` |
| Any VPS you own | reverse tunnel: `ssh -N -R 9101:127.0.0.1:9101 vps` → `http://<vps-ip>:9101` (set `GatewayPorts yes` on the VPS sshd, or relay via nginx) |
| Tunneling service (cloudflared, tailscale funnel, …) | the public URL it gives you |

The whitepaper's target design (§11.1) is persistent outbound QUIC tunnels
to the gateways themselves — NAT'd nodes will need zero setup; on the MVP
testnet the tunnel is your own (see `demo/aws/providers.sh` for a
production example of a persistent SSH tunnel keeper).

## 4. Launch

```bash
./bin/fleetd node -config my-node.json
```

What happens (all automatic):

1. llama-server starts pinned to your GPUs; the model artifact is hashed
   (its on-chain identity) and metadata extracted.
2. The node **registers permissionlessly** with the chain via any gateway
   and discovers the other gateways via peer exchange.
3. ~5 s SHADOW probation, then **ACTIVE**: you appear on
   https://fleet.lethe.gg and start receiving encrypted traffic.
4. Every served request yields a signed receipt; settlement credits your
   tip (bond first, then spendable balance).

## 5. Watch your earnings

```bash
# your node on the dashboard
open https://fleet.lethe.gg

# your balances (NIK pub is printed at node startup)
curl http://15.237.243.199:9000/v1/ledger/stats | jq '.providers["<your-nik-hex>"]'
curl http://15.237.243.199:9000/v1/ledger/account/<your-nik-hex>
```

## 6. Verify it end-to-end (optional but encouraged)

Run a client against your own model from any machine:

```bash
./bin/fleet-proxy -gateway http://15.237.243.199:9000
curl http://127.0.0.1:8080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"<your-alias>","messages":[{"role":"user","content":"hi"}],"max_tokens":200}'
curl http://127.0.0.1:8080/fleet/status     # the receipt your node signed
```

## Operational notes

- **Stopping**: just kill the process — in-flight requests fail over;
  your identity, bond, and balance persist on-chain. Ordinary downtime is
  never slashed (§10): you only stop earning while away.
- **Multiple GPUs**: one node per model is the simple path (`gpus` pins
  each); a single node can also split one big model across GPUs
  (`"gpus": "0,1"` → llama.cpp layer split).
- **Privacy**: you serve Class O — you *can* see the plaintext of requests
  routed to you, and clients know that (§3.2). Don't log it; the testnet
  corpus is synthetic anyway.
- **Keep your tunnel alive** if you use one — when your beacon goes stale
  (> 6 s) the network simply stops routing to you until it returns.
