package channeldb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/kvdb"
)

// spliceReservesKey, in a channel's bucket, holds its SpliceReserves.
var spliceReservesKey = []byte("splice-reserves")

// SpliceReserves records a spliced channel's reserves across connections:
// those it applied at its last connection, and, after a connection at which
// they changed, the ones before the change. BOLT 2 has a receiver accept an
// update_add_htlc or update_fee the earlier reserves allow until it receives
// its peer's first commitment_signed after the change, on that connection or
// a later one: updates sent again on reconnecting were added under them, and
// failing one would close the connection on every reconnection. It has to
// survive a restart for the same reason.
type SpliceReserves struct {
	// InForceLocal and InForceRemote are our reserve and the peer's as
	// applied at the channel's last connection.
	InForceLocal  btcutil.Amount
	InForceRemote btcutil.Amount

	// OpeningLocal and OpeningRemote are the reserves of the channel's
	// opening, as recorded the first time: a channel never spliced keeps
	// them. The channel's config can be written with other reserves
	// applied in memory, so they are not read back from it.
	OpeningLocal  btcutil.Amount
	OpeningRemote btcutil.Amount

	// PeerCommitSeen is set once the peer's first commitment_signed since
	// the reserves changed has been received and revoked for, while the
	// leniency lasts: from then on its updates meet the current reserves,
	// on this connection or a later one.
	PeerCommitSeen bool

	// Lenient holds, from a connection at which the reserves changed
	// until the leniency it owes is over, every set of reserves in force
	// since the peer's last commitment_signed before the change, oldest
	// first. Empty otherwise.
	Lenient []ReservePair
}

// ReservePair is our channel reserve and the peer's.
type ReservePair struct {
	Local  btcutil.Amount
	Remote btcutil.Amount
}

// MaxLenientReserves bounds SpliceReserves.Lenient: each entry is a change
// of reserves, one per splice lock, with no commitment_signed from the peer
// in between, which a splice's own negotiation already makes rare.
const MaxLenientReserves = 8

// FetchSpliceReserves returns the channel's SpliceReserves, or false if it
// has none (never spliced, or spliced before they were recorded).
func (c *OpenChannel) FetchSpliceReserves() (SpliceReserves, bool, error) {
	c.RLock()
	defer c.RUnlock()

	var (
		r     SpliceReserves
		found bool
	)
	if c.Db == nil {
		return r, false, nil
	}

	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil

		case err != nil:
			return err
		}

		value := chanBucket.Get(spliceReservesKey)
		if value == nil {
			return nil
		}

		r, err = decodeSpliceReserves(value)
		if err != nil {
			return err
		}
		found = true

		return nil
	}, func() {
		r, found = SpliceReserves{}, false
	})

	return r, found, err
}

// PutSpliceReserves records the channel's SpliceReserves. A channel not yet
// saved, or one without a database (as in tests), keeps nothing.
func (c *OpenChannel) PutSpliceReserves(r SpliceReserves) error {
	c.Lock()
	defer c.Unlock()

	if c.Db == nil {
		return nil
	}

	value, err := encodeSpliceReserves(r)
	if err != nil {
		return err
	}

	return kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil

		case err != nil:
			return err
		}

		return chanBucket.Put(spliceReservesKey, value)
	}, func() {})
}

func encodeSpliceReserves(r SpliceReserves) ([]byte, error) {
	if len(r.Lenient) > MaxLenientReserves {
		r.Lenient = r.Lenient[len(r.Lenient)-MaxLenientReserves:]
	}

	var b bytes.Buffer
	err := WriteElements(&b, uint64(r.InForceLocal),
		uint64(r.InForceRemote), uint64(r.OpeningLocal),
		uint64(r.OpeningRemote), r.PeerCommitSeen,
		uint8(len(r.Lenient)))
	if err != nil {
		return nil, err
	}
	for _, p := range r.Lenient {
		err := WriteElements(&b, uint64(p.Local), uint64(p.Remote))
		if err != nil {
			return nil, err
		}
	}

	return b.Bytes(), nil
}

func decodeSpliceReserves(value []byte) (SpliceReserves, error) {
	var (
		r                     SpliceReserves
		local, remote         uint64
		openLocal, openRemote uint64
		n                     uint8
	)
	rd := bytes.NewReader(value)
	err := ReadElements(rd, &local, &remote, &openLocal, &openRemote,
		&r.PeerCommitSeen, &n)
	if err != nil {
		return r, fmt.Errorf("splice reserves: %w", err)
	}
	r.OpeningLocal = btcutil.Amount(openLocal)
	r.OpeningRemote = btcutil.Amount(openRemote)
	if n > MaxLenientReserves {
		return r, fmt.Errorf("splice reserves: %d lenient sets", n)
	}
	r.InForceLocal = btcutil.Amount(local)
	r.InForceRemote = btcutil.Amount(remote)
	for i := uint8(0); i < n; i++ {
		var l, rm uint64
		if err := ReadElements(rd, &l, &rm); err != nil {
			return r, fmt.Errorf("splice reserves: %w", err)
		}
		r.Lenient = append(r.Lenient, ReservePair{
			Local: btcutil.Amount(l), Remote: btcutil.Amount(rm),
		})
	}

	return r, nil
}
