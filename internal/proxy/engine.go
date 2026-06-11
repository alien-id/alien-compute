// Package proxy implements the Fleet local proxy (whitepaper §11.1 "Local
// proxy" mode): a daemon on localhost speaking plain OpenAI HTTP, holding
// the session key and payment channel, sealing every request end-to-end to
// the chosen node's NEK. Confidentiality never leaves this process (X1).
package proxy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	mrand "math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"alien.org/fleet/internal/gateway"
	"alien.org/fleet/internal/ledger"
	"alien.org/fleet/internal/wire"
)

type Config struct {
	Gateway      string
	Listen       string
	DefaultModel string
	StateDir     string
	DepositNano  uint64 // channel deposit
	AgentIDCLI   string // optional: agent-id-auth cli.mjs path for DPoP faucet claims
}

type channelState struct {
	ID         string `json:"id"`
	Deposit    uint64 `json:"deposit"`
	KnownSpent uint64 `json:"known_spent"`
	Nonce      uint64 `json:"nonce"`
}

type Engine struct {
	cfg Config

	session    ed25519.PrivateKey
	sessionPub ed25519.PublicKey

	// Gateway pool (§11.1: race pings, keep the best, re-race periodically).
	// cfg.Gateway is a comma-separated bootstrap list; peer exchange
	// (/v1/net/peers) discovers the rest.
	gwMu sync.Mutex
	gws  []string // ordered: best (lowest ping) first

	mu      sync.Mutex
	channel channelState
	pins    map[string]string // alias -> bundle hash (TOFU, AX5)
	params  ledger.Params

	receiptsMu sync.Mutex
	receipts   []ReceiptView

	hc *http.Client
}

type ReceiptView struct {
	ReqID      string `json:"req_id"`
	Node       string `json:"node"`
	Model      string `json:"model"`
	InTokens   uint64 `json:"in_tokens"`
	OutTokens  uint64 `json:"out_tokens"`
	Price      string `json:"price"`
	PriceNano  uint64 `json:"price_nano"`
	TTFTms     uint64 `json:"ttft_ms"`
	Verified   bool   `json:"verified"`
	CounterSig bool   `json:"countersigned"`
	At         int64  `json:"at"`
}

func NewEngine(cfg Config) (*Engine, error) {
	e := &Engine{cfg: cfg, pins: map[string]string{},
		hc: &http.Client{Timeout: 0}}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, err
	}
	// Session key (§6.2): days-lived, signs vouchers + countersigns receipts.
	seedPath := filepath.Join(cfg.StateDir, "session.seed")
	seed, err := os.ReadFile(seedPath)
	if err != nil || len(seed) != ed25519.SeedSize {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.WriteFile(seedPath, seed, 0o600); err != nil {
			return nil, err
		}
	}
	e.session = ed25519.NewKeyFromSeed(seed)
	e.sessionPub = e.session.Public().(ed25519.PublicKey)

	if b, err := os.ReadFile(filepath.Join(cfg.StateDir, "channel.json")); err == nil {
		_ = json.Unmarshal(b, &e.channel)
	}
	if b, err := os.ReadFile(filepath.Join(cfg.StateDir, "alias_pins.json")); err == nil {
		_ = json.Unmarshal(b, &e.pins)
	}
	return e, nil
}

func (e *Engine) SessionPubHex() string { return hex.EncodeToString(e.sessionPub) }

func (e *Engine) persistChannel() {
	b, _ := json.MarshalIndent(e.channel, "", " ")
	_ = os.WriteFile(filepath.Join(e.cfg.StateDir, "channel.json"), b, 0o644)
}

func (e *Engine) persistPins() {
	b, _ := json.MarshalIndent(e.pins, "", " ")
	_ = os.WriteFile(filepath.Join(e.cfg.StateDir, "alias_pins.json"), b, 0o644)
}

// gwBases returns the gateway pool, best-first.
func (e *Engine) gwBases() []string {
	e.gwMu.Lock()
	defer e.gwMu.Unlock()
	return append([]string(nil), e.gws...)
}

func (e *Engine) gwURL(path string) string {
	bases := e.gwBases()
	if len(bases) == 0 {
		return strings.TrimRight(e.cfg.Gateway, "/") + path
	}
	return bases[0] + path
}

// DiscoverGateways pulls /v1/net/peers from every known gateway, races
// health pings, and orders the pool by latency (§11.1). One live
// bootstrap entrypoint is enough to find the whole gateway set.
func (e *Engine) DiscoverGateways() {
	known := map[string]bool{}
	for _, g := range strings.Split(e.cfg.Gateway, ",") {
		if g = strings.TrimSpace(strings.TrimRight(g, "/")); g != "" {
			known[g] = true
		}
	}
	for _, g := range e.gwBases() {
		known[g] = true
	}
	cli := &http.Client{Timeout: 5 * time.Second}
	for g := range known {
		resp, err := cli.Get(g + "/v1/net/peers")
		if err != nil {
			continue
		}
		var doc struct {
			Peers []struct {
				URL  string `json:"url"`
				Name string `json:"name"`
			} `json:"peers"`
		}
		if json.NewDecoder(resp.Body).Decode(&doc) == nil {
			for _, p := range doc.Peers {
				if u := strings.TrimRight(p.URL, "/"); u != "" && !known[u] {
					known[u] = true
					log.Printf("fleet-proxy: discovered gateway %q at %s via peer exchange", p.Name, u)
				}
			}
		}
		resp.Body.Close()
	}
	// race pings, order by latency
	type timed struct {
		url string
		rtt time.Duration
	}
	var alive []timed
	for g := range known {
		start := time.Now()
		resp, err := cli.Get(g + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		resp.Body.Close()
		alive = append(alive, timed{g, time.Since(start)})
	}
	sort.Slice(alive, func(i, j int) bool { return alive[i].rtt < alive[j].rtt })
	urls := make([]string, len(alive))
	for i, t := range alive {
		urls[i] = t.url
	}
	if len(urls) > 0 {
		e.gwMu.Lock()
		e.gws = urls
		e.gwMu.Unlock()
	}
}

func (e *Engine) gwGetJSON(path string, out any) error {
	cli := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	bases := e.gwBases()
	if len(bases) == 0 {
		bases = []string{strings.TrimRight(strings.Split(e.cfg.Gateway, ",")[0], "/")}
	}
	for _, base := range bases {
		resp, err := cli.Get(base + path)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			var eo struct{ Error string }
			_ = json.NewDecoder(resp.Body).Decode(&eo)
			return fmt.Errorf("gateway %s: %d %s", path, resp.StatusCode, eo.Error)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return fmt.Errorf("all gateways unreachable: %w", lastErr)
}

// agentIDHeaders mints DPoP headers for the faucet via the agent-id CLI.
func (e *Engine) agentIDHeaders(url string) (auth, dpop string, err error) {
	cmd := exec.Command("node", e.cfg.AgentIDCLI, "header", "--url", url, "--method", "POST", "--raw")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("agent-id cli: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Authorization:"); ok {
			auth = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "DPoP:"); ok {
			dpop = strings.TrimSpace(v)
		}
	}
	if auth == "" || dpop == "" {
		return "", "", errors.New("agent-id cli returned no headers")
	}
	return auth, dpop, nil
}

// EnsureFunded claims the faucet trial credit (once) and opens the payment
// channel (§15.1 consumer onboarding, collapsed to: faucet → openFor).
func (e *Engine) EnsureFunded() error {
	if err := e.gwGetJSON("/v1/ledger/params", &e.params); err != nil {
		return fmt.Errorf("params: %w", err)
	}
	e.mu.Lock()
	hasChannel := e.channel.ID != ""
	e.mu.Unlock()
	if hasChannel {
		// Validate it still exists (gateway may have been reset).
		var ch ledger.Channel
		if err := e.gwGetJSON("/v1/ledger/channel/"+e.channel.ID, &ch); err == nil {
			return nil
		}
		e.mu.Lock()
		e.channel = channelState{}
		e.mu.Unlock()
	}

	acct := e.SessionPubHex()
	var bal struct {
		Balance uint64 `json:"balance"`
	}
	if err := e.gwGetJSON("/v1/ledger/account/"+acct, &bal); err != nil {
		return err
	}
	if bal.Balance == 0 {
		faucetURL := e.gwURL("/v1/ledger/faucet")
		body, _ := json.Marshal(map[string]string{"account_pub": acct})
		req, _ := http.NewRequest("POST", faucetURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		idMode := "dev"
		if e.cfg.AgentIDCLI != "" {
			auth, dpop, err := e.agentIDHeaders(faucetURL)
			if err != nil {
				return fmt.Errorf("agent-id required but unavailable: %w", err)
			}
			req.Header.Set("Authorization", auth)
			req.Header.Set("DPoP", dpop)
			idMode = "agent-id"
		}
		cli := &http.Client{Timeout: 15 * time.Second}
		resp, err := cli.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var fr struct {
			Granted  uint64 `json:"granted"`
			Balance  uint64 `json:"balance"`
			Identity string `json:"identity"`
			Error    string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&fr)
		if resp.StatusCode != 200 {
			return fmt.Errorf("faucet: %s", fr.Error)
		}
		bal.Balance = fr.Balance
		fmt.Printf("fleet-proxy: faucet granted %s (identity: %s, mode %s)\n",
			wire.FmtFLEET(fr.Granted), fr.Identity, idMode)
	}

	deposit := e.cfg.DepositNano
	if deposit == 0 || deposit > bal.Balance {
		deposit = bal.Balance * 9 / 10
	}
	msg := gateway.ChannelOpenMsg{
		FunderPub: acct,
		Deposit:   deposit,
		TSms:      uint64(time.Now().UnixMilli()),
	}
	msg.Sig = ed25519.Sign(e.session, msg.SigningBytes())
	b, _ := json.Marshal(&msg)
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Post(e.gwURL("/v1/ledger/channel/open"), "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var eo struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&eo)
		return fmt.Errorf("channel open: %s", eo.Error)
	}
	var ch ledger.Channel
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		return err
	}
	e.mu.Lock()
	e.channel = channelState{ID: ch.ID, Deposit: ch.Deposit}
	e.persistChannel()
	e.mu.Unlock()
	fmt.Printf("fleet-proxy: payment channel %s… open, deposit %s\n", ch.ID[:12], wire.FmtFLEET(ch.Deposit))
	return nil
}

// Resolve maps a model alias to a bundle hash with TOFU pinning (AX5).
func (e *Engine) Resolve(alias string) (string, error) {
	var doc struct {
		Models []struct {
			Alias  string `json:"alias"`
			Bundle string `json:"bundle_hash"`
		} `json:"models"`
	}
	if err := e.gwGetJSON("/v1/registry/models", &doc); err != nil {
		return "", err
	}
	for _, m := range doc.Models {
		if m.Alias == alias {
			e.mu.Lock()
			defer e.mu.Unlock()
			if pinned, ok := e.pins[alias]; ok && pinned != m.Bundle {
				return "", fmt.Errorf("alias %q REBOUND from pinned bundle %s to %s — refusing (AX5)",
					alias, pinned[:16], m.Bundle[:16])
			}
			if _, ok := e.pins[alias]; !ok {
				e.pins[alias] = m.Bundle
				e.persistPins()
			}
			return m.Bundle, nil
		}
	}
	return "", fmt.Errorf("unknown model %q", alias)
}

// PickNode implements hard filters + power-of-two-choices sampling
// ∝ S²/price (§11.3).
func (e *Engine) PickNode(bundle string) (*gateway.NodeInfo, error) {
	var doc struct {
		Nodes []gateway.NodeInfo `json:"nodes"`
	}
	if err := e.gwGetJSON("/v1/registry/nodes?bundle="+bundle, &doc); err != nil {
		return nil, err
	}
	if len(doc.Nodes) == 0 {
		return nil, errors.New(wire.RNoNode + ": no active node serves this bundle")
	}
	if len(doc.Nodes) == 1 {
		return &doc.Nodes[0], nil
	}
	weight := func(n *gateway.NodeInfo) float64 {
		price := 1.0
		for _, mo := range n.Models {
			if mo.BundleHash == bundle {
				price = float64(mo.Tip.OutPerKTok+mo.Tip.InPerKTok) + 1
			}
		}
		return (n.Score*n.Score + 0.01) / price
	}
	// sample two distinct candidates by weight, take the less loaded
	idx := weightedSample(doc.Nodes, weight, 2)
	a, b := &doc.Nodes[idx[0]], &doc.Nodes[idx[1]]
	if b.QueueTokens < a.QueueTokens {
		return b, nil
	}
	return a, nil
}

func weightedSample(nodes []gateway.NodeInfo, w func(*gateway.NodeInfo) float64, k int) []int {
	type item struct {
		i   int
		key float64
	}
	items := make([]item, len(nodes))
	for i := range nodes {
		wt := w(&nodes[i])
		if wt <= 0 {
			wt = 1e-9
		}
		// Efraimidis–Spirakis weighted sampling without replacement
		items[i] = item{i, math.Pow(mrand.Float64(), 1/wt)}
	}
	for i := 0; i < k && i < len(items); i++ {
		best := i
		for j := i + 1; j < len(items); j++ {
			if items[j].key > items[best].key {
				best = j
			}
		}
		items[i], items[best] = items[best], items[i]
	}
	out := make([]int, 0, k)
	for i := 0; i < k && i < len(items); i++ {
		out = append(out, items[i].i)
	}
	return out
}

// reserve allocates a voucher for one request, bounded by channel capacity.
func (e *Engine) reserve(maxForRequest uint64) (*wire.Voucher, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.channel.ID == "" {
		return nil, errors.New("no payment channel")
	}
	remaining := e.channel.Deposit - e.channel.KnownSpent
	if maxForRequest > remaining {
		return nil, fmt.Errorf("%s: channel remaining %s < request cap %s — top up or open a new channel",
			wire.EVoucher, wire.FmtFLEET(remaining), wire.FmtFLEET(maxForRequest))
	}
	e.channel.Nonce++
	chID, _ := hex.DecodeString(e.channel.ID)
	v := &wire.Voucher{
		ChannelID:     chID,
		CumAmount:     e.channel.KnownSpent + maxForRequest,
		MaxForRequest: maxForRequest,
		Nonce:         e.channel.Nonce,
		Epoch:         uint64(time.Now().Unix() / 3600),
	}
	v.Sign(e.session)
	e.persistChannel()
	return v, nil
}

func (e *Engine) recordSpend(price uint64) {
	e.mu.Lock()
	e.channel.KnownSpent += price
	e.persistChannel()
	e.mu.Unlock()
}

func (e *Engine) Status() map[string]any {
	e.mu.Lock()
	ch := e.channel
	e.mu.Unlock()
	e.receiptsMu.Lock()
	rs := append([]ReceiptView(nil), e.receipts...)
	e.receiptsMu.Unlock()
	var spent uint64
	for _, r := range rs {
		spent += r.PriceNano
	}
	var bal struct {
		Balance uint64 `json:"balance"`
	}
	_ = e.gwGetJSON("/v1/ledger/account/"+e.SessionPubHex(), &bal)
	return map[string]any{
		"gateways":        e.gwBases(),
		"session_pub":     e.SessionPubHex(),
		"wallet_balance":  wire.FmtFLEET(bal.Balance),
		"channel_id":      ch.ID,
		"channel_deposit": wire.FmtFLEET(ch.Deposit),
		"channel_spent":   wire.FmtFLEET(ch.KnownSpent),
		"requests_paid":   len(rs),
		"total_paid":      wire.FmtFLEET(spent),
		"receipts":        rs,
	}
}
