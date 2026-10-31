package channeldb

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestSpliceFundingRoundTrip checks the splice funding record: a channel
// never spliced reports its original funding outpoint, and one with the
// record keeps it through the database while FundingOutpoint still names the
// channel.
func TestSpliceFundingRoundTrip(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	plain := createTestChannel(t, cdb, openChannelOption())
	require.Equal(t, plain.FundingOutpoint, plain.CurrentFundingOutpoint())
	require.True(t, plain.SpliceFundingInfo().IsNone())

	spliced := SpliceFunding{
		OutPoint: wire.OutPoint{Hash: chainhash.Hash{0x51}, Index: 2},
		Scid: lnwire.ShortChannelID{
			BlockHeight: 961_700, TxIndex: 7, TxPosition: 2,
		},
	}
	channel := createTestChannel(
		t, cdb, openChannelOption(),
		func(p *testChannelParams) {
			p.channel.spliceFunding = fn.Some(spliced)
		},
	)

	fetched, err := cdb.FetchChannel(channel.FundingOutpoint)
	require.NoError(t, err)
	require.Equal(t, spliced.OutPoint, fetched.CurrentFundingOutpoint())
	require.Equal(t, fn.Some(spliced), fetched.SpliceFundingInfo())

	// The channel is still found, and named, by its original outpoint.
	require.Equal(t, channel.FundingOutpoint, fetched.FundingOutpoint)
	require.NotEqual(t, fetched.FundingOutpoint,
		fetched.CurrentFundingOutpoint())

	// A refresh keeps it.
	require.NoError(t, fetched.Refresh())
	require.Equal(t, spliced.OutPoint, fetched.CurrentFundingOutpoint())
}

// TestSpliceFundingEncoding checks the record's layout and its refusal of a
// wrong length.
func TestSpliceFundingEncoding(t *testing.T) {
	t.Parallel()

	f := SpliceFunding{
		OutPoint: wire.OutPoint{Hash: chainhash.Hash{1, 2, 3}, Index: 9},
		Scid:     lnwire.NewShortChanIDFromInt(0x0102030405060708),
	}
	b := encodeSpliceFunding(f)
	require.Len(t, b, spliceFundingSize)

	got, err := decodeSpliceFunding(b)
	require.NoError(t, err)
	require.Equal(t, f, got)

	_, err = decodeSpliceFunding(b[:spliceFundingSize-1])
	require.Error(t, err)
}

// testSpliceLock returns a splice lock onto a new funding output with fresh
// funding keys.
func testSpliceLock(t *testing.T, txid byte,
	capacity btcutil.Amount) SpliceLock {

	t.Helper()

	key := func() keychain.KeyDescriptor {
		priv, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyMultiSig, Index: 7,
			},
			PubKey: priv.PubKey(),
		}
	}

	return SpliceLock{
		Funding: SpliceFunding{
			OutPoint: wire.OutPoint{
				Hash: chainhash.Hash{txid}, Index: 1,
			},
			Scid: lnwire.ShortChannelID{
				BlockHeight: 961_000 + uint32(txid), TxIndex: 3,
				TxPosition: 1,
			},
		},
		Capacity:          capacity,
		LocalMultiSigKey:  key(),
		RemoteMultiSigKey: key(),
	}
}

// TestLockSplice checks that a locked splice moves the channel's funding
// output, capacity and keys, in memory and on disk, and that the channel is
// then found by every funding output it has had while keeping its name.
func TestLockSplice(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	channel := createTestChannel(t, cdb, openChannelOption())
	name := channel.FundingOutpoint
	scid := channel.ShortChannelID

	first := testSpliceLock(t, 0x61, 3_000_000)
	require.NoError(t, channel.LockSplice(first))

	check := func(c *OpenChannel, l SpliceLock) {
		t.Helper()

		require.Equal(t, name, c.FundingOutpoint)
		require.Equal(t, scid, c.ShortChannelID)
		require.Equal(t, l.Funding.OutPoint, c.CurrentFundingOutpoint())
		require.Equal(t, fn.Some(l.Funding), c.SpliceFundingInfo())
		require.Equal(t, l.Capacity, c.Capacity)
		require.True(t, l.LocalMultiSigKey.PubKey.IsEqual(
			c.LocalChanCfg.MultiSigKey.PubKey,
		))
		require.Equal(t, l.LocalMultiSigKey.KeyLocator,
			c.LocalChanCfg.MultiSigKey.KeyLocator)
		require.True(t, l.RemoteMultiSigKey.PubKey.IsEqual(
			c.RemoteChanCfg.MultiSigKey.PubKey,
		))
	}
	check(channel, first)

	for _, point := range []wire.OutPoint{name, first.Funding.OutPoint} {
		fetched, err := cdb.FetchChannel(point)
		require.NoError(t, err)
		check(fetched, first)
	}

	// A second splice: every funding output the channel has had still
	// finds it.
	second := testSpliceLock(t, 0x62, 2_500_000)
	require.NoError(t, channel.LockSplice(second))
	for _, point := range []wire.OutPoint{
		name, first.Funding.OutPoint, second.Funding.OutPoint,
	} {
		fetched, err := cdb.FetchChannel(point)
		require.NoError(t, err)
		check(fetched, second)
	}

	// An unknown outpoint is still not found.
	_, err = cdb.FetchChannel(wire.OutPoint{Hash: chainhash.Hash{0x63}})
	require.ErrorIs(t, err, ErrChannelNotFound)

	// Once closed, the channel is found by none of them.
	require.NoError(t, channel.CloseChannel(&ChannelCloseSummary{
		ChanPoint: name, RemotePub: channel.IdentityPub,
	}))
	for _, point := range []wire.OutPoint{
		name, first.Funding.OutPoint, second.Funding.OutPoint,
	} {
		_, err := cdb.FetchChannel(point)
		require.ErrorIs(t, err, ErrChannelNotFound)
	}
}

// TestLockSpliceRefused checks that a splice can't move a channel onto an
// outpoint that names a channel or already funds another one, and that a
// refused lock changes nothing.
func TestLockSpliceRefused(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	a := createTestChannel(t, cdb, openChannelOption())
	b := createTestChannel(
		t, cdb, openChannelOption(), fundingPointOption(wire.OutPoint{
			Hash: chainhash.Hash{0x71}, Index: 0,
		}),
	)

	// Onto its own name, or another channel's.
	own := testSpliceLock(t, 0x72, 1_000_000)
	own.Funding.OutPoint = a.FundingOutpoint
	require.ErrorIs(t, a.LockSplice(own), ErrSpliceFundingInUse)

	other := testSpliceLock(t, 0x73, 1_000_000)
	other.Funding.OutPoint = b.FundingOutpoint
	require.ErrorIs(t, a.LockSplice(other), ErrSpliceFundingInUse)

	// Onto another channel's spliced funding.
	moved := testSpliceLock(t, 0x74, 1_000_000)
	require.NoError(t, b.LockSplice(moved))
	require.ErrorIs(t, a.LockSplice(moved), ErrSpliceFundingInUse)

	// Without the peer's key.
	noKey := testSpliceLock(t, 0x75, 1_000_000)
	noKey.RemoteMultiSigKey.PubKey = nil
	require.Error(t, a.LockSplice(noKey))

	// Nothing changed on a.
	fetched, err := cdb.FetchChannel(a.FundingOutpoint)
	require.NoError(t, err)
	require.True(t, fetched.SpliceFundingInfo().IsNone())
	require.Equal(t, a.Capacity, fetched.Capacity)
	require.True(t, a.SpliceFundingInfo().IsNone())

	// The same lock again is fine (a retry after a crash).
	require.NoError(t, b.LockSplice(moved))
}

// TestLockSplicePreviousFundings checks that each lock keeps the funding the
// channel moved away from, with its keys, and that a repeated lock adds
// nothing.
func TestLockSplicePreviousFundings(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())
	original := channel.FundingOutpoint
	originalKey := channel.LocalChanCfg.MultiSigKey
	originalCapacity := channel.Capacity

	first := testSpliceLock(t, 0x81, 3_000_000)
	require.NoError(t, channel.LockSplice(first))
	require.NoError(t, channel.LockSplice(first))
	second := testSpliceLock(t, 0x82, 2_000_000)
	require.NoError(t, channel.LockSplice(second))

	prev, err := channel.PreviousFundings()
	require.NoError(t, err)
	require.Len(t, prev, 2)

	byOutpoint := make(map[wire.OutPoint]PreviousFunding)
	for _, f := range prev {
		byOutpoint[f.OutPoint] = f
	}
	f := byOutpoint[original]
	require.Equal(t, originalCapacity, f.Capacity)
	require.Equal(t, first.Funding.OutPoint, f.LockedTo)
	require.True(t, originalKey.PubKey.IsEqual(f.LocalKey.PubKey))
	require.Equal(t, originalKey.KeyLocator, f.LocalKey.KeyLocator)

	require.Equal(t, channel.ShortChannelID, f.Scid)

	f = byOutpoint[first.Funding.OutPoint]
	require.Equal(t, first.Capacity, f.Capacity)
	require.Equal(t, second.Funding.OutPoint, f.LockedTo)
	require.True(t, first.RemoteMultiSigKey.PubKey.IsEqual(f.RemoteKey))
	require.Equal(t, first.Funding.Scid, f.Scid)

	// Every scid the channel has had besides its name, the current one
	// included.
	require.Equal(t, second.Funding.Scid, channel.CurrentShortChanID())
	scids, err := channel.SpliceScids()
	require.NoError(t, err)
	require.ElementsMatch(t, []lnwire.ShortChannelID{
		first.Funding.Scid, second.Funding.Scid,
	}, scids)
}

// TestLockSpliceSwapsCommitments checks a lock with commitments: the
// channel's commitments, and a pending one with our signatures from the
// splice's record, move to the new funding in the same transaction, the
// splice records go, and inconsistent locks change nothing.
func TestLockSpliceSwapsCommitments(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()
	channel := createTestChannel(t, cdb)

	r := testSpliceRecord(t, channel)
	lock := testSpliceLock(t, 0, 0)
	lock.Funding.OutPoint = r.OutPoint
	lock.Capacity = r.Capacity
	lock.LocalMultiSigKey = r.LocalKey
	lock.RemoteMultiSigKey = keychain.KeyDescriptor{PubKey: r.RemoteKey}

	local := spliceTestCommit(
		channel.LocalCommitment, channel.LocalCommitment.CommitHeight,
		r.OutPoint, 4e8,
	)
	remote := spliceTestCommit(
		channel.RemoteCommitment,
		channel.RemoteCommitment.CommitHeight, r.OutPoint, 3e8,
	)
	tip := spliceTestCommit(
		channel.RemoteCommitment,
		channel.RemoteCommitment.CommitHeight+1, r.OutPoint, 2e8,
	)
	tipSig := &lnwire.CommitSig{
		ChanID: lnwire.ChannelID{5}, CommitSig: wireSig,
		HtlcSigs: []lnwire.Sig{wireSig},
	}
	r.RemoteTip, r.RemoteTipSig = tip, tipSig
	require.NoError(t, channel.PutSpliceRecord(r))

	// A pending commitment for the peer on the current funding.
	pending := channel.RemoteCommitment
	pending.CommitHeight++
	require.NoError(t, channel.AppendRemoteCommitChain(&CommitDiff{
		Commitment: pending,
		CommitSig: &lnwire.CommitSig{
			ChanID: lnwire.ChannelID(key), CommitSig: wireSig,
		},
	}))

	// Inconsistent locks are refused and change nothing.
	bad := lock
	bad.LocalCommit, bad.RemoteCommit = local, remote
	require.Error(t, channel.LockSplice(bad), "without the tip")

	bad.RemoteTip = tip
	bad.RemoteCommit = nil
	require.Error(t, channel.LockSplice(bad), "without remote")

	bad.RemoteCommit = spliceTestCommit(
		channel.RemoteCommitment,
		channel.RemoteCommitment.CommitHeight+5, r.OutPoint, 3e8,
	)
	require.Error(t, channel.LockSplice(bad), "other height")

	fetched, err := cdb.FetchChannel(channel.FundingOutpoint)
	require.NoError(t, err)
	require.True(t, fetched.SpliceFundingInfo().IsNone())
	records, err := fetched.SpliceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)

	// The consistent lock.
	good := lock
	good.LocalCommit, good.RemoteCommit, good.RemoteTip = local, remote, tip
	require.NoError(t, channel.LockSplice(good))

	fetched, err = cdb.FetchChannel(r.OutPoint)
	require.NoError(t, err)
	require.Equal(t, r.OutPoint, fetched.CurrentFundingOutpoint())
	require.Equal(t, local.CommitTx.TxHash(),
		fetched.LocalCommitment.CommitTx.TxHash())
	require.Equal(t, remote.CommitTx.TxHash(),
		fetched.RemoteCommitment.CommitTx.TxHash())

	diff, err := fetched.RemoteCommitChainTip()
	require.NoError(t, err)
	require.Equal(t, tip.CommitTx.TxHash(),
		diff.Commitment.CommitTx.TxHash())
	requireSigEqual(t, tipSig, diff.CommitSig)

	// The splice's record is gone; the funding left behind stays as a
	// retired record with the peer's commitments on it, unrevoked at the
	// lock.
	records, err = fetched.SpliceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	retired := records[0]
	require.True(t, retired.Retired)
	require.Equal(t, channel.FundingOutpoint, retired.OutPoint)
	oldTail := retired.RemoteTail
	require.Equal(t, remote.CommitHeight, oldTail.CommitHeight)
	require.NotEqual(t, remote.CommitTx.TxHash(),
		oldTail.CommitTx.TxHash())
	require.Equal(t, pending.CommitTx.TxHash(),
		retired.RemoteTip.CommitTx.TxHash())

	// The peer revokes both: each goes into the old funding's log, then
	// the record goes.
	retiredTxid := channel.FundingOutpoint.Hash
	for i, height := range []uint64{
		oldTail.CommitHeight, oldTail.CommitHeight + 1,
	} {
		next, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		nextPoint := channel.RemoteNextRevocation.UnwrapOrFail(t)
		channel.RemoteCurrentRevocation = &nextPoint
		channel.RemoteNextRevocation = fn.Some(*next.PubKey())

		if i == 1 {
			pending := channel.RemoteCommitment
			pending.CommitHeight = height + 1
			require.NoError(t, channel.AppendRemoteCommitChain(
				&CommitDiff{
					Commitment: pending,
					CommitSig: &lnwire.CommitSig{
						ChanID:    lnwire.ChannelID(key),
						CommitSig: wireSig,
					},
				},
			))
		}
		require.NoError(t, channel.AdvanceCommitChainTail(
			NewFwdPkg(channel.ShortChanID(), height, nil, nil),
			nil, 0, 1, SpliceCommitWrite{
				Txid: retiredTxid, OurOutputIndex: 1,
				TheirOutputIndex: 0,
			},
		))

		rl, _, err := channel.FindPreviousStateFor(
			channel.FundingOutpoint, height,
		)
		require.NoError(t, err)
		require.EqualValues(t, 1, rl.OurOutputIndex.Val)

		// The new funding's commitment at that height is in the
		// channel's own log.
		own, _, err := channel.FindPreviousStateFor(r.OutPoint, height)
		require.NoError(t, err)
		require.NotEqual(t, own.CommitTxHash.Val, rl.CommitTxHash.Val)
	}

	rl, _, err := channel.FindPreviousStateFor(
		channel.FundingOutpoint, oldTail.CommitHeight,
	)
	require.NoError(t, err)
	require.Equal(t, [32]byte(oldTail.CommitTx.TxHash()), rl.CommitTxHash.Val)

	records, err = channel.SpliceRecords()
	require.NoError(t, err)
	require.Empty(t, records)
}

// TestSpliceFundingCopy checks that a copy of a spliced channel reports the
// same current funding: code that works from a copy would otherwise watch or
// name the funding the splice spent.
func TestSpliceFundingCopy(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())

	lock := testSpliceLock(t, 0x91, 3_000_000)
	require.NoError(t, channel.LockSplice(lock))

	clone := channel.Copy()
	require.Equal(t, lock.Funding.OutPoint, clone.CurrentFundingOutpoint())
	require.Equal(t, fn.Some(lock.Funding), clone.SpliceFundingInfo())
}

// TestSpliceReserve checks the reserves of a spliced channel: none for one
// never spliced, which keeps those of its opening; 1% of the capacity after
// a splice, or the dust limit if more (ours, for a channel we opened). A
// lock itself changes neither side's stored reserve: the peer's channel
// process keeps what it started with until it restarts.
func TestSpliceReserve(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	cdb := fullDB.ChannelStateDB()

	channel := createTestChannel(t, cdb, openChannelOption(),
		func(p *testChannelParams) {
			p.channel.IsInitiator = true
			p.channel.LocalChanCfg.DustLimit = 354
			p.channel.RemoteChanCfg.DustLimit = 500
			p.channel.LocalChanCfg.ChanReserve = 20_000
			p.channel.RemoteChanCfg.ChanReserve = 20_000
		})
	_, ok := channel.SpliceReserve()
	require.False(t, ok)

	require.NoError(t, channel.LockSplice(
		testSpliceLock(t, 0x71, 3_000_000),
	))
	reserve, ok := channel.SpliceReserve()
	require.True(t, ok)
	require.Equal(t, btcutil.Amount(30_000), reserve)
	require.Equal(t, btcutil.Amount(20_000), channel.LocalChanCfg.ChanReserve)
	require.Equal(t, btcutil.Amount(20_000), channel.RemoteChanCfg.ChanReserve)

	// Read back.
	fetched, err := cdb.FetchChannel(channel.FundingOutpoint)
	require.NoError(t, err)
	reserve, ok = fetched.SpliceReserve()
	require.True(t, ok)
	require.Equal(t, btcutil.Amount(30_000), reserve)

	// Below the dust limit: the opener's, ours as we opened the
	// channel, the peer's had it.
	require.NoError(t, channel.LockSplice(
		testSpliceLock(t, 0x72, 20_000),
	))
	reserve, _ = channel.SpliceReserve()
	require.Equal(t, btcutil.Amount(354), reserve)
	channel.IsInitiator = false
	reserve, _ = channel.SpliceReserve()
	require.Equal(t, btcutil.Amount(500), reserve)
}
