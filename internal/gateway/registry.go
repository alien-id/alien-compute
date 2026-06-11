package gateway

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"alien.org/fleet/internal/ledger"
	"alien.org/fleet/internal/wire"
)

// ModelMeta is the exact-artifact metadata extracted from the inference
// runtime (GGUF header via llama-server) and registered on-chain (AX5:
// humans use aliases, the registry speaks exact artifacts).
type ModelMeta struct {
	NParams   uint64 `json:"n_params,omitempty"`    // parameter count
	SizeBytes uint64 `json:"size_bytes,omitempty"`  // artifact size
	NCtxTrain int    `json:"n_ctx_train,omitempty"` // training context length
	NEmbd     int    `json:"n_embd,omitempty"`
	Quant     string `json:"quant,omitempty"`   // e.g. Q4_K_M
	Runtime   string `json:"runtime,omitempty"` // e.g. llama.cpp b1-fdc3db9
}

// ModelOffer is one content-addressed bundle served by a node (§8.2).
type ModelOffer struct {
	BundleHash string         `json:"bundle_hash"` // hex SHA-256 of the GGUF artifact
	Name       string         `json:"name"`        // exact artifact name, e.g. gemma-4-12B-it-Q4_K_M
	Aliases    []string       `json:"aliases"`     // governed aliases (AX5)
	CtxLen     int            `json:"ctx_len"`     // served context length
	Meta       ModelMeta      `json:"meta"`
	Tip        ledger.TipRate `json:"tip"` // provider-advertised rate (§12.2)
}

// NodeStatus per §8.1 / §14.
type NodeStatus string

const (
	StatusShadow NodeStatus = "SHADOW"
	StatusActive NodeStatus = "ACTIVE"
)

type NodeInfo struct {
	NIK        string       `json:"nik"` // hex Ed25519 pub — node identity
	NEKPub     []byte       `json:"nek_pub"`
	PrevNEKPub []byte       `json:"prev_nek_pub,omitempty"` // 10-min drain overlap (§6.3)
	Addr       string       `json:"addr"`                   // node ingress (gateway-relayed)
	Profile    string       `json:"profile"`                // §5.2, e.g. cuda-consumer
	GPUs       string       `json:"gpus"`                   // CUDA_VISIBLE_DEVICES pinning
	TPSCap     float64      `json:"tps_cap"`
	Slots      int          `json:"slots"` // concurrent request slots
	Models     []ModelOffer `json:"models"`
	Status     NodeStatus   `json:"status"`
	JoinedAt   int64        `json:"joined_at"`
	Score      float64      `json:"score"` // S (§9.1); MVP: passive plane only

	// Measured performance (from settled receipt timings; §11.2 quality plane).
	TPSEwma    float64 `json:"tps_ewma"`     // tokens/s, generation
	TTFTEwmaMs float64 `json:"ttft_ewma_ms"` // time-to-first-token
	Served     uint64  `json:"served"`       // settled receipts

	// Advisory load plane (§11.2 beacons).
	LastBeacon  time.Time `json:"-"`
	Accepting   bool      `json:"accepting"`
	QueueTokens uint64    `json:"queue_tokens"`
	Inflight    int       `json:"inflight"`
}

type Registry struct {
	mu          sync.Mutex
	nodes       map[string]*NodeInfo
	aliases     map[string]string // alias -> bundle hash (first registration wins, AX5 TOFU)
	shadowFor   time.Duration
	beaconStale time.Duration
}

func NewRegistry(shadowFor time.Duration) *Registry {
	return &Registry{
		nodes:       map[string]*NodeInfo{},
		aliases:     map[string]string{},
		shadowFor:   shadowFor,
		beaconStale: 6 * time.Second, // §11.2
	}
}

// RegisterMsg is the permissionless join request, signed by the NIK.
type RegisterMsg struct {
	NIKPub  []byte       `json:"nik_pub"`
	NEKPub  []byte       `json:"nek_pub"`
	Addr    string       `json:"addr"`
	Profile string       `json:"profile"`
	GPUs    string       `json:"gpus"`
	TPSCap  float64      `json:"tps_cap"`
	Slots   int          `json:"slots"`
	Models  []ModelOffer `json:"models"`
	TSms    uint64       `json:"ts_ms"`
	Sig     []byte       `json:"sig"` // Ed25519(NIK, "FLEET/v1/register" ‖ canonical fields)
}

func (m *RegisterMsg) SigningBytes() []byte {
	c := *m
	c.Sig = nil
	return append([]byte(wire.DomainReg), wire.MustMarshal(&c)...)
}

func (r *Registry) Register(m *RegisterMsg) (*NodeInfo, error) {
	if len(m.NIKPub) != ed25519.PublicKeySize {
		return nil, errors.New("bad NIK")
	}
	if !ed25519.Verify(m.NIKPub, m.SigningBytes(), m.Sig) {
		return nil, errors.New("bad registration signature")
	}
	if len(m.Models) == 0 {
		return nil, errors.New("no models offered")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	nik := hex.EncodeToString(m.NIKPub)

	// Alias TOFU (AX5): an alias binds to the first bundle hash that claims
	// it; rebinding to a different artifact is refused.
	for _, mo := range m.Models {
		for _, al := range mo.Aliases {
			if existing, ok := r.aliases[al]; ok && existing != mo.BundleHash {
				return nil, fmt.Errorf("alias %q already bound to bundle %s", al, existing[:16])
			}
		}
	}
	for _, mo := range m.Models {
		for _, al := range mo.Aliases {
			r.aliases[al] = mo.BundleHash
		}
	}

	n, exists := r.nodes[nik]
	if !exists {
		n = &NodeInfo{NIK: nik, JoinedAt: time.Now().Unix(), Status: StatusShadow, Score: 0.5}
		r.nodes[nik] = n
	} else if len(n.NEKPub) > 0 && hex.EncodeToString(n.NEKPub) != hex.EncodeToString(m.NEKPub) {
		n.PrevNEKPub = n.NEKPub // re-attestation: 10-min key overlap for drain (§6.3)
	}
	n.NEKPub = m.NEKPub
	n.Addr = m.Addr
	n.Profile = m.Profile
	n.GPUs = m.GPUs
	n.TPSCap = m.TPSCap
	n.Slots = m.Slots
	n.Models = m.Models
	n.LastBeacon = time.Now()
	n.Accepting = true
	return n, nil
}

// BeaconMsg is the 2-second advisory load beacon (§11.2), signed by the NIK.
type BeaconMsg struct {
	NIKPub      []byte `json:"nik_pub"`
	QueueTokens uint64 `json:"queue_tokens"`
	Inflight    int    `json:"inflight"`
	Accepting   bool   `json:"accepting"`
	TSms        uint64 `json:"ts_ms"`
	Sig         []byte `json:"sig"`
}

func (m *BeaconMsg) SigningBytes() []byte {
	c := *m
	c.Sig = nil
	return append([]byte(wire.DomainBeacon), wire.MustMarshal(&c)...)
}

func (r *Registry) Beacon(m *BeaconMsg) error {
	if len(m.NIKPub) != ed25519.PublicKeySize || !ed25519.Verify(m.NIKPub, m.SigningBytes(), m.Sig) {
		return errors.New("bad beacon signature")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[hex.EncodeToString(m.NIKPub)]
	if !ok {
		return errors.New("unknown node")
	}
	n.LastBeacon = time.Now()
	n.QueueTokens = m.QueueTokens
	n.Inflight = m.Inflight
	n.Accepting = m.Accepting
	// SHADOW -> ACTIVE promotion after the (testnet-shortened) shadow window.
	if n.Status == StatusShadow && time.Since(time.Unix(n.JoinedAt, 0)) >= r.shadowFor {
		n.Status = StatusActive
	}
	return nil
}

// ResolveAlias maps a human alias to a bundle hash (AX5).
func (r *Registry) ResolveAlias(alias string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.aliases[alias]
	return h, ok
}

func (r *Registry) Aliases() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.aliases {
		out[k] = v
	}
	return out
}

// Candidates returns ACTIVE, fresh-beacon nodes serving the bundle.
func (r *Registry) Candidates(bundleHash string) []NodeInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []NodeInfo
	for _, n := range r.nodes {
		if n.Status != StatusActive || !n.Accepting {
			continue
		}
		if time.Since(n.LastBeacon) > r.beaconStale {
			continue // stale beacon => not accepting (§11.2)
		}
		for _, mo := range n.Models {
			if mo.BundleHash == bundleHash {
				out = append(out, *n)
				break
			}
		}
	}
	return out
}

func (r *Registry) Node(nik string) (*NodeInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[nik]
	if !ok {
		return nil, false
	}
	c := *n
	return &c, true
}

func (r *Registry) All() []NodeInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []NodeInfo
	for _, n := range r.nodes {
		out = append(out, *n)
	}
	return out
}

// ObserveResult feeds the passive measurement plane (§11.2): a simple
// ok-rate/TTFT EWMA stands in for the probe-issuer score until probes ship.
func (r *Registry) ObserveResult(nik string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, found := r.nodes[nik]
	if !found {
		return
	}
	target := 0.0
	if ok {
		target = 1.0
	}
	const alpha = 0.1
	n.Score = n.Score*(1-alpha) + target*alpha
}

// ObservePerf folds settled receipt timings into the node's measured
// throughput/latency (the quality plane of §11.2, derived from receipts —
// the protocol's atomic facts — rather than self-reports).
func (r *Registry) ObservePerf(nik string, ttftMs, tps float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, found := r.nodes[nik]
	if !found {
		return
	}
	const alpha = 0.3
	if ttftMs > 0 {
		if n.TTFTEwmaMs == 0 {
			n.TTFTEwmaMs = ttftMs
		} else {
			n.TTFTEwmaMs = n.TTFTEwmaMs*(1-alpha) + ttftMs*alpha
		}
	}
	if tps > 0 {
		if n.TPSEwma == 0 {
			n.TPSEwma = tps
		} else {
			n.TPSEwma = n.TPSEwma*(1-alpha) + tps*alpha
		}
	}
	n.Served++
}

// NodeCapacity is one node's contribution to a bundle's capacity.
type NodeCapacity struct {
	NIK        string         `json:"nik"`
	Status     NodeStatus     `json:"status"`
	Accepting  bool           `json:"accepting"`
	Profile    string         `json:"profile"`
	GPUs       string         `json:"gpus"`
	Slots      int            `json:"slots"`
	Inflight   int            `json:"inflight"`
	FreeSlots  int            `json:"free_slots"`
	CtxLen     int            `json:"ctx_len"`
	Score      float64        `json:"score"`
	TPSEwma    float64        `json:"measured_tps"`
	TTFTEwmaMs float64        `json:"measured_ttft_ms"`
	Served     uint64         `json:"served_receipts"`
	Tip        ledger.TipRate `json:"tip"`
}

// BundleCapacity answers "what can the network serve, and how fast" for
// one exact model artifact.
type BundleCapacity struct {
	BundleHash string         `json:"bundle_hash"`
	Name       string         `json:"name"` // exact artifact name
	Aliases    []string       `json:"aliases"`
	Meta       ModelMeta      `json:"meta"`
	Nodes      []NodeCapacity `json:"nodes"`
	NodeCount  int            `json:"node_count"`
	TotalSlots int            `json:"total_slots"`
	FreeSlots  int            `json:"free_slots"`
	TPSTotal   float64        `json:"measured_tps_total"` // sum of accepting nodes' EWMA
}

// Capacity aggregates the live model/capacity view across the network.
func (r *Registry) Capacity() []BundleCapacity {
	r.mu.Lock()
	defer r.mu.Unlock()
	byBundle := map[string]*BundleCapacity{}
	aliasesFor := map[string][]string{}
	for alias, bundle := range r.aliases {
		aliasesFor[bundle] = append(aliasesFor[bundle], alias)
	}
	for _, n := range r.nodes {
		fresh := time.Since(n.LastBeacon) <= r.beaconStale
		for _, mo := range n.Models {
			bc := byBundle[mo.BundleHash]
			if bc == nil {
				bc = &BundleCapacity{
					BundleHash: mo.BundleHash,
					Name:       mo.Name,
					Aliases:    aliasesFor[mo.BundleHash],
					Meta:       mo.Meta,
				}
				byBundle[mo.BundleHash] = bc
			}
			free := n.Slots - n.Inflight
			if free < 0 {
				free = 0
			}
			accepting := n.Status == StatusActive && n.Accepting && fresh
			if !accepting {
				free = 0
			}
			bc.Nodes = append(bc.Nodes, NodeCapacity{
				NIK: n.NIK, Status: n.Status, Accepting: accepting,
				Profile: n.Profile, GPUs: n.GPUs,
				Slots: n.Slots, Inflight: n.Inflight, FreeSlots: free,
				CtxLen: mo.CtxLen, Score: n.Score,
				TPSEwma: n.TPSEwma, TTFTEwmaMs: n.TTFTEwmaMs, Served: n.Served,
				Tip: mo.Tip,
			})
			bc.NodeCount++
			bc.TotalSlots += n.Slots
			bc.FreeSlots += free
			if accepting {
				bc.TPSTotal += n.TPSEwma
			}
		}
	}
	out := make([]BundleCapacity, 0, len(byBundle))
	for _, bc := range byBundle {
		out = append(out, *bc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
