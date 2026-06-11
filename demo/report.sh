#!/usr/bin/env bash
# Final economic report: provider earnings, burns, network stats.
set -euo pipefail
python3 - <<'EOF'
import json
from urllib.request import urlopen
d = json.load(urlopen('http://127.0.0.1:9000/v1/ledger/stats'))
cap = json.load(urlopen('http://127.0.0.1:9000/v1/registry/capacity'))

# map NIK -> model/gpu info from capacity
nodeinfo = {}
for m in cap['models']:
    for n in m['nodes']:
        nodeinfo[n['nik']] = (m['name'], n['gpus'], n['measured_tps'], n['measured_ttft_ms'])

print()
print("PROVIDER EARNINGS (compute providers are paid in testnet FLEET)")
print("-" * 78)
total_tips = 0
for nik, p in sorted(d.get('providers', {}).items(), key=lambda kv: -kv[1]['tips_earned_nano']):
    name, gpus, tps, ttft = nodeinfo.get(nik, ('?', '?', 0, 0))
    total_tips += p['tips_earned_nano']
    print(f" node {nik[:12]}…  [{name}, GPU {gpus}]")
    print(f"   receipts served : {p['receipts']:4d}   tokens in/out: {p['in_tokens']}/{p['out_tokens']}")
    print(f"   TIPS EARNED     : {p['tips_earned']:>22s}")
    print(f"     bond (escrow) : {p['bond_held']:>22s}  (refundable, §8.1)")
    print(f"     spendable     : {p['spendable']:>22s}")
    print(f"   measured perf   : {tps:.0f} tok/s, TTFT {ttft:.0f} ms")
    print()
print("NETWORK")
print("-" * 78)
print(f" receipts settled  : {d['receipts']}  (dual-signed: {d['dual_signed']})")
print(f" minted (faucet)   : {d['minted']}")
print(f" burned (base fees): {d['burned']}   <- value destroyed per E1, anti-wash")
fmt = lambda nano: f"{nano//10**9}.{nano%10**9:09d} FLEET"
print(f" provider tips     : {fmt(total_tips)}   <- value paid to compute providers")
print(f" open channels     : {d['channels']}")
EOF
