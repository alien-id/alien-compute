package node

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"alien.org/fleet/internal/gateway"
	"alien.org/fleet/internal/ledger"
	"alien.org/fleet/internal/wire"
)

func writeNodeErr(w http.ResponseWriter, code, msg string) {
	status := wire.ErrHTTPStatus[code]
	if status == 0 {
		status = 500
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}

// handleInfer implements §6.5 request handling: voucher checked BEFORE
// decryption (DoS shield) → HPKE-open → bucket enforcement → inference →
// encrypted frames → signed Receipt as the final frame.
func (n *Node) handleInfer(w http.ResponseWriter, r *http.Request) {
	tAccept := uint64(time.Now().UnixMilli())
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(wire.InputBuckets[len(wire.InputBuckets)-1]+(64<<10))))
	if err != nil {
		writeNodeErr(w, wire.EInternal, "read")
		return
	}
	var env wire.Envelope
	if err := wire.DecMode.Unmarshal(body, &env); err != nil {
		writeNodeErr(w, wire.EInternal, "bad envelope cbor")
		return
	}
	if env.Version != wire.Version {
		writeNodeErr(w, wire.EPolicy, "unsupported version")
		return
	}
	if !bytes.Equal(env.TargetNIK, n.nikPub) {
		writeNodeErr(w, wire.EPolicy, "envelope pinned to a different NIK")
		return
	}
	if !bytes.Equal(env.Bundle, n.bundleHash) {
		writeNodeErr(w, wire.EModel, "bundle not served here")
		return
	}
	if int(env.Bucket) >= len(wire.InputBuckets) || len(env.CT) > wire.InputBuckets[env.Bucket]+64 {
		writeNodeErr(w, wire.EBucket, "ciphertext exceeds declared bucket")
		return
	}

	// --- voucher gate (before decryption, §6.5) ---
	if env.Voucher == nil {
		writeNodeErr(w, wire.EVoucher, "voucher required")
		return
	}
	chanID := hex.EncodeToString(env.Voucher.ChannelID)
	ch, err := n.channelState(chanID)
	if err != nil {
		writeNodeErr(w, wire.EVoucher, "channel lookup: "+err.Error())
		return
	}
	funderPub, err := hex.DecodeString(ch.FunderPub)
	if err != nil || !env.Voucher.Verify(ed25519.PublicKey(funderPub)) {
		writeNodeErr(w, wire.EVoucher, "bad voucher signature")
		return
	}
	if env.Voucher.CumAmount > ch.Deposit || ch.Spent+env.Voucher.MaxForRequest > ch.Deposit {
		writeNodeErr(w, wire.EVoucher, "channel exhausted")
		return
	}
	if env.MaxPrice > env.Voucher.MaxForRequest {
		writeNodeErr(w, wire.EVoucher, "envelope max_price exceeds voucher cap")
		return
	}
	// Exact-replay guard only: concurrent requests may legitimately arrive
	// out of nonce order; double-redeem is enforced by the settlement layer
	// (the receipt binds the consumed nonce, §7.2).
	n.nonceMu.Lock()
	seen := n.seenNonce[chanID]
	if seen == nil {
		seen = map[uint64]bool{}
		n.seenNonce[chanID] = seen
	}
	if seen[env.Voucher.Nonce] {
		n.nonceMu.Unlock()
		writeNodeErr(w, wire.EVoucher, "voucher nonce replayed")
		return
	}
	seen[env.Voucher.Nonce] = true
	n.nonceMu.Unlock()

	// --- HPKE open (current NEK, then previous during drain overlap) ---
	cur, prev := n.currentNEKs()
	var opened *wire.OpenResult
	for _, nek := range []*wire.NEK{cur, prev} {
		if nek == nil {
			continue
		}
		if res, err := wire.Open(nek, env.HPKEInfo(), env.AAD(), env.Enc, env.CT); err == nil {
			opened = res
			break
		}
	}
	if opened == nil {
		writeNodeErr(w, wire.EPolicy, "hpke open failed (stale NEK?)")
		return
	}
	var payload wire.Payload
	if err := wire.DecMode.Unmarshal(opened.Plaintext, &payload); err != nil {
		writeNodeErr(w, wire.EPolicy, "bad payload")
		return
	}

	var oai map[string]any
	if err := json.Unmarshal(payload.OpenAIJSON, &oai); err != nil {
		writeNodeErr(w, wire.EPolicy, "bad openai json")
		return
	}
	stream, _ := oai["stream"].(bool)
	if stream {
		// usage in the final chunk so the receipt has true token counts
		oai["stream_options"] = map[string]any{"include_usage": true}
	}

	n.inflight.Add(1)
	defer n.inflight.Add(-1)

	// From here on the response is the encrypted frame stream.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	sealer, err := wire.NewStreamSealer(&flushWriter{w}, opened.RespKey)
	if err != nil {
		return
	}

	respHash := sha256.New()
	var inTok, outTok uint64
	var tFirst uint64

	if stream {
		inTok, outTok, tFirst, err = n.runStreaming(oai, sealer, respHash)
	} else {
		inTok, outTok, tFirst, err = n.runBlocking(oai, sealer, respHash)
	}
	if err != nil {
		log.Printf("[%s] inference error: %v", n.cfg.Name, err)
		errData := wire.MustMarshal(&wire.FrameErr{Code: wire.EInternal, Msg: err.Error()})
		_ = sealer.WriteFrame(wire.FrameError, errData)
		return
	}
	tDone := uint64(time.Now().UnixMilli())

	burn, tip := ledger.ComputePrice(n.params, n.tip, inTok, outTok)
	price := burn + tip
	receipt := wire.Receipt{
		ReqID:            env.ReqID,
		ReqCommit:        sha256Sum(body),
		RespCommit:       respHash.Sum(nil),
		Bundle:           n.bundleHash,
		Profile:          n.cfg.Profile,
		InTokens:         inTok,
		OutTokens:        outTok,
		TAccept:          tAccept,
		TFirst:           tFirst,
		TDone:            tDone,
		PriceCharged:     price,
		VoucherNonce:     env.Voucher.Nonce,
		ClientSessionPub: payload.ClientSessionPub,
	}
	if price > env.MaxPrice {
		// Never charge above the client's cap (§7.4: clients verify
		// price ≤ max_price). Refuse settlement; emit error frame.
		errData := wire.MustMarshal(&wire.FrameErr{Code: wire.RPrice,
			Msg: fmt.Sprintf("price %d exceeds max_price %d", price, env.MaxPrice)})
		_ = sealer.WriteFrame(wire.FrameError, errData)
		return
	}
	signed := wire.SignedReceipt{
		Receipt: receipt,
		NIKPub:  n.nikPub,
		Sig:     receipt.Sign(n.nik),
	}
	_ = sealer.WriteFrame(wire.FrameReceipt, wire.MustMarshal(&signed))

	// Settlement (§12.3; testnet: immediate, no 6 h batching).
	go n.settle(signed, *env.Voucher)
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *flushWriter) Flush() {
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// runStreaming forwards the request to llama-server with SSE streaming and
// seals each chunk as a frame. Returns token counts and t_first.
func (n *Node) runStreaming(oai map[string]any, sealer *wire.StreamSealer, respHash hash.Hash) (inTok, outTok, tFirst uint64, err error) {
	reqBody, _ := json.Marshal(oai)
	resp, err := http.Post(n.llama.BaseURL()+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, 0, fmt.Errorf("llama-server %d: %s", resp.StatusCode, string(b))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		if tFirst == 0 {
			tFirst = uint64(time.Now().UnixMilli())
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     uint64 `json:"prompt_tokens"`
				CompletionTokens uint64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil {
			inTok = chunk.Usage.PromptTokens
			outTok = chunk.Usage.CompletionTokens
		}
		respHash.Write([]byte(data))
		if err := sealer.WriteFrame(wire.FrameSSE, []byte(data)); err != nil {
			return inTok, outTok, tFirst, err
		}
	}
	return inTok, outTok, tFirst, sc.Err()
}

func (n *Node) runBlocking(oai map[string]any, sealer *wire.StreamSealer, respHash hash.Hash) (inTok, outTok, tFirst uint64, err error) {
	reqBody, _ := json.Marshal(oai)
	resp, err := http.Post(n.llama.BaseURL()+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, 0, err
	}
	if resp.StatusCode != 200 {
		return 0, 0, 0, fmt.Errorf("llama-server %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Usage struct {
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
		} `json:"usage"`
		Timings struct {
			PredictedMs float64 `json:"predicted_ms"`
		} `json:"timings"`
	}
	_ = json.Unmarshal(b, &out)
	// Blocking call: derive t_first from the runtime's measured generation
	// time so receipt timings stay meaningful (t_first ≈ end of prefill).
	tFirst = uint64(time.Now().UnixMilli()) - uint64(out.Timings.PredictedMs)
	respHash.Write(b)
	if err := sealer.WriteFrame(wire.FrameJSON, b); err != nil {
		return out.Usage.PromptTokens, out.Usage.CompletionTokens, tFirst, err
	}
	return out.Usage.PromptTokens, out.Usage.CompletionTokens, tFirst, nil
}

func (n *Node) settle(signed wire.SignedReceipt, voucher wire.Voucher) {
	req := gateway.SettleReq{
		Receipt:    signed.Receipt,
		ReceiptSig: signed.Sig,
		NIKPub:     signed.NIKPub,
		Voucher:    voucher,
	}
	body := wire.MustMarshal(&req)
	var resp *http.Response
	var err error
	for _, base := range n.gwBases() {
		resp, err = n.hc.Post(base+"/v1/ledger/settle", "application/cbor", bytes.NewReader(body))
		if err == nil {
			n.pinGateway(base)
			break
		}
	}
	if err != nil || resp == nil {
		log.Printf("[%s] settle failed on all gateways: %v", n.cfg.Name, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("[%s] settle rejected: %d %s", n.cfg.Name, resp.StatusCode, string(b))
		return
	}
	var res ledger.SettleResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	log.Printf("[%s] settled: +%s tip (bond %s) burn %s", n.cfg.Name,
		wire.FmtFLEET(res.Tip), wire.FmtFLEET(res.BondHeld), wire.FmtFLEET(res.Burned))
	// Invalidate the channel mirror so the next request sees fresh spend.
	n.chanMu.Lock()
	delete(n.chanCache, hex.EncodeToString(voucher.ChannelID))
	n.chanMu.Unlock()
}
