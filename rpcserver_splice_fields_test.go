package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

// TestRPCPendingSplice checks how a pending splice is described: its state
// through the steps, deltas, new channel point and capacity, and the fee
// from its transaction.
func TestRPCPendingSplice(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{Value: 1_200_000})
	tx.AddTxOut(&wire.TxOut{Value: 49_000})

	rec := &channeldb.SpliceRecord{
		OutPoint:   wire.OutPoint{Hash: chainhash.Hash{7}, Index: 0},
		Capacity:   1_200_000,
		LocalDelta: 200_000,
	}

	ps := rpcPendingSplice(rec)
	require.Equal(t, "negotiating", ps.State)
	require.Equal(t, lnrpc.Initiator_INITIATOR_REMOTE, ps.Initiator)
	require.Equal(t, int64(200_000), ps.LocalDeltaSat)
	require.Equal(t, rec.OutPoint.String(), ps.NewChannelPoint)
	require.Equal(t, int64(1_200_000), ps.NewCapacitySat)

	rec.Negotiation = &channeldb.SpliceNegotiation{
		WeInitiated: true, Tx: tx, FeeRate: 2_500,
		Inputs: []channeldb.SpliceInput{
			{Value: 1_000_000, Shared: true}, {Value: 250_000},
		},
	}
	rec.SpliceSig = &lnwire.CommitSig{}
	ps = rpcPendingSplice(rec)
	require.Equal(t, "signed", ps.State)
	require.Equal(t, lnrpc.Initiator_INITIATOR_LOCAL, ps.Initiator)
	require.Equal(t, int64(1_000), ps.FeeSat)
	require.Equal(t, int64(1_000), ps.OurFeeSat)
	require.Equal(t, uint64(10), ps.SatPerVbyte)

	rec.Negotiation.OurTxSignatures = &lnwire.TxSignatures{}
	rec.Negotiation.TheirTxSignatures = &lnwire.TxSignatures{}
	rec.Negotiation.BroadcastHeight = 900_001
	ps = rpcPendingSplice(rec)
	require.Equal(t, "broadcast", ps.State)
	require.Equal(t, uint32(900_001), ps.BroadcastHeight)

	// The peer's replacement of our splice is still ours.
	rec.Negotiation.WeInitiated = false
	rec.Negotiation.OursReplaced = true
	require.Equal(t, lnrpc.Initiator_INITIATOR_LOCAL,
		rpcPendingSplice(rec).Initiator)

	rec.Negotiation.LockedSent = true
	ps = rpcPendingSplice(rec)
	require.Equal(t, "confirmed", ps.State)
	require.True(t, ps.WeLocked)
	require.False(t, ps.PeerLocked)
}

// TestSetSpliceDepth checks a pending splice's confirmations and lock depth,
// and that its first confirmation makes it "confirmed".
func TestSetSpliceDepth(t *testing.T) {
	t.Parallel()

	ps := &lnrpc.PendingSplice{State: "broadcast"}
	setSpliceDepth(ps, 0, 6)
	require.Equal(t, "broadcast", ps.State)
	require.Equal(t, uint32(6), ps.LockDepth)

	setSpliceDepth(ps, 2, 6)
	require.Equal(t, "confirmed", ps.State)
	require.Equal(t, uint32(2), ps.Confirmations)

	// Still signing: confirmations can't be known yet.
	ps = &lnrpc.PendingSplice{State: "signed"}
	setSpliceDepth(ps, 0, 6)
	require.Equal(t, "signed", ps.State)
}

// TestSpliceLockDepth checks the lock depth: 6 at least, the channel's own
// when larger, and up to the hold's depth by the coins the peer put in.
func TestSpliceLockDepth(t *testing.T) {
	t.Parallel()

	require.Equal(t, uint32(6), spliceLockDepth(3, 0, 36))
	require.Equal(t, uint32(8), spliceLockDepth(8, 0, 36))
	require.Equal(t, uint32(6), spliceLockDepth(3, -5, 36))

	// A sat of the peer's is a block more; half a channel's worth 15.
	require.Equal(t, uint32(7), spliceLockDepth(3, 1, 36))
	require.Equal(t, uint32(21), spliceLockDepth(3, 16_777_215/2, 36))
	require.Equal(t, uint32(36), spliceLockDepth(3, 16_777_215, 36))
	require.Equal(t, uint32(36), spliceLockDepth(3, 1_000_000_000, 36))

	// The channel's own depth wins when larger.
	require.Equal(t, uint32(40), spliceLockDepth(40, 1_000_000, 36))

	// The ceiling is the hold's depth: none held, none deeper.
	require.Equal(t, uint32(16), spliceLockDepth(3, 1_000_000_000, 16))
	require.Equal(t, uint32(6), spliceLockDepth(3, 1_000_000_000, 0))
}

// TestSpliceBalances checks what a splice adds to the channel balance's
// pending fields: nothing before its transaction went out, our
// contribution after, and what it pays our wallet only while unconfirmed.
func TestSpliceBalances(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 1_000_000})
	tx.AddTxOut(&wire.TxOut{Value: 300_000})
	ours := func(_ *wire.MsgTx, idx uint32) btcutil.Amount {
		require.Equal(t, uint32(0), idx)
		return 300_000
	}
	confs := uint32(0)
	confirmations := func() uint32 { return confs }

	in := &channeldb.SpliceRecord{
		LocalDelta:  200_000,
		Negotiation: &channeldb.SpliceNegotiation{Tx: tx},
	}
	a, b := spliceBalances(in, confirmations, ours)
	require.Zero(t, a, "counted while the wallet still holds the coins")
	require.Zero(t, b)

	in.Negotiation.OurTxSignatures = &lnwire.TxSignatures{}
	in.Negotiation.TheirTxSignatures = &lnwire.TxSignatures{}
	a, _ = spliceBalances(in, confirmations, ours)
	require.EqualValues(t, 200_000, a)

	out := &channeldb.SpliceRecord{
		LocalDelta: -300_200,
		Negotiation: &channeldb.SpliceNegotiation{
			Tx:              tx,
			OurTxSignatures: &lnwire.TxSignatures{},
		},
	}
	_, b = spliceBalances(out, confirmations, ours)
	require.Zero(t, b, "not out yet")

	out.Negotiation.TheirTxSignatures = &lnwire.TxSignatures{}
	a, b = spliceBalances(out, confirmations, ours)
	require.Zero(t, a)
	require.EqualValues(t, 300_000, b)

	confs = 1
	_, b = spliceBalances(out, confirmations, ours)
	require.Zero(t, b, "confirmed: the wallet counts it as confirmed")

	out.Retired = true
	confs = 0
	_, b = spliceBalances(out, confirmations, ours)
	require.Zero(t, b)

	// Dead: its coins are the wallet's again.
	in.Negotiation.Dead = true
	a, _ = spliceBalances(in, confirmations, ours)
	require.Zero(t, a)
}

// TestShownSplice checks the splice transaction shown for a channel, and
// whose coins are counted once: one that can still confirm before a dead
// one, then the highest fee rate; never a retired one.
func TestShownSplice(t *testing.T) {
	t.Parallel()

	rec := func(tag byte, rate uint64,
		dead, retired bool) *channeldb.SpliceRecord {

		return &channeldb.SpliceRecord{
			OutPoint: wire.OutPoint{Hash: chainhash.Hash{tag}},
			Retired:  retired,
			Negotiation: &channeldb.SpliceNegotiation{
				FeeRate: rate, Dead: dead,
			},
		}
	}
	require.Nil(t, shownSplice(nil))

	low, high := rec(1, 2_500, false, false), rec(2, 5_000, false, false)
	require.Equal(t, high, shownSplice([]*channeldb.SpliceRecord{low, high}))

	deadHigh := rec(3, 9_000, true, false)
	require.Equal(t, low, shownSplice(
		[]*channeldb.SpliceRecord{deadHigh, low},
	))
	require.Equal(t, low, shownSplice(
		[]*channeldb.SpliceRecord{low, deadHigh},
	))
	require.Equal(t, deadHigh, shownSplice(
		[]*channeldb.SpliceRecord{deadHigh, rec(4, 1, false, true)},
	))

	ps := rpcPendingSplice(deadHigh)
	require.True(t, ps.Dead)
}

// TestSpliceOverdue checks when a peer is late with the steps after a
// splice confirms.
func TestSpliceOverdue(t *testing.T) {
	t.Parallel()

	// splice_locked: 144 blocks past our lock depth.
	require.False(t, spliceOverdue(149, 6, spliceLockedGrace))
	require.True(t, spliceOverdue(150, 6, spliceLockedGrace))

	// The announcement: 72 blocks past the six-block depth.
	require.False(t, spliceOverdue(77, 0, spliceAnnounceGrace))
	require.True(t, spliceOverdue(78, 0, spliceAnnounceGrace))
}

// TestLeastCandidateBalance checks the balance a channel with splices pending
// reports: the least on any funding that can still confirm, a dead splice's
// left out, as a close pays out the current funding's.
func TestLeastCandidateBalance(t *testing.T) {
	t.Parallel()

	rec := func(local lnwire.MilliSatoshi, dead,
		retired bool) *channeldb.SpliceRecord {

		return &channeldb.SpliceRecord{
			Retired: retired,
			LocalCommit: &channeldb.ChannelCommitment{
				LocalBalance: local,
			},
			Negotiation: &channeldb.SpliceNegotiation{Dead: dead},
		}
	}
	require.EqualValues(t, 5_000, leastCandidateBalance(nil, 5_000))
	require.EqualValues(t, 3_000, leastCandidateBalance(
		[]*channeldb.SpliceRecord{rec(3_000, false, false)}, 5_000,
	))
	require.EqualValues(t, 5_000, leastCandidateBalance(
		[]*channeldb.SpliceRecord{
			rec(1_000, true, false), rec(2_000, false, true),
			rec(9_000, false, false),
		}, 5_000,
	))
}

// TestSpliceStatusOfChannelCopy checks that a channel's splice status can be
// read from a copy of it, as a channel update event carries: the copy has no
// database, so a stored shutdown cannot be read from it.
func TestSpliceStatusOfChannelCopy(t *testing.T) {
	t.Parallel()

	ch := (&channeldb.OpenChannel{}).Copy()
	require.Nil(t, ch.Db)
	require.NotPanics(t, func() {
		require.False(t, hasShutdownInfo(ch))
	})
}

// TestSpliceFieldsOfChannelUpdate checks that the splice fields of a channel
// update event, which carries a copy of the channel, come from the stored
// channel: here, that a shutdown sent on it makes it closing.
func TestSpliceFieldsOfChannelUpdate(t *testing.T) {
	t.Parallel()

	db, err := channeldb.MakeTestDB(t)
	require.NoError(t, err)
	cdb := db.ChannelStateDB()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	remote := keychain.KeyDescriptor{PubKey: priv.PubKey()}
	op := wire.OutPoint{Hash: chainhash.Hash{5}}
	require.NoError(t, cdb.RestoreChannelShells(&channeldb.ChannelShell{
		Chan: &channeldb.OpenChannel{
			ChainHash:               chainreg.BitcoinMainNetParams.ChainHash,
			FundingOutpoint:         op,
			IdentityPub:             priv.PubKey(),
			RemoteCurrentRevocation: priv.PubKey(),
			RevocationStore:         shachain.NewRevocationStore(),
			RevocationProducer: shachain.NewRevocationProducer(
				chainhash.Hash{},
			),
			LocalChanCfg: channeldb.ChannelConfig{
				MultiSigKey: remote,
			},
			RemoteChanCfg: channeldb.ChannelConfig{
				MultiSigKey:         remote,
				RevocationBasePoint: remote,
				PaymentBasePoint:    remote,
				DelayBasePoint:      remote,
				HtlcBasePoint:       remote,
			},
		},
	}))

	stored, err := cdb.FetchChannel(op)
	require.NoError(t, err)
	require.NoError(t, stored.MarkShutdownSent(
		channeldb.NewShutdownInfo(lnwire.DeliveryAddress{0x00}, true),
	))

	r := &rpcServer{server: &server{chanStateDB: cdb}}
	channel := &lnrpc.Channel{}
	r.addSpliceFields(stored.Copy(), channel)
	require.Equal(t, spliceStatusClosing, channel.SpliceStatus)
	require.Equal(t, op.String(), channel.ChannelPoint)
}
