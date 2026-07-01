package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"alien.org/fleet/internal/gateway"
	"alien.org/fleet/internal/ledger"
	"alien.org/fleet/internal/wire"
)

type Server struct {
	E *Engine
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /fleet/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, s.E.Status())
	})
	// Full network capacity view (models × nodes × slots × measured perf).
	mux.HandleFunc("GET /fleet/capacity", func(w http.ResponseWriter, _ *http.Request) {
		var doc map[string]any
		if err := s.E.gwGetJSON("/v1/registry/capacity", &doc); err != nil {
			writeOpenAIErr(w, 502, "api_error", err.Error())
			return
		}
		writeJSON(w, 200, doc)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "service": "fleet-proxy"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}

// handleModels exposes the network's models with exact artifact names,
// metadata, and live capacity (sourced from /v1/registry/capacity).
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	var doc struct {
		Models []gateway.BundleCapacity `json:"models"`
	}
	if err := s.E.gwGetJSON("/v1/registry/capacity", &doc); err != nil {
		writeOpenAIErr(w, 502, "api_error", err.Error())
		return
	}
	list := []map[string]any{}
	for _, m := range doc.Models {
		ids := m.Aliases
		if len(ids) == 0 {
			ids = []string{m.Name}
		}
		fleet := map[string]any{
			"exact_name":         m.Name,
			"bundle_hash":        m.BundleHash,
			"aliases":            m.Aliases,
			"meta":               m.Meta,
			"node_count":         m.NodeCount,
			"free_slots":         m.FreeSlots,
			"measured_tps_total": m.TPSTotal,
		}
		for _, id := range ids {
			list = append(list, map[string]any{
				"id": id, "object": "model", "owned_by": "alien-fleet",
				"fleet": fleet,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": list})
}

// estimateMax computes the voucher cap for a request (client-side worst case).
func (s *Server) estimateMax(oai map[string]any, body []byte, tip ledger.TipRate) uint64 {
	inTok := uint64(len(body)/3) + 64
	outTok := uint64(2048)
	if mt, ok := oai["max_tokens"].(float64); ok && mt > 0 {
		outTok = uint64(mt)
	} else if mt, ok := oai["max_completion_tokens"].(float64); ok && mt > 0 {
		outTok = uint64(mt)
	}
	burn, tipAmt := ledger.ComputePrice(s.E.params, tip, inTok, outTok)
	est := burn + tipAmt
	return est*2 + wire.NanoFLEET/1000 // 2× headroom + 0.000001 FLEET
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIErr(w, 400, "invalid_request_error", "read body")
		return
	}
	var oai map[string]any
	if err := json.Unmarshal(body, &oai); err != nil {
		writeOpenAIErr(w, 400, "invalid_request_error", "invalid JSON")
		return
	}
	model, _ := oai["model"].(string)
	if model == "" {
		model = s.E.cfg.DefaultModel
		oai["model"] = model
	}
	stream, _ := oai["stream"].(bool)

	bundle, err := s.E.Resolve(model)
	if err != nil {
		writeOpenAIErr(w, 404, "invalid_request_error", err.Error())
		return
	}
	node, err := s.E.PickNode(bundle)
	if err != nil {
		writeOpenAIErr(w, 503, "api_error", err.Error())
		return
	}
	var tip ledger.TipRate
	for _, mo := range node.Models {
		if mo.BundleHash == bundle {
			tip = mo.Tip
		}
	}

	openaiJSON, _ := json.Marshal(oai)
	maxForRequest := s.estimateMax(oai, openaiJSON, tip)

	// Relay through the best gateway; fail over to the next on connection
	// errors (any gateway can relay — they are interchangeable PoPs). Each
	// attempt reserves a FRESH voucher (new nonce) and re-seals, so a failover
	// after the first PoP already delivered to the node cannot trip the node's
	// exact-nonce guard (E_VOUCHER "voucher nonce replayed"). This is safe for
	// billing: CumAmount is cumulative and KnownSpent only advances on a
	// verified receipt, so an orphaned attempt does not double-charge.
	var resp *http.Response
	var relayErr error
	var voucher *wire.Voucher
	var respKey []byte
	var reqCommit [32]byte
	for _, base := range s.E.gwBases() {
		v, err := s.E.reserve(maxForRequest)
		if err != nil {
			writeOpenAIErr(w, 402, "insufficient_quota", err.Error())
			return
		}
		env, rk, err := s.seal(node, bundle, openaiJSON, v, maxForRequest)
		if err != nil {
			writeOpenAIErr(w, 500, "api_error", "seal: "+err.Error())
			return
		}
		envBytes := wire.MustMarshal(env)
		relayReq, _ := http.NewRequestWithContext(r.Context(), "POST", base+"/v1/relay", bytes.NewReader(envBytes))
		relayReq.Header.Set("Content-Type", "application/cbor")
		resp, relayErr = s.E.hc.Do(relayReq)
		if relayErr == nil {
			voucher, respKey, reqCommit = v, rk, sha256.Sum256(envBytes)
			break
		}
	}
	if relayErr != nil || resp == nil {
		writeOpenAIErr(w, 502, "api_error", fmt.Sprintf("relay: all gateways failed: %v", relayErr))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeOpenAIErr(w, resp.StatusCode, "api_error", "fleet: "+strings.TrimSpace(string(b)))
		return
	}

	opener, err := wire.NewStreamOpener(resp.Body, respKey)
	if err != nil {
		writeOpenAIErr(w, 500, "api_error", "stream key")
		return
	}

	respHash := sha256.New()
	headersSent := false
	fl, _ := w.(http.Flusher)
	var receipt *wire.SignedReceipt

	for {
		frame, ferr := opener.ReadFrame()
		if ferr == io.EOF {
			break
		}
		if ferr != nil {
			if !headersSent {
				writeOpenAIErr(w, 502, "api_error", "frame: "+ferr.Error())
			}
			return
		}
		switch frame.Type {
		case wire.FrameSSE:
			respHash.Write(frame.Data)
			if !headersSent {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(200)
				headersSent = true
			}
			fmt.Fprintf(w, "data: %s\n\n", frame.Data)
			if fl != nil {
				fl.Flush()
			}
		case wire.FrameJSON:
			respHash.Write(frame.Data)
			if !headersSent {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				headersSent = true
			}
			_, _ = w.Write(frame.Data)
		case wire.FrameError:
			var fe wire.FrameErr
			_ = wire.DecMode.Unmarshal(frame.Data, &fe)
			if !headersSent {
				status := wire.ErrHTTPStatus[fe.Code]
				if status == 0 {
					status = 502
				}
				writeOpenAIErr(w, status, "api_error", fe.Code+": "+fe.Msg)
			} else if stream {
				fmt.Fprintf(w, "data: {\"error\":{\"type\":\"api_error\",\"message\":%q}}\n\n", fe.Code+": "+fe.Msg)
			}
			return
		case wire.FrameReceipt:
			var sr wire.SignedReceipt
			if err := wire.DecMode.Unmarshal(frame.Data, &sr); err == nil {
				receipt = &sr
			}
		}
	}

	// Receipt verification (§15.1 per-request: verify & countersign).
	if receipt == nil {
		log.Printf("proxy: WARNING no receipt received for request")
		if stream && headersSent {
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
		return
	}
	rec := &receipt.Receipt
	verified := bytes.Equal(rec.ReqCommit, reqCommit[:]) &&
		bytes.Equal(rec.RespCommit, respHash.Sum(nil)) &&
		hex.EncodeToString(rec.Bundle) == bundle &&
		rec.PriceCharged <= voucher.MaxForRequest &&
		hex.EncodeToString(receipt.NIKPub) == node.NIK &&
		rec.VerifySig(receipt.NIKPub, receipt.Sig)
	countersigned := false
	if verified {
		s.E.recordSpend(rec.PriceCharged)
		ack := rec.Ack(s.E.session)
		go s.postAck(rec, ack)
		countersigned = true
	} else {
		log.Printf("proxy: RECEIPT VERIFICATION FAILED for req %s (refusing countersign)",
			hex.EncodeToString(rec.ReqID))
	}
	ttft := uint64(0)
	if rec.TFirst >= rec.TAccept {
		ttft = rec.TFirst - rec.TAccept
	}
	s.E.receiptsMu.Lock()
	s.E.receipts = append(s.E.receipts, ReceiptView{
		ReqID: hex.EncodeToString(rec.ReqID), Node: node.NIK[:16], Model: model,
		InTokens: rec.InTokens, OutTokens: rec.OutTokens,
		Price: wire.FmtFLEET(rec.PriceCharged), PriceNano: rec.PriceCharged,
		TTFTms: ttft, Verified: verified, CounterSig: countersigned,
		At: time.Now().Unix(),
	})
	s.E.receiptsMu.Unlock()
	log.Printf("proxy: %s via node %s… in=%d out=%d price=%s ttft=%dms verified=%v e2e=%s",
		model, node.NIK[:12], rec.InTokens, rec.OutTokens,
		wire.FmtFLEET(rec.PriceCharged), ttft, verified, time.Since(start).Round(time.Millisecond))

	if stream && headersSent {
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

// seal builds the padded payload and HPKE-seals it to the node's NEK (§7).
func (s *Server) seal(node *gateway.NodeInfo, bundleHex string, openaiJSON []byte, voucher *wire.Voucher, maxPrice uint64) (*wire.Envelope, []byte, error) {
	nekPub, err := wire.ParseNEKPub(node.NEKPub)
	if err != nil {
		return nil, nil, err
	}
	bundle, err := hex.DecodeString(bundleHex)
	if err != nil {
		return nil, nil, err
	}
	nik, err := hex.DecodeString(node.NIK)
	if err != nil {
		return nil, nil, err
	}
	reqID := make([]byte, 16)
	if _, err := rand.Read(reqID); err != nil {
		return nil, nil, err
	}

	payload := wire.Payload{
		OpenAIJSON:       openaiJSON,
		ClientSessionPub: s.E.sessionPub,
		Paced:            false,
	}
	// Pad-to-bucket (§7.3): iterate until the serialized payload exactly
	// fills the chosen bucket (CBOR length headers shift the size).
	raw := wire.MustMarshal(&payload)
	bucketIdx, err := wire.PickBucket(len(raw) + 16)
	if err != nil {
		return nil, nil, err
	}
	target := wire.InputBuckets[bucketIdx]
	for i := 0; i < 8; i++ {
		diff := target - len(raw)
		if diff == 0 {
			break
		}
		padLen := len(payload.Pad) + diff
		if padLen < 0 {
			padLen = 0
		}
		payload.Pad = make([]byte, padLen)
		raw = wire.MustMarshal(&payload)
	}

	env := &wire.Envelope{
		Version:   wire.Version,
		ReqID:     reqID,
		Bundle:    bundle,
		Bucket:    uint64(bucketIdx),
		TargetNIK: nik,
		Voucher:   voucher,
		TTLms:     120_000,
		MaxPrice:  maxPrice,
	}
	sealRes, err := wire.Seal(nekPub, env.HPKEInfo(), env.AAD(), raw)
	if err != nil {
		return nil, nil, err
	}
	env.Enc = sealRes.Enc
	env.CT = sealRes.CT
	return env, sealRes.RespKey, nil
}

func (s *Server) postAck(rec *wire.Receipt, ack []byte) {
	req := gateway.AckReq{Receipt: *rec, ClientPub: s.E.sessionPub, Ack: ack}
	body := wire.MustMarshal(&req)
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Post(s.E.gwURL("/v1/ledger/ack"), "application/cbor", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}
