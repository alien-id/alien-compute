package wire

import (
	"crypto/rand"
	"fmt"

	"github.com/cloudflare/circl/hpke"
	"github.com/cloudflare/circl/kem"
)

// HPKE profile (§7.1): DHKEM(X25519, HKDF-SHA256) / HKDF-SHA256 / ChaCha20-Poly1305, Base mode.
var (
	hpkeKEM   = hpke.KEM_X25519_HKDF_SHA256
	hpkeKDF   = hpke.KDF_HKDF_SHA256
	hpkeAEAD  = hpke.AEAD_ChaCha20Poly1305
	hpkeSuite = hpke.NewSuite(hpkeKEM, hpkeKDF, hpkeAEAD)
)

// NEK is the node encryption key (X25519 HPKE recipient key, §6.2).
type NEK struct {
	Priv kem.PrivateKey
	Pub  kem.PublicKey
}

func GenerateNEK() (*NEK, error) {
	pub, priv, err := hpkeKEM.Scheme().GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	return &NEK{Priv: priv, Pub: pub}, nil
}

func (n *NEK) PubBytes() []byte {
	b, err := n.Pub.MarshalBinary()
	if err != nil {
		panic(err)
	}
	return b
}

func ParseNEKPub(b []byte) (kem.PublicKey, error) {
	return hpkeKEM.Scheme().UnmarshalBinaryPublicKey(b)
}

// SealResult is the client-side output of sealing a payload.
type SealResult struct {
	Enc     []byte
	CT      []byte
	RespKey []byte // ChaCha20-Poly1305 key for the response frame stream
}

// Seal HPKE-seals plaintext to the node's NEK with the envelope's info/AAD.
func Seal(nekPub kem.PublicKey, info, aad, plaintext []byte) (*SealResult, error) {
	sender, err := hpkeSuite.NewSender(nekPub, info)
	if err != nil {
		return nil, err
	}
	enc, sealer, err := sender.Setup(rand.Reader)
	if err != nil {
		return nil, err
	}
	ct, err := sealer.Seal(plaintext, aad)
	if err != nil {
		return nil, err
	}
	return &SealResult{
		Enc:     enc,
		CT:      ct,
		RespKey: sealer.Export([]byte(ExportLabelResp), 32),
	}, nil
}

// OpenResult is the node-side output of opening an envelope.
type OpenResult struct {
	Plaintext []byte
	RespKey   []byte
}

// Open HPKE-opens an envelope ciphertext with the node's NEK.
func Open(nek *NEK, info, aad, enc, ct []byte) (*OpenResult, error) {
	recv, err := hpkeSuite.NewReceiver(nek.Priv, info)
	if err != nil {
		return nil, err
	}
	opener, err := recv.Setup(enc)
	if err != nil {
		return nil, fmt.Errorf("hpke setup: %w", err)
	}
	pt, err := opener.Open(ct, aad)
	if err != nil {
		return nil, fmt.Errorf("hpke open: %w", err)
	}
	return &OpenResult{
		Plaintext: pt,
		RespKey:   opener.Export([]byte(ExportLabelResp), 32),
	}, nil
}
