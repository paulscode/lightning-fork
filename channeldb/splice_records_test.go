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

// spliceTestCommit returns a commitment at height whose transaction spends
// the given funding outpoint, made distinct by balance.
func spliceTestCommit(base ChannelCommitment, height uint64,
	funding wire.OutPoint, balance lnwire.MilliSatoshi) *ChannelCommitment {

	c := base
	c.CommitHeight = height
	c.LocalBalance = balance
	c.CommitTx = base.CommitTx.Copy()
	c.CommitTx.TxIn[0].PreviousOutPoint = funding
	c.CommitTx.TxOut[0].Value = int64(balance.ToSatoshis())
	c.Htlcs = nil

	return &c
}

func testSpliceRecord(t *testing.T, channel *OpenChannel) *SpliceRecord {
	t.Helper()

	local, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	remote, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	op := wire.OutPoint{Hash: chainhash.Hash{0x5b, 1}, Index: 0}

	return &SpliceRecord{
		OutPoint: op,
		Capacity: channel.Capacity + 250_000,
		LocalKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyMultiSig, Index: 41,
			},
			PubKey: local.PubKey(),
		},
		RemoteKey:   remote.PubKey(),
		LocalDelta:  300_000,
		RemoteDelta: -50_000,
		RemoteTail: spliceTestCommit(
			channel.RemoteCommitment,
			channel.RemoteCommitment.CommitHeight, op, 7e8,
		),
		SpliceSig: &lnwire.CommitSig{
			ChanID:    lnwire.ChannelID{9},
			CommitSig: wireSig,
		},
	}
}

func requireSigEqual(t *testing.T, want, got *lnwire.CommitSig) {
	t.Helper()

	if want == nil {
		require.Nil(t, got)
		return
	}
	require.NotNil(t, got)
	require.Equal(t, want.ChanID, got.ChanID)
	require.Equal(t, want.CommitSig, got.CommitSig)
	require.Equal(t, len(want.HtlcSigs), len(got.HtlcSigs))
}

// TestSpliceRecordRoundTrip checks a record through the database: every
// field, negative deltas, absent optional parts, and its removal.
func TestSpliceRecordRoundTrip(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())

	r := testSpliceRecord(t, channel)
	require.NoError(t, channel.PutSpliceRecord(r))

	records, err := channel.SpliceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	got := records[0]

	require.Equal(t, r.OutPoint, got.OutPoint)
	require.Equal(t, r.Capacity, got.Capacity)
	require.Equal(t, r.LocalKey.KeyLocator, got.LocalKey.KeyLocator)
	require.True(t, r.LocalKey.PubKey.IsEqual(got.LocalKey.PubKey))
	require.True(t, r.RemoteKey.IsEqual(got.RemoteKey))
	require.Equal(t, r.LocalDelta, got.LocalDelta)
	require.Equal(t, r.RemoteDelta, got.RemoteDelta)
	require.Nil(t, got.LocalCommit)
	require.Nil(t, got.RemoteTip)
	require.Nil(t, got.RemoteTipSig)
	require.Equal(t, r.RemoteTail, got.RemoteTail)
	requireSigEqual(t, r.SpliceSig, got.SpliceSig)

	require.NoError(t, channel.DeleteSpliceRecord(r.Txid()))
	records, err = channel.SpliceRecords()
	require.NoError(t, err)
	require.Empty(t, records)
	require.ErrorIs(t, channel.DeleteSpliceRecord(r.Txid()),
		ErrSpliceRecordNotFound)

	// A record without keys is refused.
	r.RemoteKey = nil
	require.Error(t, channel.PutSpliceRecord(r))
}

// TestSpliceCommitWrites checks the writes a pending splice makes with each
// commitment chain step: in the same transaction as the step, and the
// revocation of the peer's commitment on the splice's funding into its own
// log.
func TestSpliceCommitWrites(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())

	r := testSpliceRecord(t, channel)
	require.NoError(t, channel.PutSpliceRecord(r))
	txid := r.Txid()

	fetch := func() *SpliceRecord {
		t.Helper()

		records, err := channel.SpliceRecords()
		require.NoError(t, err)
		require.Len(t, records, 1)

		return records[0]
	}

	// Our new commitment, with the one on the splice's funding.
	height := channel.LocalCommitment.CommitHeight + 1
	local := channel.LocalCommitment
	local.CommitHeight = height
	splLocal := spliceTestCommit(local, height, r.OutPoint, 6e8)
	_, err = channel.UpdateCommitment(&local, nil, SpliceCommitWrite{
		Txid: txid, LocalCommit: splLocal,
	})
	require.NoError(t, err)
	assertCommitmentEqual(t, splLocal, fetch().LocalCommit)

	// The peer's next commitment and our signatures for it.
	tailHeight := channel.RemoteCommitment.CommitHeight
	remote := channel.RemoteCommitment
	remote.CommitHeight = tailHeight + 1
	diff := &CommitDiff{
		Commitment: remote,
		CommitSig: &lnwire.CommitSig{
			ChanID: lnwire.ChannelID(key), CommitSig: wireSig,
		},
	}
	splTip := spliceTestCommit(remote, tailHeight+1, r.OutPoint, 5e8)
	tipSig := &lnwire.CommitSig{
		ChanID: lnwire.ChannelID(key), CommitSig: wireSig,
		HtlcSigs: []lnwire.Sig{wireSig},
	}

	// A write naming an unknown splice fails the whole step.
	err = channel.AppendRemoteCommitChain(diff, SpliceCommitWrite{
		Txid: chainhash.Hash{0xee}, RemoteTip: splTip,
		RemoteTipSig: tipSig,
	})
	require.ErrorIs(t, err, ErrSpliceRecordNotFound)
	_, err = channel.RemoteCommitChainTip()
	require.ErrorIs(t, err, ErrNoPendingCommit)

	require.NoError(t, channel.AppendRemoteCommitChain(
		diff, SpliceCommitWrite{
			Txid: txid, RemoteTip: splTip, RemoteTipSig: tipSig,
		},
	))
	got := fetch()
	assertCommitmentEqual(t, splTip, got.RemoteTip)
	requireSigEqual(t, tipSig, got.RemoteTipSig)

	// The peer revokes: its commitment on the splice's funding at the
	// old height goes into the splice's revocation log with its own
	// output indexes, and the tip becomes the tail.
	oldTail := got.RemoteTail
	curRev := channel.RemoteNextRevocation.UnwrapOrFail(t)
	channel.RemoteCurrentRevocation = &curRev
	next, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	channel.RemoteNextRevocation = fn.Some(*next.PubKey())
	fwdPkg := NewFwdPkg(channel.ShortChanID(), tailHeight, nil, nil)
	require.NoError(t, channel.AdvanceCommitChainTail(
		fwdPkg, nil, 0, 1, SpliceCommitWrite{
			Txid: txid, OurOutputIndex: 3, TheirOutputIndex: 2,
		},
	))

	rl, err := channel.FetchSpliceRevocationLog(txid, tailHeight)
	require.NoError(t, err)
	require.Equal(t, [32]byte(oldTail.CommitTx.TxHash()),
		rl.CommitTxHash.Val)
	require.EqualValues(t, 3, rl.OurOutputIndex.Val)
	require.EqualValues(t, 2, rl.TheirOutputIndex.Val)

	// The channel's own log has its own entry at that height.
	own, _, err := channel.FindPreviousState(tailHeight)
	require.NoError(t, err)
	require.NotEqual(t, own.CommitTxHash.Val, rl.CommitTxHash.Val)
	require.EqualValues(t, 0, own.OurOutputIndex.Val)

	got = fetch()
	assertCommitmentEqual(t, splTip, got.RemoteTail)
	require.Nil(t, got.RemoteTip)
	require.Nil(t, got.RemoteTipSig)

	// Nothing more to revoke on the splice: a second revocation write
	// without a tip fails.
	remote.CommitHeight = tailHeight + 2
	diff.Commitment = remote
	require.NoError(t, channel.AppendRemoteCommitChain(diff))
	err = channel.AdvanceCommitChainTail(
		NewFwdPkg(channel.ShortChanID(), tailHeight+1, nil, nil), nil,
		0, 1, SpliceCommitWrite{Txid: txid},
	)
	require.Error(t, err)

	_, err = channel.FetchSpliceRevocationLog(txid, tailHeight+1)
	require.ErrorIs(t, err, ErrLogEntryNotFound)
	_, err = channel.FetchSpliceRevocationLog(
		chainhash.Hash{0xee}, tailHeight,
	)
	require.ErrorIs(t, err, ErrLogEntryNotFound)
}

// TestEnsureAndUpdateSpliceRecord checks creating a record only once, with
// the same funding, and changing one in place.
func TestEnsureAndUpdateSpliceRecord(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())

	r := testSpliceRecord(t, channel)
	r.RemoteTail, r.SpliceSig = nil, nil
	require.NoError(t, channel.EnsureSpliceRecord(r))

	// Again with the same funding: kept as it is, including what was
	// added since.
	sig := &lnwire.CommitSig{ChanID: lnwire.ChannelID{7}, CommitSig: wireSig}
	require.NoError(t, channel.UpdateSpliceRecord(r.Txid(),
		func(rec *SpliceRecord) error {
			rec.SpliceSig = sig
			return nil
		}))
	require.NoError(t, channel.EnsureSpliceRecord(r))
	records, err := channel.SpliceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	requireSigEqual(t, sig, records[0].SpliceSig)

	// With another funding: refused.
	other := *r
	other.LocalDelta++
	require.Error(t, channel.EnsureSpliceRecord(&other))

	// An update can't move the record to another outpoint, and an
	// unknown splice can't be updated.
	require.Error(t, channel.UpdateSpliceRecord(r.Txid(),
		func(rec *SpliceRecord) error {
			rec.OutPoint.Hash[0] ^= 1
			return nil
		}))
	require.ErrorIs(t, channel.UpdateSpliceRecord(chainhash.Hash{1},
		func(*SpliceRecord) error { return nil }),
		ErrSpliceRecordNotFound)
}

// TestSpliceNegotiationRoundTrip checks the splice machine's part of a
// record through the database, and that updating the rest keeps it.
func TestSpliceNegotiationRoundTrip(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())

	r := testSpliceRecord(t, channel)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 3}})
	tx.AddTxOut(&wire.TxOut{Value: 12345, PkScript: []byte{0, 1, 2}})
	tx.LockTime = 900_000
	ourSigs := &lnwire.TxSignatures{
		ChannelID: lnwire.ChannelID{1}, TxID: tx.TxHash(),
		Witnesses: []wire.TxWitness{{{1, 2}, {3}}},
	}
	r.Negotiation = &SpliceNegotiation{
		WeInitiated: true,
		Tx:          tx,
		FeeRate:     2_500,
		OurInputs: []wire.OutPoint{
			{Hash: chainhash.Hash{9}, Index: 1},
			{Hash: chainhash.Hash{8}, Index: 7},
		},
		OurTxSignatures: ourSigs,
		BroadcastHeight: 901_000,
		LockedSent:      true,
		LockedScid: lnwire.ShortChannelID{
			BlockHeight: 901_006, TxIndex: 2, TxPosition: 1,
		},
		OursReplaced: true,
		Dead:         true,
		Disowned:     true,
		AgreedAt:     1_790_000_000,
		SignFailedAt: 1_790_000_100,
		SignFailures: 3,
		OurOutputs: []*wire.TxOut{
			{Value: 40_000, PkScript: []byte{0, 0x14, 7}},
		},
		Inputs: []SpliceInput{
			{SerialID: 0, Value: 1_000_000, PkScript: []byte{
				0, 0x20, 1,
			}, Shared: true},
			{SerialID: 3, Value: 50_000, PkScript: []byte{
				0, 0x14, 2,
			}},
		},
	}
	require.NoError(t, channel.PutSpliceRecord(r))

	// A commitment step updates the record; the negotiation stays.
	require.NoError(t, channel.UpdateSpliceRecord(r.Txid(),
		func(rec *SpliceRecord) error {
			rec.SpliceSig = &lnwire.CommitSig{CommitSig: wireSig}
			return nil
		}))

	records, err := channel.SpliceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	got := records[0].Negotiation
	require.NotNil(t, got)
	require.True(t, got.WeInitiated)
	require.Equal(t, tx.TxHash(), got.Tx.TxHash())
	require.Equal(t, uint64(2_500), got.FeeRate)
	require.Equal(t, r.Negotiation.OurInputs, got.OurInputs)
	require.Equal(t, ourSigs.TxID, got.OurTxSignatures.TxID)
	require.Equal(t, ourSigs.Witnesses, got.OurTxSignatures.Witnesses)
	require.Nil(t, got.TheirTxSignatures)
	require.Equal(t, uint32(901_000), got.BroadcastHeight)
	require.True(t, got.LockedSent)
	require.Equal(t, r.Negotiation.LockedScid, got.LockedScid)
	require.Equal(t, r.Negotiation.AgreedAt, got.AgreedAt)
	require.Equal(t, r.Negotiation.SignFailedAt, got.SignFailedAt)
	require.Equal(t, r.Negotiation.SignFailures, got.SignFailures)
	require.False(t, got.LockedReceived)
	require.True(t, got.OursReplaced)
	require.True(t, got.Dead)
	require.True(t, got.Disowned)
	require.Equal(t, r.Negotiation.OurOutputs, got.OurOutputs)
	require.Equal(t, r.Negotiation.Inputs, got.Inputs)

	// A record without one has none.
	r2 := testSpliceRecord(t, channel)
	r2.OutPoint.Hash[0] ^= 0xff
	require.NoError(t, channel.PutSpliceRecord(r2))
	records, err = channel.SpliceRecords()
	require.NoError(t, err)
	for _, rec := range records {
		if rec.Txid() == r2.Txid() {
			require.Nil(t, rec.Negotiation)
		}
	}
}

// TestLastSplice checks the record of a channel's most recent splice.
func TestLastSplice(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())

	last, err := channel.LastSplice()
	require.NoError(t, err)
	require.Nil(t, last)

	want := &LastSplice{
		WeInitiated: true, LocalDelta: -250_000, RemoteDelta: 0,
		Txid: chainhash.Hash{4}, Time: 1_790_000_000,
		Outcome: "aborted", Reason: "insufficient funds, says the peer",
		ReasonFromPeer: true,
	}
	require.NoError(t, channel.PutLastSplice(want))
	last, err = channel.LastSplice()
	require.NoError(t, err)
	require.Equal(t, want, last)

	// A newer one replaces it.
	newer := &LastSplice{
		LocalDelta: 0, RemoteDelta: 1_000_000, Txid: chainhash.Hash{5},
		Time: 1_790_000_100, Outcome: "locked",
	}
	require.NoError(t, channel.PutLastSplice(newer))
	last, err = channel.LastSplice()
	require.NoError(t, err)
	require.Equal(t, newer, last)
}

// TestSpliceHolds checks the record of the peer's coins held in locked
// splices: added, replaced for the same splice, kept apart for different
// ones, deleted, and gone with the last.
func TestSpliceHolds(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	channel := createTestChannel(t, fullDB.ChannelStateDB())

	holds, err := channel.SpliceHolds()
	require.NoError(t, err)
	require.Empty(t, holds)

	a := SpliceHold{
		Txid: chainhash.Hash{1}, Amount: 300_000,
		PkScript: []byte{0x00, 0x20, 1, 2, 3}, HeightHint: 900_006,
	}
	b := SpliceHold{
		Txid: chainhash.Hash{2}, Amount: 50_000,
		PkScript: []byte{0x00, 0x20, 4}, HeightHint: 900_100,
	}
	require.NoError(t, channel.PutSpliceHold(a))
	require.NoError(t, channel.PutSpliceHold(b))
	a.Amount = 310_000
	require.NoError(t, channel.PutSpliceHold(a))

	holds, err = channel.SpliceHolds()
	require.NoError(t, err)
	require.ElementsMatch(t, []SpliceHold{a, b}, holds)

	require.NoError(t, channel.DeleteSpliceHold(a.Txid))
	holds, err = channel.SpliceHolds()
	require.NoError(t, err)
	require.Equal(t, []SpliceHold{b}, holds)

	require.NoError(t, channel.DeleteSpliceHold(b.Txid))
	require.NoError(t, channel.DeleteSpliceHold(b.Txid))
	holds, err = channel.SpliceHolds()
	require.NoError(t, err)
	require.Empty(t, holds)
}

// TestRestoreShellMarksSplicing checks that restoring a backup's channel
// with splice data moves the database to the splicing version, as running
// with splicing on does, so a version that can't read that data refuses the
// database; one without leaves the version as it was.
func TestRestoreShellMarksSplicing(t *testing.T) {
	t.Parallel()

	version := func(db *DB) uint32 {
		meta, err := db.FetchMeta()
		require.NoError(t, err)

		return meta.DbVersionNumber
	}

	plainDB, err := MakeTestDB(t)
	require.NoError(t, err)
	before := version(plainDB)
	require.Less(t, before, uint32(splicingDBVersion))
	plain, err := genRandomChannelShell()
	require.NoError(t, err)
	require.NoError(t, plainDB.ChannelStateDB().RestoreChannelShells(plain))
	require.Equal(t, before, version(plainDB))

	splicedDB, err := MakeTestDB(t)
	require.NoError(t, err)
	shell, err := genRandomChannelShell()
	require.NoError(t, err)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	shell.Splice = &ShellSplice{Candidates: []*SpliceRecord{{
		OutPoint:  wire.OutPoint{Hash: chainhash.Hash{7}},
		Capacity:  2_000_000,
		LocalKey:  keychain.KeyDescriptor{PubKey: key.PubKey()},
		RemoteKey: key.PubKey(),
	}}}
	require.NoError(t, splicedDB.ChannelStateDB().RestoreChannelShells(
		shell,
	))
	require.Equal(t, uint32(splicingDBVersion), version(splicedDB))
}

// TestSplicePeerAtRisk checks what of a splice the peer could undo: its
// inputs' value, or what it put in if more; never our inputs or the shared
// one.
func TestSplicePeerAtRisk(t *testing.T) {
	t.Parallel()

	inputs := []SpliceInput{
		{SerialID: 0, Value: 2_000_000, Shared: true},
		{SerialID: 2, Value: 300_000},
		{SerialID: 1, Value: 5_000_000},
		{SerialID: 3, Value: 1_000_000},
	}

	// The peer started it: its serial ids are even.
	r := &SpliceRecord{RemoteDelta: 100_000, Negotiation: &SpliceNegotiation{
		WeInitiated: false, Inputs: inputs,
	}}
	require.Equal(t, btcutil.Amount(300_000), r.PeerAtRisk())

	// We started it: the peer's are odd. It took most of them back.
	r.Negotiation.WeInitiated = true
	require.Equal(t, btcutil.Amount(6_000_000), r.PeerAtRisk())

	// No inputs of its own: what it put in, never below zero.
	r.Negotiation.Inputs = inputs[:2]
	require.Equal(t, btcutil.Amount(100_000), r.PeerAtRisk())
	r.RemoteDelta = -50_000
	require.Zero(t, r.PeerAtRisk())
	r.Negotiation = nil
	require.Zero(t, r.PeerAtRisk())
}
