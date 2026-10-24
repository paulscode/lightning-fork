package shachain

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// independentSecretTag separates the secrets of an IndependentProducer from
// any other use of the same root.
const independentSecretTag = "independent-per-commitment-secret"

// IndependentProducer is a Producer whose secrets have no shachain
// relationship: the secret at index i is an HMAC of i keyed by the root. That
// is one of the ways option_independent_secrets suggests ("a key derivation
// function applied to a private seed, a value unique to the channel ... and
// the commitment number"): the root is already unique to the channel, as
// lnwallet derives it for each channel from a fresh revocation root key and
// the channel's own funding key, so it stands for both the seed and the
// channel's value, and the commitment number is the index. Like a shachain it
// needs no state beyond its root, so a node can always regenerate any secret
// it has sent the point for, but a peer can't compress the secrets, so it
// only works with a peer that stores every secret.
//
// It exists so that a node can exercise the receiving side of its peer, in
// tests and on test networks; a single signer gains nothing from it.
type IndependentProducer struct {
	root chainhash.Hash
}

// A compile time check to ensure IndependentProducer implements the Producer
// interface.
var _ Producer = (*IndependentProducer)(nil)

// NewIndependentProducer creates an IndependentProducer from a root.
func NewIndependentProducer(root chainhash.Hash) *IndependentProducer {
	return &IndependentProducer{root: root}
}

// NewIndependentProducerFrom creates an IndependentProducer with the same root
// as p, read from p's encoding. Because Encode writes that same root, a
// channel can switch producer kinds without changing what it stores, and
// anything else derived from the encoded root (such as taproot nonces) stays
// the same.
func NewIndependentProducerFrom(p Producer) (*IndependentProducer, error) {
	var b bytes.Buffer
	if err := p.Encode(&b); err != nil {
		return nil, err
	}
	if b.Len() != chainhash.HashSize {
		return nil, fmt.Errorf("producer encoding is %d bytes, want %d",
			b.Len(), chainhash.HashSize)
	}

	var root chainhash.Hash
	copy(root[:], b.Bytes())

	return NewIndependentProducer(root), nil
}

// AtIndex returns the secret at index v. It is always a valid secp256k1
// private key: in the negligible case that the HMAC isn't one, a counter is
// mixed in until it is.
//
// NOTE: This function is part of the Producer interface.
func (p *IndependentProducer) AtIndex(v uint64) (*chainhash.Hash, error) {
	// tag || index (8 bytes, big endian) || counter (1 byte).
	var msg [len(independentSecretTag) + 8 + 1]byte
	n := copy(msg[:], independentSecretTag)
	binary.BigEndian.PutUint64(msg[n:], v)

	for counter := 0; counter < 256; counter++ {
		msg[n+8] = byte(counter)

		mac := hmac.New(sha256.New, p.root[:])
		mac.Write(msg[:])

		var secret chainhash.Hash
		copy(secret[:], mac.Sum(nil))

		var s btcec.ModNScalar
		if overflow := s.SetByteSlice(secret[:]); !overflow &&
			!s.IsZero() {

			return &secret, nil
		}
	}

	return nil, fmt.Errorf("no valid secret for index %d", v)
}

// Encode writes the root, in the same format as RevocationProducer.Encode.
//
// NOTE: This function is part of the Producer interface.
func (p *IndependentProducer) Encode(w io.Writer) error {
	_, err := w.Write(p.root[:])
	return err
}
