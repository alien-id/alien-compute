package gateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	mrand "math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"alien.org/fleet/internal/wire"
)

// Gateway peer discovery, modeled on the bootstrap-entrypoint + signed-record
// + peer-exchange pattern of Ethereum's discv5 (ENRs from bootnodes) and
// Solana's gossip (ContactInfo CRDS from entrypoints): a client or node needs
// any ONE live gateway URL; everything else is learned from the network.
//
// A PeerRecord is the MVP's ENR: a self-describing, Ed25519-signed
// advertisement of a gateway. Records are verified, then health-checked
// before being served onward — gossip spreads reachability, probing proves it.

const DomainPeer = "FLEET/v1/peer"

type PeerRecord struct {
	URL  string `json:"url"`  // public base URL, e.g. http://15.237.243.199:9000
	Kind string `json:"kind"` // "gateway"
	Name string `json:"name"`
	TSms uint64 `json:"ts_ms"`
	Pub  []byte `json:"pub"` // gateway identity key
	Sig  []byte `json:"sig"`
}

func (p *PeerRecord) signingBytes() []byte {
	c := *p
	c.Sig = nil
	return append([]byte(DomainPeer), wire.MustMarshal(&c)...)
}

func (p *PeerRecord) Verify() bool {
	if len(p.Pub) != ed25519.PublicKeySize || len(p.Sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(p.Pub), p.signingBytes(), p.Sig)
}

type peerState struct {
	rec     PeerRecord
	healthy bool
	lastOK  time.Time
}

type PeerTable struct {
	mu    sync.Mutex
	self  PeerRecord
	key   ed25519.PrivateKey
	peers map[string]*peerState // url -> state
}

func NewPeerTable(name, publicURL string, bootstrap []string) *PeerTable {
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	pt := &PeerTable{key: sk, peers: map[string]*peerState{}}
	pt.self = PeerRecord{URL: publicURL, Kind: "gateway", Name: name,
		TSms: uint64(time.Now().UnixMilli()), Pub: sk.Public().(ed25519.PublicKey)}
	pt.self.Sig = ed25519.Sign(sk, pt.self.signingBytes())
	for _, b := range bootstrap {
		b = strings.TrimSpace(strings.TrimRight(b, "/"))
		if b != "" && b != publicURL {
			pt.peers[b] = &peerState{rec: PeerRecord{URL: b, Kind: "gateway"}}
		}
	}
	return pt
}

func (pt *PeerTable) Self() PeerRecord { return pt.self }

// Healthy returns self + all peers that answered a recent health probe.
func (pt *PeerTable) Healthy() []PeerRecord {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	out := []PeerRecord{pt.self}
	for _, ps := range pt.peers {
		if ps.healthy {
			out = append(out, ps.rec)
		}
	}
	return out
}

// Add merges a record (signature required for named records; bootstrap
// entries are bare URLs probed before first gossip).
func (pt *PeerTable) Add(rec PeerRecord) bool {
	rec.URL = strings.TrimRight(rec.URL, "/")
	if rec.URL == "" || rec.URL == pt.self.URL || rec.Kind != "gateway" {
		return false
	}
	if len(rec.Pub) > 0 && !rec.Verify() {
		return false
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()
	ps, ok := pt.peers[rec.URL]
	if !ok {
		pt.peers[rec.URL] = &peerState{rec: rec}
		return true
	}
	if rec.TSms > ps.rec.TSms {
		ps.rec = rec
	}
	return false
}

func (pt *PeerTable) snapshotURLs() []string {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	urls := make([]string, 0, len(pt.peers))
	for u := range pt.peers {
		urls = append(urls, u)
	}
	return urls
}

func (pt *PeerTable) setHealth(url string, ok bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if ps, found := pt.peers[url]; found {
		ps.healthy = ok
		if ok {
			ps.lastOK = time.Now()
		} else if time.Since(ps.lastOK) > 10*time.Minute && !ps.lastOK.IsZero() {
			delete(pt.peers, url) // expire long-dead peers
		}
	}
}

// Gossip runs the discovery loop: probe peers, announce self to a random
// peer, and pull the peer's table (pull-based exchange, à la Solana).
func (pt *PeerTable) Gossip() {
	cli := &http.Client{Timeout: 5 * time.Second}
	for {
		urls := pt.snapshotURLs()
		// 1. health-probe everyone (cheap on testnet scale)
		for _, u := range urls {
			resp, err := cli.Get(u + "/healthz")
			ok := err == nil && resp.StatusCode == 200
			if resp != nil {
				resp.Body.Close()
			}
			pt.setHealth(u, ok)
		}
		// 2. announce self + pull peers from one random healthy peer
		healthy := []string{}
		pt.mu.Lock()
		for u, ps := range pt.peers {
			if ps.healthy {
				healthy = append(healthy, u)
			}
		}
		pt.mu.Unlock()
		if len(healthy) > 0 {
			peer := healthy[mrand.Intn(len(healthy))]
			self := pt.Self()
			body, _ := json.Marshal(&self)
			if resp, err := cli.Post(peer+"/v1/net/announce", "application/json", bytes.NewReader(body)); err == nil {
				resp.Body.Close()
			}
			if resp, err := cli.Get(peer + "/v1/net/peers"); err == nil {
				var doc struct {
					Peers []PeerRecord `json:"peers"`
				}
				if json.NewDecoder(resp.Body).Decode(&doc) == nil {
					for _, rec := range doc.Peers {
						if pt.Add(rec) {
							log.Printf("gossip: learned gateway %q at %s (via %s)", rec.Name, rec.URL, peer)
						}
					}
				}
				resp.Body.Close()
			}
		}
		time.Sleep(20 * time.Second)
	}
}

// --- HTTP handlers (mounted on the gateway plane) ---------------------------

func (s *Server) handlePeers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"peers": s.Peers.Healthy()})
}

func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	var rec PeerRecord
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if len(rec.Pub) == 0 || !rec.Verify() {
		writeErr(w, 400, "peer record signature required")
		return
	}
	if s.Peers.Add(rec) {
		log.Printf("gossip: gateway %q announced itself from %s (pub %s…)",
			rec.Name, rec.URL, hex.EncodeToString(rec.Pub)[:12])
	}
	writeJSON(w, 200, map[string]any{"ok": true, "peers": len(s.Peers.Healthy())})
}
