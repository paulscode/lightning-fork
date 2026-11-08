package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
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
// still accepted under the reserves before the change; once Alice has
// received his commitment_signed and signed one back, a new HTLC of Bob's is
// held to the new reserves again. Without the record of the earlier
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

		err := ForceStateTransition(bob, alice)
		if !record {
			require.ErrorIs(t, err, ErrBelowChanReserve)

			continue
		}
		require.NoError(t, err)
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
// commitment: after a restart its new updates meet the current reserves,
// while a commitment we sign is still lenient for the ones before.
func TestSpliceReserveLeniencePeerCommitSaved(t *testing.T) {
	t.Parallel()

	alice, bob := lockedSplice(t, true)
	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(1_000_000))
	addAndReceiveHTLC(t, bob, alice, htlc, nil)
	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()

	bobSig, err := bob.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.NoError(t, alice.ReceiveNewCommitment(bobSig.CommitSigs))
	_, _, _, err = alice.RevokeCurrentCommitment()
	require.NoError(t, err)

	rec, found, err := alice.channelState.FetchSpliceReserves()
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
