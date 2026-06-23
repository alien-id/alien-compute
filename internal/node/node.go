// Package node implements the Fleet Class O serving node (whitepaper §8):
// permissionless join, NIK/NEK keys, local inference backend, HPKE
// request handling, receipt signing, voucher-gated payment.
package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"alien.org/fleet/internal/gateway"
	"alien.org/fleet/internal/ledger"
	"alien.org/fleet/internal/llama"
	"alien.org/fleet/internal/vllm"
	"alien.org/fleet/internal/wire"
)

type ModelConfig struct {
	Path       string   `json:"path"`
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases"`
	CtxLen     int      `json:"ctx_len"`
	TipInKTok  uint64   `json:"tip_in_ktok"`  // nano-FLEET per kTok input
	TipOutKTok uint64   `json:"tip_out_ktok"` // nano-FLEET per kTok output
}

type Config struct {
	Name       string      `json:"name"`
	Gateway    string      `json:"gateway"`
	Listen     string      `json:"listen"`      // node ingress, relayed by the gateway
	PublicAddr string      `json:"public_addr"` // how the gateway reaches us (default http://<listen>)
	DataDir    string      `json:"data_dir"`
	Runtime    string      `json:"runtime"` // "llama" (default) or "vllm"
	LlamaBin   string      `json:"llama_bin"`
	LlamaPort  int         `json:"llama_port"`
	LlamaArgs  []string    `json:"llama_args"`
	VLLMBin    string      `json:"vllm_bin"`
	VLLMPort   int         `json:"vllm_port"`
	VLLMArgs   []string    `json:"vllm_args"`
	GPUs       string      `json:"gpus"` // CUDA_VISIBLE_DEVICES pinning
	Profile    string      `json:"profile"`
	NParallel  int         `json:"n_parallel"`
	Model      ModelConfig `json:"model"`
	NEKRotate  string      `json:"nek_rotate"` // e.g. "1h"
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Profile == "" {
		c.Profile = "cuda-consumer"
	}
	if c.PublicAddr == "" {
		c.PublicAddr = "http://" + c.Listen
	}
	return &c, nil
}

type inferenceServer interface {
	Start() error
	Stop()
	BaseURL() string
	Pid() int
	FetchMeta() (*llama.Meta, error)
}

type chanCacheEntry struct {
	ch *ledger.Channel
	at time.Time
}

type Node struct {
	cfg Config

	nik    ed25519.PrivateKey
	nikPub ed25519.PublicKey

	nekMu   sync.Mutex
	nek     *wire.NEK
	prevNEK *wire.NEK

	runtime    inferenceServer
	runtimeTyp string
	bundleHash []byte
	modelMeta  gateway.ModelMeta
	params     ledger.Params
	tip        ledger.TipRate

	inflight atomic.Int64

	chanMu    sync.Mutex
	chanCache map[string]chanCacheEntry
	nonceMu   sync.Mutex
	seenNonce map[string]map[uint64]bool // channel id -> nonces seen locally (exact-replay guard; double-redeem is enforced at settlement)

	// Gateway pool: cfg.Gateway is a comma-separated bootstrap list; peer
	// exchange (/v1/net/peers) fills the rest. Requests fail over.
	gwMu  sync.Mutex
	gws   []string
	gwIdx int

	hc *http.Client
}

func New(cfg Config) *Node {
	n := &Node{
		cfg:       cfg,
		chanCache: map[string]chanCacheEntry{},
		seenNonce: map[string]map[uint64]bool{},
		hc:        &http.Client{Timeout: 10 * time.Second},
	}
	for _, g := range strings.Split(cfg.Gateway, ",") {
		if g = strings.TrimSpace(strings.TrimRight(g, "/")); g != "" {
			n.gws = append(n.gws, g)
		}
	}
	return n
}

// discoverGateways merges /v1/net/peers from known gateways (bootstrap +
// gossip pattern: one live entrypoint is enough to find the rest).
func (n *Node) discoverGateways() {
	n.gwMu.Lock()
	known := append([]string(nil), n.gws...)
	n.gwMu.Unlock()
	for _, g := range known {
		resp, err := n.hc.Get(g + "/v1/net/peers")
		if err != nil {
			continue
		}
		var doc struct {
			Peers []struct {
				URL string `json:"url"`
			} `json:"peers"`
		}
		err = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		if err != nil {
			continue
		}
		n.gwMu.Lock()
		for _, p := range doc.Peers {
			u := strings.TrimRight(p.URL, "/")
			found := false
			for _, e := range n.gws {
				if e == u {
					found = true
					break
				}
			}
			if !found && u != "" {
				n.gws = append(n.gws, u)
				log.Printf("[%s] discovered gateway %s via peer exchange", n.cfg.Name, u)
			}
		}
		n.gwMu.Unlock()
	}
}

// gwBases returns the pool ordered current-first for failover.
func (n *Node) gwBases() []string {
	n.gwMu.Lock()
	defer n.gwMu.Unlock()
	out := make([]string, 0, len(n.gws))
	for i := 0; i < len(n.gws); i++ {
		out = append(out, n.gws[(n.gwIdx+i)%len(n.gws)])
	}
	return out
}

func (n *Node) pinGateway(base string) {
	n.gwMu.Lock()
	defer n.gwMu.Unlock()
	for i, g := range n.gws {
		if g == base {
			if i != n.gwIdx {
				log.Printf("[%s] failing over to gateway %s", n.cfg.Name, base)
			}
			n.gwIdx = i
			return
		}
	}
}

// loadNIK loads or generates the node identity key (§6.2). Persisted so a
// node keeps its identity (and earned reputation, AX3) across restarts.
func (n *Node) loadNIK() error {
	if err := os.MkdirAll(n.cfg.DataDir, 0o755); err != nil {
		return err
	}
	seedPath := filepath.Join(n.cfg.DataDir, "nik.seed")
	seed, err := os.ReadFile(seedPath)
	if err != nil || len(seed) != ed25519.SeedSize {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return err
		}
		if err := os.WriteFile(seedPath, seed, 0o600); err != nil {
			return err
		}
	}
	n.nik = ed25519.NewKeyFromSeed(seed)
	n.nikPub = n.nik.Public().(ed25519.PublicKey)
	return nil
}

func (n *Node) runtimeType() string {
	if n.cfg.Runtime == "" {
		return "llama"
	}
	return strings.ToLower(n.cfg.Runtime)
}

func (n *Node) modelName() string {
	if n.cfg.Model.Name != "" {
		return n.cfg.Model.Name
	}
	if len(n.cfg.Model.Aliases) > 0 {
		return n.cfg.Model.Aliases[0]
	}
	base := filepath.Base(n.cfg.Model.Path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func (n *Node) rotateNEK() error {
	nek, err := wire.GenerateNEK()
	if err != nil {
		return err
	}
	n.nekMu.Lock()
	n.prevNEK = n.nek
	n.nek = nek
	n.nekMu.Unlock()
	return nil
}

func (n *Node) currentNEKs() (cur, prev *wire.NEK) {
	n.nekMu.Lock()
	defer n.nekMu.Unlock()
	return n.nek, n.prevNEK
}

// gwGet / gwPostJSON walk the gateway pool with failover: a connection
// error advances to the next gateway; an HTTP-level error is final (it
// means the gateway answered).
func (n *Node) gwGet(path string, out any) error {
	var lastErr error
	for _, base := range n.gwBases() {
		resp, err := n.hc.Get(base + path)
		if err != nil {
			lastErr = err
			continue
		}
		n.pinGateway(base)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			var e struct{ Error string }
			_ = json.NewDecoder(resp.Body).Decode(&e)
			return fmt.Errorf("gateway %s: %d %s", path, resp.StatusCode, e.Error)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return fmt.Errorf("all gateways unreachable: %w", lastErr)
}

func (n *Node) gwPostJSON(path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var lastErr error
	for _, base := range n.gwBases() {
		resp, err := n.hc.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			lastErr = err
			continue
		}
		n.pinGateway(base)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			var e struct{ Error string }
			_ = json.NewDecoder(resp.Body).Decode(&e)
			return fmt.Errorf("gateway %s: %d %s", path, resp.StatusCode, e.Error)
		}
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	}
	return fmt.Errorf("all gateways unreachable: %w", lastErr)
}

func (n *Node) register() error {
	nek, _ := n.currentNEKs()
	msg := gateway.RegisterMsg{
		NIKPub:  n.nikPub,
		NEKPub:  nek.PubBytes(),
		Addr:    n.cfg.PublicAddr,
		Profile: n.cfg.Profile,
		GPUs:    n.cfg.GPUs,
		TPSCap:  0,
		Slots:   n.cfg.NParallel,
		Models: []gateway.ModelOffer{{
			BundleHash: hex.EncodeToString(n.bundleHash),
			Name:       n.modelName(),
			Aliases:    n.cfg.Model.Aliases,
			CtxLen:     n.cfg.Model.CtxLen,
			Meta:       n.modelMeta,
			Tip:        n.tip,
		}},
		TSms: uint64(time.Now().UnixMilli()),
	}
	msg.Sig = ed25519.Sign(n.nik, msg.SigningBytes())
	var ni gateway.NodeInfo
	if err := n.gwPostJSON("/v1/registry/node", &msg, &ni); err != nil {
		return err
	}
	log.Printf("[%s] registered with gateway: status=%s nik=%s…", n.cfg.Name, ni.Status, ni.NIK[:12])
	return nil
}

func (n *Node) beaconLoop() {
	for {
		inflight := int(n.inflight.Load())
		msg := gateway.BeaconMsg{
			NIKPub:      n.nikPub,
			QueueTokens: uint64(inflight) * 512,
			Inflight:    inflight,
			Accepting:   inflight < n.cfg.NParallel*2+2,
			TSms:        uint64(time.Now().UnixMilli()),
		}
		msg.Sig = ed25519.Sign(n.nik, msg.SigningBytes())
		if err := n.gwPostJSON("/v1/registry/beacon", &msg, nil); err != nil {
			// Gateway restarted or registry lost us — rejoin permissionlessly.
			if strings.Contains(err.Error(), "unknown node") {
				if rerr := n.register(); rerr != nil {
					log.Printf("[%s] re-register failed: %v", n.cfg.Name, rerr)
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func (n *Node) nekRotateLoop() {
	period := time.Hour
	if n.cfg.NEKRotate != "" {
		if d, err := time.ParseDuration(n.cfg.NEKRotate); err == nil {
			period = d
		}
	}
	for {
		time.Sleep(period)
		if err := n.rotateNEK(); err != nil {
			log.Printf("[%s] NEK rotation failed: %v", n.cfg.Name, err)
			continue
		}
		if err := n.register(); err != nil {
			log.Printf("[%s] re-registration failed: %v", n.cfg.Name, err)
		} else {
			log.Printf("[%s] NEK rotated + re-registered (prev key drains 10m)", n.cfg.Name)
		}
		// Erase the previous key after the drain overlap (§6.3).
		go func() {
			time.Sleep(10 * time.Minute)
			n.nekMu.Lock()
			n.prevNEK = nil
			n.nekMu.Unlock()
		}()
	}
}

// channelState mirrors chain state ≤ epoch-stale (§12.3; testnet: 10 s).
func (n *Node) channelState(chanID string) (*ledger.Channel, error) {
	n.chanMu.Lock()
	if e, ok := n.chanCache[chanID]; ok && time.Since(e.at) < 10*time.Second {
		n.chanMu.Unlock()
		return e.ch, nil
	}
	n.chanMu.Unlock()
	var ch ledger.Channel
	if err := n.gwGet("/v1/ledger/channel/"+chanID, &ch); err != nil {
		return nil, err
	}
	n.chanMu.Lock()
	n.chanCache[chanID] = chanCacheEntry{ch: &ch, at: time.Now()}
	n.chanMu.Unlock()
	return &ch, nil
}

func (n *Node) Run() error {
	if err := n.loadNIK(); err != nil {
		return fmt.Errorf("nik: %w", err)
	}
	if err := n.rotateNEK(); err != nil {
		return fmt.Errorf("nek: %w", err)
	}
	log.Printf("[%s] NIK %s… (GPUs [%s], profile %s)", n.cfg.Name,
		hex.EncodeToString(n.nikPub)[:12], n.cfg.GPUs, n.cfg.Profile)

	log.Printf("[%s] hashing model bundle %s …", n.cfg.Name, filepath.Base(n.cfg.Model.Path))
	h, err := llama.BundleHash(n.cfg.Model.Path)
	if err != nil {
		return fmt.Errorf("bundle hash: %w", err)
	}
	n.bundleHash = h
	n.tip = ledger.TipRate{InPerKTok: n.cfg.Model.TipInKTok, OutPerKTok: n.cfg.Model.TipOutKTok}
	log.Printf("[%s] bundle %s = %s…", n.cfg.Name, filepath.Base(n.cfg.Model.Path), hex.EncodeToString(h)[:16])

	// Genesis pricing parameters from the chain-sim.
	if err := n.gwGet("/v1/ledger/params", &n.params); err != nil {
		return fmt.Errorf("fetch params: %w", err)
	}

	if n.cfg.NParallel == 0 {
		n.cfg.NParallel = 2
	}
	n.runtimeTyp = n.runtimeType()
	switch n.runtimeTyp {
	case "llama":
		n.runtime = llama.New(llama.Config{
			Bin: n.cfg.LlamaBin, ModelPath: n.cfg.Model.Path, Port: n.cfg.LlamaPort,
			CtxLen: n.cfg.Model.CtxLen, GPUs: n.cfg.GPUs, NParallel: n.cfg.NParallel,
			ExtraArgs: n.cfg.LlamaArgs,
			LogPath:   filepath.Join(n.cfg.DataDir, "llama-server.log"),
		})
	case "vllm":
		n.runtime = vllm.New(vllm.Config{
			Bin: n.cfg.VLLMBin, ModelPath: n.cfg.Model.Path, ModelName: n.modelName(),
			Port: n.cfg.VLLMPort, CtxLen: n.cfg.Model.CtxLen, GPUs: n.cfg.GPUs,
			ExtraArgs: n.cfg.VLLMArgs,
			LogPath:   filepath.Join(n.cfg.DataDir, "vllm.log"),
		})
	default:
		return fmt.Errorf("unsupported runtime %q (expected llama or vllm)", n.cfg.Runtime)
	}
	log.Printf("[%s] starting %s backend (CUDA_VISIBLE_DEVICES=%s) …", n.cfg.Name, n.runtimeTyp, n.cfg.GPUs)
	if err := n.runtime.Start(); err != nil {
		return err
	}
	defer n.runtime.Stop()
	log.Printf("[%s] %s backend ready (pid %d) on %s", n.cfg.Name, n.runtimeTyp, n.runtime.Pid(), n.runtime.BaseURL())

	// Exact-artifact metadata for the on-chain ModelRegistry (AX5).
	if meta, err := n.runtime.FetchMeta(); err == nil {
		n.modelMeta = gateway.ModelMeta{
			NParams: meta.NParams, SizeBytes: meta.SizeBytes,
			NCtxTrain: meta.NCtxTrain, NEmbd: meta.NEmbd,
			Quant: meta.Quant, Runtime: meta.Runtime,
		}
		log.Printf("[%s] model metadata: %.1fB params, %s, ctx_train=%d, %s",
			n.cfg.Name, float64(meta.NParams)/1e9, meta.Quant, meta.NCtxTrain, meta.Runtime)
	} else {
		log.Printf("[%s] model metadata unavailable: %v", n.cfg.Name, err)
	}

	// Permissionless join (§8.1): register, then beacon.
	for i := 0; ; i++ {
		if err := n.register(); err != nil {
			if i > 30 {
				return fmt.Errorf("register: %w", err)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
	go n.beaconLoop()
	go n.nekRotateLoop()
	n.discoverGateways()
	go func() {
		for {
			time.Sleep(time.Minute)
			n.discoverGateways()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/infer", n.handleInfer)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "node": n.cfg.Name, "nik": hex.EncodeToString(n.nikPub),
			"bundle": hex.EncodeToString(n.bundleHash), "gpus": n.cfg.GPUs,
		})
	})
	log.Printf("[%s] serving on %s", n.cfg.Name, n.cfg.Listen)
	return http.ListenAndServe(n.cfg.Listen, mux)
}
