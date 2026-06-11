// Package identity implements the IIdentityAttestor adapter (whitepaper
// §13): Alien Agent ID verification via RFC 9449 DPoP + RFC 9068 at+jwt,
// ported from agent-id/docs/INTEGRATION.md ("Option B"). The gateway
// derives a per-protocol nullifier from the SSO-attested owner — no
// personal data touches the ledger.
package identity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Result struct {
	Sub       string // SSO-attested human owner (AlienID address)
	JKT       string // agent key thumbprint
	Nullifier string // per-protocol pseudonym (§13.1)
}

type Verifier interface {
	Verify(r *http.Request) (*Result, error)
}

// Nullifier derives the per-protocol pseudonymous identifier of a verified
// human (§13.1): opaque, stable across that human's keys and agents.
func Nullifier(iss, sub string) string {
	h := sha256.Sum256([]byte("FLEET/v1/nullifier\x00" + iss + "\x00" + sub))
	return hex.EncodeToString(h[:])
}

const proofMaxAgeSec = 30

// DPoPVerifier verifies Alien Agent ID requests against the Alien SSO JWKS.
type DPoPVerifier struct {
	Issuer      string // e.g. https://sso.alien-api.com
	ExpectedAud string // optional audience allow-list (single value)
	ExternalURL string // external base URL for htu reconstruction

	mu       sync.Mutex
	jwks     map[string]json.RawMessage // kid -> JWK
	jwksAt   time.Time
	seenJTIs map[string]int64
}

func NewDPoPVerifier(issuer, expectedAud, externalURL string) *DPoPVerifier {
	return &DPoPVerifier{Issuer: issuer, ExpectedAud: expectedAud, ExternalURL: externalURL,
		seenJTIs: map[string]int64{}}
}

func b64u(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func splitJWS(tok string) (h, p, sig []byte, signingInput string, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil, nil, "", errors.New("malformed_jws")
	}
	if h, err = b64u(parts[0]); err != nil {
		return nil, nil, nil, "", errors.New("malformed_jws_header")
	}
	if p, err = b64u(parts[1]); err != nil {
		return nil, nil, nil, "", errors.New("malformed_jws_payload")
	}
	if sig, err = b64u(parts[2]); err != nil {
		return nil, nil, nil, "", errors.New("malformed_jws_sig")
	}
	return h, p, sig, parts[0] + "." + parts[1], nil
}

// jwkThumbprint computes RFC 7638 for an OKP/Ed25519 JWK.
func jwkThumbprint(crv, kty, x string) string {
	canonical := fmt.Sprintf(`{"crv":%q,"kty":%q,"x":%q}`, crv, kty, x)
	h := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func (v *DPoPVerifier) fetchJWKS() (map[string]json.RawMessage, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.jwks != nil && time.Since(v.jwksAt) < time.Hour {
		return v.jwks, nil
	}
	cli := &http.Client{Timeout: 10 * time.Second}
	// Discover jwks_uri via OIDC configuration (INTEGRATION.md), falling
	// back to the conventional well-known path.
	jwksURL := strings.TrimRight(v.Issuer, "/") + "/.well-known/jwks.json"
	if resp, err := cli.Get(strings.TrimRight(v.Issuer, "/") + "/.well-known/openid-configuration"); err == nil {
		var oidc struct {
			JwksURI string `json:"jwks_uri"`
		}
		if json.NewDecoder(resp.Body).Decode(&oidc) == nil && oidc.JwksURI != "" {
			jwksURL = oidc.JwksURI
		}
		resp.Body.Close()
	}
	resp, err := cli.Get(jwksURL)
	if err != nil {
		return nil, fmt.Errorf("jwks_fetch: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, errors.New("jwks_parse")
	}
	out := map[string]json.RawMessage{}
	for _, k := range doc.Keys {
		var meta struct {
			Kid string `json:"kid"`
		}
		_ = json.Unmarshal(k, &meta)
		out[meta.Kid] = k
	}
	v.jwks = out
	v.jwksAt = time.Now()
	return out, nil
}

func verifyATSig(alg, kid, signingInput string, sig []byte, jwks map[string]json.RawMessage) error {
	jwk, ok := jwks[kid]
	if !ok {
		// single-key JWKS without kid: try all
		for _, k := range jwks {
			if verifyOneATSig(alg, signingInput, sig, k) == nil {
				return nil
			}
		}
		return errors.New("kid_not_found")
	}
	return verifyOneATSig(alg, signingInput, sig, jwk)
}

func verifyOneATSig(alg, signingInput string, sig []byte, jwkRaw json.RawMessage) error {
	switch alg {
	case "RS256":
		var k struct{ N, E string }
		if err := json.Unmarshal(jwkRaw, &k); err != nil {
			return errors.New("jwk_parse")
		}
		nb, err1 := b64u(k.N)
		eb, err2 := b64u(k.E)
		if err1 != nil || err2 != nil {
			return errors.New("jwk_parse")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		digest := sha256.Sum256([]byte(signingInput))
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
	case "EdDSA":
		var k struct{ X string }
		if err := json.Unmarshal(jwkRaw, &k); err != nil {
			return errors.New("jwk_parse")
		}
		xb, err := b64u(k.X)
		if err != nil || len(xb) != ed25519.PublicKeySize {
			return errors.New("jwk_parse")
		}
		if !ed25519.Verify(ed25519.PublicKey(xb), []byte(signingInput), sig) {
			return errors.New("at_sig_invalid")
		}
		return nil
	default:
		return errors.New("at_alg_unsupported")
	}
}

func (v *DPoPVerifier) Verify(r *http.Request) (*Result, error) {
	// 1. headers
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, errors.New("missing_auth_header")
	}
	if !strings.HasPrefix(auth, "DPoP ") {
		return nil, errors.New("bad_auth_scheme")
	}
	at := strings.TrimSpace(strings.TrimPrefix(auth, "DPoP "))
	proof := r.Header.Get("DPoP")
	if proof == "" {
		return nil, errors.New("missing_dpop_header")
	}

	// 2–4. proof shape + EdDSA signature against embedded JWK
	ph, pp, psig, pinput, err := splitJWS(proof)
	if err != nil {
		return nil, err
	}
	var proofHdr struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		JWK struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			D   string `json:"d"`
		} `json:"jwk"`
	}
	if json.Unmarshal(ph, &proofHdr) != nil {
		return nil, errors.New("proof_header_parse")
	}
	if proofHdr.Typ != "dpop+jwt" || proofHdr.Alg != "EdDSA" ||
		proofHdr.JWK.Kty != "OKP" || proofHdr.JWK.Crv != "Ed25519" || proofHdr.JWK.D != "" {
		return nil, errors.New("proof_header_invalid")
	}
	xb, err := b64u(proofHdr.JWK.X)
	if err != nil || len(xb) != ed25519.PublicKeySize {
		return nil, errors.New("proof_jwk_invalid")
	}
	if !ed25519.Verify(ed25519.PublicKey(xb), []byte(pinput), psig) {
		return nil, errors.New("proof_sig_invalid")
	}

	var proofClaims struct {
		Htm string  `json:"htm"`
		Htu string  `json:"htu"`
		Iat float64 `json:"iat"`
		Jti string  `json:"jti"`
		Ath string  `json:"ath"`
	}
	if json.Unmarshal(pp, &proofClaims) != nil {
		return nil, errors.New("proof_payload_parse")
	}

	// 5–6. htm / htu
	if proofClaims.Htm != r.Method {
		return nil, errors.New("htm_mismatch")
	}
	base := v.ExternalURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		host := r.Host
		// Honor X-Forwarded-* from our own gateways (testnet runs everything
		// on localhost; production would gate this on a trusted-proxy CIDR
		// list per INTEGRATION.md).
		if xh := r.Header.Get("X-Forwarded-Host"); xh != "" {
			host = xh
			if xp := r.Header.Get("X-Forwarded-Proto"); xp != "" {
				scheme = xp
			}
		}
		base = scheme + "://" + host
	}
	expectedHtu := strings.TrimRight(base, "/") + r.URL.Path
	if proofClaims.Htu != expectedHtu {
		return nil, fmt.Errorf("htu_mismatch (want %s)", expectedHtu)
	}

	// 7–8. iat freshness + jti replay
	now := time.Now().Unix()
	iat := int64(proofClaims.Iat)
	if proofClaims.Jti == "" {
		return nil, errors.New("jti_missing")
	}
	if iat < now-proofMaxAgeSec || iat > now+proofMaxAgeSec {
		return nil, errors.New("proof_stale")
	}
	v.mu.Lock()
	if _, seen := v.seenJTIs[proofClaims.Jti]; seen {
		v.mu.Unlock()
		return nil, errors.New("jti_replay")
	}
	if len(v.seenJTIs) > 65536 {
		v.seenJTIs = map[string]int64{}
	}
	v.seenJTIs[proofClaims.Jti] = iat
	v.mu.Unlock()

	// 9–11. access token shape, signature, claims
	ah, ap, asig, ainput, err := splitJWS(at)
	if err != nil {
		return nil, err
	}
	var atHdr struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(ah, &atHdr) != nil {
		return nil, errors.New("at_header_parse")
	}
	switch atHdr.Typ {
	case "at+jwt", "jwt", "application/at+jwt", "application/jwt":
	default:
		return nil, errors.New("at_typ_invalid")
	}
	jwks, err := v.fetchJWKS()
	if err != nil {
		return nil, err
	}
	if err := verifyATSig(atHdr.Alg, atHdr.Kid, ainput, asig, jwks); err != nil {
		return nil, err
	}
	var atClaims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp float64         `json:"exp"`
		Cnf struct {
			Jkt string `json:"jkt"`
		} `json:"cnf"`
	}
	if json.Unmarshal(ap, &atClaims) != nil {
		return nil, errors.New("at_payload_parse")
	}
	if atClaims.Iss != v.Issuer {
		return nil, errors.New("iss_mismatch")
	}
	if atClaims.Sub == "" {
		return nil, errors.New("sub_missing")
	}
	if !(atClaims.Exp > float64(now)) { // NaN-safe (INTEGRATION.md caveat)
		return nil, errors.New("at_expired")
	}
	if v.ExpectedAud != "" && atClaims.Aud != nil && !strings.Contains(string(atClaims.Aud), v.ExpectedAud) {
		return nil, errors.New("aud_mismatch")
	}

	// 12. cnf.jkt binding
	if atClaims.Cnf.Jkt != jwkThumbprint(proofHdr.JWK.Crv, proofHdr.JWK.Kty, proofHdr.JWK.X) {
		return nil, errors.New("jkt_mismatch")
	}

	// 13. ath binding
	athWant := sha256.Sum256([]byte(at))
	if proofClaims.Ath != base64.RawURLEncoding.EncodeToString(athWant[:]) {
		return nil, errors.New("ath_mismatch")
	}

	return &Result{
		Sub:       atClaims.Sub,
		JKT:       atClaims.Cnf.Jkt,
		Nullifier: Nullifier(atClaims.Iss, atClaims.Sub),
	}, nil
}
