// Package wire implements the Alien Fleet v1 wire protocol (whitepaper §7):
// deterministic-CBOR envelopes, HPKE end-to-end encryption, vouchers,
// receipts, and the encrypted response frame stream.
package wire

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// Protocol constants (whitepaper §6.2, §7).
const (
	Version = 1

	DomainBind    = "FLEET/v1/bind"
	DomainReceipt = "FLEET/v1/receipt"
	DomainAck     = "FLEET/v1/ack"
	DomainVoucher = "FLEET/v1/voucher"
	DomainBeacon  = "FLEET/v1/beacon"
	DomainReg     = "FLEET/v1/register"
	DomainChan    = "FLEET/v1/chanopen"

	HPKEInfoPrefix  = "FLEET/v1"
	ExportLabelResp = "FLEET/v1/resp"
)

// InputBuckets are payload size buckets in BYTES (MVP deviation: the
// whitepaper buckets by kilotokens; the client cannot count tokens without
// the model tokenizer, so the MVP buckets the padded ciphertext by size).
var InputBuckets = []int{4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20}

// Amounts are in nano-FLEET (1 FLEET = 1e9 nanoFLEET). MVP deviation: the
// whitepaper denominates in atto-FLEET (1e18) which overflows uint64 for
// balances above ~18 FLEET; the chain-sim uses nano precision instead.
const NanoFLEET = 1_000_000_000

func FmtFLEET(nano uint64) string {
	return fmt.Sprintf("%d.%09d FLEET", nano/NanoFLEET, nano%NanoFLEET)
}

var encOpts = cbor.CoreDetEncOptions() // deterministic CBOR (§7.2)
var EncMode, _ = encOpts.EncMode()
var DecMode, _ = cbor.DecOptions{}.DecMode()

func MustMarshal(v any) []byte {
	b, err := EncMode.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Envelope is the request envelope (§7.2).
type Envelope struct {
	Version   uint64   `cbor:"1,keyasint"`
	ReqID     []byte   `cbor:"2,keyasint"`           // 16 bytes
	Bundle    []byte   `cbor:"3,keyasint"`           // 32-byte model bundle hash
	Bucket    uint64   `cbor:"4,keyasint"`           // index into InputBuckets
	TargetNIK []byte   `cbor:"5,keyasint"`           // 32-byte Ed25519 node identity key (pin)
	Enc       []byte   `cbor:"6,keyasint"`           // HPKE encapsulated key
	CT        []byte   `cbor:"7,keyasint"`           // HPKE-sealed Payload
	Voucher   *Voucher `cbor:"8,keyasint"`           // outside AAD by design (§7.2)
	CacheKey  []byte   `cbor:"9,keyasint,omitempty"` // prompt-cache hints (§7.5; unused in MVP)
	TTLms     uint64   `cbor:"10,keyasint"`
	MaxPrice  uint64   `cbor:"11,keyasint"` // nano-FLEET
}

// AAD = SHA-256(canonical CBOR of fields 1–5, 9–11) (§7.2). The MVP
// canonicalization is a deterministic CBOR array in field order.
func (e *Envelope) AAD() []byte {
	arr := []any{e.Version, e.ReqID, e.Bundle, e.Bucket, e.TargetNIK, e.CacheKey, e.TTLms, e.MaxPrice}
	h := sha256.Sum256(MustMarshal(arr))
	return h[:]
}

// HPKEInfo = "FLEET/v1" ‖ model_bundle_hash ‖ bucket_id (§7.1).
func (e *Envelope) HPKEInfo() []byte {
	info := append([]byte(HPKEInfoPrefix), e.Bundle...)
	return append(info, byte(e.Bucket))
}

// Payload is the HPKE-sealed request body (§7.2).
type Payload struct {
	OpenAIJSON       []byte `cbor:"1,keyasint"` // verbatim OpenAI-compatible request JSON
	ClientSessionPub []byte `cbor:"2,keyasint"` // Ed25519, countersigns receipts, owns vouchers
	Paced            bool   `cbor:"3,keyasint"`
	Pad              []byte `cbor:"4,keyasint"` // pad-to-bucket
}

// Voucher is the AX2 payment primitive (§12.3).
type Voucher struct {
	ChannelID     []byte `cbor:"1,keyasint"`
	CumAmount     uint64 `cbor:"2,keyasint"` // cumulative authorized spend, nano-FLEET
	MaxForRequest uint64 `cbor:"3,keyasint"` // nano-FLEET cap for this request
	Nonce         uint64 `cbor:"4,keyasint"`
	Epoch         uint64 `cbor:"5,keyasint"`
	Sig           []byte `cbor:"6,keyasint,omitempty"` // Ed25519 by channel funder
}

func (v *Voucher) signingHash() []byte {
	c := *v
	c.Sig = nil
	h := sha256.Sum256(MustMarshal(&c))
	return append([]byte(DomainVoucher), h[:]...)
}

func (v *Voucher) Sign(sk ed25519.PrivateKey) { v.Sig = ed25519.Sign(sk, v.signingHash()) }

func (v *Voucher) Verify(funderPub ed25519.PublicKey) bool {
	if len(v.Sig) != ed25519.SignatureSize || len(funderPub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(funderPub, v.signingHash(), v.Sig)
}

// Receipt is the protocol's atomic fact (§7.4).
type Receipt struct {
	ReqID            []byte `cbor:"1,keyasint"`
	ReqCommit        []byte `cbor:"2,keyasint"` // SHA-256(envelope CBOR)
	RespCommit       []byte `cbor:"3,keyasint"` // SHA-256(output plaintext = concat of data-frame bytes)
	Bundle           []byte `cbor:"4,keyasint"`
	Profile          string `cbor:"5,keyasint"` // node measurement-or-profile (Class O: hardware profile)
	InTokens         uint64 `cbor:"6,keyasint"`
	OutTokens        uint64 `cbor:"7,keyasint"`
	TAccept          uint64 `cbor:"8,keyasint"`  // unix ms
	TFirst           uint64 `cbor:"9,keyasint"`  // unix ms
	TDone            uint64 `cbor:"10,keyasint"` // unix ms
	PriceCharged     uint64 `cbor:"11,keyasint"` // nano-FLEET
	VoucherNonce     uint64 `cbor:"12,keyasint"`
	ClientSessionPub []byte `cbor:"13,keyasint"`
	CachedInTokens   uint64 `cbor:"14,keyasint"`
	CacheWrites      uint64 `cbor:"15,keyasint"`
}

func (r *Receipt) Hash() []byte {
	h := sha256.Sum256(MustMarshal(r))
	return h[:]
}

func (r *Receipt) Sign(nik ed25519.PrivateKey) []byte {
	return ed25519.Sign(nik, append([]byte(DomainReceipt), r.Hash()...))
}

func (r *Receipt) VerifySig(nikPub ed25519.PublicKey, sig []byte) bool {
	if len(sig) != ed25519.SignatureSize || len(nikPub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(nikPub, append([]byte(DomainReceipt), r.Hash()...), sig)
}

func (r *Receipt) Ack(clientSession ed25519.PrivateKey) []byte {
	return ed25519.Sign(clientSession, append([]byte(DomainAck), r.Hash()...))
}

func (r *Receipt) VerifyAck(clientPub ed25519.PublicKey, ack []byte) bool {
	if len(ack) != ed25519.SignatureSize || len(clientPub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(clientPub, append([]byte(DomainAck), r.Hash()...), ack)
}

// SignedReceipt travels as the final response frame and to settlement.
type SignedReceipt struct {
	Receipt   Receipt `cbor:"1,keyasint"`
	NIKPub    []byte  `cbor:"2,keyasint"`
	Sig       []byte  `cbor:"3,keyasint"`
	ClientAck []byte  `cbor:"4,keyasint,omitempty"` // dual receipt (§7.4)
}

// Node error codes (§7.4).
const (
	EVoucher  = "E_VOUCHER"
	EBucket   = "E_BUCKET"
	EModel    = "E_MODEL"
	ETTL      = "E_TTL"
	ECapacity = "E_CAPACITY"
	EPolicy   = "E_POLICY"
	EInternal = "E_INTERNAL"
	RNoNode   = "R_NO_NODE"
	RPrice    = "R_PRICE"
	RTimeout  = "R_TIMEOUT"
)

var ErrHTTPStatus = map[string]int{
	EVoucher: 402, EBucket: 413, EModel: 404, ETTL: 408,
	ECapacity: 429, EPolicy: 451, EInternal: 500,
	RNoNode: 503, RPrice: 409, RTimeout: 504,
}

// PickBucket returns the smallest bucket index that fits n bytes.
func PickBucket(n int) (int, error) {
	for i, b := range InputBuckets {
		if n <= b {
			return i, nil
		}
	}
	return 0, fmt.Errorf("payload of %d bytes exceeds largest input bucket", n)
}
