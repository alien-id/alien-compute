package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"testing"
)

func TestEnvelopeRoundtrip(t *testing.T) {
	nek, err := GenerateNEK()
	if err != nil {
		t.Fatal(err)
	}
	_, clientSK, _ := ed25519.GenerateKey(rand.Reader)
	nikPub, nikSK, _ := ed25519.GenerateKey(rand.Reader)

	openai := []byte(`{"model":"gemma4","messages":[{"role":"user","content":"hi"}]}`)
	payload := Payload{OpenAIJSON: openai, ClientSessionPub: clientSK.Public().(ed25519.PublicKey)}
	raw := MustMarshal(&payload)

	bundle := bytes.Repeat([]byte{0xAB}, 32)
	reqID := bytes.Repeat([]byte{1}, 16)
	voucher := &Voucher{ChannelID: []byte{1, 2}, CumAmount: 100, MaxForRequest: 100, Nonce: 1}
	voucher.Sign(clientSK)

	env := &Envelope{
		Version: Version, ReqID: reqID, Bundle: bundle, Bucket: 0,
		TargetNIK: nikPub, Voucher: voucher, TTLms: 1000, MaxPrice: 100,
	}
	sealRes, err := Seal(nek.Pub, env.HPKEInfo(), env.AAD(), raw)
	if err != nil {
		t.Fatal(err)
	}
	env.Enc, env.CT = sealRes.Enc, sealRes.CT

	// node side
	b := MustMarshal(env)
	var env2 Envelope
	if err := DecMode.Unmarshal(b, &env2); err != nil {
		t.Fatal(err)
	}
	if !env2.Voucher.Verify(clientSK.Public().(ed25519.PublicKey)) {
		t.Fatal("voucher verify failed")
	}
	opened, err := Open(nek, env2.HPKEInfo(), env2.AAD(), env2.Enc, env2.CT)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened.Plaintext, raw) {
		t.Fatal("plaintext mismatch")
	}
	if !bytes.Equal(opened.RespKey, sealRes.RespKey) {
		t.Fatal("exported response keys differ")
	}

	// tampered AAD must fail
	env2.MaxPrice++
	if _, err := Open(nek, env2.HPKEInfo(), env2.AAD(), env2.Enc, env2.CT); err == nil {
		t.Fatal("tampered AAD accepted")
	}
	env2.MaxPrice--

	// frame stream roundtrip
	var buf bytes.Buffer
	sealer, _ := NewStreamSealer(&buf, sealRes.RespKey)
	rec := Receipt{ReqID: reqID, ReqCommit: sha(b), RespCommit: sha([]byte("out")), Bundle: bundle,
		InTokens: 10, OutTokens: 5, PriceCharged: 42, VoucherNonce: 1,
		ClientSessionPub: clientSK.Public().(ed25519.PublicKey)}
	signed := SignedReceipt{Receipt: rec, NIKPub: nikPub, Sig: rec.Sign(nikSK)}
	if err := sealer.WriteFrame(FrameSSE, []byte("chunk1")); err != nil {
		t.Fatal(err)
	}
	if err := sealer.WriteFrame(FrameReceipt, MustMarshal(&signed)); err != nil {
		t.Fatal(err)
	}
	opener, _ := NewStreamOpener(&buf, opened.RespKey)
	f1, err := opener.ReadFrame()
	if err != nil || f1.Type != FrameSSE || string(f1.Data) != "chunk1" {
		t.Fatalf("frame1: %v %+v", err, f1)
	}
	f2, err := opener.ReadFrame()
	if err != nil || f2.Type != FrameReceipt {
		t.Fatalf("frame2: %v", err)
	}
	var sr SignedReceipt
	if err := DecMode.Unmarshal(f2.Data, &sr); err != nil {
		t.Fatal(err)
	}
	if !sr.Receipt.VerifySig(nikPub, sr.Sig) {
		t.Fatal("receipt sig verify failed")
	}
	ack := sr.Receipt.Ack(clientSK)
	if !sr.Receipt.VerifyAck(clientSK.Public().(ed25519.PublicKey), ack) {
		t.Fatal("ack verify failed")
	}
	if _, err := opener.ReadFrame(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func sha(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func TestPickBucket(t *testing.T) {
	if i, _ := PickBucket(100); i != 0 {
		t.Fatal("bucket 0 expected")
	}
	if i, _ := PickBucket(5000); i != 1 {
		t.Fatal("bucket 1 expected")
	}
	if _, err := PickBucket(2 << 20); err == nil {
		t.Fatal("oversize accepted")
	}
}
