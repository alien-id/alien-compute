// Package gateway implements the regional PoP (whitepaper §11): registry,
// ciphertext relay, passive measurement — plus, for the Phase-0 testnet,
// the embedded chain-sim (ledger) and the identity-gated faucet.
package gateway

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"alien.org/fleet/internal/identity"
	"alien.org/fleet/internal/ledger"
	"alien.org/fleet/internal/wire"
)

// Server hosts one or both planes:
//
//   - the CHAIN plane (Phase-0 chain-sim standing in for the §14 L2 rollup):
//     ProviderRegistry, ModelRegistry, PaymentHub, faucet — the network's
//     decentralized source of truth, simulated as one process on testnet;
//   - the GATEWAY plane (§11 PoP): ciphertext relay + passive measurement.
//     Gateways are stateless, interchangeable, and any number may run; they
//     read/write chain state through the chain API.
type Server struct {
	Registry   *Registry
	Ledger     *ledger.Ledger
	Identity   identity.Verifier
	DevFaucet  bool   // Phase-0: allow faucet claims without an Agent ID
	ChainURL   string // gateway plane: upstream chain endpoint ("" = embedded)
	EmbedRelay bool   // single-process convenience: chain + relay together
	Name       string
	Peers      *PeerTable // gateway discovery (bootstrap + gossip)

	relayClient *http.Client
}

func NewChainServer(reg *Registry, led *ledger.Ledger, idv identity.Verifier, devFaucet bool) *Server {
	return &Server{
		Registry: reg, Ledger: led, Identity: idv, DevFaucet: devFaucet, Name: "fleet-chain",
		relayClient: &http.Client{Timeout: 0},
	}
}

func NewGatewayServer(name, chainURL, publicURL string, bootstrapPeers []string) *Server {
	s := &Server{Name: name, ChainURL: chainURL, relayClient: &http.Client{Timeout: 0}}
	s.Peers = NewPeerTable(name, publicURL, bootstrapPeers)
	go s.Peers.Gossip()
	return s
}

// ChainRoutes serves the chain-sim API (registry + ledger contracts).
func (s *Server) ChainRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/registry/node", s.handleRegister)
	mux.HandleFunc("POST /v1/registry/beacon", s.handleBeacon)
	mux.HandleFunc("POST /v1/registry/observe", s.handleObserve)
	mux.HandleFunc("GET /v1/registry/nodes", s.handleCandidates)
	mux.HandleFunc("GET /v1/registry/node/{nik}", s.handleNodeGet)
	mux.HandleFunc("GET /v1/registry/models", s.handleModels)
	mux.HandleFunc("GET /v1/registry/capacity", s.handleCapacity)
	mux.HandleFunc("GET /v1/registry/all", s.handleAll)
	mux.HandleFunc("POST /v1/ledger/faucet", s.handleFaucet)
	mux.HandleFunc("POST /v1/ledger/channel/open", s.handleChannelOpen)
	mux.HandleFunc("GET /v1/ledger/channel/{id}", s.handleChannelGet)
	mux.HandleFunc("GET /v1/ledger/account/{pub}", s.handleAccount)
	mux.HandleFunc("POST /v1/ledger/settle", s.handleSettle)
	mux.HandleFunc("POST /v1/ledger/ack", s.handleAck)
	mux.HandleFunc("GET /v1/ledger/stats", s.handleStats)
	mux.HandleFunc("GET /v1/ledger/params", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, s.Ledger.Params)
	})
}

// GatewayRoutes serves the PoP plane: relay + chain API forwarding +
// peer discovery (bootstrap/gossip à la discv5 bootnodes).
func (s *Server) GatewayRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/relay", s.handleRelay)
	if s.ChainURL != "" {
		mux.HandleFunc("/v1/registry/", s.forwardToChain)
		mux.HandleFunc("/v1/ledger/", s.forwardToChain)
	}
	if s.Peers != nil {
		mux.HandleFunc("GET /v1/net/peers", s.handlePeers)
		mux.HandleFunc("POST /v1/net/announce", s.handleAnnounce)
	}
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	if s.Ledger != nil && s.ChainURL == "" {
		s.ChainRoutes(mux) // chain plane (or embedded single-process mode)
		if s.EmbedRelay {
			mux.HandleFunc("POST /v1/relay", s.handleRelay)
		}
	} else {
		s.GatewayRoutes(mux) // pure gateway PoP
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "service": s.Name, "chain": s.ChainURL})
	})
	return mux
}

// forwardToChain proxies chain API calls through this gateway so clients
// and nodes need only a gateway URL. X-Forwarded-* lets the chain's
// identity verifier reconstruct the htu the agent actually signed.
func (s *Server) forwardToChain(w http.ResponseWriter, r *http.Request) {
	url := strings.TrimRight(s.ChainURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		writeErr(w, 500, "forward")
		return
	}
	req.Header = r.Header.Clone()
	req.Header.Set("X-Forwarded-Host", r.Host)
	req.Header.Set("X-Forwarded-Proto", "http")
	resp, err := s.relayClient.Do(req)
	if err != nil {
		writeErr(w, 502, "chain unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) handleNodeGet(w http.ResponseWriter, r *http.Request) {
	n, ok := s.Registry.Node(r.PathValue("nik"))
	if !ok {
		writeErr(w, 404, "unknown node")
		return
	}
	writeJSON(w, 200, n)
}

// ObserveMsg feeds the passive measurement plane from gateways.
type ObserveMsg struct {
	NIK string `json:"nik"`
	OK  bool   `json:"ok"`
}

func (s *Server) handleObserve(w http.ResponseWriter, r *http.Request) {
	var m ObserveMsg
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	s.Registry.ObserveResult(m.NIK, m.OK)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// --- registry -------------------------------------------------------------

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var m RegisterMsg
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	n, err := s.Registry.Register(&m)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("registry: node %s… joined permissionlessly (profile=%s gpus=[%s] models=%d) status=%s",
		n.NIK[:12], n.Profile, n.GPUs, len(n.Models), n.Status)
	writeJSON(w, 200, n)
}

func (s *Server) handleBeacon(w http.ResponseWriter, r *http.Request) {
	var m BeaconMsg
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.Registry.Beacon(&m); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	bundle := r.URL.Query().Get("bundle")
	if alias := r.URL.Query().Get("alias"); alias != "" {
		h, ok := s.Registry.ResolveAlias(alias)
		if !ok {
			writeErr(w, 404, wire.EModel+": unknown alias "+alias)
			return
		}
		bundle = h
	}
	if bundle == "" {
		writeErr(w, 400, "bundle or alias required")
		return
	}
	writeJSON(w, 200, map[string]any{"bundle": bundle, "nodes": s.Registry.Candidates(bundle)})
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Alias  string `json:"alias"`
		Bundle string `json:"bundle_hash"`
		Nodes  int    `json:"nodes"`
	}
	var out []entry
	for alias, bundle := range s.Registry.Aliases() {
		out = append(out, entry{alias, bundle, len(s.Registry.Candidates(bundle))})
	}
	writeJSON(w, 200, map[string]any{"models": out})
}

func (s *Server) handleAll(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"nodes": s.Registry.All()})
}

// handleCapacity answers "what models can the network serve right now,
// with what metadata, capacity, and measured performance".
func (s *Server) handleCapacity(w http.ResponseWriter, _ *http.Request) {
	caps := s.Registry.Capacity()
	totalFree := 0
	for _, c := range caps {
		totalFree += c.FreeSlots
	}
	writeJSON(w, 200, map[string]any{
		"models": caps,
		"network": map[string]any{
			"models":     len(caps),
			"free_slots": totalFree,
			"pricing": map[string]any{
				"base_in_per_ktok":  s.Ledger.Params.BaseFeeInKTok,
				"base_out_per_ktok": s.Ledger.Params.BaseFeeOutKTok,
				"unit":              "nano-FLEET (burned); tips per node in `models[].nodes[].tip`",
			},
		},
	})
}

// --- ledger / chain-sim ----------------------------------------------------

func (s *Server) handleFaucet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountPub string `json:"account_pub"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.AccountPub) != 64 {
		writeErr(w, 400, "account_pub (hex ed25519) required")
		return
	}
	nullifier := ""
	idKind := "dev (Phase-0 faucet, no identity)"
	if r.Header.Get("Authorization") != "" || r.Header.Get("DPoP") != "" {
		res, err := s.Identity.Verify(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token", error_description="`+err.Error()+`"`)
			writeErr(w, 401, "identity verification failed: "+err.Error())
			return
		}
		nullifier = res.Nullifier
		idKind = fmt.Sprintf("Agent ID (owner sub=%s… jkt=%s…)", trunc(res.Sub, 8), trunc(res.JKT, 8))
	}
	grant, err := s.Ledger.Faucet(req.AccountPub, nullifier, s.DevFaucet)
	if err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	log.Printf("faucet: granted %s to %s… via %s", wire.FmtFLEET(grant), req.AccountPub[:12], idKind)
	writeJSON(w, 200, map[string]any{
		"granted":  grant,
		"balance":  s.Ledger.Balance(req.AccountPub),
		"identity": idKind,
	})
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ChannelOpenMsg is signed by the funder key (§12.3 openFor).
type ChannelOpenMsg struct {
	FunderPub string `json:"funder_pub"`
	Deposit   uint64 `json:"deposit"`
	TSms      uint64 `json:"ts_ms"`
	Sig       []byte `json:"sig"`
}

func (m *ChannelOpenMsg) SigningBytes() []byte {
	c := *m
	c.Sig = nil
	return append([]byte(wire.DomainChan), wire.MustMarshal(&c)...)
}

func (s *Server) handleChannelOpen(w http.ResponseWriter, r *http.Request) {
	var m ChannelOpenMsg
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	pub, err := hex.DecodeString(m.FunderPub)
	if err != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, m.SigningBytes(), m.Sig) {
		writeErr(w, 400, "bad funder signature")
		return
	}
	ch, err := s.Ledger.OpenChannel(m.FunderPub, m.Deposit)
	if err != nil {
		writeErr(w, 402, err.Error())
		return
	}
	log.Printf("ledger: channel %s… opened by %s… deposit %s", ch.ID[:12], m.FunderPub[:12], wire.FmtFLEET(ch.Deposit))
	writeJSON(w, 200, ch)
}

func (s *Server) handleChannelGet(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.Ledger.GetChannel(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "unknown channel")
		return
	}
	writeJSON(w, 200, ch)
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	pub := r.PathValue("pub")
	writeJSON(w, 200, map[string]any{"account": pub, "balance": s.Ledger.Balance(pub)})
}

// SettleReq is the node's CBOR settlement submission.
type SettleReq struct {
	Receipt    wire.Receipt `cbor:"1,keyasint"`
	ReceiptSig []byte       `cbor:"2,keyasint"`
	NIKPub     []byte       `cbor:"3,keyasint"`
	Voucher    wire.Voucher `cbor:"4,keyasint"`
	ClientAck  []byte       `cbor:"5,keyasint,omitempty"`
}

func (s *Server) handleSettle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, "read")
		return
	}
	var req SettleReq
	if err := wire.DecMode.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, "bad cbor: "+err.Error())
		return
	}
	nik := hex.EncodeToString(req.NIKPub)
	node, ok := s.Registry.Node(nik)
	if !ok {
		writeErr(w, 404, "unknown node")
		return
	}
	// The settled tip rate is the node's *advertised* rate for this bundle —
	// the registry disciplines pricing (§12.2).
	var tip ledger.TipRate
	bundleHex := hex.EncodeToString(req.Receipt.Bundle)
	found := false
	for _, mo := range node.Models {
		if mo.BundleHash == bundleHex {
			tip = mo.Tip
			found = true
			break
		}
	}
	if !found {
		writeErr(w, 404, wire.EModel+": node does not serve receipt bundle")
		return
	}
	res, err := s.Ledger.Settle(ledger.SettleArgs{
		Receipt: req.Receipt, ReceiptSig: req.ReceiptSig, NIKPub: req.NIKPub,
		Voucher: req.Voucher, ClientAck: req.ClientAck, TipRate: tip,
	})
	if err != nil {
		writeErr(w, 402, err.Error())
		return
	}
	// Receipt timings feed the measured-performance plane (§11.2): TTFT and
	// generation TPS derived from the signed receipt, not self-reports.
	if req.Receipt.TFirst >= req.Receipt.TAccept && req.Receipt.TDone >= req.Receipt.TFirst {
		ttft := float64(req.Receipt.TFirst - req.Receipt.TAccept)
		tps := 0.0
		if genMs := float64(req.Receipt.TDone - req.Receipt.TFirst); genMs >= 50 {
			tps = float64(req.Receipt.OutTokens) / (genMs / 1000.0)
		}
		s.Registry.ObservePerf(nik, ttft, tps)
	}
	log.Printf("settle: receipt %s… node=%s… in=%d out=%d price=%s (burn %s, tip %s) bond=%s",
		hex.EncodeToString(req.Receipt.Hash())[:12], nik[:12],
		req.Receipt.InTokens, req.Receipt.OutTokens,
		wire.FmtFLEET(req.Receipt.PriceCharged), wire.FmtFLEET(res.Burned), wire.FmtFLEET(res.Tip),
		wire.FmtFLEET(res.BondHeld))
	writeJSON(w, 200, res)
}

// AckReq records the client countersignature (dual receipt, §7.4).
type AckReq struct {
	Receipt   wire.Receipt `cbor:"1,keyasint"`
	ClientPub []byte       `cbor:"2,keyasint"`
	Ack       []byte       `cbor:"3,keyasint"`
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, "read")
		return
	}
	var req AckReq
	if err := wire.DecMode.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, "bad cbor")
		return
	}
	ok := s.Ledger.MarkDual(hex.EncodeToString(req.Receipt.Hash()), req.ClientPub, req.Ack, &req.Receipt)
	writeJSON(w, 200, map[string]bool{"dual_signed": ok})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.Ledger.Stats())
}

// --- relay (data plane) ----------------------------------------------------

// handleRelay forwards a sealed envelope to the pinned target node and
// streams the encrypted frames back. The gateway sees ciphertext only (I1).
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(wire.InputBuckets[len(wire.InputBuckets)-1]+(64<<10))))
	if err != nil {
		writeErr(w, 400, "read")
		return
	}
	var env wire.Envelope
	if err := wire.DecMode.Unmarshal(body, &env); err != nil {
		writeErr(w, 400, "bad envelope cbor")
		return
	}
	nik := hex.EncodeToString(env.TargetNIK)
	node, ok := s.lookupNode(nik)
	if !ok || node.Status != StatusActive {
		writeErr(w, wire.ErrHTTPStatus[wire.RNoNode], wire.RNoNode+": target node not active")
		return
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(r.Context(), "POST",
		strings.TrimRight(node.Addr, "/")+"/v1/infer", strings.NewReader(string(body)))
	if err != nil {
		writeErr(w, 500, "relay request")
		return
	}
	req.Header.Set("Content-Type", "application/cbor")
	resp, err := s.relayClient.Do(req)
	if err != nil {
		s.observe(nik, false)
		writeErr(w, wire.ErrHTTPStatus[wire.RTimeout], wire.RTimeout+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Fleet-Node", nik)
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	s.observe(nik, resp.StatusCode == 200)
	log.Printf("relay: %s… -> node %s… status=%d dur=%s (ciphertext only)",
		hex.EncodeToString(env.ReqID), nik[:12], resp.StatusCode, time.Since(start).Round(time.Millisecond))
}

var nodeCache sync.Map // nik -> nodeCacheEntry

type nodeCacheEntry struct {
	node *NodeInfo
	at   time.Time
}

// lookupNode resolves a node from local state (embedded/chain mode) or the
// chain API (gateway mode, short cache).
func (s *Server) lookupNode(nik string) (*NodeInfo, bool) {
	if s.Registry != nil {
		return s.Registry.Node(nik)
	}
	if e, ok := nodeCache.Load(nik); ok {
		ent := e.(nodeCacheEntry)
		if time.Since(ent.at) < 2*time.Second {
			return ent.node, ent.node != nil
		}
	}
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(strings.TrimRight(s.ChainURL, "/") + "/v1/registry/node/" + nik)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		nodeCache.Store(nik, nodeCacheEntry{nil, time.Now()})
		return nil, false
	}
	var n NodeInfo
	if json.NewDecoder(resp.Body).Decode(&n) != nil {
		return nil, false
	}
	nodeCache.Store(nik, nodeCacheEntry{&n, time.Now()})
	return &n, true
}

func (s *Server) observe(nik string, ok bool) {
	if s.Registry != nil {
		s.Registry.ObserveResult(nik, ok)
		return
	}
	b, _ := json.Marshal(ObserveMsg{NIK: nik, OK: ok})
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Post(strings.TrimRight(s.ChainURL, "/")+"/v1/registry/observe", "application/json", bytes.NewReader(b))
	if err == nil {
		resp.Body.Close()
	}
}
