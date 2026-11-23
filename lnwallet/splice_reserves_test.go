package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// lockedSplice returns a pair whose splice adds 1000 BTC of Alice's and has
// locked, the reserves still those of the opening (0.1 BTC each). With
// record, both sides applied their reserves at a connection before the lock,
// as a node running this release does; without, the channel last connected
// before they were recorded.
func lockedSplice(t *testing.T, record bool) (*LightningChannel,
	*LightningChannel) {

	t.Helper()

	alice, bob, txid := splicedPair(t, 1_000*btcutil.SatoshiPerBitcoin, 0)
	if record {
		alice.ApplySpliceReserves()
		bob.ApplySpliceReserves()
	}

	scid := lnwire.ShortChannelID{BlockHeight: 900_100, TxIndex: 4}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))

	return alice, bob
}

// TestSpliceReserveLenience checks the leniency BOLT 2 asks for once a
// splice's reserves take effect at a connection. Bob adds an HTLC under the
// opening reserves after the lock; at the next connection the reserves are
// 1% of the new capacity, 10.1 BTC each, more than Bob's 5 BTC. The
// commitments that carry his HTLC, as the reconnection sends them again, are
// still accepted under the reserves before the change, all through that
// connection; on the next, once Alice has received his commitment_signed
// and signed one back, a new HTLC of Bob's is held to the new reserves
// again. Without the record of the earlier
// reserves (a channel last connected before this release), the same
// commitment fails on the new reserves, which is what the leniency is for.
func TestSpliceReserveLenience(t *testing.T) {
	t.Parallel()

	for _, record := range []bool{true, false} {
		alice, bob := lockedSplice(t, record)
		opening := alice.channelState.LocalChanCfg.ChanReserve

		htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
		addAndReceiveHTLC(t, bob, alice, htlc, nil)

		// The reconnection: both apply the reserves after the lock.
		alice.ApplySpliceReserves()
		bob.ApplySpliceReserves()
		newReserve := alice.channelState.Capacity / 100
		require.Equal(t, newReserve,
			alice.channelState.RemoteChanCfg.ChanReserve)
		require.Greater(t, newReserve, opening)

		// Without a record (a channel last connected before they were
		// kept), the reserve of the funding before the last splice
		// stands in for the earlier ones, so the dance passes too.
		err := ForceStateTransition(bob, alice)
		require.NoError(t, err)
		if !record {
			continue
		}
		require.NotEmpty(t, alice.spliceLenient,
			"leniency over on the connection that changed them")

		// The next connection: Bob's first commitment_signed there,
		// signed back, ends it.
		alice.ApplySpliceReserves()
		bob.ApplySpliceReserves()
		ours, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(1_000_000))
		addAndReceiveHTLC(t, alice, bob, ours, nil)
		require.NoError(t, ForceStateTransition(alice, bob))
		require.Empty(t, alice.spliceLenient, "leniency not over")

		rec, found, err := alice.channelState.FetchSpliceReserves()
		require.NoError(t, err)
		require.True(t, found)
		require.Empty(t, rec.Lenient)
		require.Equal(t, newReserve, rec.InForceRemote)

		// A new HTLC from Bob: the new reserves again.
		htlc, _ = createHTLC(2, lnwire.NewMSatFromSatoshis(1_000_000))
		_, err = alice.ReceiveHTLC(htlc)
		require.ErrorIs(t, err, ErrBelowChanReserve)
	}
}

// TestSpliceReserveLenienceRestart checks that the leniency is recorded: a
// restart between the connection that changed the reserves and the peer's
// commitment_signed keeps it, and applying the reserves again at the next
// connection, unchanged, adds no second entry.
func TestSpliceReserveLenienceRestart(t *testing.T) {
	t.Parallel()

	alice, bob := lockedSplice(t, true)
	opening := alice.channelState.LocalChanCfg.ChanReserve
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	want := []channeldb.ReservePair{{Local: opening, Remote: opening}}
	require.Equal(t, want, alice.spliceLenient)

	restarted, err := restartChannel(alice)
	require.NoError(t, err)
	restarted.ApplySpliceReserves()
	require.Equal(t, want, restarted.spliceLenient)
	require.Equal(t, alice.channelState.Capacity/100,
		restarted.channelState.RemoteChanCfg.ChanReserve)

	// A retransmitted HTLC of Bob's under the opening reserves is taken.
	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
	_, err = restarted.ReceiveHTLC(htlc)
	require.NoError(t, err)
}

// TestSpliceReservesNeverSpliced checks that a channel never spliced keeps
// each side's opening reserve, and that it is recorded, so the first
// splice's change is seen.
func TestSpliceReservesNeverSpliced(t *testing.T) {
	t.Parallel()

	alice, _, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)
	local, remote := alice.channelState.Reserves()

	alice.ApplySpliceReserves()
	gotLocal, gotRemote := alice.channelState.Reserves()
	require.Equal(t, local, gotLocal)
	require.Equal(t, remote, gotRemote)
	require.Empty(t, alice.spliceLenient)

	rec, found, err := alice.channelState.FetchSpliceReserves()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, local, rec.InForceLocal)
	require.Equal(t, remote, rec.InForceRemote)
	require.Equal(t, local, rec.OpeningLocal)
	require.Equal(t, remote, rec.OpeningRemote)

	// The channel's config written with other reserves (a splice's,
	// applied in memory, that then never locked): the next connection
	// takes the opening ones from the record.
	alice.channelState.SetReserves(local * 50)
	alice.ApplySpliceReserves()
	gotLocal, gotRemote = alice.channelState.Reserves()
	require.Equal(t, local, gotLocal)
	require.Equal(t, remote, gotRemote)
	require.Empty(t, alice.spliceLenient)
}

// TestSpliceReserveLenienceSplicePending checks the leniency with a second
// splice pending: its commitments are checked against the reserves too, and
// the same earlier reserves apply there, or the commitment that carries
// Bob's HTLC would fail on the pending splice's funding and the connection
// close on every reconnection.
func TestSpliceReserveLenienceSplicePending(t *testing.T) {
	t.Parallel()

	alice, bob := lockedSplice(t, true)

	// A second splice, adding 1 BTC of Alice's, pending.
	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x6d}
	addSplice(t, alice, bob, keys, txid, btcutil.SatoshiPerBitcoin, 0)
	exchangeSpliceCommitments(t, alice, bob, txid)

	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
	addAndReceiveHTLC(t, bob, alice, htlc, nil)

	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	require.NotEmpty(t, alice.spliceLenient)

	forceBatchTransition(t, bob, alice)
}

// TestSpliceReserveLenienceOurAdd checks an add of ours while the peer's
// HTLC, taken under the earlier reserves, is in the view: our balance meets
// the current reserve, and the peer's below it is not ours to refuse.
func TestSpliceReserveLenienceOurAdd(t *testing.T) {
	t.Parallel()

	alice, bob := lockedSplice(t, true)
	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
	addAndReceiveHTLC(t, bob, alice, htlc, nil)
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()

	ours, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(2_000_000))
	_, err := alice.AddHTLC(ours, nil)
	require.NoError(t, err)
}

// TestSpliceReserveSignLenience checks the leniency for each path while it
// lasts: what the peer sends is lenient until its first commitment_signed
// since the change, a commitment we sign is lenient until the leniency ends,
// and an add of ours is held to the current reserve on our side.
func TestSpliceReserveSignLenience(t *testing.T) {
	t.Parallel()

	alice, _ := lockedSplice(t, true)
	require.Equal(t, lenientNone, alice.signLenience())
	require.Equal(t, lenientNone, alice.receiveLenience())
	require.Equal(t, lenientNone, alice.addLenience())

	alice.spliceLenient = []channeldb.ReservePair{{Local: 1, Remote: 1}}
	require.Equal(t, lenientFull, alice.signLenience())
	require.Equal(t, lenientFull, alice.receiveLenience())
	require.Equal(t, lenientTheirs, alice.addLenience())

	alice.spliceLenientCS = true
	require.Equal(t, lenientFull, alice.signLenience())
	require.Equal(t, lenientNone, alice.receiveLenience())
}

// TestSpliceReserveLeniencePeerCommitSaved checks that the peer's
// commitment_signed since the change is recorded when we revoke owing a
// commitment, on a connection after the one that changed the reserves: after
// a restart its new updates meet the current reserves, while a commitment we
// sign is still lenient for the ones before. On the connection that changed
// them it isn't recorded, the peer possibly still sending under the earlier
// ones.
func TestSpliceReserveLeniencePeerCommitSaved(t *testing.T) {
	t.Parallel()

	alice, bob := lockedSplice(t, true)
	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
	addAndReceiveHTLC(t, bob, alice, htlc, nil)
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	require.NoError(t, ForceStateTransition(bob, alice))
	rec, found, err := alice.channelState.FetchSpliceReserves()
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, rec.PeerCommitSeen)
	require.NotEmpty(t, rec.Lenient)

	// The next connection: Bob fulfils Alice's first HTLC and signs.
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	_, preimage := createHTLC(0, lnwire.NewMSatFromSatoshis(150_000))
	require.NoError(t, bob.SettleHTLC(preimage, 0, nil, nil, nil))
	require.NoError(t, alice.ReceiveHTLCSettle(preimage, 0))

	bobSig, err := bob.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.NoError(t, alice.ReceiveNewCommitment(bobSig.CommitSigs))
	_, _, _, err = alice.RevokeCurrentCommitment()
	require.NoError(t, err)

	rec, found, err = alice.channelState.FetchSpliceReserves()
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, rec.PeerCommitSeen)
	require.NotEmpty(t, rec.Lenient)

	restarted, err := restartChannel(alice)
	require.NoError(t, err)
	restarted.ApplySpliceReserves()
	require.Equal(t, lenientNone, restarted.receiveLenience())
	require.Equal(t, lenientFull, restarted.signLenience())

	// A new HTLC of Bob's under the earlier reserve only: refused.
	htlc, _ = createHTLC(2, lnwire.NewMSatFromSatoshis(1_000_000))
	_, err = restarted.ReceiveHTLC(htlc)
	require.ErrorIs(t, err, ErrBelowChanReserve)
}

// TestSpliceReserveLenienceCrossing checks a commitment we sign after
// crossing commitment_signed: Alice's own add and Bob's HTLC from before the
// change cross, Alice revokes his and must wait for his revocation to sign
// back, and in the meantime Bob fulfils an HTLC of hers. That fulfil comes
// after his commitment_signed but is not in what Alice signs until she has
// it in a commitment of hers, so her signature still covers his HTLC under
// the earlier reserves and must not fail on the current ones; failing it
// sends the peer an error, on which it may force close.
func TestSpliceReserveLenienceCrossing(t *testing.T) {
	t.Parallel()

	alice, bob := lockedSplice(t, true)
	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
	addAndReceiveHTLC(t, bob, alice, htlc, nil)
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()

	ours, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(2_000_000))
	addAndReceiveHTLC(t, alice, bob, ours, nil)

	aliceSig, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	bobSig, err := bob.SignNextCommitment(ctxb)
	require.NoError(t, err)

	require.NoError(t, alice.ReceiveNewCommitment(bobSig.CommitSigs))
	aliceRev, _, _, err := alice.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, err = alice.SignNextCommitment(ctxb)
	require.ErrorIs(t, err, ErrNoWindow)

	// Bob takes Alice's commitment_signed, which she sent first, then
	// fulfils her first HTLC, after his own commitment_signed, and only
	// then revokes; her revocation reaches him last.
	require.NoError(t, bob.ReceiveNewCommitment(aliceSig.CommitSigs))
	_, preimage := createHTLC(0, lnwire.NewMSatFromSatoshis(150_000))
	require.NoError(t, bob.SettleHTLC(preimage, 0, nil, nil, nil))
	require.NoError(t, alice.ReceiveHTLCSettle(preimage, 0))
	bobRev, _, _, err := bob.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = bob.ReceiveRevocation(aliceRev)
	require.NoError(t, err)
	_, _, err = alice.ReceiveRevocation(bobRev)
	require.NoError(t, err)

	_, err = alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
}

// TestSpliceReserveLenienceUpgrade checks a spliced channel with no record
// (last connected before they were kept): the reserve of the funding its
// last splice replaced is taken as the one applied before, one entry, over
// once the leniency ends as usual.
func TestSpliceReserveLenienceUpgrade(t *testing.T) {
	t.Parallel()

	alice, _ := lockedSplice(t, false)
	opening := alice.channelState.LocalChanCfg.ChanReserve
	alice.ApplySpliceReserves()
	require.Equal(t, []channeldb.ReservePair{
		{Local: opening, Remote: opening},
	}, alice.spliceLenient)

	rec, found, err := alice.channelState.FetchSpliceReserves()
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, rec.Lenient, 1)
	require.Equal(t, alice.channelState.Capacity/100, rec.InForceRemote)
}

// TestSpliceReserveLenienceEndsAtSplice checks that a new splice ends the
// leniency: it needs the channel quiescent, so nothing taken under earlier
// reserves is left to be sent again, and a peer that never sends
// commitment_signed can't stack the reserves of several splices.
func TestSpliceReserveLenienceEndsAtSplice(t *testing.T) {
	t.Parallel()

	alice, bob := lockedSplice(t, true)
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	require.NotEmpty(t, alice.spliceLenient)

	keys := newSpliceKeys(t, alice, bob)
	addSplice(t, alice, bob, keys, chainhash.Hash{0x7e},
		btcutil.SatoshiPerBitcoin, 0)
	require.Empty(t, alice.spliceLenient)

	rec, found, err := alice.channelState.FetchSpliceReserves()
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, rec.Lenient)
	require.False(t, rec.PeerCommitSeen)
}

// TestSpliceReserveLenienceSwitchConnection checks the leniency on the
// connection at which the reserves change, against a peer that switches one
// connection later: Bob never got Alice's splice_locked, so he completes the
// lock from her channel_reestablish and, applying reserves as the channel
// loads, keeps the opening ones for that connection. Alice, whose lock was
// complete, loads the splice's. Bob's adds under the opening reserves are
// accepted for the whole connection, not only until his first
// commitment_signed, and on the next connection until his first
// commitment_signed there; refusing one sends the peer an error, on which
// it may fail the channel. With a restart in between, the same.
func TestSpliceReserveLenienceSwitchConnection(t *testing.T) {
	t.Parallel()

	for _, restart := range []bool{false, true} {
		alice, bob := lockedSplice(t, true)

		// The switch connection: Alice applies the splice's reserves,
		// Bob keeps the opening ones.
		alice.ApplySpliceReserves()
		require.Greater(t, alice.channelState.RemoteChanCfg.ChanReserve,
			bob.channelState.LocalChanCfg.ChanReserve)

		htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
		addAndReceiveHTLC(t, bob, alice, htlc, nil)
		require.NoError(t, ForceStateTransition(bob, alice))

		// After his first commitment_signed, Bob still sends under
		// the opening reserves on this connection.
		htlc, _ = createHTLC(2, lnwire.NewMSatFromSatoshis(1_000_000))
		addAndReceiveHTLC(t, bob, alice, htlc, nil)
		require.NoError(t, ForceStateTransition(bob, alice))

		// The next connection: both switch. Until Bob's first
		// commitment_signed there, what the earlier reserves allow is
		// still taken from him (sent again, or sent before he
		// switched); once it is in and signed back, the new reserves
		// hold.
		if restart {
			var err error
			alice, err = restartChannel(alice)
			require.NoError(t, err)
		}
		alice.ApplySpliceReserves()
		bob.ApplySpliceReserves()
		require.Equal(t, lenientFull, alice.receiveLenience())

		ours, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(1_000_000))
		addAndReceiveHTLC(t, alice, bob, ours, nil)
		require.NoError(t, ForceStateTransition(alice, bob))
		require.Equal(t, lenientNone, alice.receiveLenience())
		require.Empty(t, alice.spliceLenient)
	}
}

// TestSpliceReserveFloor checks the sending floor: after a splice-out lowers
// the reserves from 0.1 BTC to 0.06, the connection that applies them still
// keeps our own adds to the earlier 0.1, in what we report as available and
// in what we add, so a peer that switches later never sees us break them.
// The next connection drops it.
func TestSpliceReserveFloor(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, -4*btcutil.SatoshiPerBitcoin, 0)
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	scid := lnwire.ShortChannelID{BlockHeight: 900_100, TxIndex: 4}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))

	alice.ApplySpliceReserves()
	require.Equal(t, btcutil.Amount(6_000_000),
		alice.channelState.LocalChanCfg.ChanReserve)

	withFloor := alice.AvailableBalance()
	floor := alice.spliceFloor
	alice.spliceFloor = nil
	without := alice.AvailableBalance()
	alice.spliceFloor = floor
	require.Equal(t, lnwire.NewMSatFromSatoshis(4_000_000),
		without-withFloor)

	// An add the new reserve allows and the earlier one doesn't.
	htlc, _ := createHTLC(5, withFloor+lnwire.NewMSatFromSatoshis(2_000_000))
	_, err := alice.AddHTLC(htlc, nil)
	require.ErrorIs(t, err, ErrBelowChanReserve)

	// The next connection: no floor, and the same add goes.
	alice.ApplySpliceReserves()
	require.Empty(t, alice.spliceFloor)
	require.Equal(t, without, alice.AvailableBalance())
	_, err = alice.AddHTLC(htlc, nil)
	require.NoError(t, err)
}

// substitutedPair returns a locked splice whose reserves changed two
// connections ago, the leniency for that change over: Alice holds both sides
// to the splice's 10.1 BTC with nothing owed. Bob is set back to the opening
// reserves, as a node that lost its record of the lock would hold them.
func substitutedPair(t *testing.T) (*LightningChannel, *LightningChannel,
	btcutil.Amount) {

	t.Helper()

	alice, bob := lockedSplice(t, true)
	opening := alice.channelState.LocalChanCfg.ChanReserve
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	ours, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(1_000_000))
	addAndReceiveHTLC(t, alice, bob, ours, nil)
	require.NoError(t, ForceStateTransition(alice, bob))
	alice.ApplySpliceReserves()
	require.Equal(t, lenientNone, alice.receiveLenience())

	bob.channelState.SetChanReserves(opening, opening)

	return alice, bob, opening
}

// TestSpliceReserveSubstitution checks the leniency of a node that
// substitutes its peer's last splice_locked: Bob's channel_reestablish
// names the funding transaction (a restore, or a crash that lost his record
// of the lock), nothing, or a transaction Alice doesn't know. Alice keeps
// the splice's reserves, and on this connection takes Bob's updates under
// the opening ones that transaction gives, before and after his
// commitment_signed. Bob naming the splice itself gets no leniency, and the
// next connection ends it.
func TestSpliceReserveSubstitution(t *testing.T) {
	t.Parallel()

	for name, named := range map[string]func(*LightningChannel) fn.Option[chainhash.Hash]{
		"funding": func(lc *LightningChannel) fn.Option[chainhash.Hash] {
			return fn.Some(lc.channelState.FundingOutpoint.Hash)
		},
		"missing": func(*LightningChannel) fn.Option[chainhash.Hash] {
			return fn.None[chainhash.Hash]()
		},
		"unknown": func(*LightningChannel) fn.Option[chainhash.Hash] {
			return fn.Some(chainhash.Hash{0xee})
		},
	} {
		t.Run(name, func(t *testing.T) {
			alice, bob, opening := substitutedPair(t)
			current := alice.channelState.CurrentFundingOutpoint().Hash
			alice.ApplyReestablishReserves(fn.Some(current),
				named(alice))
			require.Equal(t, []channeldb.ReservePair{
				{Local: opening, Remote: opening},
			}, alice.spliceSubst)
			require.Greater(t,
				alice.channelState.RemoteChanCfg.ChanReserve,
				opening, "Alice's reserves stay the splice's")

			for i := uint64(1); i < 3; i++ {
				htlc, _ := createHTLC(int(i),
					lnwire.NewMSatFromSatoshis(1_000_000))
				addAndReceiveHTLC(t, bob, alice, htlc, nil)
				require.NoError(t, ForceStateTransition(bob, alice))
			}

			alice.ApplySpliceReserves()
			require.Empty(t, alice.spliceSubst)
			htlc, _ := createHTLC(3,
				lnwire.NewMSatFromSatoshis(1_000_000))
			_, err := alice.ReceiveHTLC(htlc)
			require.ErrorIs(t, err, ErrBelowChanReserve)
		})
	}

	// Bob naming the splice: no leniency.
	alice, bob, _ := substitutedPair(t)
	current := alice.channelState.CurrentFundingOutpoint().Hash
	alice.ApplyReestablishReserves(fn.Some(current), fn.Some(current))
	require.Empty(t, alice.spliceSubst)
	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
	_, err := bob.AddHTLC(htlc, nil)
	require.NoError(t, err)
	_, err = alice.ReceiveHTLC(htlc)
	require.ErrorIs(t, err, ErrBelowChanReserve)
}

// TestSpliceReserveFloorLockAtReestablish checks the floor for a lock that
// completes at channel_reestablish, both naming the pending splice: a peer
// switching at the exchange holds us to the splice's reserves at once, so
// ours keep them on its funding on this connection. Bob, with 5 BTC against
// the splice's 10.1 BTC reserve, can add nothing; Alice, whose balance on
// the splice's funding is 1005 BTC, is not held back on the current one,
// where she has 5. Both naming different transactions adds no floor.
func TestSpliceReserveFloorLockAtReestablish(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 1_000*btcutil.SatoshiPerBitcoin, 0)
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()

	current := bob.channelState.CurrentFundingOutpoint().Hash
	bob.ApplyReestablishReserves(fn.Some(txid), fn.Some(current))
	require.Empty(t, bob.spliceFloor)

	bob.ApplyReestablishReserves(fn.Some(txid), fn.Some(txid))
	alice.ApplyReestablishReserves(fn.Some(txid), fn.Some(txid))
	require.Len(t, bob.spliceFloor, 1)

	htlc, _ := createHTLC(5, lnwire.NewMSatFromSatoshis(1_000_000))
	_, err := bob.AddHTLC(htlc, nil)
	require.ErrorIs(t, err, ErrSpliceReserve)
	require.Zero(t, bob.AvailableBalance())

	ours, _ := createHTLC(6, lnwire.NewMSatFromSatoshis(
		btcutil.SatoshiPerBitcoin))
	_, err = alice.AddHTLC(ours, nil)
	require.NoError(t, err)
}
