package channeldb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// spliceFundingIndexBucket maps every funding outpoint a splice moved
	// a channel to onto the outpoint that names the channel, so that a
	// lookup by the output on chain (the graph's, a peer's) finds it.
	// Entries are never removed: a closed channel resolves to its name and
	// is then not found, as for any closed channel.
	//
	// funding outpoint -> original funding outpoint
	spliceFundingIndexBucket = []byte("splice-funding-index")

	// ErrSpliceFundingInUse is returned when a splice would move a
	// channel to a funding outpoint that already names or funded a
	// channel.
	ErrSpliceFundingInUse = errors.New("funding outpoint already in use")
)

// SpliceLock is what changes on a channel when a splice locks: the funding
// output, and with it the capacity and both 2-of-2 keys, since each splice
// uses a new funding key.
type SpliceLock struct {
	// Funding is the new funding output and its short channel id.
	Funding SpliceFunding

	// Capacity is the value of the new funding output.
	Capacity btcutil.Amount

	// LocalMultiSigKey is our key in the new funding output's script.
	LocalMultiSigKey keychain.KeyDescriptor

	// RemoteMultiSigKey is the peer's key in the new funding output's
	// script.
	RemoteMultiSigKey keychain.KeyDescriptor

	// LocalCommit and RemoteCommit, when set, replace the channel's
	// current commitments with the same ones on the new funding. Without
	// them only the funding moves (a channel being built in a test, or
	// restored).
	LocalCommit, RemoteCommit *ChannelCommitment

	// RemoteTip, when the peer has a commitment we signed and it hasn't
	// acknowledged, is that commitment on the new funding. It replaces
	// the pending one, with our signatures for it from the splice's
	// record, so a retransmission names the new funding.
	RemoteTip *ChannelCommitment
}

// LockSplice moves the channel to the funding output of a splice that has
// locked, in one transaction with the index that finds the channel by it.
// FundingOutpoint and ShortChannelID keep naming the channel.
func (c *OpenChannel) LockSplice(l SpliceLock) error {
	c.Lock()
	defer c.Unlock()

	if l.Funding.OutPoint == c.FundingOutpoint {
		return ErrSpliceFundingInUse
	}
	if l.LocalMultiSigKey.PubKey == nil ||
		l.RemoteMultiSigKey.PubKey == nil {

		return errors.New("splice lock without both funding keys")
	}

	var name bytes.Buffer
	if err := graphdb.WriteOutpoint(&name, &c.FundingOutpoint); err != nil {
		return err
	}
	var current bytes.Buffer
	err := graphdb.WriteOutpoint(&current, &l.Funding.OutPoint)
	if err != nil {
		return err
	}

	err = kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		// The new outpoint must not name a channel, nor already be
		// another channel's funding.
		opBucket := tx.ReadWriteBucket(outpointBucket)
		if opBucket != nil && opBucket.Get(current.Bytes()) != nil {
			return ErrSpliceFundingInUse
		}
		index, err := tx.CreateTopLevelBucket(spliceFundingIndexBucket)
		if err != nil {
			return err
		}
		prev := index.Get(current.Bytes())
		if prev != nil && !bytes.Equal(prev, name.Bytes()) {
			return ErrSpliceFundingInUse
		}

		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		channel, err := fetchOpenChannel(
			chanBucket, &c.FundingOutpoint,
		)
		if err != nil {
			return err
		}
		// Already moved: a retry after a crash.
		if channel.CurrentFundingOutpoint() == l.Funding.OutPoint {
			return nil
		}

		previous := PreviousFunding{
			OutPoint:  channel.CurrentFundingOutpoint(),
			Capacity:  channel.Capacity,
			LocalKey:  channel.LocalChanCfg.MultiSigKey,
			RemoteKey: channel.RemoteChanCfg.MultiSigKey.PubKey,
			LockedTo:  l.Funding.OutPoint,
			Scid:      channel.CurrentShortChanID(),
		}
		if l.LocalCommit != nil {
			err := swapSpliceCommitments(chanBucket, channel, l)
			if err != nil {
				return err
			}
		}
		applySpliceLock(channel, l)
		if err := putOpenChannel(chanBucket, channel); err != nil {
			return err
		}
		err = putPreviousFunding(chanBucket, &previous)
		if err != nil {
			return err
		}

		return index.Put(current.Bytes(), name.Bytes())
	}, func() {})
	if err != nil {
		return err
	}

	applySpliceLock(c, l)

	return nil
}

// SpliceReserve returns both sides' channel reserve once the channel has been
// spliced, as BOLT 2 has it for a channel whose capacity both sides funded
// and as the Core Lightning port sets it at each lock: 1% of the capacity,
// or the opener's dust limit if more (the Core Lightning port's is the
// chain's, 546). False for a channel never spliced, which keeps those of its
// opening.
//
// They take effect at the next connection, not at the lock nor after a
// tx_abort exchange, as the Core Lightning port now does too (bolts#15): the
// peer applies them when the channel is loaded for that connection
// (LightningChannel.ApplySpliceReserves), so both sides hold each other to
// the same.
func (c *OpenChannel) SpliceReserve() (btcutil.Amount, bool) {
	c.RLock()
	defer c.RUnlock()

	if c.spliceFunding.IsNone() {
		return 0, false
	}
	dust := c.RemoteChanCfg.DustLimit
	if c.IsInitiator {
		dust = c.LocalChanCfg.DustLimit
	}

	return max(c.Capacity/100, dust), true
}

// Reserves returns our channel reserve and the peer's, under the channel's
// lock: a spliced channel's change as the link applies them.
func (c *OpenChannel) Reserves() (btcutil.Amount, btcutil.Amount) {
	c.RLock()
	defer c.RUnlock()

	return c.LocalChanCfg.ChanReserve, c.RemoteChanCfg.ChanReserve
}

// SetReserves sets both sides' channel reserve, in memory: a spliced
// channel's, as SpliceReserve returns them.
func (c *OpenChannel) SetReserves(reserve btcutil.Amount) {
	c.Lock()
	defer c.Unlock()

	c.LocalChanCfg.ChanReserve = reserve
	c.RemoteChanCfg.ChanReserve = reserve
}

// applySpliceLock sets a splice lock's fields on a channel.
func applySpliceLock(c *OpenChannel, l SpliceLock) {
	c.spliceFunding = fn.Some(l.Funding)
	c.Capacity = l.Capacity
	c.LocalChanCfg.MultiSigKey = l.LocalMultiSigKey
	c.RemoteChanCfg.MultiSigKey = l.RemoteMultiSigKey
	if l.LocalCommit != nil {
		c.LocalCommitment = *l.LocalCommit
	}
	if l.RemoteCommit != nil {
		c.RemoteCommitment = *l.RemoteCommit
	}
}

// swapSpliceCommitments checks a lock's commitments against the channel's and
// moves the pending commitment diff, if any, to the new funding. The splice's
// record and every other pending splice's go: once one locks, the others
// can't confirm.
func swapSpliceCommitments(chanBucket kvdb.RwBucket, channel *OpenChannel,
	l SpliceLock) error {

	switch {
	case l.RemoteCommit == nil:
		return errors.New("splice lock with only our commitment")

	case l.LocalCommit.CommitHeight != channel.LocalCommitment.CommitHeight,
		l.RemoteCommit.CommitHeight !=
			channel.RemoteCommitment.CommitHeight:

		return errors.New("splice lock commitments at other heights")
	}

	txid := l.Funding.OutPoint.Hash
	record, err := fetchSpliceRecord(chanBucket, txid)
	if err != nil {
		return err
	}

	// The funding being left, with the peer's commitments on it not yet
	// revoked: they are logged as they are revoked.
	oldRemote := channel.RemoteCommitment
	retired := &SpliceRecord{
		OutPoint:   channel.CurrentFundingOutpoint(),
		Capacity:   channel.Capacity,
		LocalKey:   channel.LocalChanCfg.MultiSigKey,
		RemoteKey:  channel.RemoteChanCfg.MultiSigKey.PubKey,
		RemoteTail: &oldRemote,
		Retired:    true,
	}

	diffBytes := chanBucket.Get(commitDiffKey)
	switch {
	case diffBytes == nil && l.RemoteTip != nil:
		return errors.New("splice lock with a pending commitment the " +
			"channel doesn't have")

	case diffBytes != nil && l.RemoteTip == nil:
		return errors.New("splice lock without the pending commitment")

	case diffBytes != nil:
		diff, err := deserializeCommitDiff(bytes.NewReader(diffBytes))
		if err != nil {
			return err
		}
		if record.RemoteTipSig == nil ||
			diff.Commitment.CommitHeight != l.RemoteTip.CommitHeight {

			return errors.New("splice lock pending commitment " +
				"doesn't match")
		}
		oldTip := diff.Commitment
		retired.RemoteTip = &oldTip
		diff.Commitment = *l.RemoteTip
		diff.CommitSig = record.RemoteTipSig

		var b bytes.Buffer
		if err := serializeCommitDiff(&b, diff); err != nil {
			return err
		}
		if err := chanBucket.Put(commitDiffKey, b.Bytes()); err != nil {
			return err
		}
	}

	// The splice locked goes, and every other transaction of it,
	// replacements that lost, is retired: a reorganisation could still
	// confirm one. Earlier retired fundings stay until their commitments
	// are logged.
	bucket := chanBucket.NestedReadWriteBucket(spliceRecordsBucket)
	var others []chainhash.Hash
	err = bucket.ForEach(func(k, v []byte) error {
		r, err := decodeSpliceRecord(bytes.NewReader(v))
		if err != nil {
			return err
		}
		if !r.Retired && r.Txid() != txid {
			others = append(others, r.Txid())
		}

		return nil
	})
	if err != nil {
		return err
	}
	if err := deleteSpliceRecord(chanBucket, txid); err != nil {
		return err
	}
	for _, other := range others {
		if _, err := retireSpliceRecord(chanBucket, other); err != nil {
			return err
		}
	}

	return putSpliceRecord(chanBucket, retired)
}

// splicePreviousFundingsBucket keeps the funding outputs a channel moved
// away from, with their keys: until a splice is buried deep enough, a reorg
// can bring the previous funding back, and a revoked commitment on it must
// still be punished.
//
// previous funding outpoint -> PreviousFunding
var splicePreviousFundingsBucket = []byte("splice-previous-fundings")

// PreviousFunding is a funding output a channel moved away from by a splice.
type PreviousFunding struct {
	// OutPoint is the previous funding output.
	OutPoint wire.OutPoint

	// Capacity is its value.
	Capacity btcutil.Amount

	// LocalKey and RemoteKey are the 2-of-2 keys of its script.
	LocalKey  keychain.KeyDescriptor
	RemoteKey *btcec.PublicKey

	// LockedTo is the funding output the splice that spent it moved the
	// channel to.
	LockedTo wire.OutPoint

	// Scid is its short channel id. Payments may still name it: an
	// invoice's route hints carry the one current when it was issued.
	// Written last; zero in a record written without it.
	Scid lnwire.ShortChannelID
}

// putPreviousFunding records a funding output the channel moved away from.
func putPreviousFunding(chanBucket kvdb.RwBucket, f *PreviousFunding) error {
	bucket, err := chanBucket.CreateBucketIfNotExists(
		splicePreviousFundingsBucket,
	)
	if err != nil {
		return err
	}

	var key, value bytes.Buffer
	if err := graphdb.WriteOutpoint(&key, &f.OutPoint); err != nil {
		return err
	}
	if err := graphdb.WriteOutpoint(&value, &f.LockedTo); err != nil {
		return err
	}
	err = WriteElements(&value, uint64(f.Capacity),
		uint32(f.LocalKey.Family), f.LocalKey.Index, f.LocalKey.PubKey,
		f.RemoteKey, f.Scid.ToUint64(),
	)
	if err != nil {
		return err
	}

	return bucket.Put(key.Bytes(), value.Bytes())
}

// PreviousFundings returns the funding outputs the channel moved away from.
func (c *OpenChannel) PreviousFundings() ([]PreviousFunding, error) {
	c.RLock()
	defer c.RUnlock()

	if c.Db == nil {
		return nil, nil
	}

	var fundings []PreviousFunding
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		// A channel not yet saved (during funding) has none.
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil

		case err != nil:
			return err
		}
		bucket := chanBucket.NestedReadBucket(
			splicePreviousFundingsBucket,
		)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			var (
				f        PreviousFunding
				capacity uint64
				family   uint32
			)
			err := graphdb.ReadOutpoint(bytes.NewReader(k), &f.OutPoint)
			if err != nil {
				return err
			}
			r := bytes.NewReader(v)
			if err := graphdb.ReadOutpoint(r, &f.LockedTo); err != nil {
				return err
			}
			err = ReadElements(r, &capacity, &family,
				&f.LocalKey.Index, &f.LocalKey.PubKey, &f.RemoteKey,
			)
			if err != nil {
				return err
			}
			if r.Len() > 0 {
				var scid uint64
				if err := ReadElements(r, &scid); err != nil {
					return err
				}
				f.Scid = lnwire.NewShortChanIDFromInt(scid)
			}
			f.Capacity = btcutil.Amount(capacity)
			f.LocalKey.Family = keychain.KeyFamily(family)
			fundings = append(fundings, f)

			return nil
		})
	}, func() {
		fundings = nil
	})

	return fundings, err
}

// channelName returns the outpoint that names the channel funded, now or
// before a splice, by chanPoint: chanPoint itself unless a splice moved a
// channel to it.
func (c *ChannelStateDB) channelName(chanPoint wire.OutPoint) (wire.OutPoint,
	error) {

	var key bytes.Buffer
	if err := graphdb.WriteOutpoint(&key, &chanPoint); err != nil {
		return chanPoint, err
	}

	name := chanPoint
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		index := tx.ReadBucket(spliceFundingIndexBucket)
		if index == nil {
			return nil
		}
		v := index.Get(key.Bytes())
		if v == nil {
			return nil
		}

		return graphdb.ReadOutpoint(bytes.NewReader(v), &name)
	}, func() {
		name = chanPoint
	})
	if err != nil {
		return chanPoint, fmt.Errorf("splice funding index: %w", err)
	}

	return name, nil
}

// SpliceView returns a copy of the channel as it is on a pending splice's
// funding output: that output, its capacity and keys, and the commitments on
// it. For judging a spend of that output once the splice has confirmed and
// before it has locked. Writes through the copy reach the channel's own
// record, which is named by FundingOutpoint as always.
func (c *OpenChannel) SpliceView(r *SpliceRecord) *OpenChannel {
	view := c.Copy()
	view.Db = c.Db
	view.spliceFunding = fn.Some(SpliceFunding{OutPoint: r.OutPoint})
	view.Capacity = r.Capacity
	view.LocalChanCfg.MultiSigKey = r.LocalKey
	view.RemoteChanCfg.MultiSigKey = keychain.KeyDescriptor{
		PubKey: r.RemoteKey,
	}
	if r.LocalCommit != nil {
		view.LocalCommitment = *r.LocalCommit
	}
	if r.RemoteTail != nil {
		view.RemoteCommitment = *r.RemoteTail
	}

	return view
}

// ShellSplice is what a backup knew of a restored channel's splices.
type ShellSplice struct {
	// Funding, when a splice had moved the channel, is the funding output
	// it was on; the shell's capacity and 2-of-2 keys are that output's.
	Funding fn.Option[SpliceFunding]

	// Original is the funding output the channel was opened on, needed
	// with Funding: the channel's later backups describe it.
	Original *PreviousFunding

	// Candidates are the splices pending on it, without commitments: a
	// restored channel has none to publish.
	Candidates []*SpliceRecord
}

// restoreShellSplice writes what a backup knew of a restored channel's
// splices, in the transaction that writes the channel.
func restoreShellSplice(tx kvdb.RwTx, c *OpenChannel, s *ShellSplice) error {
	chanBucket, err := fetchChanBucketRw(
		tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
	)
	if err != nil {
		return err
	}

	if s.Funding.IsSome() {
		f := s.Funding.UnsafeFromSome()
		if s.Original == nil {
			return errors.New("restored splice without the " +
				"original funding")
		}
		original := *s.Original
		original.LockedTo = f.OutPoint
		original.Scid = c.ShortChannelID
		if err := putPreviousFunding(chanBucket, &original); err != nil {
			return err
		}

		var name, current bytes.Buffer
		err := graphdb.WriteOutpoint(&name, &c.FundingOutpoint)
		if err != nil {
			return err
		}
		if err := graphdb.WriteOutpoint(&current, &f.OutPoint); err != nil {
			return err
		}
		index, err := tx.CreateTopLevelBucket(spliceFundingIndexBucket)
		if err != nil {
			return err
		}
		prev := index.Get(current.Bytes())
		if prev != nil && !bytes.Equal(prev, name.Bytes()) {
			return ErrSpliceFundingInUse
		}
		if err := index.Put(current.Bytes(), name.Bytes()); err != nil {
			return err
		}
	}

	for _, r := range s.Candidates {
		if err := putSpliceRecord(chanBucket, r); err != nil {
			return err
		}
	}

	return nil
}

// SpliceScids returns the short channel ids the channel has had besides
// ShortChannelID, the current one included: a payment may name any of them.
func (c *OpenChannel) SpliceScids() ([]lnwire.ShortChannelID, error) {
	previous, err := c.PreviousFundings()
	if err != nil {
		return nil, err
	}

	name := c.ShortChanID()
	seen := map[lnwire.ShortChannelID]bool{name: true, {}: true}
	var scids []lnwire.ShortChannelID
	add := func(scid lnwire.ShortChannelID) {
		if !seen[scid] {
			seen[scid] = true
			scids = append(scids, scid)
		}
	}
	for _, pf := range previous {
		add(pf.Scid)
	}
	add(c.CurrentShortChanID())

	return scids, nil
}
