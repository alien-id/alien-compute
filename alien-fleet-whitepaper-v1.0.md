# Alien Fleet

**A confidential, self-verifying market for distributed LLM inference**

| | |
|---|---|
| Network | **Alien Fleet** (alien.org ecosystem) — compute unit **FLEET**, an MVP placeholder pending the token decision (§12.1) |
| Version | 1.0 (text of consolidated draft 1.0-rc2, network renamed) |
| Date | 2026-06-11 |
| Status | Implementation-ready; normative where RFC 2119 keywords appear |

---

## Abstract

Alien Fleet is a permissionless market where anyone can sell or buy LLM inference, with a hardware-enforced guarantee that **machine operators never observe plaintext prompts, completions, or licensed model weights**. The protocol's central idea is that one primitive — remote attestation of a measured confidential VM — simultaneously yields a *privacy proof* (encryption keys exist only inside the enclave), a *proof of useful work* (enclave-signed receipts drive settlement and token emission), and *unforgeable benchmarking* (paid audit traffic is indistinguishable from customer traffic, so measured latency and throughput cannot be gamed). Around this core, Alien Fleet adds an open tier for consumer hardware whose integrity is verified by sampled re-execution inside the attested tier; a latency-engineered access network with explicit routing mathematics; a prompt-cache layer that makes prefix reuse — the majority of real-world inference — cheap without making it visible; a burn-and-mint token economy that is wash-trading-proof by construction; and identity-gated subsidies (via Alien ID / Agent ID) that confine sybil resistance to the only places it is needed. Onboarding is a first-class, conformance-tested metric: first inference in 60 seconds for consumers, first earnings in 10 minutes for consumer-GPU providers, an attested datacenter pod in 45 minutes.

---

## Table of contents

1. [Introduction](#1-introduction)
2. [Design axioms](#2-design-axioms)
3. [System overview](#3-system-overview)
4. [Threat model & protocol invariants](#4-threat-model--protocol-invariants)
5. [Hardware foundation](#5-hardware-foundation)
6. [Attested execution: the Pod](#6-attested-execution-the-pod)
7. [Wire protocol & receipts](#7-wire-protocol--receipts)
8. [The open tier](#8-the-open-tier)
9. [Verification & measurement](#9-verification--measurement)
10. [Availability & commitments](#10-availability--commitments)
11. [Access network & latency engineering](#11-access-network--latency-engineering)
12. [Economics](#12-economics)
13. [Identity & sybil resistance](#13-identity--sybil-resistance)
14. [On-chain layer & governance](#14-on-chain-layer--governance)
15. [Lifecycles & onboarding SLOs](#15-lifecycles--onboarding-slos)
16. [Disputes & slashing](#16-disputes--slashing)
17. [Security considerations](#17-security-considerations)
18. [Reference implementation](#18-reference-implementation)
19. [Consolidated genesis parameters](#19-consolidated-genesis-parameters)
20. [Roadmap](#20-roadmap)

---

## 1. Introduction

Inference is becoming a commodity, but the market for it is not commodity-shaped: buyers must trust operators with their most sensitive data, operators must be trusted to run the model they advertise, and every marketplace that has tried to decentralize supply has rediscovered the same three frauds — peeking, shirking, and benchmark gaming. Existing answers each solve one corner: confidential-cloud offerings solve peeking but not open participation; optimistic re-execution networks solve shirking at high cost; subjective validator scoring solves nothing it cannot itself be gamed on.

Alien Fleet's thesis is that GPU-era trusted execution environments collapse all three problems into one. When a provider's only admissible runtime is a reproducibly-built confidential VM whose keys are born inside the enclave and bound into a hardware-signed quote:

- **Peeking is impossible** — clients encrypt end-to-end to a key the operator provably never holds (and licensed weights are encrypted to the same enclave, so model IP is protected from the host symmetrically).
- **Shirking is impossible** — the enclave signs a receipt binding the exact model artifact, input commitment, output commitment, token counts, and timestamps of every request; receipts are the only thing the protocol pays for or mints against.
- **Gaming is impossible** — because all traffic reaching a provider is ciphertext, protocol-issued audit requests are cryptographically indistinguishable from revenue. A provider that performs well for auditors has, by that very fact, performed well for everyone.

Everything else in this paper — the consumer-hardware tier, the routing mathematics, the token mechanics, the identity layer, the onboarding paths — is built to preserve those three properties while making the network cheap to join, fast to use, and honest to price.

## 2. Design axioms

Six axioms govern every mechanism below; implementations MUST NOT introduce parallel machinery where an axiom already covers the need.

**AX1 — One image, four roles.** Serving pod, verifier pod, arbiter pod, and batch-escrow pod are the *same* reproducible CVM image distinguished by a `mode` flag in its measured manifest. One build to audit, one measurement set to govern, one attestation path to maintain; idle serving capacity becomes verification capacity automatically.

**AX2 — One payment primitive.** Payment channels with signed vouchers are the only money pipe. Self-custody wallets, managed billing, sponsored trials, probe funding, and demand rebates are all the same channel opened by different funders under a declarative `SpendPolicy`.

**AX3 — One identity ladder.** A Fleet identity is a keypair, optionally + bond, optionally + attestation, optionally + a verified-human anchor (§13). Client, open-tier node, and attested pod are rungs of one ladder; reputation rides the identity upward, which is what lets reputation collateralize capital (§12.6).

**AX4 — One corpus.** Benchmark jobs, burn-in traffic, audit probes, and verification replays draw from a single governed corpus pipeline with one statistical-realism calibration loop.

**AX5 — Humans never type hashes.** Model artifacts are addressed by content hash everywhere the protocol speaks (envelopes, receipts, registry); humans use governed aliases that SDKs resolve once and pin (trust-on-first-use, hard warning on rebind).

**AX6 — Anonymous by default, identified only where subsidized.** Inference itself never requires identity: a self-funded channel and a session key are sufficient forever. Proof-of-personhood (§13) is demanded exactly where free value or per-actor caps create sybil incentives — trial credits, emission caps, auditor seats — and nowhere else.

## 3. System overview

### 3.1 Actors

| Actor | Role |
|---|---|
| **Client** | Encrypts requests end-to-end, pays via channel vouchers, countersigns receipts |
| **Provider / Pod (Class A)** | TEE-equipped operator running the attested CVM; cannot see plaintext |
| **Open node (Class O)** | Consumer-hardware operator; sees plaintext of jobs explicitly routed to it |
| **Gateway** | Regional PoP performing routing, relay, queueing, and passive measurement; sees ciphertext + metadata only |
| **Probe issuer** | Staked auditor sending indistinguishable paid traffic; one seat per verified human per region |
| **Model owner** | Registers open weights, or licenses encrypted weights servable only inside attested pods |
| **Chain (L2)** | Registry, attestation verification, stake, payments, fees, emissions, scores |
| **Identity attestor** | Issues proof-of-unique-personhood; genesis attestor: Alien (Alien ID / Agent ID, §13) |

### 3.2 Two assurance classes

| | **Class A (attested)** | **Class O (open)** |
|---|---|---|
| In-flight confidentiality | End-to-end HPKE; network sees ciphertext | **Identical** — end-to-end HPKE; network sees ciphertext |
| At-node confidentiality | Operator cannot see plaintext (TEE) | **Operator can see plaintext** |
| Result integrity | Hardware-attested execution + signed receipts | Signed receipts + sampled re-verification by Class A |
| Eligible hardware | TEE CPU + CC-mode GPU (§5) | Any GPU ≥ 8 GB, Apple Silicon, CPU-batch |
| Eligible models | Any, incl. licensed proprietary weights | Open-weights bundles only |
| Intended traffic | Anything, incl. confidential | Public, synthetic, low-sensitivity |

SDKs default to Class A; Class O requires explicit opt-in with the at-node-visibility caveat surfaced at configuration time. Registries are disjoint namespaces and gateways MUST NOT cross-route.

### 3.3 Architecture

```
 ┌──────────┐   E2E-encrypted (HPKE) request/response     ┌──────────────────────────┐
 │  Client   │◄───────────────────────────────────────────►│ Provider host (untrusted) │
 │ SDK/proxy │         ciphertext only ▲                   │  ┌────────────────────┐  │
 └────┬─────┘                          │                   │  │ CVM "Pod" (TEE)     │  │
      │ verify quotes,          ┌──────┴──────┐  cipher-   │  │ keys live here only │  │
      │ vouchers, receipts      │  Gateway PoP │  text     │  │ GPU in CC mode      │  │
      ▼                         │ route·relay· │──────────►│  │ signs receipts      │  │
 ┌──────────┐                   │ queue·measure│           │  └────────────────────┘  │
 │ Chain L2  │◄──────────────────┴──────┬──────┴───────────┴───────────┐
 │ registry, attestation, stake, channels, fees/burn, emissions, scores │
 └──────────┘◄──────── signed measurements ◄── Probe issuers ◄── Alien ID seats
                                  ▲
        Class O nodes (NAT'd) ────┘ persistent reverse tunnels; plaintext visible
                                    at the node only, by declared assurance class
```

## 4. Threat model & protocol invariants

### 4.1 Adversaries

**A1** malicious provider (root, hypervisor, physical access, RAM/PCIe snooping, full network control, probe-detection attempts) · **A2** malicious gateway (sees all ciphertext, metadata, timing; may censor/misroute) · **A3** malicious client (payment fraud, false disputes, DoS) · **A4** colluding probe issuers · **A5** network adversary (traffic analysis) · **A6** sybil actor (farming subsidies, evading caps, stuffing auditor seats).

Residual risks accepted and mitigated rather than eliminated (§17): compromise of CPU/GPU vendor attestation roots or a TEE microarchitectural break; compromise of the client's own device; global traffic-correlation beyond bucketing and pacing.

### 4.2 Invariants (normative)

- **I1** No plaintext request/response bytes exist outside the client process and TEE-protected memory (Class A) or the explicitly chosen serving node (Class O). GPU memory in Class A MUST be in confidential-computing mode.
- **I2** Enclave private keys are generated inside the Pod, are non-exportable, and are destroyed on shutdown or rotation.
- **I3** A Pod MUST NOT serve unless a fresh attestation (≤ 24 h) is registered on-chain.
- **I4** Every paid request yields a node-signed Receipt; settlement, scores, and emissions derive only from Receipts, Vouchers, and probe measurements.
- **I5** Probe traffic is indistinguishable from customer traffic: **cryptographically** in Class A (ciphertext + relayed origin + ordinary payment), **statistically** in Class O (organically-distributed plaintext corpus, rotating relays and channels). Dashboards and documentation MUST label the distinction.
- **I6** SDK defaults never silently downgrade assurance class, disable padding, or bypass quote verification; every relaxation is an explicit, logged client choice.
- **E1** (anti-wash, Class A) Emission value minted per unit of work is strictly less than base fee burned per unit (genesis ≤ 80 %), so self-dealing volume destroys value.
- **E2** (anti-wash, Class O) Emissions are volume-independent: paid on probe-measured capacity × honored availability, never on traffic counts.
- **X1** (custody split) Any party may hold payment float on a client's behalf; no party other than the client and TEEs may ever hold plaintext. Payment custody and confidentiality are independent by construction.

## 5. Hardware foundation

### 5.1 Class A platform matrix

Eligibility requires both rows and presence of the combination in the governed `PlatformAllowlist`:

| Component | Requirement |
|---|---|
| CPU | Intel Xeon 5th-gen+ (Emerald Rapids+) with **TDX**, or AMD EPYC 9xx4 (Genoa)+ with **SEV-SNP**; firmware/TCB ≥ governed minimum |
| GPU | NVIDIA Hopper (H100/H200, incl. NVL) in CC mode, or Blackwell (B200/B300, RTX PRO 6000 Blackwell SE) in TEE mode; Blackwell+ MAY run multi-GPU TEE (1/2/4/8, encrypted NVLink); VBIOS/driver pairs per NVIDIA's Secure AI compatibility matrix |
| Clock / network | NTP-disciplined host; ≥ 1 Gbps symmetric or relay mode |

Measured CC-mode overhead on transformer inference is ≈ 2–5 % with one-time attestation cost at provisioning — confidentiality is not a meaningful performance tax. Consumer GeForce parts lack CC mode and are Class O only. AMD Instinct and Intel accelerators join the allowlist when PCIe TDISP/TEE-I/O device attestation ships; the attestation interface is deliberately vendor-pluggable. Apple Silicon exposes no operator-excluding TEE and is therefore Class O only — where its unified memory makes it the cheapest 70B-4bit hardware on the network. Rented confidential cloud instances (Azure NCC-class, GCP confidential A3) are fully eligible: attestation chains to the silicon, not the renter, and the cloud operator is simply another A1 adversary excluded by I1–I2.

### 5.2 Class O hardware floor

| Profile | Hardware | Runtime | Serveable classes |
|---|---|---|---|
| `cuda-consumer` | GeForce/RTX ≥ 8 GB (3060+) | llama.cpp / vLLM-lite | nano–small; medium 4-bit on 16–24 GB |
| `rocm-consumer` | Radeon ≥ 16 GB | llama.cpp ROCm | nano–small |
| `apple-metal` | Apple Silicon ≥ 16 GB unified | MLX / llama.cpp-Metal | nano–medium; 70B-4bit on ≥ 64 GB |
| `cpu-batch` | ≥ 32 GB RAM, AVX2/SVE | llama.cpp | nano, embeddings, batch-only |

Floor: ≥ 50/20 Mbps, NTP sync, 64-bit OS. Profiles pin determinism behavior for verification (§9.2).

---

## 6. Attested execution: the Pod

### 6.1 Image (AX1)

One reproducibly-built CVM image, governance-published per release with source hash, build instructions, and expected measurements; anyone can rebuild bit-for-bit. Contents: minimal read-only guest OS (dm-verity, no shell/SSH/serial in production), `pod-agent` (attestation, keys, registration, metering, receipt signing), pinned inference runtime + CUDA stack, content-addressed model loader, optional policy module, and the `mode` flag selecting serve / verify / arbitrate / escrow. Logs exit only through a sanitizer (counters, latencies, error codes). No persistent plaintext storage, no debug paths, no key export.

### 6.2 Keys

| Key | Type | Where | Lifetime | Purpose |
|---|---|---|---|---|
| Operator key | secp256k1 | Provider wallet | long | stake, registration, payout |
| NIK | Ed25519 | inside Pod | pod lifetime | signs receipts, beacons, settlement batches |
| NEK | X25519 | inside Pod | ≤ 24 h | HPKE recipient key |
| Client session key | Ed25519 | client SDK | days | signs vouchers (self-custody mode), countersigns receipts |
| Channel funder key | per funder | funder | — | signs vouchers in delegated/virtual-channel mode (§12.3) |
| Epoch beacon | — | chain | 1 h | quote anti-replay nonce, probe VRF input |

Forward secrecy is bounded, not absolute: HPKE base mode means compromise of a NEK exposes ciphertexts recorded during that key's ≤ 24 h window; rotation plus in-enclave erasure (which, uniquely, is *real* erasure) caps the blast radius. Clients MAY require stricter `max_nek_age` in policy.

### 6.3 Boot, binding, registration

```
1. Host launches CVM (measured boot).
2. pod-agent verifies GPU CC mode; obtains SPDM GPU evidence; fails closed.
3. Generates NIK + NEK in enclave memory.
4. Fetches model bundles; verifies SHA-256 manifests against ModelRegistry;
   loads into encrypted GPU memory.
5. Requests platform quote with
   report_data = SHA-512("FLEET/v1/bind" ‖ NIK_pub ‖ NEK_pub ‖ SHA-256(Manifest))[0..64].
6. Host submits AttestationBundle{manifest, keys, cpu_evidence+chain,
   gpu_evidence+chain, epoch_beacon, platform_id} on-chain (no secrets inside).
7. Verification (§6.4) → status SHADOW for 24 h (probe-only traffic —
   indistinguishable to the Pod) → ACTIVE.
8. Re-attest every 24 h with fresh NEK + current epoch beacon; 10-min key
   overlap for drain, then erasure.
```

### 6.4 Attestation verification (normative checks)

1. CPU evidence chains to Intel/AMD root; TCB ≥ `TcbPolicy[platform]`; debug off.
2. `report_data` binding equation holds exactly.
3. Measurement equals the `ImageAllowlist` expectation for the manifest's image id.
4. GPU evidence chains to NVIDIA root; CC/TEE mode on; VBIOS+driver allowlisted; GPU nonce = SHA-256(NIK_pub ‖ epoch_beacon).
5. Beacon is current-or-previous epoch (≤ 2 h).
6. **Dual-root rule:** checks 1 and 4 MUST chain to two independent vendor roots. All v1 platforms satisfy this naturally (CPU vendor + NVIDIA); the explicit rule future-proofs single-vendor SoCs.

Verification is pluggable: v1 ships *optimistic* mode (staked verifier set posts results; 24 h fraud window; anyone may submit a full on-chain DCAP/SNP verification as a fraud proof), with zk-verified quotes on the roadmap.

### 6.5 Request handling

Voucher checked **before** decryption (DoS shield) → HPKE-open with current/previous NEK → bucket-size enforcement → inference → encrypted paced frames → signed Receipt as final frame → plaintext buffers erased. KV state persists and is reused only under the prompt-cache layer (§7.5): hints route, but the enclave reuses cached KV only after exact token-block equality is confirmed on the decrypted prefix.

## 7. Wire protocol & receipts

### 7.1 Transport & cryptography

Client ↔ gateway: HTTP/2 or QUIC; gateway ↔ node: pooled mTLS/gRPC (transport hygiene only — confidentiality never depends on it); host ↔ Pod: vsock. Privacy-sensitive clients front through OHTTP relays (RFC 9458); probe traffic always does.

HPKE profile (RFC 9180): DHKEM(X25519, HKDF-SHA256) / HKDF-SHA256 / ChaCha20-Poly1305, Base mode; `info = "FLEET/v1" ‖ model_bundle_hash ‖ bucket_id`. Response key = `Context.Export("FLEET/v1/resp", 32)`; frames sealed with sequence nonces.

### 7.2 Envelope (CBOR, deterministic)

```cbor-diag
Envelope = {
  1: 1,                  ; version
  2: bstr .size 16,      ; req_id
  3: bstr .size 32,      ; model_bundle_hash
  4: uint,               ; bucket_id
  5: bstr .size 32,      ; target NIK (pinning — gateways MUST honor)
  6: bstr .size 32,      ; hpke_enc
  7: bstr,               ; ct = HPKE.Seal(Payload)
  8: Voucher,            ; payment — attachable post-seal (delegated mode)
  9: CacheKey / null,   ; prompt-cache routing hints (§7.5); subsumes session affinity
 10: uint,               ; ttl_ms
 11: uint                ; max_price (atto-FLEET)
}
; AAD = SHA-256(canonical CBOR of fields 1–5, 9–11).
; The Voucher (8) is deliberately outside the AAD so a billing delegate can
; attach it after client-side sealing; payment integrity is restored by the
; Receipt, which binds the consumed voucher nonce. Swapping a voucher can
; only change who pays. Confidentiality is unaffected: sealing is always
; client-side (X1).

Payload = { 1: bstr OpenAI-JSON, 2: bstr client_session_pub,
            3: {paced: bool, frame_tokens: uint}, 4: pad-to-bucket }
```

### 7.3 Size & timing privacy

Input buckets: ≤ 1 / 4 / 16 / 64 / 200 KiTok, payload padded to the boundary. Output hidden by **paced streaming** (default on): fixed 16-token frames at a per-class cadence, random encrypted tail padding to output buckets {256, 1 Ki, 4 Ki, 16 Ki}. Realtime class defaults pacing off as an explicit latency-for-leakage trade (I6: surfaced, logged). Pricing always uses true token counts from the Receipt, never padded counts.

### 7.4 Receipt

```cbor-diag
Receipt = {
  1: req_id, 2: req_commit = SHA-256(Envelope),
  3: resp_commit = SHA-256(output plaintext), 4: model_bundle_hash,
  5: pod/node measurement-or-profile, 6: in_tokens, 7: out_tokens,
  8: t_accept, 9: t_first, 10: t_done,
 11: price_charged, 12: voucher_nonce, 13: client_session_pub,
 14: cached_in_tokens, 15: cache_writes }
ReceiptSig  = Ed25519(NIK, "FLEET/v1/receipt" ‖ SHA-256(Receipt))
DualReceipt = + Ed25519(client_session, "FLEET/v1/ack" ‖ SHA-256(Receipt))
```

Receipts are the protocol's atomic fact: settlement Merkle-batches them; emissions count them; scores ingest their timings; disputes replay them. Clients verify commitments, key binding, and `price ≤ max_price` before countersigning. Error codes a node may emit: `E_VOUCHER 402 · E_BUCKET 413 · E_MODEL 404 · E_TTL 408 · E_CAPACITY 429 · E_POLICY 451 · E_INTERNAL 500`; gateway-level `R_NO_NODE · R_PRICE · R_TIMEOUT`.

### 7.5 Prompt cache layer

In production traffic the majority of input tokens are re-prefills of prefixes the network has already processed — system prompts, agent scaffolds, multi-turn histories, shared RAG context. Alien Fleet makes that reuse first-class while keeping it inside the privacy model: **hints route, plaintext decides, enclaves hold.**

**Cache keys.** The client (the only plaintext holder) chunks the token prefix into 1 kTok blocks and computes a chained hint per block. Two scopes:

```
private (default):  hint_k = H( HKDF(client_session_key, "FLEET/cache") ‖ hint_{k−1} ‖ block_k )
shared  (opt-in):   hint_k = H( prefix_bundle_hash ‖ k )      ; Prefix Bundles only
```

Private-scope hints are meaningful only to the client that minted them: the gateway observes that *this client* is reusing *some* prefix — exactly the information the old affinity token already disclosed — and nothing about its content, and no other client can construct the same hint. Shared scope exists **only** for registry-published **Prefix Bundles** (content-addressed, explicitly published artifacts: open system prompts, tool schemas, public RAG chunks). Restricting shared caching to published content neutralizes cross-client cache-probing by construction — a timing oracle can only confirm that someone used a prefix that is public anyway. Organic user content MUST NOT be shared-scope cached.

**Hits are decided on plaintext, not hints.** Hints are routing metadata and are never trusted for correctness: after HPKE-open, the node reuses cached KV only where decrypted token blocks are byte-equal to the cached ones. A colliding or stale hint therefore degrades to a cache miss — cache poisoning is structurally impossible.

**Class A storage.** KV pages live in CC-mode GPU memory; overflow is sealed (AES-256-GCM under a pod-lifetime Pod Cache Key generated in-enclave, non-exportable) to host RAM/NVMe as opaque blobs and erased with the pod. NEK rotation does not flush the cache (the pod process persists across re-attestation); pod exit does. Class O nodes follow the identical wire protocol and billing rules with plain local storage — at-node visibility there is already the declared assurance class.

**Pricing.** Cached input tokens bill at **0.1×** (both the burned base-in component and the tip); cache writes are free (the first request paid full prefill); default retention is 10 min best-effort under LRU. Clients MAY buy a **lease** — retention in kTok·hours at the node's advertised rate, paid through the ordinary voucher, split 80 % tip / 20 % burn (it is a storage service), bounded by pod lifetime. Receipts carry `cached_in_tokens` and `cache_writes`; emissions weight cached tokens at 0.1× of the input weight so the anti-wash invariant E1 carries over unchanged.

**Honesty.** A Class A pod cannot misreport hits — the accounting is part of the measured image. Class O hit accounting is audited by repeat-prefix probes (§9.1): claiming a miss on a probe-proven warm prefix is slashable billing fraud (§16). Note the incentive geometry: under-reporting a hit overcharges the client (detectable, slashable); over-reporting one undercharges the node itself (self-punishing).

**Routing.** Beacons advertise warm shared Prefix Bundles; gateways additionally remember, per client, which node last matched a private hint chain. The §11.3 predictor consumes both: `Prefill()` is computed over *uncached* tokens, which simultaneously generalizes session affinity and prices migration correctly — moving a conversation is exactly the cost of re-prefilling its cached portion elsewhere. Verification (§9.2) is unaffected: verifier pods re-run full prefill regardless, and a receipt's cache fields never alter correctness checks.


## 8. The open tier

### 8.1 Onboarding & ladder

```bash
curl -fsSL https://fleet.alien.org/edge | sh
fleet-edge init      # detects hardware → profile; generates NIK-O/NEK-O;
                    # admission benchmark (TPS_cap per bundle);
                    # bond: 500 FLEET deposit OR earned-escrow (first 500
                    # of earnings held) — zero-capital entry
fleet-edge serve --models auto --commit "18:00-08:00 daily" --max-vram 20G
```

Ladder: `PROBATION` (first 1,000 receipts; verification rate ρ = 10 %; batch-only) → `STANDARD` (ρ = 3 %; interactive-eligible if availability permits) → `TRUSTED` (≥ 30 d, A ≥ 0.99, zero adverse verdicts; ρ = 1 %; realtime-eligible). Any fraud verdict → `BANNED` (key and bonded wallet).

### 8.2 Model serving

Every quantization is its own content-addressed bundle, so receipts bind the exact artifact. `ModelRegistry.tier_allowed ∈ {A, O, AO}`: proprietary owners set `A` (Class O could exfiltrate weights); `O`-eligible bundles must carry open licenses. Single-node classes: nano ≤ 4 B, small ≤ 14 B, medium ≤ 34 B quantized, large-q (70B-4bit on ≥ 48 GB), plus embed/rerank utility classes.

**Hot set:** gateways publish a demand-weighted bundle ranking per class each epoch; `--models auto` nodes pull the top entries fitting their VRAM, with anti-herding jitter (sample top-k ∝ demand/supply gap). Scarce-but-demanded bundles earn a scarcity multiplier (≤ 2.0) when probes confirm ≤ 5 servers exist.

**Shard groups (batch-only):** nodes may register layer-range shards of large bundles; gateways assemble contiguous pipelines. Client seals to the group leader; inter-stage activations are re-encrypted hop-to-hop (network-blind, but member nodes see activations — sharding does NOT raise assurance class). Each stage signs a timing-chain entry; the final receipt is cosigned by all members; payment splits by layer-share × stage time; the timing chain attributes stragglers individually. Consumer-WAN latency confines groups to deadline ≥ 10 min work; interactive sharding is roadmap.

**Distribution & warm-up:** bundles fetch via hash-verified swarm (≤ 8 peers + mirrors — untrusted peers are safe by construction) and nodes begin serving the smallest hot bundle the moment its admission benchmark passes (~90 s) while larger bundles continue downloading.

## 9. Verification & measurement

One section, two trust planes, three instruments — all drawing on the AX4 corpus.

### 9.1 Paid probes (the binding plane)

Staked issuers (≥ 100,000 FLEET; seat allocation §13.3) send paid requests through OHTTP relays from ordinary-looking channels: per Class A pod, jittered-Poisson rate max(1 % of traffic, 12/h), k = 3 issuers chosen per epoch by beacon-seeded VRF; per Class O node, liveness probes at mean 6/h during committed hours. Indistinguishability is cryptographic in Class A and statistical in Class O (I5) — and in Class O the residual gameability is bounded by economics: the probe SLA *is* the customer SLA, so selectively serving suspected probes means serving everyone.

Measurements (TTFT, TPS, ok-rate, receipt validity) aggregate per epoch as median-across-issuers; an issuer deviating > 3× MAD from medians in > 20 % of epochs over a week is flagged and challengeable (anti-A4). The resulting score

```
S_epoch = 0.35·norm(TPS) + 0.25·norm(1/TTFT) + 0.30·availability + 0.10·(1 − receipt_mismatch)
S = EWMA(S_epoch, half-life 24 h)
```

is public chain state and drives pricing tiers, routing weight (∝ S²/price), eligibility, and emissions inputs. Probes also audit cache billing: a **repeat-prefix probe** resends an identical prefix inside the retention window — a second receipt claiming `cached_in_tokens = 0` at full input price is on-chain billing-fraud evidence (§16). *This transparency is the pricing layer*: quality-adjusted compute priced on unforgeable numbers.

### 9.2 Teacher-forced re-verification (Class O integrity)

**Class A judges Class O.** For a ρ-sampled receipt, a verifier pod (AX1 `mode=verify`, consuming idle attested capacity) receives the transcript re-encrypted client-to-TEE and runs **one prefill pass** over `prompt ‖ claimed_output` on the exact bundle:

- `DET` profiles / greedy / seeded: exact token match for min(256, len) positions, ≥ 99 % thereafter.
- Sampled decode: per-position logprobs of claimed tokens vs published per-bundle thresholds (`mean ≥ τ_class`, no 32-token window < `τ_window`) — catches model substitution, quant downgrades, truncation, garbage.

Cost ≈ 5–15 % of original generation; at ρ = 3 % the network-level overhead is ≈ 0.5 %, funded by the 6 % verification tax (§12.4). A failure escalates to 2 independent re-verifications; 2-of-3 fail ⇒ fraud verdict (§16). **Sizing rule (normative):** detection probability × slashable value ≥ 20 × cheat gain per request; genesis numbers give ≈ 450× negative expected value. Clients wanting immediate assurance may instead buy `redundancy: r` (fan-out to r nodes, SDK compares, bill r×).

### 9.3 Arbiter pods (content-private disputes)

A client disputing a Class A receipt re-encrypts its transcript to an arbiter pod (AX1 `mode=arbitrate`, ≥ 3 operators); commitment checks and tolerance-banded re-execution happen inside the TEE; only a signed verdict exits. Frivolous-dispute bond: 50 FLEET.

---

## 10. Availability & commitments

Consumer hardware churns; the protocol prices *promised-then-honored* availability rather than punishing downtime.

- **Commitment windows:** a node posts a signed schedule of committed hours (1 h granularity, ≤ 7 d horizon, revisable penalty-free with ≥ 1 h notice). Uncommitted uptime earns tips but no availability credit or emissions.
- **Measurement:** liveness probes (§9.1) during committed hours yield `A` = in-SLA answer rate over a rolling 7 d; `streak` = consecutive days with A ≥ 0.97 and no unexcused no-show; late cancellations (< 1 h notice) score as 0.5 h of failure each. Shard-group failures attribute only to the straggler stage via the timing chain.
- **Reward shaping:** emissions multiplier `M = A²` (convex — 0.99→0.98×, 0.80→0.64×) × streak bonus `(1 + min(days,14)·0.035)` up to ×1.49. Fraud or reserved no-show resets the streak.
- **Reserved capacity:** clients pre-purchase committed throughput at a 20 % premium, escrowed. A reserved-window no-show is a sold promise broken: the node's escrow refunds the client at 2× — the *only* availability event that touches the bond. Ordinary downtime merely zeroes M and streak.
- **Interactive eligibility is itself a reward:** only STANDARD+ with A ≥ 0.95 receive interactive routing; realtime requires TRUSTED + regional. Everyone else serves the queue.

Class A pods are scored by the same machinery (their availability term feeds S); they have no commitment-window construct because datacenter uptime is table stakes and routing starvation is punishment enough.

## 11. Access network & latency engineering

### 11.1 Gateways & connection modes

Gateways are regional PoPs (genesis: 6 regions; federated set of ≥ 5 independent operators, 250,000 FLEET stake each; permissionless gateways on the roadmap) combining routing, relay, durable job queue, and passive measurement. They see ciphertext and metadata only and cannot forge traffic (no client keys). Class A pods expose public ingress (gateways hold pooled connections; clients MAY go `direct`, trading −5–25 ms for IP exposure to the provider); Class O nodes, being NAT'd, hold ≥ 2 persistent outbound QUIC tunnels to nearby gateways — relayed by construction.

Clients attach in four modes — all preserving X1 (sealing is always client-side):

| Mode | Runs | Holds wallet | For |
|---|---|---|---|
| Embedded SDK | in-process (Py/TS/Rust) | app | single services |
| **Local proxy** *(recommended)* | `fleet proxy` on localhost speaking plain OpenAI HTTP | daemon | teams — every app just changes `base_url`; shared warm pools |
| Managed-billing gateway | thin client SDK; gateway signs vouchers from its own channel | gateway (float only) | wallet-averse users |
| Browser/WASM | WebCrypto HPKE + WebTransport | browser session | web apps |

SDKs race pings to the 3 nearest gateways at startup, hold warm 0-RTT QUIC sessions to the best 2, re-race every 10 min.

### 11.2 Two signal planes

| Plane | Signals | Freshness | Trust | Drives |
|---|---|---|---|---|
| Quality (binding) | paid probes → S, TTFT/TPS distributions | ~1 h | high | pricing, emissions, eligibility |
| Load (advisory) | NIK-signed beacons (2 s: queue tokens, batch occupancy, KV free, per-bucket TTFT estimate, warm shared-prefix bundles, accepting), passive relay timing (per-node EWMA, 5 min half-life), transport pings (10 s RTT/loss) | 2–10 s | low | per-request routing only |

The advisory plane is **self-correcting**: under-report your queue → attract traffic → probe-measured TTFT degrades → score, price tier, and routing weight fall. The slow trusted plane disciplines the fast cheap one; no slashing logic required. Beacons older than 6 s ⇒ treated as not accepting.

### 11.3 Routing

```
TTFT̂_i  = RTT_net,i + Q̂_i + Prefill_i(uncached_tokens_i)   ; uncached_i from cache-hint
                                                             ; matches + warm-prefix beacons
Q̂_i     = 0.5·(queue_tokens_i / TPS_cap,i) + 0.5·EWMA(passive residual)_{i,bucket}
T̂_total = TTFT̂ + ô / TPŜ_i            (TPŜ = probe TPS de-rated by beacon occupancy)

U_i = w_lat·TTFT̂_i/T_SLO + w_price·price_i/p̄ + w_rel·(1 − S_i·A_i)
        realtime  .80/.05/.15  T_SLO 350 ms
        interactive .50/.25/.25  T_SLO 1.5 s
        batch     .05/.70/.25  deadline-driven
```

Hard filters first (tier, trust policy, max_price, bundle, class eligibility); then **power-of-two-choices**: sample d candidates without replacement ∝ S²/price (d = 2; 3 for realtime) and take argmin U. P2C, not argmin-over-all, is mandatory — it is the standard cure for stale-beacon herding. **Cache-aware stickiness (subsumes affinity):** session affinity is now just the private-scope cache hint chain (§7.5). Stay on the pod holding the longest matched prefix unless an alternative wins even after re-prefilling the cached portion (`TTFT̂_held > TTFT̂_alt + cached_tokens/PrefillRate_alt + 150 ms`); migration transparently replays, and the formula above already prices the move because Prefill() takes uncached tokens. **Hedging (realtime; opt-in interactive):** no first frame within min(1.5·TTFT̂, p95) ⇒ duplicate to next-best, first frame wins, loser gets a `CANCELLED` receipt worth 25 % of the input base fee (prefill compensation; settles, never mints). Hedges cap at 5 % of a channel's requests/epoch — beyond that, buy explicit redundancy at full price.

### 11.4 Service classes

| | realtime | interactive | batch |
|---|---|---|---|
| Contract | TTFT ≤ 350 ms p95, RTT geo-fence ≤ 50 ms | TTFT ≤ 1.5 s p95 | done by `deadline` (≥ 10 min) |
| Nodes | A-ACTIVE + O-TRUSTED (A ≥ 0.99), regional | STANDARD+ (A ≥ 0.95) | any, incl. shard groups & reservations |
| Pacing | default off (logged trade) | default on | n/a |
| Price × base | 1.5 | 1.0 | 0.6 (off-peak 0.45) |
| Queueing | reject-on-busy | ≤ 2 s buffer | durable JobQueue |

Probes are class-shaped: realtime eligibility is earned on realtime-shaped probes.

### 11.5 Batch & escrow pods

Deferred execution breaks naive HPKE (sealed-to-one-NEK envelopes cannot survive later scheduling or 24 h key rotation). **Batch Escrow Pods** (AX1 `mode=escrow`) fix it: the client seals the job to k = 2 escrow-pod NEKs and disconnects; the gateway's durable queue bins jobs by (bundle, deadline) into reserved windows and off-peak capacity (EDF within bins, price locked at submit); at dispatch the escrow pod opens the job *inside the TEE* and re-seals to the assigned executor's *current* NEK; the executor seals the result to the client-supplied `result_nek` (stored ≤ 72 h, webhook or poll). Gateway and network never see plaintext; a Class O executor sees exactly what it would have seen interactively. Escrow fee 0.02 FLEET/job; a missed deadline auto-retries once at priority, then refunds fully (plus the reserved-window 2× penalty if a reservation was sold).

### 11.6 Latency budget (Class A interactive, 4 k prompt, regional, local proxy)

| Step | p50 | p95 |
|---|---|---|
| app→proxy + seal + pad | 1 ms | 2.5 ms |
| proxy→gateway (warm QUIC) | 9 ms | 28 ms |
| admission + P2C (beacons in RAM) | 0.4 ms | 1 ms |
| gateway→pod (pooled) | 4 ms | 22 ms |
| voucher gate + HPKE open | 0.7 ms | 1.5 ms |
| queue | 25 ms | 160 ms |
| prefill 4 k (H200-class) | 80 ms | 210 ms |
| first frame return | 6 ms | 25 ms |
| **TTFT** | **≈ 126 ms** | **≈ 450 ms** |

Class O interactive (small models, residential): ≈ 300 ms p50 / 900 ms p95 — which is why realtime gates on TRUSTED+regional and batch is Class O's natural home. End-to-end plaintext locations under every mode, class, and tier: the two ends and TEEs. Nothing else, ever.

## 12. Economics

### 12.1 Token

**FLEET**, 18 decimals, genesis 1,000,000,000: 40 % emissions pool · 20 % treasury (timelocked) · 15 % contributors (4 y vest, 1 y cliff) · 15 % investors (3 y) · 10 % liquidity. Burn-and-mint equilibrium: base fees burn; emissions mint only against attested work and measured capacity. (Distribution and any sale require securities counsel; this paper specifies mechanics only.)

**Token status (MVP note).** The alien.org ecosystem already operates **ALN** (native token) and **wALN** (its Solana wrapper); the unit in which Fleet compute ultimately settles is deliberately undecided, and this paper uses **FLEET** as the MVP working unit. The mechanics are unit-agnostic by design — AX2 confines all value flow to channels and vouchers, so swapping the unit touches `PaymentHub` denominations and the `Emissions` contract and nothing else. Three integration paths stay open: **(a) standalone FLEET** exactly as specified, with ALN unused at the protocol layer; **(b) two-token** — ALN as the staking/governance asset and FLEET as a pure burn-and-mint compute credit minted by burning ALN, concentrating value accrual in ALN while compute prices stay denominated in work; **(c) direct ALN settlement** — channels, fees, burns, and emissions denominated in ALN itself. Under every option, wALN gives Solana-side wallets a native funding path into `PaymentHub`. Whichever path is chosen, invariants E1/E2 MUST be restated in the chosen unit before mainnet.

### 12.2 Price of a request

```
price = baseFee_in(class)·⌈in/1000⌉ + baseFee_out(class)·⌈out/1000⌉   → burned
      + tip  (provider-advertised rate, disciplined by routing weight)  → provider
      + royalty_bps(model)·tip                                          → model owner
      · cached input tokens bill at 0.1× (base_in burned and tip alike);
        cache writes are free; retention beyond 10 min is a lease:
        kTok·hours at the node's advertised rate (80 % tip / 20 % burn)
      + 6 % verification tax (Class O only)                             → verifier pods
      − 3 % verified rebate (Class O clients participating in §9.2)
```

Base fees update per epoch per class: `b ← b·(1 + 0.125·(u − 0.8)/0.8)`, clamped ±12.5 %, where u = served/advertised capacity from gateway telemetry. Class multipliers per §11.4. Class O fees settle 5–10× below Class A (no TEE capex, marginal electricity) — the verification tax flowing to Class A verifiers is the cross-tier value loop.

### 12.3 Channels & vouchers (AX2)

`PaymentHub.openFor(clientSessionPub, deposit, SpendPolicy{tierMask, expiry, refundToFunder, perEpochCap})` covers every funding pattern: self-custody, managed billing, **virtual channels** (a gateway's large on-chain channel backs instantly-issued client sub-ledgers — no chain tx on the critical path; float ≤ 200 FLEET/client; gateway collateral ≥ 110 % of issued float), **sponsored trials** (§13.2), probe faucets, and demand rebates. Vouchers `{channel_id, cum_amount, max_for_request, nonce, chain_epoch}` are signed by whichever key owns the channel; nodes verify against ≤ 1-epoch-stale mirrored state before decrypting; receipts bind the consumed nonce. Redemption batches every 6 h with a receipt Merkle root: tips out, base fees burned, royalties accrued. Channel close: 7-day challenge window.

### 12.4 Emissions

Weekly: `E(w) = 3,000,000 · 2^(−w/52)` FLEET (yearly half-life; the 40 % pool lasts ≥ 8 y). Split: **65 % Class A** (pro-rata by settled SIUs, where `SIU = out_kTok·W(class) + 0.25·in_kTok·W(class) + 0.025·cached_kTok·W(class)` (cached tokens earn 0.1× the input weight, matching their 0.1× burn so invariant E1 holds identically) with the weight table recalibrated quarterly from probe-measured FLOP-seconds) · **20 % Class O** (per E2: `TPS_cap × committed-hours × A² × streak × scarcity` — volume-independent) · **10 %** probe issuers + probe faucet · **5 %** gateways. Per-operator caps (5 % of the A pool, 2 % of the O pool) are enforced **per verified human, not per wallet** (§13.3). E1 binds the A pool: emission value per SIU ≤ 80 % of base fee burned per SIU, so wash trading strictly destroys value; E2 makes O-pool wash trading meaningless and sybil-splitting capacity-neutral.

### 12.5 Bootstrap phases

| Phase | Trigger | Supply | Demand |
|---|---|---|---|
| 0 Testnet | — | faucet | synthetic |
| 1 Genesis | mainnet | whitelisted pods; emissions on benchmark jobs = the probe corpus (burn-in and calibration are the same work, AX4) | closed beta, 100 % rebate |
| 2 Open | ≥ 50 pods, ≥ 3 platforms | permissionless | public SDK; 50 % rebate −5 pp/month |
| 3 Fee-dominant | 4-wk burn ≥ 50 % of emissions | governance halving accelerates | no rebates |

### 12.6 Delegated stake — reputation as collateral (AX3 payoff)

`StakeManager.delegate(operator, amount)`: token holders underwrite operators for a 0–25 % commission, slashed pro-rata, `totalDelegated ≤ 4× selfStake`. The underwriting signal is already on-chain: ladder standing, A-history, and settled volume ride the operator identity upward — reputation earned on a gaming GPU literally collateralizes entry to the attested tier. With earned-escrow at the bottom and delegation at the top, the graduation flywheel (consumer card → Class O earnings → delegated Class A pod) has no capital cliff anywhere.

### 12.7 What runs where

| Workload | Tier |
|---|---|
| Synthetic data, evals, distillation, embeddings of public corpora | O (batch) |
| Latency-tolerant agent steps on public data | O (interactive) |
| Anything touching PII, secrets, proprietary prompts | **A only** |
| Proprietary-weights models | **A only** (registry-enforced) |

---

## 13. Identity & sybil resistance

### 13.1 Principle (AX6)

Inference is anonymous: a session key plus a self-funded channel is a complete, permanent identity for buying compute, and a bond plus hardware is complete for selling it. Sybil resistance is required only where the protocol *gives value away or caps it per actor* — trial subsidies, per-operator emission caps, auditor seats. There, Alien Fleet consumes proof-of-unique-personhood from a pluggable attestor:

```solidity
interface IIdentityAttestor {
  // opaque, per-protocol pseudonymous identifier of a unique verified human;
  // carries no personal data; stable across that human's keys and agents
  function nullifierOf(bytes calldata attestorProof) external view returns (bytes32);
}
```

**Genesis attestor: Alien (alien.org).** Alien ID provides privacy-preserving proof of unique humanity (device-side verification; no permanent biometric storage, no government ID); **Agent ID** extends it to autonomous agents — an agent proves it acts on behalf of a verified human *without revealing which one*. Gateways verify Alien sessions / AgentHooks off-chain at issuance time and submit only the resulting `nullifier` on-chain. The attestor is replaceable by governance and multiple attestors MAY coexist; nothing else in the protocol changes.

### 13.2 Where identity is required

| Surface | Rule |
|---|---|
| **Treasury trial credits** | One 5-FLEET trial allowance per Alien ID nullifier (30 d, tierMask O + A-batch, refund-to-treasury). A human's Agent IDs draw from that human's single pool — agents onboard with the same one-command flow, sybil-free, which is precisely the agentic-economy case Agent ID exists for. Private sponsors (model owners, apps) MAY gate their own `openFor` budgets however they like; only treasury money REQUIRES the nullifier. |
| **Emission caps** | The 5 % (Class A) and 2 % (Class O) per-operator weekly caps bind on the nullifier behind the operator identity, closing the split-wallet evasion that per-address caps invite. Operators above the de-minimis emission share (> 0.5 % of a pool) MUST attach a nullifier; smaller operators may remain anonymous and simply inherit the de-minimis cap. |
| **Probe-issuer seats** | One issuer seat per nullifier per region — k = 3 median-of-issuers is only meaningful if the three are actually independent humans (anti-A4). |
| **Delegation underwriting (optional)** | An operator MAY anchor its AX3 reputation to its nullifier so history credibly survives key rotation and cannot be rented out; delegators can price the difference. |

### 13.3 What identity is never used for

Routing, pricing, serving, model access, dispute standing, or self-funded usage. No personal data, no Alien account identifiers, and no cross-application linkage ever touch the chain — only the per-protocol nullifier. A user who never wants identity simply never takes subsidies, never exceeds de-minimis emissions, and never audits: full functionality, zero identification.

## 14. On-chain layer & governance

App-specific optimistic rollup (OP Stack or Arbitrum Orbit) settling to Ethereum, gas in FLEET. Contracts: `ProviderRegistry` (A and O namespaces; status SHADOW/ACTIVE/JAILED/EXITING), `AttestationVerifier` (optimistic + fraud-proof path), `ImageAllowlist`/`PlatformAllowlist`/`TcbPolicy`, `RevocationList` (instant multisig + retroactive ratification — the only non-timelocked path), `ModelRegistry` (bundles, aliases, tier_allowed, royalty), `StakeManager` (stake, delegation, slashing, 7 d unbond), `PaymentHub` (openFor/topUp/redeem/close), `FeeController`, `ScoreOracle`, `Emissions`, `Beacon`, `IdentityRegistry` (nullifier bindings). Everything else upgrades behind a 7-day timelock. New images require ≥ 3 independent reproducible-build attestations + 7-day review; vendor TCB recoveries grant a 48 h patch-and-reattest grace window before jailing.

Abridged interfaces (full ABI in `specs/`):

```solidity
interface IProviderRegistry { function registerPod(bytes attBundle, uint256 stake) external;
  function reattest(bytes32 nik, bytes attBundle) external; function startExit(bytes32 nik) external; }
interface IPaymentHub { function openFor(bytes32 sessionPub, uint256 deposit, SpendPolicy p)
  external returns (bytes32); function redeem(bytes32 ch, uint256 cum, bytes32 receiptRoot,
  bytes sigFunder, bytes sigNik) external; function startClose(bytes32 ch) external; }
interface IStakeManager { function delegate(address op, uint256 amt) external;
  function setCommission(uint16 bps) external; }
```

## 15. Lifecycles & onboarding SLOs

Onboarding latency is a conformance metric measured by the same probe infrastructure as serving latency. Speed comes from logistics — distribution, capital structure, payment plumbing — never from thinning trust: the 24 h SHADOW probation, bonds-or-escrow, quote verification, and assurance defaults are not skippable on any path.

### 15.1 Consumer — first token ≤ 60 s p50

```bash
pip install alien-fleet && fleet init
# session key → gateway race → sponsored virtual channel (Alien ID scan via
# QR / Agent ID for agents; instant — no chain tx) → "5.00 FLEET trial credit"
fleet run "hello"            # ≈ T+40 s
fleet topup --card 20        # later: gateway market-buys FLEET — token is plumbing
fleet upgrade --self-custody # one tx; vouchers switch signer; nothing else changes
```

Defaults: Class A wherever the trial allows, pacing on, governance allowlist followed, OHTTP relay auto, alias TOFU-pinning on. Per-request: resolve → verify cached quote → seal padded payload → voucher → stream → verify & countersign receipt. `fleet usage` exports receipts — an auditable compute trail. Enterprises run their own gateway + relay and may pin self-rebuilt image measurements, reducing their trust base to: hardware vendors + code they compiled.

### 15.2 Class O provider — first earnings ≤ 10 min p50

One install command, one serve command (§8.1); swarm distribution + ladder warm-up put the node on the trust ladder and earning availability credit in ~90 s while large bundles download; earned-escrow means zero capital anywhere on the path.

### 15.3 Class A provider — attested pod ≤ 45 min p50 (+ 24 h SHADOW by design)

```bash
fleet cloud up --provider azure --sku NCC-H100v5 --region eastus2 \
              --models llama-3.3-70b --stake delegated
```

Provisions the confidential instance (provider-pluggable terraform), boots the allowlisted image, attests, registers. `--stake delegated` opens the operator to §12.6 underwriting — self-stake as low as 5,000 FLEET (4× cap binds), itself fundable from Class O earnings. Operations thereafter: automatic 24 h re-attestation (failure ⇒ self-jail + page), blue/green image upgrades with zero registry downtime, 6 h auto-settlement, sanitized-metrics observability (operators see load, never content), `startExit` + 7 d unbond covering the final dispute window.

## 16. Disputes & slashing

| Offense | Proof | Penalty |
|---|---|---|
| Forged/replayed attestation | fraud proof | 100 % stake; NIK + operator (nullifier) ban |
| Receipt fraud, Class A | arbiter-pod verdict | 10 % stake per incident + score reset |
| Fraud verdict, Class O (§9.2) | 2-of-3 verifier pods | bond + escrow 100 %; ban; window's clients auto-refunded from slash |
| Voucher double-redeem / nonce reuse | on-chain | 2× disputed amount |
| Cache billing fraud (full-price receipt on a probe-proven warm prefix) | repeat-prefix probe receipts | Class O: treated as fraud verdict (bond+escrow, ban); Class A: 10 % stake (indicates image tampering → escalates to attestation fraud review) |
| Revoked image/TCB past 48 h grace | chain state | JAILED; 5 % after 7 d |
| Reserved-window no-show | reservation record | 2× reserved fee from escrow |
| Probe-issuer measurement fraud | MAD-outlier + challenge | 25 % issuer stake; seat (nullifier) revoked |
| Frivolous dispute | arbiter verdict | 50 FLEET bond |
| Ordinary downtime / low score | probes | no slash — M and routing starvation only |

## 17. Security considerations

**Vendor roots & TEE breaks.** The Class A guarantee reduces to the open-source image (reproducible, audited) plus Intel/AMD + NVIDIA attestation integrity. Mitigations: dual-root requirement; instant revocation with 48 h TCB grace; 24 h NEK rotation bounding recorded-traffic exposure; and the structural property that a TEE break degrades *privacy, never funds* — payments require client-signed vouchers regardless.

**Side channels.** Size → buckets; output timing → paced streaming; kernel-timing → mandatory batch mixing (≥ 4 co-batched requests when load permits). Per-token power/thermal analysis by a physical host is acknowledged open research, not a claim.

**Metadata.** Gateways see (model, bucket, timing, IP); OHTTP relays remove IP linkage; probe traffic doubles as cover; maximalists self-host gateways.

**DoS & griefing.** Voucher-before-decrypt, per-channel rate limits, stake-gated registration, dispute bonds, hedge caps.

**Identity layer.** The attestor is a liveness/issuance dependency only — an attestor outage stalls *new subsidized onboarding*, never serving or settlement. Nullifiers are per-protocol pseudonyms; chain state carries no personal data; attestor replacement is a governance action (§13.1).

**Not protected.** The client's own device; content visibility to a Class O node the client explicitly chose; opted-into model-owner policy modules; legal process against plaintext holders — which, by construction, are only clients and TEEs.

## 18. Reference implementation

| Layer | Choice |
|---|---|
| CVM | Confidential Containers / Constellation-style measured guest; one mkosi reproducible build targeting TDX + SNP |
| Attestation | NVIDIA nvtrust + Local GPU Verifier (NRAS cross-check); Intel DCAP / AMD SNP tools; Automata-style on-chain verifier as fraud-proof path |
| Inference | vLLM (pinned) per NVIDIA CC matrix (Class A); llama.cpp / MLX / vLLM-lite (Class O) |
| Crypto | hpke-rs / go-hpke; libsodium Ed25519/X25519; ciborium deterministic CBOR |
| Chain | OP Stack rollup; Solidity 0.8.x; Foundry |
| Daemons | **Go**: `fleetd` single binary (`gateway` / `node` roles, AX1 applied to daemons); `fleet-proxy` single binary (local proxy mode of §11.1); prober |
| SDKs | Python + TypeScript (OpenAI-compatible) + local proxy + WASM |
| Identity | Alien ID / Agent ID via AgentHooks at gateways; `IIdentityAttestor` adapter |

```
fleet/ ├─ contracts/ ├─ pod/ ├─ fleetd/ ├─ edge/ ├─ gateway/ ├─ sdk/{python,ts,wasm}/
      ├─ prober/ ├─ verifier/ ├─ identity/ ├─ specs/ (schemas + test vectors) └─ infra/
```

Conformance = `specs/vectors/` (HPKE, envelope canonicalization, receipt signing, quote binding, teacher-forced verification per profile, capacity-hour accounting) + integration suite (shadow probing, settlement, dispute round-trip, onboarding SLOs).

## 19. Consolidated genesis parameters

| Domain | Parameter | Value |
|---|---|---|
| Time | epoch / settlement / emission period | 1 h / 6 h / 1 wk |
| Attestation | re-attest & NEK rotation / SHADOW / TCB grace | 24 h / 24 h / 48 h |
| Stakes & bonds | A pod (self-min w/ delegation) / O bond / issuer / gateway | 25 k (5 k) / 500 / 100 k / 250 k FLEET |
| Delegation | commission / cap | 0–25 % / ≤ 4× self-stake |
| Exit | unbond, channel close, dispute window / dispute bond | 7 d / 24 h, 50 FLEET |
| Privacy | in-buckets (kTok) / out-buckets (tok) / frame | 1·4·16·64·200 / 256·1 Ki·4 Ki·16 Ki / 16 tok |
| Cache | hint block / read multiplier / default TTL / lease split / shared scope | 1 kTok blocks / 0.1× / 10 min best-effort / 80 % tip·20 % burn / published Prefix Bundles only |
| Probes | Class A rate / O liveness / issuers per target | max(1 %, 12/h) / 6/h committed / k = 3 |
| Score | weights / EWMA / routing weight | .35·.25·.30·.10 / 24 h / S²/price |
| Verification | ρ probation·standard·trusted / tax / rebate / sizing rule | 10·3·1 % / 6 % / 3 % / q·S ≥ 20·g |
| Availability | multiplier / streak cap / reservation premium / no-show | A² / ×1.49 @14 d / +20 % / 2× |
| Fees | update clamp / target u / class × (rt·int·batch·off-peak) | ±12.5 %/epoch / 0.8 / 1.5·1.0·0.6·0.45 |
| Emissions | E₀ / half-life / split / caps per nullifier / E1 bound / scarcity cap | 3 M/wk / 1 y / 65·20·10·5 % / 5 % A, 2 % O / ≤ 80 % of burn / 2.0 |
| Routing | β / d (int·rt) / θ_move / hedge trigger·cap·compensation | 0.5 / 2·3 / 150 ms / min(1.5·TTFT̂, p95)·5 %·25 % in-fee |
| Signals | beacon / staleness / ping / passive EWMA | 2 s / 6 s / 10 s / 5 min |
| Batch | BEP k / retention / escrow fee / min deadline / stage deadline | 2 / 72 h / 0.02 FLEET / 10 min / 90 s |
| Access | gateway regions / SDK warm sessions / virtual-channel float / collateral | 6 / 2 / ≤ 200 FLEET / ≥ 110 % |
| Identity | trial per nullifier / de-minimis emission share / issuer seats | 5 FLEET·30 d / 0.5 % / 1 per nullifier·region |
| Onboarding SLOs | consumer TTFI / O TTE / A pod | ≤ 60 s p50 / ≤ 10 min p50 / ≤ 45 min p50 |

## 20. Roadmap

**v1.1** permissionless gateways · zk attestation verification · final compute-token decision (standalone FLEET vs ALN integration, §12.1). **v2** AMD Instinct / Intel accelerators via PCIe TDISP · ARM CCA edge (consumer devices graduating from Class O into attested Realms) · interactive shard groups · additional identity attestors · TEE-to-TEE sealed-KV migration (escrow-pod mechanics applied to very long contexts). **Research** per-token power side channels · PIR-style private node selection · MPC fallback tier.

---

## Appendix A — MVP reference implementation (testnet Phase 0)

A working Phase-0 testnet implementation accompanies this paper (Go, two
single binaries: `fleetd` with `chain` / `gateway` / `node` roles, and
`fleet-proxy`). It implements the §7 wire protocol faithfully (deterministic
CBOR envelopes, RFC 9180 HPKE with the §7.1 profile, vouchers, signed +
countersigned receipts, encrypted frame streams) against a llama.cpp
backend, with the following deliberate testnet deviations. Each is a
simplification of logistics, never of the trust model; where the deviation
exposed a spec bug, the spec text below is amended.

**A.1 Topology.** The on-chain layer (§14) is simulated by `fleetd chain` —
an in-process "chain-sim" holding the provider/model registries, payment
hub, faucet, and settlement, persisted as a JSON snapshot. It stands in for
the decentralized L2 rollup, which remains the mainnet target; its HTTP API
mirrors the contract interfaces so swapping in the rollup touches transport,
not logic. Gateways (`fleetd gateway`) are *stateless* PoPs: they relay
ciphertext, forward chain API calls, and feed passive measurements; any
number may run and nodes/clients may use any of them interchangeably — the
testnet launches two. No gateway holds funds, keys, or authoritative state.

**A.2 Assurance classes.** The testnet machines carry consumer GPUs (no CC
mode), so all nodes are Class O (`cuda-consumer` profile); the Class A
attestation path (§6) is not exercised. Receipts carry the §5.2 profile
string in the measurement field, as specified for Class O.

**A.3 Denomination.** Amounts are denominated in **nano-FLEET** (1e9 per
FLEET) rather than atto-FLEET: atto overflows uint64 above ~18.4 FLEET, and
the MVP deliberately avoids big-integer arithmetic in the hot path. §7.2's
`max_price` is nano-FLEET in the MVP. (Restating E1/E2 in the final unit is
already required by §12.1 before mainnet.)

**A.4 Size buckets.** Input buckets are byte-denominated
(4/16/64/256 KiB, 1 MiB padded payload) rather than kilotoken-denominated:
the client cannot count tokens without the model tokenizer. Output pacing
and tail padding are not yet implemented; frames stream as generated.
`AAD` is the SHA-256 of the deterministic-CBOR *array* of fields
1–5, 9–11 in field order.

**A.5 Payments.** Channel state is mirrored by nodes with ≤ 10 s staleness
(the §12.3 "≤ 1-epoch-stale" rule with the testnet epoch shortened);
settlement is per-receipt rather than 6-hourly Merkle batches. The
earned-escrow bond (§8.1) is enforced: the first 5 testnet-FLEET of tips
are held as the node's bond. Vouchers, nonce-replay rejection, base-fee
burn, tips, and client countersignatures (dual receipts) work as specified.

**A.6 Identity.** The faucet implements §13.2 with **Alien Agent ID** as
genesis attestor: an RFC 9449 DPoP + RFC 9068 `at+jwt` verifier (ported
from `agent-id/docs/INTEGRATION.md`) authenticates the claiming agent
against Alien SSO, and the trial credit is keyed on
`nullifier = SHA-256("FLEET/v1/nullifier" ‖ iss ‖ sub)` — one claim per
verified owner, no personal data on the ledger. JWKS discovery uses the
issuer's `/.well-known/openid-configuration` (`jwks_uri`), falling back to
`/.well-known/jwks.json`; §13.1 should be read accordingly. A `-dev-faucet`
flag additionally allows identityless claims, per the §12.5 Phase-0 row
("faucet / synthetic demand").

**A.7 Probation & scoring.** SHADOW lasts 5 s on the testnet (vs 24 h);
probe issuers are not yet implemented, so S is seeded at 0.5 and updated
only by the gateways' passive plane (ok-rate EWMA). Routing uses the §11.3
hard filters plus power-of-two-choices sampling ∝ S²/price with the load
beacon as tie-breaker; the full TTFT̂ predictor, hedging, and service
classes beyond `interactive` are future work.

**A.8 Deferred.** Prompt-cache layer (§7.5 — receipts carry the fields,
always 0), emissions (§12.4), shard groups, batch escrow pods, disputes
(§16), OHTTP relays. None of these affect the demonstrated invariants
I1/I2 (HPKE end-to-end, gateways see ciphertext only), I4 (settlement
strictly from receipts+vouchers), I6, and X1.

**A.9 Backend.** Inference runs on llama.cpp `llama-server` (pinned fresh
master, CUDA), one process per node, GPU-pinned via `CUDA_VISIBLE_DEVICES`
(multi-GPU nodes use layer-split). Bundle hashes are the SHA-256 of the
GGUF artifact, registered with human aliases under AX5 TOFU pinning
(rebinding refused). The testnet default alias `gemma4` resolves to the
Gemma 4 12B instruction-tuned Q4_K_M bundle; other nodes serve
heterogeneous bundles (Gemma 4 26B-A4B across two GPUs, Gemma 4 E4B).

**A.10 Model & capacity discovery.** The ModelRegistry entry for each
bundle carries the **exact artifact name** alongside its aliases, plus
metadata extracted from the artifact by the serving runtime (parameter
count, quantization, artifact size, training context, runtime version).
`GET /v1/registry/capacity` (forwarded by every gateway; surfaced by the
proxy as `/fleet/capacity` and folded into OpenAI `/v1/models`) answers
"what can the network serve right now": per bundle — metadata, serving
nodes with status/slots/in-flight load, and **measured** TPS/TTFT EWMAs
derived from settled receipt timings rather than self-reports, consistent
with the §11.2 rule that the trusted plane disciplines the advisory one.
A live network dashboard rendering this view ships embedded in the gateway
binary (served at `/`).

**A.11 Gateway discovery.** Gateways discover each other the way Ethereum
(discv5 bootnodes + signed ENRs) and Solana (entrypoints + gossiped
ContactInfo) do: each gateway publishes an Ed25519-signed **peer record**
(`/v1/net/peers`, `/v1/net/announce`) and runs a pull-based peer-exchange
loop with health probing. Clients and nodes treat their configured gateway
URL as a **bootstrap entrypoint only** — they pull the peer table, race
pings (§11.1), and fail over per request, so any single gateway (including
the entrypoint) can vanish without breaking the network. Verified on the
deployed testnet: stopping a gateway mid-session left clients and all
providers serving through the survivor. NAT'd providers reach gateways
outbound and are relayed inbound through persistent reverse tunnels (§11.1
"persistent outbound QUIC tunnels"; MVP: SSH).

---

*Alien Fleet whitepaper v1.0 — text of consolidated draft 1.0-rc2 (working name OPAL) with the network renamed; all amendments merged, terminology unified (gateway ⊃ router role), AAD/emission/cap definitions stated once and finally; the prompt-cache layer is §7.5. Appendix A documents the Phase-0 MVP reference implementation.*
