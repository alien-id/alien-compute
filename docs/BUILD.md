# Building & running Alien Fleet

## Prerequisites

| What | Version used | Notes |
|---|---|---|
| Go | ≥ 1.22 (tested 1.25) | pure-Go daemons, no CGO |
| CUDA toolkit | 12.x / 13.x | for local GPU backends (providers only) |
| gcc | a CUDA-compatible host compiler (e.g. `gcc-14`) | providers only |
| cmake, git, curl | any recent | |
| NVIDIA driver | matching the toolkit | `nvidia-smi` must work |

Clients (`fleet-proxy`) need **only Go** — or just grab a prebuilt binary; they
do no inference and need no GPU.

## 1. Go binaries

```bash
make build          # bin/fleetd, bin/fleet-proxy
make dist           # cross-compiled client binaries in bin/dist/
make test vet       # unit tests (wire protocol) + lint
```

`fleetd` is one binary with three roles (whitepaper AX1):

```
fleetd chain    — Phase-0 testnet chain-sim (registry, PaymentHub, faucet, settlement)
fleetd gateway  — stateless PoP: ciphertext relay, chain API forwarding, peer gossip, dashboard
fleetd node     — Class O compute provider (llama.cpp or vLLM backend, GPU-pinned)
```

`fleet-proxy` is the client: a local OpenAI-compatible proxy holding the
session key and payment channel.

## 2. Inference backend (providers only)

```bash
make llama                      # clones + builds llama.cpp with CUDA
make llama CUDA_ARCH=90         # H100; 86 = RTX 30xx, 89 = RTX 40xx (default)
make models                     # downloads the three Gemma 4 Q4_K_M bundles (~27 GB)
```

Any GGUF works — drop it in `models/` and point a node config at it. The
bundle hash (SHA-256 of the artifact) is computed automatically and is the
model's on-chain identity; aliases are bound first-come (TOFU, AX5).

Providers can also use a vLLM OpenAI-compatible backend by installing `vllm`
on the node image and setting `"runtime": "vllm"` in the node config. For
local model directories the bundle hash is computed over the directory
contents. For remote model IDs such as `zai-org/GLM-5.2-FP8`, Fleet hashes the
symbolic ID; prefer a local snapshot path when you need exact artifact identity.

## 3. Node configuration

One JSON file per provider (see `demo/node-a.json`):

```jsonc
{
  "name": "node-a",
  "gateway": "http://127.0.0.1:9000",   // comma-separated bootstrap entrypoints
  "listen": "127.0.0.1:9101",           // node ingress (gateway relays to this)
  "public_addr": "http://127.0.0.1:9101", // how gateways reach us (tunnel endpoint when NAT'd)
  "data_dir": "demo/run/node-a",        // NIK seed (node identity) lives here
  "runtime": "llama",                    // default; use "vllm" for vLLM
  "llama_bin": "third_party/llama.cpp/build/bin/llama-server",
  "llama_port": 9201,
  "llama_args": ["--jinja"],             // optional llama-server flags
  "gpus": "0",                          // CUDA_VISIBLE_DEVICES pinning; "1,2" = 2-GPU layer split
  "profile": "cuda-consumer",           // §5.2 hardware profile
  "n_parallel": 2,                      // concurrent slots
  "nek_rotate": "1h",                   // HPKE key rotation (§6.2: ≤ 24h)
  "model": {
    "path": "models/gemma-4-12B-it-Q4_K_M.gguf",
    "aliases": ["gemma4", "gemma-4-12b-it"],
    "ctx_len": 16384,
    "tip_in_ktok": 1000000,             // provider tip, nano-FLEET per kTok in
    "tip_out_ktok": 5000000             //                                  out
  }
}
```

vLLM example:

```jsonc
{
  "name": "glm52-vllm",
  "gateway": "http://127.0.0.1:9000",
  "listen": "127.0.0.1:9104",
  "public_addr": "http://127.0.0.1:9104",
  "data_dir": "demo/run/glm52-vllm",
  "runtime": "vllm",
  "vllm_bin": "vllm",
  "vllm_port": 9204,
  "vllm_startup_timeout": "45m",
  "vllm_args": [
    "--kv-cache-dtype", "fp8",
    "--reasoning-parser", "glm45",
    "--tool-call-parser", "glm47",
    "--enable-auto-tool-choice"
  ],
  "gpus": "0,1,2,3",
  "profile": "cuda-consumer",
  "n_parallel": 2,
  "model": {
    "path": "zai-org/GLM-5.2-FP8",
    "name": "GLM-5.2-FP8",
    "aliases": ["glm-5.2", "glm5.2"],
    "ctx_len": 1048576,
    "tip_in_ktok": 2000000,
    "tip_out_ktok": 10000000
  }
}
```

## 4. Local testnet (one machine)

```bash
FRESH=1 ./demo/testnet.sh    # chain + 2 gateways + 3 GPU-pinned nodes
./demo/demo.sh               # client demo: faucet, channel, all models, streaming, report
./demo/report.sh             # provider-earnings / burn report any time
```

Ports: chain 9500, gateways 9000/9001, nodes 9101–9103, llama-servers 9201–9203,
proxy 8080. All state under `demo/run/` (wipe with `FRESH=1`).

## 5. Chain / gateway / provider / client, by hand

```bash
# chain-sim (the testnet stand-in for the L2)
fleetd chain -listen 127.0.0.1:9500 -state ledger.json -shadow 5s -dev-faucet -node-bond 0.05

# any number of gateways, peered via gossip
fleetd gateway -name gw-1 -listen 0.0.0.0:9000 -chain http://127.0.0.1:9500 \
               -public-url http://<host>:9000 -peers http://<other-gw>:9001

# provider
fleetd node -config my-node.json

# client (any machine; one entrypoint is enough — discovery finds the rest)
fleet-proxy -gateway http://<any-gateway>:9000
curl http://127.0.0.1:8080/v1/chat/completions -d '{"model":"gemma4","messages":[...]}'
```

The gateway serves the live network dashboard at `/` and the network APIs
under `/v1/` (registry, capacity, ledger, peers, relay).

## 6. Identity-gated faucet (optional)

With [alien-id](https://github.com/alien-id/agent-id) bound on the client
machine, pass the CLI so the faucet claim is authenticated per verified
owner (RFC 9449 DPoP; one trial credit per Agent ID nullifier):

```bash
fleet-proxy -gateway ... -agent-id-cli ~/agent-id/plugins/agent-id-auth/bin/cli.mjs
```

Without it, `-dev-faucet` on the chain allows anonymous Phase-0 claims.

## Troubleshooting

- `llama-server: error while loading shared libraries` — the node sets
  `LD_LIBRARY_PATH` to the llama.cpp build dir automatically; if running
  llama-server by hand, export it yourself.
- Node stuck in SHADOW — it serves probe-only until the `-shadow` window
  passes (5 s testnet default), then beacons promote it to ACTIVE.
- `all gateways unreachable` — check `curl <gateway>/healthz`, then
  `/v1/net/peers` to see what the network knows about itself.
