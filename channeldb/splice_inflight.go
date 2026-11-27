package channeldb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// spliceInFlightKey, in a channel's bucket, holds its SpliceInFlight.
var spliceInFlightKey = []byte("splice-in-flight")

// SpliceInFlight keeps what a spliced channel's max_htlc_value_in_flight_msat
// depends on besides the records of its splices (bolts#20): the values of
// the channel's opening, and the values set for the splice transaction the
// channel is on now. BOLT 2 has each node's value in force be the greatest of
// the one it set at opening and every one it set for the transaction both
// nodes' locks agree on and for the splice transactions that transaction
// spends; those of earlier fundings are kept with their PreviousFunding,
// those of a splice not yet locked with its SpliceNegotiation.
//
// "Ours" is the value this node set, what it requires of the peer (the
// RemoteChanCfg's MaxPendingAmount at opening); "Theirs" is the one the peer
// set, what it requires of this node (the LocalChanCfg's).
type SpliceInFlight struct {
	// OpeningOurs and OpeningTheirs are the values of the channel's
	// opening, as recorded the first time. The channel's config can be
	// written with other values applied in memory, so they are not read
	// back from it once recorded.
	OpeningOurs   lnwire.MilliSatoshi
	OpeningTheirs lnwire.MilliSatoshi

	// Funding is the splice funding output CurrentOurs and CurrentTheirs
	// were set for: the channel's current one, as moved by the last lock
	// this node recorded. Zero if no splice has locked since they were
	// kept.
	Funding wire.OutPoint

	// CurrentOurs and CurrentTheirs are the values set for Funding's
	// transaction, zero if not set. At the next lock they go to the
	// PreviousFunding written for Funding.
	CurrentOurs   lnwire.MilliSatoshi
	CurrentTheirs lnwire.MilliSatoshi
}

// SpliceInFlight TLV types, odd so that a later record type an older build
// doesn't know is skipped.
const (
	inFlightOpeningOursType   tlv.Type = 1
	inFlightOpeningTheirsType tlv.Type = 3
	inFlightFundingType       tlv.Type = 5
	inFlightCurrentOursType   tlv.Type = 7
	inFlightCurrentTheirsType tlv.Type = 9
)

// openingInFlight returns the values of a channel's opening as its config
// holds them: only right before nothing has applied others to it.
func openingInFlight(c *OpenChannel) SpliceInFlight {
	return SpliceInFlight{
		OpeningOurs:   c.RemoteChanCfg.MaxPendingAmount,
		OpeningTheirs: c.LocalChanCfg.MaxPendingAmount,
	}
}

func encodeSpliceInFlight(r SpliceInFlight) ([]byte, error) {
	var funding bytes.Buffer
	if err := graphdb.WriteOutpoint(&funding, &r.Funding); err != nil {
		return nil, err
	}
	var (
		openOurs     = uint64(r.OpeningOurs)
		openTheirs   = uint64(r.OpeningTheirs)
		fundingBytes = funding.Bytes()
		curOurs      = uint64(r.CurrentOurs)
		curTheirs    = uint64(r.CurrentTheirs)
	)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(inFlightOpeningOursType, &openOurs),
		tlv.MakePrimitiveRecord(inFlightOpeningTheirsType, &openTheirs),
		tlv.MakePrimitiveRecord(inFlightFundingType, &fundingBytes),
		tlv.MakePrimitiveRecord(inFlightCurrentOursType, &curOurs),
		tlv.MakePrimitiveRecord(inFlightCurrentTheirsType, &curTheirs),
	)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func decodeSpliceInFlight(value []byte) (SpliceInFlight, error) {
	var (
		r                    SpliceInFlight
		openOurs, openTheirs uint64
		curOurs, curTheirs   uint64
		funding              []byte
	)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(inFlightOpeningOursType, &openOurs),
		tlv.MakePrimitiveRecord(inFlightOpeningTheirsType, &openTheirs),
		tlv.MakePrimitiveRecord(inFlightFundingType, &funding),
		tlv.MakePrimitiveRecord(inFlightCurrentOursType, &curOurs),
		tlv.MakePrimitiveRecord(inFlightCurrentTheirsType, &curTheirs),
	)
	if err != nil {
		return r, err
	}
	if err := stream.Decode(bytes.NewReader(value)); err != nil {
		return r, fmt.Errorf("splice in flight: %w", err)
	}
	if len(funding) > 0 {
		rd := bytes.NewReader(funding)
		if err := graphdb.ReadOutpoint(rd, &r.Funding); err != nil {
			return r, fmt.Errorf("splice in flight: %w", err)
		}
	}
	r.OpeningOurs = lnwire.MilliSatoshi(openOurs)
	r.OpeningTheirs = lnwire.MilliSatoshi(openTheirs)
	r.CurrentOurs = lnwire.MilliSatoshi(curOurs)
	r.CurrentTheirs = lnwire.MilliSatoshi(curTheirs)

	return r, nil
}

// fetchSpliceInFlight reads a channel's SpliceInFlight from its bucket, false
// if it has none.
func fetchSpliceInFlight(chanBucket kvdb.RBucket) (SpliceInFlight, bool,
	error) {

	value := chanBucket.Get(spliceInFlightKey)
	if value == nil {
		return SpliceInFlight{}, false, nil
	}
	r, err := decodeSpliceInFlight(value)
	if err != nil {
		return r, false, err
	}

	return r, true, nil
}

// putSpliceInFlight writes a channel's SpliceInFlight to its bucket.
func putSpliceInFlight(chanBucket kvdb.RwBucket, r SpliceInFlight) error {
	value, err := encodeSpliceInFlight(r)
	if err != nil {
		return err
	}

	return chanBucket.Put(spliceInFlightKey, value)
}

// spliceLockInFlight moves the values set for the channel's current funding
// into the PreviousFunding a lock writes for it, and keeps those set for the
// splice that locks as the current ones, in the lock's transaction. The
// splice's record, which holds them, must still be in the bucket. A channel
// with no SpliceInFlight yet records its opening values from its stored
// config first.
//
// An older build that locked a splice in between left Funding behind,
// recorded with no values in the PreviousFunding it wrote for it; they are
// written there, so the chain keeps them.
func spliceLockInFlight(chanBucket kvdb.RwBucket, channel *OpenChannel,
	previous *PreviousFunding, locked wire.OutPoint) error {

	rec, found, err := fetchSpliceInFlight(chanBucket)
	if err != nil {
		return err
	}
	if !found {
		rec = openingInFlight(channel)
	}

	// Kept for the funding being left only if they were set for it: a
	// channel that spliced before they were kept has none for it.
	set := rec.Funding != (wire.OutPoint{})
	switch {
	case set && rec.Funding == previous.OutPoint:
		previous.OurMaxInFlight = rec.CurrentOurs
		previous.TheirMaxInFlight = rec.CurrentTheirs

	case set:
		err := keepLeftInFlight(chanBucket, rec)
		if err != nil {
			return err
		}
	}

	rec.Funding = locked
	rec.CurrentOurs, rec.CurrentTheirs = 0, 0
	splice, err := fetchSpliceRecord(chanBucket, locked.Hash)
	switch {
	case errors.Is(err, ErrSpliceRecordNotFound):

	case err != nil:
		return err

	case splice.Negotiation != nil:
		rec.CurrentOurs = splice.Negotiation.OurMaxInFlight
		rec.CurrentTheirs = splice.Negotiation.TheirMaxInFlight
	}

	return putSpliceInFlight(chanBucket, rec)
}

// keepLeftInFlight writes rec's current values into the PreviousFunding of
// rec.Funding, when an older build wrote that without them.
func keepLeftInFlight(chanBucket kvdb.RwBucket, rec SpliceInFlight) error {
	if rec.CurrentOurs == 0 && rec.CurrentTheirs == 0 {
		return nil
	}
	previous, err := readPreviousFundings(chanBucket)
	if err != nil {
		return err
	}
	for _, p := range previous {
		if p.OutPoint != rec.Funding {
			continue
		}
		if p.OurMaxInFlight != 0 || p.TheirMaxInFlight != 0 {
			return nil
		}
		p.OurMaxInFlight = rec.CurrentOurs
		p.TheirMaxInFlight = rec.CurrentTheirs

		return putPreviousFunding(chanBucket, &p)
	}

	return nil
}

// SpliceInFlight returns the channel's SpliceInFlight. A channel has one from
// its first save, and lnd records it at startup for a channel opened before
// it was kept, before loading the channels. One still without it records it
// here, with the opening values its config holds in memory, right only while
// nothing has set others on the config. A channel held only in memory, as in
// tests, or not yet saved, gets them without a record.
//
// It changes nothing on the channel in memory, so a read lock is enough: the
// lazy record is written in one transaction that checks again for one
// written meanwhile.
func (c *OpenChannel) SpliceInFlight() (SpliceInFlight, error) {
	c.RLock()
	defer c.RUnlock()

	opening := openingInFlight(c)
	if c.Db == nil {
		return opening, nil
	}

	// unsaved is set for a channel not yet saved (during funding).
	unsaved := func(err error) bool {
		return errors.Is(err, ErrNoChanDBExists) ||
			errors.Is(err, ErrNoActiveChannels) ||
			errors.Is(err, ErrChannelNotFound)
	}

	var (
		rec   SpliceInFlight
		found bool
	)
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		case unsaved(err):
			rec, found = opening, true

			return nil

		case err != nil:
			return err
		}

		rec, found, err = fetchSpliceInFlight(chanBucket)

		return err
	}, func() {
		rec, found = SpliceInFlight{}, false
	})
	if err != nil || found {
		return rec, err
	}

	err = kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		case unsaved(err):
			rec = opening

			return nil

		case err != nil:
			return err
		}

		// Recorded meanwhile, through another copy of the channel.
		var found bool
		rec, found, err = fetchSpliceInFlight(chanBucket)
		if err != nil || found {
			return err
		}
		rec = opening

		return putSpliceInFlight(chanBucket, rec)
	}, func() {
		rec = SpliceInFlight{}
	})

	return rec, err
}

// SpliceMaxInFlight returns the max_htlc_value_in_flight_msat each node set
// that applies with a funding transaction of the channel as the reserve
// transaction (bolts#20): the greatest of the one it set at opening, the one
// it set for that transaction, and those it set for every splice transaction
// it spends. funding is the channel's current funding output, one it moved
// away from, or a pending splice's. ours is what this node requires of the
// peer, theirs what the peer requires of this node. known is false for a
// funding output the channel doesn't know, for which only the opening values
// count, and for a closed channel, whose values are gone (all zero then).
//
// It reads the channel's records as one snapshot. It only reads what was
// set: applying the values, and every rule about when they take effect, is
// the caller's.
func (c *OpenChannel) SpliceMaxInFlight(funding wire.OutPoint) (ours,
	theirs lnwire.MilliSatoshi, known bool, err error) {

	// Recorded first if it isn't yet.
	rec, err := c.SpliceInFlight()
	if err != nil {
		return 0, 0, false, err
	}

	c.RLock()
	defer c.RUnlock()

	snap := inFlightSnapshot{
		rec:      rec,
		original: c.FundingOutpoint,
		current: c.spliceFunding.UnwrapOr(SpliceFunding{
			OutPoint: c.FundingOutpoint,
		}).OutPoint,
	}
	if c.Db == nil {
		ours, theirs, known = snap.maxInFlight(funding)

		return ours, theirs, known, nil
	}

	closed := false
	err = kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		// Not among the open channels: closed, or not yet saved
		// (during funding), which takes the opening's.
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			closed, err = isClosedChannel(tx, &c.FundingOutpoint)

			return err

		case err != nil:
			return err
		}

		stored, err := fetchOpenChannel(chanBucket, &c.FundingOutpoint)
		if err != nil {
			return err
		}
		snap.current = stored.CurrentFundingOutpoint()
		r, found, err := fetchSpliceInFlight(chanBucket)
		if err != nil {
			return err
		}
		if found {
			snap.rec = r
		}
		snap.previous, err = readPreviousFundings(chanBucket)
		if err != nil {
			return err
		}
		snap.records, err = readSpliceRecords(chanBucket)

		return err
	}, func() {
		closed = false
		snap.previous, snap.records = nil, nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	// A closed channel's values are gone with it.
	if closed {
		return 0, 0, false, nil
	}
	ours, theirs, known = snap.maxInFlight(funding)

	return ours, theirs, known, nil
}

// inFlightSnapshot is what SpliceMaxInFlight reads, as of one moment.
type inFlightSnapshot struct {
	rec      SpliceInFlight
	original wire.OutPoint
	current  wire.OutPoint
	previous []PreviousFunding
	records  []*SpliceRecord
}

// maxInFlight returns SpliceMaxInFlight's values from the snapshot.
func (s inFlightSnapshot) maxInFlight(funding wire.OutPoint) (ours,
	theirs lnwire.MilliSatoshi, known bool) {

	rec := s.rec
	ours, theirs = rec.OpeningOurs, rec.OpeningTheirs
	add := func(o, t lnwire.MilliSatoshi) {
		ours, theirs = max(ours, o), max(theirs, t)
	}

	// The values kept for rec.Funding count wherever it is in the chain:
	// the current funding, or one an older build moved the channel away
	// from without keeping them in its PreviousFunding.
	addFunding := func(op wire.OutPoint) {
		if rec.Funding != (wire.OutPoint{}) && op == rec.Funding {
			add(rec.CurrentOurs, rec.CurrentTheirs)
		}
	}

	// The transaction's own values, and where the chain of transactions
	// it spends starts.
	from := funding
	switch {
	case funding == s.current:
		addFunding(funding)

	default:
		found := false
		for _, p := range s.previous {
			if p.OutPoint == funding {
				add(p.OurMaxInFlight, p.TheirMaxInFlight)
				addFunding(funding)
				found = true

				break
			}
		}
		if found || funding == s.original {
			break
		}

		// A splice not yet locked spends the current funding.
		for _, r := range s.records {
			if r.Retired || r.OutPoint != funding {
				continue
			}
			if r.Negotiation != nil {
				add(
					r.Negotiation.OurMaxInFlight,
					r.Negotiation.TheirMaxInFlight,
				)
			}
			found = true
		}
		if !found {
			return rec.OpeningOurs, rec.OpeningTheirs, false
		}
		addFunding(s.current)
		from = s.current
	}

	// Every earlier funding the chain spends, back to the original: each
	// PreviousFunding names the output its lock moved the channel to. A
	// chain is never longer than the records, which bounds a loop.
	for range len(s.previous) {
		next := false
		for _, p := range s.previous {
			if p.LockedTo == from {
				add(p.OurMaxInFlight, p.TheirMaxInFlight)
				addFunding(p.OutPoint)
				from = p.OutPoint
				next = true

				break
			}
		}
		if !next {
			break
		}
	}

	return ours, theirs, true
}

// isClosedChannel reports whether the channel of the funding outpoint given
// is closed: marked so in the outpoint index, as fetchChanBucket reads it, or
// with a close summary.
func isClosedChannel(tx kvdb.RTx, op *wire.OutPoint) (bool, error) {
	var b bytes.Buffer
	if err := graphdb.WriteOutpoint(&b, op); err != nil {
		return false, err
	}
	opBucket := tx.ReadBucket(outpointBucket)
	closed, err := isOutpointClosed(opBucket, b.Bytes())
	if err != nil || closed {
		return closed, err
	}
	closeBucket := tx.ReadBucket(closedChannelBucket)

	return closeBucket != nil && closeBucket.Get(b.Bytes()) != nil, nil
}

// RecordOpeningInFlight records, for every open channel that has no
// SpliceInFlight yet, its opening values from its stored config: for
// channels opened before it was kept, before anything sets others on a
// config in memory. A channel that has one is passed over without reading
// its state, so after the first run this only walks the buckets.
func (c *ChannelStateDB) RecordOpeningInFlight() error {
	missing, err := c.channelsWithoutInFlight()
	if err != nil || len(missing) == 0 {
		return err
	}

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		for _, m := range missing {
			chanBucket, err := fetchChanBucketRw(
				tx, m.node, &m.op, m.chain,
			)
			switch {
			// Closed or gone meanwhile.
			case errors.Is(err, ErrChannelNotFound):
				continue

			case err != nil:
				return err
			}
			if chanBucket.Get(spliceInFlightKey) != nil {
				continue
			}
			channel, err := fetchOpenChannel(chanBucket, &m.op)
			if err != nil {
				return err
			}
			err = putSpliceInFlight(
				chanBucket, openingInFlight(channel),
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// inFlightChan names a channel's bucket.
type inFlightChan struct {
	node  *btcec.PublicKey
	chain chainhash.Hash
	op    wire.OutPoint
}

// channelsWithoutInFlight returns the open channels that have no
// SpliceInFlight, without reading any channel's state.
func (c *ChannelStateDB) channelsWithoutInFlight() ([]inFlightChan, error) {
	var missing []inFlightChan
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		openChanBucket := tx.ReadBucket(openChannelBucket)
		if openChanBucket == nil {
			return nil
		}
		opBucket := tx.ReadBucket(outpointBucket)

		return openChanBucket.ForEach(func(nodeKey, v []byte) error {
			nodeBucket := openChanBucket.NestedReadBucket(nodeKey)
			if v != nil || nodeBucket == nil {
				return nil
			}
			node, err := btcec.ParsePubKey(nodeKey)
			if err != nil {
				return nil
			}

			return nodeBucket.ForEach(func(chain, v []byte) error {
				b := nodeBucket.NestedReadBucket(chain)
				if v != nil || b == nil {
					return nil
				}
				var hash chainhash.Hash
				if len(chain) != chainhash.HashSize {
					return nil
				}
				copy(hash[:], chain)

				return b.ForEach(func(op, v []byte) error {
					return missingInFlight(
						b, opBucket, node, hash, op,
						v, &missing,
					)
				})
			})
		})
	}, func() {
		missing = nil
	})

	return missing, err
}

// missingInFlight adds the channel of bucket key op in chainBucket to missing
// if it is open and has no SpliceInFlight.
func missingInFlight(chainBucket, opBucket kvdb.RBucket,
	node *btcec.PublicKey, chain chainhash.Hash, op, v []byte,
	missing *[]inFlightChan) error {

	chanBucket := chainBucket.NestedReadBucket(op)
	if v != nil || chanBucket == nil {
		return nil
	}
	if chanBucket.Get(spliceInFlightKey) != nil {
		return nil
	}
	closed, err := isOutpointClosed(opBucket, op)
	if err != nil || closed {
		return err
	}
	var outPoint wire.OutPoint
	err = graphdb.ReadOutpoint(bytes.NewReader(op), &outPoint)
	if err != nil {
		return err
	}
	*missing = append(*missing, inFlightChan{
		node: node, chain: chain, op: outPoint,
	})

	return nil
}
