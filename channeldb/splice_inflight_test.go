package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestSpliceNegotiationInFlight checks the values each node set for a splice
// attempt through the negotiation's encoding, that a negotiation without
// them reads zero, and that a decoder that doesn't know them, as an older
// build's, skips them.
func TestSpliceNegotiationInFlight(t *testing.T) {
	t.Parallel()

	n := &SpliceNegotiation{
		FeeRate:          253,
		OurMaxInFlight:   1_980_000_000,
		TheirMaxInFlight: 990_000_001,
	}
	var b bytes.Buffer
	require.NoError(t, n.encode(&b))
	encoded := b.Bytes()

	got, err := decodeSpliceNegotiation(bytes.NewReader(encoded))
	require.NoError(t, err)
	require.Equal(t, n.OurMaxInFlight, got.OurMaxInFlight)
	require.Equal(t, n.TheirMaxInFlight, got.TheirMaxInFlight)
	require.Equal(t, n.FeeRate, got.FeeRate)

	// Without them, nothing is written for them and they read zero.
	b.Reset()
	require.NoError(t, (&SpliceNegotiation{FeeRate: 253}).encode(&b))
	require.NotContains(t, string(b.Bytes()), string([]byte{19, 8}))
	got, err = decodeSpliceNegotiation(bytes.NewReader(b.Bytes()))
	require.NoError(t, err)
	require.Zero(t, got.OurMaxInFlight)
	require.Zero(t, got.TheirMaxInFlight)

	// An older build's decoder has no records 19 and 21: odd, they are
	// skipped.
	var feeRate uint64
	old, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(negFeeRateType, &feeRate),
	)
	require.NoError(t, err)
	require.NoError(t, old.Decode(bytes.NewReader(encoded)))
	require.Equal(t, uint64(253), feeRate)
}

// TestSpliceInFlightEncoding checks SpliceInFlight's encoding, and that an
// odd record it doesn't know is skipped.
func TestSpliceInFlightEncoding(t *testing.T) {
	t.Parallel()

	r := SpliceInFlight{
		OpeningOurs:   990_000_000,
		OpeningTheirs: 980_000_000,
		Funding: wire.OutPoint{
			Hash: chainhash.Hash{0x91}, Index: 2,
		},
		CurrentOurs:   2_970_000_000,
		CurrentTheirs: 1,
	}
	value, err := encodeSpliceInFlight(r)
	require.NoError(t, err)
	got, err := decodeSpliceInFlight(value)
	require.NoError(t, err)
	require.Equal(t, r, got)

	// A later record type, odd, as a later build may add.
	value = append(value, 11, 1, 0xaa)
	got, err = decodeSpliceInFlight(value)
	require.NoError(t, err)
	require.Equal(t, r, got)

	_, err = decodeSpliceInFlight([]byte{1, 8, 0})
	require.Error(t, err)
}

// TestPreviousFundingInFlightCompat checks that a PreviousFunding written
// before the values were kept, with or without its scid, reads them as zero,
// and that one written now keeps them.
func TestPreviousFundingInFlightCompat(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())
	key := channel.LocalChanCfg.MultiSigKey
	remote := channel.RemoteChanCfg.MultiSigKey.PubKey

	// What putPreviousFunding wrote before each field was added.
	write := func(op wire.OutPoint, withScid bool) {
		t.Helper()

		var k, v bytes.Buffer
		require.NoError(t, graphdb.WriteOutpoint(&k, &op))
		lockedTo := wire.OutPoint{Hash: chainhash.Hash{0xee}}
		require.NoError(t, graphdb.WriteOutpoint(&v, &lockedTo))
		require.NoError(t, WriteElements(&v, uint64(1_000_000),
			uint32(key.Family), key.Index, key.PubKey, remote))
		if withScid {
			require.NoError(t, WriteElements(&v, uint64(77)))
		}
		err := kvdb.Update(fullDB.ChannelStateDB().backend,
			func(tx kvdb.RwTx) error {
				chanBucket, err := fetchChanBucketRw(
					tx, channel.IdentityPub,
					&channel.FundingOutpoint,
					channel.ChainHash,
				)
				if err != nil {
					return err
				}
				bucket, err := chanBucket.
					CreateBucketIfNotExists(
						splicePreviousFundingsBucket,
					)
				if err != nil {
					return err
				}

				return bucket.Put(k.Bytes(), v.Bytes())
			}, func() {})
		require.NoError(t, err)
	}
	noScid := wire.OutPoint{Hash: chainhash.Hash{0xa1}}
	withScid := wire.OutPoint{Hash: chainhash.Hash{0xa2}}
	write(noScid, false)
	write(withScid, true)

	prev, err := channel.PreviousFundings()
	require.NoError(t, err)
	require.Len(t, prev, 2)
	for _, p := range prev {
		require.Zero(t, p.OurMaxInFlight)
		require.Zero(t, p.TheirMaxInFlight)
		if p.OutPoint == withScid {
			require.Equal(t, uint64(77), p.Scid.ToUint64())
		} else {
			require.Zero(t, p.Scid.ToUint64())
		}
	}
}

// inFlightSplice stores a pending splice record for a lock, with the values
// each node set for it.
func inFlightSplice(t *testing.T, channel *OpenChannel, l SpliceLock,
	ours, theirs lnwire.MilliSatoshi) {

	t.Helper()

	r := testSpliceRecord(t, channel)
	r.OutPoint = l.Funding.OutPoint
	r.Negotiation = &SpliceNegotiation{
		FeeRate:          253,
		OurMaxInFlight:   ours,
		TheirMaxInFlight: theirs,
	}
	require.NoError(t, channel.PutSpliceRecord(r))
}

// TestSpliceMaxInFlight checks the values kept across locks: the opening
// ones recorded once, each splice's moved to its PreviousFunding at the next
// lock, and the greatest along the chain of transactions a funding spends.
func TestSpliceMaxInFlight(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()
	channel := createTestChannel(t, cdb)
	original := channel.FundingOutpoint

	// The opening values, as stored: "ours" is what this node requires
	// of the peer.
	openOurs := channel.RemoteChanCfg.MaxPendingAmount
	openTheirs := channel.LocalChanCfg.MaxPendingAmount

	ours, theirs, known, err := channel.SpliceMaxInFlight(original)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, openOurs, ours)
	require.Equal(t, openTheirs, theirs)

	// Recorded the first time: values later applied to the config in
	// memory don't change them.
	channel.RemoteChanCfg.MaxPendingAmount = 1
	channel.LocalChanCfg.MaxPendingAmount = 1
	rec, err := channel.SpliceInFlight()
	require.NoError(t, err)
	require.Equal(t, openOurs, rec.OpeningOurs)
	require.Equal(t, openTheirs, rec.OpeningTheirs)

	// A first splice raises both nodes' values; a second, smaller,
	// raises ours only, above the first's; a third, pending, raises
	// theirs above everything.
	high := max(openOurs, openTheirs) + 1_000
	first := testSpliceLock(t, 0x91, 3_000_000)
	inFlightSplice(t, channel, first, high, high+1)
	require.NoError(t, channel.LockSplice(first))

	ours, theirs, known, err = channel.SpliceMaxInFlight(
		first.Funding.OutPoint,
	)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, high, ours)
	require.Equal(t, high+1, theirs)

	second := testSpliceLock(t, 0x92, 2_000_000)
	inFlightSplice(t, channel, second, high+5, 0)
	require.NoError(t, channel.LockSplice(second))

	// The same lock again (a retry after a crash) changes nothing.
	require.NoError(t, channel.LockSplice(second))

	prev, err := channel.PreviousFundings()
	require.NoError(t, err)
	byOutpoint := make(map[wire.OutPoint]PreviousFunding)
	for _, p := range prev {
		byOutpoint[p.OutPoint] = p
	}
	require.Zero(t, byOutpoint[original].OurMaxInFlight)
	require.Zero(t, byOutpoint[original].TheirMaxInFlight)
	firstPrev := byOutpoint[first.Funding.OutPoint]
	require.Equal(t, high, firstPrev.OurMaxInFlight)
	require.Equal(t, high+1, firstPrev.TheirMaxInFlight)

	rec, err = channel.SpliceInFlight()
	require.NoError(t, err)
	require.Equal(t, second.Funding.OutPoint, rec.Funding)
	require.Equal(t, high+5, rec.CurrentOurs)
	require.Zero(t, rec.CurrentTheirs)
	require.Equal(t, openOurs, rec.OpeningOurs)

	check := func(op wire.OutPoint, wantOurs,
		wantTheirs lnwire.MilliSatoshi, wantKnown bool) {

		t.Helper()

		ours, theirs, known, err := channel.SpliceMaxInFlight(op)
		require.NoError(t, err)
		require.Equal(t, wantKnown, known)
		require.Equal(t, wantOurs, ours)
		require.Equal(t, wantTheirs, theirs)
	}
	check(second.Funding.OutPoint, high+5, high+1, true)
	check(first.Funding.OutPoint, high, high+1, true)
	check(original, openOurs, openTheirs, true)

	third := testSpliceLock(t, 0x93, 2_500_000)
	inFlightSplice(t, channel, third, 0, high+9)
	check(third.Funding.OutPoint, high+5, high+9, true)

	check(wire.OutPoint{Hash: chainhash.Hash{0x99}}, openOurs, openTheirs,
		false)

	// Read back from disk, as at a restart.
	fetched, err := cdb.FetchChannel(original)
	require.NoError(t, err)
	ours, theirs, _, err = fetched.SpliceMaxInFlight(
		fetched.CurrentFundingOutpoint(),
	)
	require.NoError(t, err)
	require.Equal(t, high+5, ours)
	require.Equal(t, high+1, theirs)
}

// TestSpliceLockRecordsOpeningInFlight checks that a channel without its
// opening values recorded, as one opened before they were kept, records them
// from its stored config at its first lock, and that a splice without values
// leaves the cap where it was.
func TestSpliceLockRecordsOpeningInFlight(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())
	openOurs := channel.RemoteChanCfg.MaxPendingAmount
	openTheirs := channel.LocalChanCfg.MaxPendingAmount

	// A channel opened before the values were kept has no record.
	updateChanBucket(t, channel, func(b kvdb.RwBucket) error {
		return b.Delete(spliceInFlightKey)
	})

	// No record of the splice at all, as a lock without one.
	first := testSpliceLock(t, 0xa1, 3_000_000)
	require.NoError(t, channel.LockSplice(first))

	rec, err := channel.SpliceInFlight()
	require.NoError(t, err)
	require.Equal(t, openOurs, rec.OpeningOurs)
	require.Equal(t, openTheirs, rec.OpeningTheirs)
	require.Equal(t, first.Funding.OutPoint, rec.Funding)
	require.Zero(t, rec.CurrentOurs)
	require.Zero(t, rec.CurrentTheirs)

	ours, theirs, known, err := channel.SpliceMaxInFlight(
		first.Funding.OutPoint,
	)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, openOurs, ours)
	require.Equal(t, openTheirs, theirs)
}

// updateChanBucket runs f on the channel's bucket in a transaction of its
// own.
func updateChanBucket(t *testing.T, channel *OpenChannel,
	f func(kvdb.RwBucket) error) {

	t.Helper()

	err := kvdb.Update(channel.Db.backend, func(tx kvdb.RwTx) error {
		b, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return f(b)
	}, func() {})
	require.NoError(t, err)
}

// TestSpliceInFlightRecordedAtSave checks that a channel has its opening
// values recorded from its first save, before anything reads them, so values
// later set on its config don't count as the opening's.
func TestSpliceInFlightRecordedAtSave(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())
	openOurs := channel.RemoteChanCfg.MaxPendingAmount
	openTheirs := channel.LocalChanCfg.MaxPendingAmount

	var (
		rec   SpliceInFlight
		found bool
	)
	updateChanBucket(t, channel, func(b kvdb.RwBucket) error {
		var err error
		rec, found, err = fetchSpliceInFlight(b)

		return err
	})
	require.True(t, found)
	require.Equal(t, openOurs, rec.OpeningOurs)
	require.Equal(t, openTheirs, rec.OpeningTheirs)
	require.Equal(t, wire.OutPoint{}, rec.Funding)

	// Other values applied in memory, and even written back, aren't the
	// opening's.
	channel.RemoteChanCfg.MaxPendingAmount = openOurs * 3
	channel.LocalChanCfg.MaxPendingAmount = openTheirs * 3
	updateChanBucket(t, channel, func(b kvdb.RwBucket) error {
		return putOpenChannel(b, channel)
	})
	rec, err = channel.SpliceInFlight()
	require.NoError(t, err)
	require.Equal(t, openOurs, rec.OpeningOurs)
	require.Equal(t, openTheirs, rec.OpeningTheirs)
}

// TestSpliceInFlightOlderBuildLock checks a channel an older build locked a
// splice of in between: the values kept for the funding it moved away from,
// which it wrote no values for in that funding's PreviousFunding, still
// count, and the next lock writes them there.
func TestSpliceInFlightOlderBuildLock(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()
	channel := createTestChannel(t, cdb)
	openOurs := channel.RemoteChanCfg.MaxPendingAmount
	openTheirs := channel.LocalChanCfg.MaxPendingAmount
	high := max(openOurs, openTheirs) + 1_000

	// This build locks a first splice, which raised both values.
	first := testSpliceLock(t, 0xb1, 3_000_000)
	inFlightSplice(t, channel, first, high, high+1)
	require.NoError(t, channel.LockSplice(first))
	kept, err := channel.SpliceInFlight()
	require.NoError(t, err)
	require.Equal(t, first.Funding.OutPoint, kept.Funding)

	// An older build locks a second: it writes the first's
	// PreviousFunding without values and leaves the in-flight record as
	// it was.
	second := testSpliceLock(t, 0xb2, 3_500_000)
	require.NoError(t, channel.LockSplice(second))
	updateChanBucket(t, channel, func(b kvdb.RwBucket) error {
		prev, err := readPreviousFundings(b)
		if err != nil {
			return err
		}
		for _, p := range prev {
			if p.OutPoint != first.Funding.OutPoint {
				continue
			}
			p.OurMaxInFlight, p.TheirMaxInFlight = 0, 0
			if err := putPreviousFunding(b, &p); err != nil {
				return err
			}
		}

		return putSpliceInFlight(b, kept)
	})

	check := func(c *OpenChannel, op wire.OutPoint) {
		t.Helper()

		ours, theirs, known, err := c.SpliceMaxInFlight(op)
		require.NoError(t, err)
		require.True(t, known)
		require.Equal(t, high, ours)
		require.Equal(t, high+1, theirs)
	}
	check(channel, second.Funding.OutPoint)
	check(channel, first.Funding.OutPoint)

	// Back on this build, a third lock writes them into the first's
	// PreviousFunding, and they keep counting along the chain.
	third := testSpliceLock(t, 0xb3, 4_000_000)
	require.NoError(t, channel.LockSplice(third))

	prev, err := channel.PreviousFundings()
	require.NoError(t, err)
	var firstPrev PreviousFunding
	for _, p := range prev {
		if p.OutPoint == first.Funding.OutPoint {
			firstPrev = p
		}
	}
	require.Equal(t, high, firstPrev.OurMaxInFlight)
	require.Equal(t, high+1, firstPrev.TheirMaxInFlight)

	fetched, err := cdb.FetchChannel(channel.FundingOutpoint)
	require.NoError(t, err)
	check(fetched, third.Funding.OutPoint)
	check(fetched, second.Funding.OutPoint)
}

// TestSpliceMaxInFlightClosed checks that a closed channel's values are
// unknown, while a channel not yet saved still takes its opening ones.
func TestSpliceMaxInFlightClosed(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	// Not yet saved: the opening values, known.
	unsaved := createTestChannelState(t, cdb)
	ours, theirs, known, err := unsaved.SpliceMaxInFlight(
		unsaved.FundingOutpoint,
	)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, unsaved.RemoteChanCfg.MaxPendingAmount, ours)
	require.Equal(t, unsaved.LocalChanCfg.MaxPendingAmount, theirs)

	// Closed: unknown.
	channel := createTestChannel(t, cdb)
	err = channel.CloseChannel(&ChannelCloseSummary{
		ChanPoint: channel.FundingOutpoint,
		RemotePub: channel.IdentityPub,
		CloseType: CooperativeClose,
	})
	require.NoError(t, err)

	ours, theirs, known, err = channel.SpliceMaxInFlight(
		channel.FundingOutpoint,
	)
	require.NoError(t, err)
	require.False(t, known)
	require.Zero(t, ours)
	require.Zero(t, theirs)
}

// TestRecordOpeningInFlight checks the startup pass for channels opened
// before SpliceInFlight was kept: it records their opening values from the
// stored config, keeps a record already there, and passes over closed
// channels.
func TestRecordOpeningInFlight(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	older := createTestChannel(t, cdb)
	recorded := createTestChannel(t, cdb)
	closed := createTestChannel(t, cdb)

	stored := func(c *OpenChannel) (SpliceInFlight, bool) {
		var (
			rec   SpliceInFlight
			found bool
		)
		updateChanBucket(t, c, func(b kvdb.RwBucket) error {
			var err error
			rec, found, err = fetchSpliceInFlight(b)

			return err
		})

		return rec, found
	}

	// As an older build left them: no record.
	for _, c := range []*OpenChannel{older, closed} {
		updateChanBucket(t, c, func(b kvdb.RwBucket) error {
			return b.Delete(spliceInFlightKey)
		})
		_, found := stored(c)
		require.False(t, found)
	}
	err = closed.CloseChannel(&ChannelCloseSummary{
		ChanPoint: closed.FundingOutpoint,
		RemotePub: closed.IdentityPub,
		CloseType: CooperativeClose,
	})
	require.NoError(t, err)

	// One already recorded keeps its record.
	kept := SpliceInFlight{OpeningOurs: 7, OpeningTheirs: 8}
	updateChanBucket(t, recorded, func(b kvdb.RwBucket) error {
		return putSpliceInFlight(b, kept)
	})

	missing, err := cdb.channelsWithoutInFlight()
	require.NoError(t, err)
	require.Len(t, missing, 1)
	require.Equal(t, older.FundingOutpoint, missing[0].op)

	require.NoError(t, cdb.RecordOpeningInFlight())

	rec, found := stored(older)
	require.True(t, found)
	require.Equal(t, older.RemoteChanCfg.MaxPendingAmount, rec.OpeningOurs)
	require.Equal(t, older.LocalChanCfg.MaxPendingAmount, rec.OpeningTheirs)

	rec, found = stored(recorded)
	require.True(t, found)
	require.Equal(t, kept, rec)

	// Nothing left to record.
	missing, err = cdb.channelsWithoutInFlight()
	require.NoError(t, err)
	require.Empty(t, missing)
	require.NoError(t, cdb.RecordOpeningInFlight())
}
