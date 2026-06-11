// Package ledger is the Phase-0 testnet chain-sim (whitepaper §12, §14):
// accounts, payment channels, voucher redemption, base-fee burn, tips,
// earned-escrow bonds, and the faucet — the on-chain layer collapsed into
// an in-process service for the MVP testnet.
package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"alien.org/fleet/internal/wire"
)

// Genesis parameters (testnet Phase 0; §19 with MVP overrides).
type Params struct {
	FaucetGrant    uint64        `json:"faucet_grant"`  // nano-FLEET per trial claim (§13.2: per nullifier)
	BaseFeeInKTok  uint64        `json:"base_in_ktok"`  // burned, nano-FLEET per kTok input
	BaseFeeOutKTok uint64        `json:"base_out_ktok"` // burned, nano-FLEET per kTok output
	NodeBond       uint64        `json:"node_bond"`     // earned-escrow bond target (§8.1)
	SettleEvery    time.Duration `json:"-"`             // MVP: immediate
}

func DefaultParams() Params {
	return Params{
		FaucetGrant:    50 * wire.NanoFLEET,        // 50 FLEET trial (testnet-sized)
		BaseFeeInKTok:  2 * wire.NanoFLEET / 1000,  // 0.002 FLEET / kTok in
		BaseFeeOutKTok: 10 * wire.NanoFLEET / 1000, // 0.010 FLEET / kTok out
		NodeBond:       5 * wire.NanoFLEET,         // 5 FLEET earned-escrow (testnet-sized)
	}
}

type Channel struct {
	ID        string `json:"id"`
	FunderPub string `json:"funder_pub"` // hex Ed25519
	Deposit   uint64 `json:"deposit"`
	Spent     uint64 `json:"spent"`
	LastNonce uint64 `json:"last_nonce"`
	OpenedAt  int64  `json:"opened_at"`
}

type ReceiptRecord struct {
	ReceiptHash string `json:"receipt_hash"`
	NIK         string `json:"nik"`
	Client      string `json:"client"`
	Bundle      string `json:"bundle"`
	InTokens    uint64 `json:"in_tokens"`
	OutTokens   uint64 `json:"out_tokens"`
	Price       uint64 `json:"price"`
	Burned      uint64 `json:"burned"`
	Tip         uint64 `json:"tip"`
	TTFTms      uint64 `json:"ttft_ms"`
	DualSigned  bool   `json:"dual_signed"`
	SettledAt   int64  `json:"settled_at"`
}

type Ledger struct {
	mu sync.Mutex

	Params     Params              `json:"params"`
	Accounts   map[string]uint64   `json:"accounts"`   // hex pubkey -> nano-FLEET
	Channels   map[string]*Channel `json:"channels"`   // channel id -> state
	Nullifiers map[string]int64    `json:"nullifiers"` // faucet claims per identity nullifier
	Escrow     map[string]uint64   `json:"escrow"`     // node NIK -> earned-escrow bond held
	Burned     uint64              `json:"burned"`
	Minted     uint64              `json:"minted"`
	Receipts   []ReceiptRecord     `json:"receipts"`
	seenNonce  map[string]map[uint64]bool

	statePath string
}

func New(params Params, statePath string) *Ledger {
	l := &Ledger{
		Params:     params,
		Accounts:   map[string]uint64{},
		Channels:   map[string]*Channel{},
		Nullifiers: map[string]int64{},
		Escrow:     map[string]uint64{},
		seenNonce:  map[string]map[uint64]bool{},
		statePath:  statePath,
	}
	if statePath != "" {
		if b, err := os.ReadFile(statePath); err == nil {
			_ = json.Unmarshal(b, l)
			for id, ch := range l.Channels {
				m := map[uint64]bool{}
				for n := uint64(1); n <= ch.LastNonce; n++ {
					m[n] = true
				}
				l.seenNonce[id] = m
			}
		}
	}
	return l
}

func (l *Ledger) persist() {
	if l.statePath == "" {
		return
	}
	b, _ := json.MarshalIndent(l, "", " ")
	tmp := l.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, l.statePath)
	}
}

var (
	ErrAlreadyClaimed = errors.New("trial credit already claimed for this nullifier")
	ErrInsufficient   = errors.New("insufficient balance")
	ErrNoChannel      = errors.New("unknown channel")
	ErrNonceReused    = errors.New("voucher nonce reused (double-redeem)")
	ErrBadVoucher     = errors.New("invalid voucher")
)

// Faucet mints a trial credit to the account, one per nullifier (§13.2).
// An empty nullifier is allowed only when devMode is set (Phase-0 faucet).
func (l *Ledger) Faucet(accountPub string, nullifier string, devMode bool) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nullifier
	if key == "" {
		if !devMode {
			return 0, errors.New("identity required: faucet is gated per Agent ID nullifier")
		}
		key = "dev:" + accountPub
	}
	if _, ok := l.Nullifiers[key]; ok {
		return 0, ErrAlreadyClaimed
	}
	l.Nullifiers[key] = time.Now().Unix()
	l.Accounts[accountPub] += l.Params.FaucetGrant
	l.Minted += l.Params.FaucetGrant
	l.persist()
	return l.Params.FaucetGrant, nil
}

// OpenChannel escrows deposit from the funder's account (§12.3 PaymentHub.openFor).
func (l *Ledger) OpenChannel(funderPub string, deposit uint64) (*Channel, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Accounts[funderPub] < deposit {
		return nil, fmt.Errorf("%w: balance %s < deposit %s", ErrInsufficient,
			wire.FmtFLEET(l.Accounts[funderPub]), wire.FmtFLEET(deposit))
	}
	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	ch := &Channel{
		ID:        hex.EncodeToString(idb),
		FunderPub: funderPub,
		Deposit:   deposit,
		OpenedAt:  time.Now().Unix(),
	}
	l.Accounts[funderPub] -= deposit
	l.Channels[ch.ID] = ch
	l.seenNonce[ch.ID] = map[uint64]bool{}
	l.persist()
	return ch, nil
}

func (l *Ledger) GetChannel(id string) (*Channel, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch, ok := l.Channels[id]
	if !ok {
		return nil, false
	}
	c := *ch
	return &c, true
}

func (l *Ledger) Balance(pub string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Accounts[pub]
}

// SettleArgs carries a settlement submission (§12.3 redeem, MVP: per receipt).
type SettleArgs struct {
	Receipt    wire.Receipt
	ReceiptSig []byte
	NIKPub     []byte
	Voucher    wire.Voucher
	ClientAck  []byte // optional dual-receipt ack
	TipRate    TipRate
}

// TipRate is the provider-advertised tip (§12.2), nano-FLEET per kTok.
type TipRate struct {
	InPerKTok  uint64 `json:"in_per_ktok"`
	OutPerKTok uint64 `json:"out_per_ktok"`
}

type SettleResult struct {
	Burned    uint64 `json:"burned"`
	Tip       uint64 `json:"tip"`
	ToEscrow  uint64 `json:"to_escrow"`
	NodeBal   uint64 `json:"node_balance"`
	BondHeld  uint64 `json:"bond_held"`
	ChanSpent uint64 `json:"channel_spent"`
}

func ceilKTok(tokens uint64) uint64 { return (tokens + 999) / 1000 }

// ComputePrice computes §12.2 for the interactive class (multiplier 1.0).
// Shared by the chain-sim and the node so receipts settle byte-exact.
func ComputePrice(p Params, tip TipRate, inTok, outTok uint64) (burn, tipAmt uint64) {
	burn = p.BaseFeeInKTok*ceilKTok(inTok) + p.BaseFeeOutKTok*ceilKTok(outTok)
	tipAmt = tip.InPerKTok*ceilKTok(inTok) + tip.OutPerKTok*ceilKTok(outTok)
	return burn, tipAmt
}

func (l *Ledger) Price(inTok, outTok uint64, tip TipRate) (burn, tipAmt uint64) {
	return ComputePrice(l.Params, tip, inTok, outTok)
}

// Settle verifies a receipt+voucher pair and applies value flow:
// base fee burned, tip to the node (earned-escrow first, §8.1).
func (l *Ledger) Settle(a SettleArgs) (*SettleResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	nikHex := hex.EncodeToString(a.NIKPub)
	if !a.Receipt.VerifySig(ed25519.PublicKey(a.NIKPub), a.ReceiptSig) {
		return nil, errors.New("receipt signature invalid")
	}
	ch, ok := l.Channels[hex.EncodeToString(a.Voucher.ChannelID)]
	if !ok {
		return nil, ErrNoChannel
	}
	funderPub, err := hex.DecodeString(ch.FunderPub)
	if err != nil || !a.Voucher.Verify(ed25519.PublicKey(funderPub)) {
		return nil, fmt.Errorf("%w: bad funder signature", ErrBadVoucher)
	}
	if a.Receipt.VoucherNonce != a.Voucher.Nonce {
		return nil, fmt.Errorf("%w: receipt/voucher nonce mismatch", ErrBadVoucher)
	}
	seen := l.seenNonce[ch.ID]
	if seen == nil {
		seen = map[uint64]bool{}
		l.seenNonce[ch.ID] = seen
	}
	if seen[a.Voucher.Nonce] {
		return nil, ErrNonceReused
	}
	if a.Receipt.PriceCharged > a.Voucher.MaxForRequest {
		return nil, fmt.Errorf("%w: price %d > voucher max %d", ErrBadVoucher,
			a.Receipt.PriceCharged, a.Voucher.MaxForRequest)
	}
	if ch.Spent+a.Receipt.PriceCharged > ch.Deposit {
		return nil, fmt.Errorf("%w: channel exhausted", ErrInsufficient)
	}

	burn, tip := l.Price(a.Receipt.InTokens, a.Receipt.OutTokens, a.TipRate)
	if burn+tip != a.Receipt.PriceCharged {
		return nil, fmt.Errorf("price mismatch: receipt %d != computed %d", a.Receipt.PriceCharged, burn+tip)
	}

	seen[a.Voucher.Nonce] = true
	if a.Voucher.Nonce > ch.LastNonce {
		ch.LastNonce = a.Voucher.Nonce
	}
	ch.Spent += a.Receipt.PriceCharged
	l.Burned += burn

	// Earned-escrow bond (§8.1): first NodeBond of tips is held as the bond.
	toEscrow := uint64(0)
	if held := l.Escrow[nikHex]; held < l.Params.NodeBond {
		toEscrow = min(tip, l.Params.NodeBond-held)
		l.Escrow[nikHex] = held + toEscrow
	}
	l.Accounts[nikHex] += tip - toEscrow

	dual := false
	if len(a.ClientAck) == ed25519.SignatureSize {
		dual = a.Receipt.VerifyAck(ed25519.PublicKey(a.Receipt.ClientSessionPub), a.ClientAck)
	}
	ttft := uint64(0)
	if a.Receipt.TFirst >= a.Receipt.TAccept {
		ttft = a.Receipt.TFirst - a.Receipt.TAccept
	}
	l.Receipts = append(l.Receipts, ReceiptRecord{
		ReceiptHash: hex.EncodeToString(a.Receipt.Hash()),
		NIK:         nikHex,
		Client:      hex.EncodeToString(a.Receipt.ClientSessionPub),
		Bundle:      hex.EncodeToString(a.Receipt.Bundle),
		InTokens:    a.Receipt.InTokens,
		OutTokens:   a.Receipt.OutTokens,
		Price:       a.Receipt.PriceCharged,
		Burned:      burn,
		Tip:         tip,
		TTFTms:      ttft,
		DualSigned:  dual,
		SettledAt:   time.Now().Unix(),
	})
	l.persist()
	return &SettleResult{
		Burned: burn, Tip: tip, ToEscrow: toEscrow,
		NodeBal: l.Accounts[nikHex], BondHeld: l.Escrow[nikHex], ChanSpent: ch.Spent,
	}, nil
}

// MarkDual records a late-arriving client countersignature (§7.4 DualReceipt).
func (l *Ledger) MarkDual(receiptHash string, clientPub []byte, ack []byte, receipt *wire.Receipt) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.Receipts {
		if l.Receipts[i].ReceiptHash == receiptHash {
			if receipt != nil && receipt.VerifyAck(ed25519.PublicKey(clientPub), ack) {
				l.Receipts[i].DualSigned = true
				l.persist()
				return true
			}
			return false
		}
	}
	return false
}

// ProviderStat is one compute provider's earnings summary: what the node
// operator actually receives for serving inference (§12.2 tips; the bond
// portion per §8.1 is refundable escrow, the rest is spendable).
type ProviderStat struct {
	Receipts       int    `json:"receipts"`
	InTokens       uint64 `json:"in_tokens"`
	OutTokens      uint64 `json:"out_tokens"`
	TipsEarnedNano uint64 `json:"tips_earned_nano"`
	TipsEarned     string `json:"tips_earned"`
	BondHeld       string `json:"bond_held"`       // earned-escrow (refundable on exit)
	Spendable      string `json:"spendable"`       // account balance, withdrawable
	BurnedForWork  string `json:"burned_for_work"` // base fees destroyed for this node's work
}

type Stats struct {
	Burned       uint64                  `json:"burned_nano"`
	BurnedHuman  string                  `json:"burned"`
	Minted       uint64                  `json:"minted_nano"`
	MintedHuman  string                  `json:"minted"`
	ReceiptCount int                     `json:"receipts"`
	DualSigned   int                     `json:"dual_signed"`
	Channels     int                     `json:"channels"`
	Accounts     map[string]string       `json:"accounts"`
	Escrow       map[string]string       `json:"node_bonds"`
	Providers    map[string]ProviderStat `json:"providers"`
	Recent       []ReceiptRecord         `json:"recent_receipts"`
}

func (l *Ledger) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Stats{
		Burned: l.Burned, BurnedHuman: wire.FmtFLEET(l.Burned),
		Minted: l.Minted, MintedHuman: wire.FmtFLEET(l.Minted),
		ReceiptCount: len(l.Receipts), Channels: len(l.Channels),
		Accounts: map[string]string{}, Escrow: map[string]string{},
		Providers: map[string]ProviderStat{},
	}
	for k, v := range l.Accounts {
		s.Accounts[k] = wire.FmtFLEET(v)
	}
	for k, v := range l.Escrow {
		s.Escrow[k] = wire.FmtFLEET(v)
	}
	burnedFor := map[string]uint64{}
	for _, r := range l.Receipts {
		if r.DualSigned {
			s.DualSigned++
		}
		p := s.Providers[r.NIK]
		p.Receipts++
		p.InTokens += r.InTokens
		p.OutTokens += r.OutTokens
		p.TipsEarnedNano += r.Tip
		burnedFor[r.NIK] += r.Burned
		s.Providers[r.NIK] = p
	}
	for nik, p := range s.Providers {
		p.TipsEarned = wire.FmtFLEET(p.TipsEarnedNano)
		p.BondHeld = wire.FmtFLEET(l.Escrow[nik])
		p.Spendable = wire.FmtFLEET(l.Accounts[nik])
		p.BurnedForWork = wire.FmtFLEET(burnedFor[nik])
		s.Providers[nik] = p
	}
	n := len(l.Receipts)
	lo := n - 5
	if lo < 0 {
		lo = 0
	}
	s.Recent = append(s.Recent, l.Receipts[lo:]...)
	return s
}
