# Deploying the Fleet control plane (chain + gateways)

This documents the live testnet deployment (AWS eu-west-3, instance
`15.237.243.199`) and serves as the runbook for deploying elsewhere.

## Topology

```
                    AWS host (or any VPS)
   ┌──────────────────────────────────────────────────────┐
   │ fleet-chain        127.0.0.1:9500   (systemd)        │
   │ fleet-gateway@1    0.0.0.0:9000  ←─ peers ─┐         │
   │ fleet-gateway@2    0.0.0.0:9001  ──────────┘         │
   │ nginx: fleet.lethe.gg (TLS) → 127.0.0.1:9000         │
   │ 127.0.0.1:9101..9103  ← SSH reverse tunnels from the │
   └───────────────────────── GPU provider machine(s) ────┘
```

- The **chain-sim** binds localhost only — it is reached exclusively
  through gateways (which forward `/v1/registry/*` and `/v1/ledger/*`).
- **Gateways** are stateless; run any number, list each other in `-peers`
  (peer exchange handles the rest). Clients need any one URL.
- The **dashboard** is embedded in the gateway binary, served at `/`.

## 1. Build & ship

```bash
make dist
scp bin/dist/fleetd-linux-amd64 <host>:fleet/fleetd
scp bin/dist/fleet-proxy-*     <host>:fleet/dist/     # client downloads
```

## 2. systemd units

`/etc/systemd/system/fleet-chain.service`:

```ini
[Unit]
Description=Alien Fleet testnet chain-sim
After=network.target
[Service]
User=ec2-user
ExecStart=/home/ec2-user/fleet/fleetd chain -listen 127.0.0.1:9500 \
  -state /home/ec2-user/fleet/run/ledger.json -shadow 5s -dev-faucet -node-bond 0.05
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
```

`/etc/systemd/system/fleet-gateway@.service` (template; per-instance env
in `~/fleet/run/gateway-N.env` with `PORT=` and `PEERS=`):

```ini
[Unit]
Description=Alien Fleet gateway PoP %i
After=network.target fleet-chain.service
[Service]
User=ec2-user
EnvironmentFile=/home/ec2-user/fleet/run/gateway-%i.env
ExecStart=/home/ec2-user/fleet/fleetd gateway -name gw-aws-%i -listen 0.0.0.0:${PORT} \
  -chain http://127.0.0.1:9500 -public-url http://<PUBLIC-IP>:${PORT} -peers ${PEERS}
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now fleet-chain fleet-gateway@1 fleet-gateway@2
```

Open inbound TCP 9000–9001 (AWS: security-group ingress rule).

## 3. Dashboard domain + TLS

```bash
# DNS (Route53, zone lethe.gg): A fleet.lethe.gg -> <PUBLIC-IP>, TTL 60
# nginx vhost: proxy_pass http://127.0.0.1:9000 with X-Forwarded-* headers
sudo certbot --nginx -d fleet.lethe.gg --redirect
```

The `X-Forwarded-Host/Proto` headers matter: the chain's Agent ID (DPoP)
verifier reconstructs the `htu` the client signed from them.

## 4. Providers

GPU machines join from anywhere — see [PROVIDER.md](PROVIDER.md). For the
reference deployment the provider box runs `demo/aws/providers.sh`, which
keeps a persistent SSH reverse tunnel (`-R 9101..9103`) into this host so
gateways can relay to the NAT'd nodes via `127.0.0.1:910x`.

## 5. Ops

```bash
journalctl -u fleet-chain -f                  # settlement log (faucet, receipts, burns)
journalctl -u fleet-gateway@1 -f              # relay + gossip log
curl localhost:9000/v1/ledger/stats | jq      # economics
curl localhost:9000/v1/registry/capacity | jq # live capacity
sudo systemctl restart 'fleet-gateway@*'      # safe: gateways are stateless
```

Restarting the chain keeps the ledger (JSON snapshot) but clears the
in-memory registry — nodes re-register automatically within ~2 s (beacon
retry), and measured-perf EWMAs rebuild from the next settled receipts.

## Failure modes verified on the live deployment

- **Gateway loss**: stopping `fleet-gateway@1` mid-session — clients and
  all providers failed over to `gw-aws-2` (discovered via peer exchange)
  with zero dropped requests.
- **Chain restart**: ledger state survives; providers rejoin
  permissionlessly without operator action.
- **Tunnel drop**: the tunnel keeper reconnects in 3 s; while down, stale
  beacons (> 6 s) take the provider out of routing — no errors served.
