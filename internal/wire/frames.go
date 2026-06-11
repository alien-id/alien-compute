package wire

import (
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// Frame types on the encrypted response stream (§6.5: "encrypted paced
// frames ... signed Receipt as final frame").
const (
	FrameSSE     = 1 // one OpenAI streaming chunk (the JSON after "data: ")
	FrameJSON    = 2 // full non-streamed OpenAI response body
	FrameReceipt = 3 // CBOR SignedReceipt — always the final frame
	FrameError   = 4 // CBOR FrameErr
)

type Frame struct {
	Type uint64 `cbor:"1,keyasint"`
	Data []byte `cbor:"2,keyasint"`
}

type FrameErr struct {
	Code string `cbor:"1,keyasint"`
	Msg  string `cbor:"2,keyasint"`
}

func newAEADSeal(key []byte) (func(seq uint64, pt []byte) []byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return func(seq uint64, pt []byte) []byte {
		nonce := make([]byte, chacha20poly1305.NonceSize)
		binary.BigEndian.PutUint64(nonce[4:], seq)
		return aead.Seal(nil, nonce, pt, nil)
	}, nil
}

func newAEADOpen(key []byte) (func(seq uint64, ct []byte) ([]byte, error), error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return func(seq uint64, ct []byte) ([]byte, error) {
		nonce := make([]byte, chacha20poly1305.NonceSize)
		binary.BigEndian.PutUint64(nonce[4:], seq)
		return aead.Open(nil, nonce, ct, nil)
	}, nil
}

type StreamSealer struct {
	w    io.Writer
	fl   interface{ Flush() }
	seal func(seq uint64, pt []byte) []byte
	seq  uint64
}

func NewStreamSealer(w io.Writer, respKey []byte) (*StreamSealer, error) {
	seal, err := newAEADSeal(respKey)
	if err != nil {
		return nil, err
	}
	fl, _ := w.(interface{ Flush() })
	return &StreamSealer{w: w, fl: fl, seal: seal}, nil
}

func (s *StreamSealer) WriteFrame(ftype uint64, data []byte) error {
	pt := MustMarshal(&Frame{Type: ftype, Data: data})
	ct := s.seal(s.seq, pt)
	s.seq++
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(ct)))
	if _, err := s.w.Write(lp[:]); err != nil {
		return err
	}
	if _, err := s.w.Write(ct); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

type StreamOpener struct {
	r    io.Reader
	open func(seq uint64, ct []byte) ([]byte, error)
	seq  uint64
}

func NewStreamOpener(r io.Reader, respKey []byte) (*StreamOpener, error) {
	open, err := newAEADOpen(respKey)
	if err != nil {
		return nil, err
	}
	return &StreamOpener{r: r, open: open}, nil
}

const maxFrameSize = 8 << 20

// ReadFrame returns the next decrypted frame, or io.EOF at clean end of stream.
func (s *StreamOpener) ReadFrame() (*Frame, error) {
	var lp [4]byte
	if _, err := io.ReadFull(s.r, lp[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err
	}
	n := binary.BigEndian.Uint32(lp[:])
	if n > maxFrameSize {
		return nil, fmt.Errorf("frame too large: %d", n)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(s.r, ct); err != nil {
		return nil, err
	}
	pt, err := s.open(s.seq, ct)
	if err != nil {
		return nil, fmt.Errorf("frame %d decrypt: %w", s.seq, err)
	}
	s.seq++
	var f Frame
	if err := DecMode.Unmarshal(pt, &f); err != nil {
		return nil, fmt.Errorf("frame %d decode: %w", s.seq-1, err)
	}
	return &f, nil
}
