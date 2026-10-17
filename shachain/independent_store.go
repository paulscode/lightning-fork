package shachain

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

const (
	// independentStoreMarker is the first byte of an encoded
	// IndependentStore. An encoded RevocationStore starts with its bucket
	// count, which is at most maxHeight (48), so the two encodings can
	// never be confused.
	independentStoreMarker byte = 0xff

	// independentStoreVersion is the version of the IndependentStore
	// encoding.
	independentStoreVersion byte = 0x01
)

var (
	// ErrSecretNotRetained is returned by IndependentStore.LookUp for an
	// index other than the two most recent ones. On channels with
	// option_independent_secrets every revoked secret is persisted with
	// its revocation log entry instead, which is where callers must look
	// for older states.
	ErrSecretNotRetained = errors.New("per-commitment secret not " +
		"retained by the store; look it up in the revocation log")

	// errBadIndependentStore is returned when an encoded IndependentStore
	// is malformed.
	errBadIndependentStore = errors.New("malformed independent secret " +
		"store")
)

// IndependentStore is the Store used for channels that negotiated
// option_independent_secrets. On such channels the remote party may generate
// its per-commitment secrets by any method (from a seed, at random, or
// jointly across several signers), so the secrets can't be checked against,
// or compressed into, a shachain.
//
// The store therefore keeps only the two most recent secrets, which is all
// the channel needs in memory: channel_reestablish sends the last one back to
// the peer as your_last_per_commitment_secret. The one before it is kept so
// that a channel_reestablish built between receiving a revoke_and_ack and
// advancing the remote commitment (which still names the earlier height)
// can be answered too. Every secret is also persisted alongside the
// revocation log entry of the commitment it revokes, which is where a breach
// retribution reads it from.
type IndependentStore struct {
	// numEntries is the number of secrets added so far. The next secret
	// added gets this index.
	numEntries uint64

	// latest is the most recently added secret, at index numEntries-1.
	latest chainhash.Hash

	// previous is the secret added before latest, at index numEntries-2.
	previous chainhash.Hash
}

// A compile time check to ensure IndependentStore implements the Store
// interface.
var _ Store = (*IndependentStore)(nil)

// NewIndependentStore creates an empty IndependentStore.
func NewIndependentStore() *IndependentStore {
	return &IndependentStore{}
}

// LookUp returns the secret at index v, which must be one of the two most
// recently added. Older secrets are not retained in memory;
// ErrSecretNotRetained is returned for them.
//
// NOTE: This is part of the Store interface.
func (s *IndependentStore) LookUp(v uint64) (*chainhash.Hash, error) {
	if s.numEntries == 0 || v >= s.numEntries {
		return nil, fmt.Errorf("unable to get hash at index %d: no "+
			"such entry", v)
	}

	switch v {
	case s.numEntries - 1:
		latest := s.latest
		return &latest, nil

	case s.numEntries - 2:
		previous := s.previous
		return &previous, nil

	default:
		return nil, ErrSecretNotRetained
	}
}

// AddNextEntry records the next secret. Unlike RevocationStore it does not
// check that the secret derives the earlier ones: on these channels there is
// no such relationship. The caller is still responsible for checking that the
// secret generates the per-commitment point it revokes.
//
// NOTE: This is part of the Store interface.
func (s *IndependentStore) AddNextEntry(hash *chainhash.Hash) error {
	if hash == nil {
		return errors.New("nil per-commitment secret")
	}

	s.previous = s.latest
	s.latest = *hash
	s.numEntries++

	return nil
}

// NumEntries returns the number of secrets added to the store so far.
func (s *IndependentStore) NumEntries() uint64 {
	return s.numEntries
}

// Encode writes the store to w: a marker byte that tells it apart from an
// encoded RevocationStore, a version byte, the number of entries, then the
// most recent secret and the one before it, as far as they exist.
//
// NOTE: This is part of the Store interface.
func (s *IndependentStore) Encode(w io.Writer) error {
	if _, err := w.Write([]byte{
		independentStoreMarker, independentStoreVersion,
	}); err != nil {
		return err
	}

	err := binary.Write(w, binary.BigEndian, s.numEntries)
	if err != nil {
		return err
	}

	if s.numEntries == 0 {
		return nil
	}

	if _, err := w.Write(s.latest[:]); err != nil {
		return err
	}

	if s.numEntries == 1 {
		return nil
	}

	_, err = w.Write(s.previous[:])

	return err
}

// decodeIndependentStore reads an IndependentStore whose marker byte has
// already been consumed.
func decodeIndependentStore(r io.Reader) (*IndependentStore, error) {
	var version [1]byte
	if _, err := io.ReadFull(r, version[:]); err != nil {
		return nil, err
	}
	if version[0] != independentStoreVersion {
		return nil, fmt.Errorf("%w: unknown version %d",
			errBadIndependentStore, version[0])
	}

	s := &IndependentStore{}
	if err := binary.Read(r, binary.BigEndian, &s.numEntries); err != nil {
		return nil, err
	}

	if s.numEntries == 0 {
		return s, nil
	}

	if _, err := io.ReadFull(r, s.latest[:]); err != nil {
		return nil, err
	}

	if s.numEntries == 1 {
		return s, nil
	}

	if _, err := io.ReadFull(r, s.previous[:]); err != nil {
		return nil, err
	}

	return s, nil
}

// NewStoreFromBytes decodes a Store written by either RevocationStore.Encode
// or IndependentStore.Encode, telling them apart by the first byte. It reads
// exactly the bytes of the encoded store, so further data may follow it in r.
func NewStoreFromBytes(r io.Reader) (Store, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return nil, err
	}

	if first[0] == independentStoreMarker {
		return decodeIndependentStore(r)
	}

	return NewRevocationStoreFromBytes(
		io.MultiReader(bytes.NewReader(first[:]), r),
	)
}
