package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// independentSplicedPair is splicedPair on a channel with
// option_independent_secrets, where Bob reveals random secrets: Alice can
// only punish him with the secrets she stored.
func independentSplicedPair(t *testing.T, aliceDelta,
	bobDelta btcutil.Amount) (*LightningChannel, *LightningChannel,
	chainhash.Hash, *randomSecretProducer) {

	t.Helper()

	alice, bob, err := CreateTestChannels(
		t, spliceChanTypes["anchors"]|channeldb.IndependentSecretsBit,
	)
	require.NoError(t, err)

	_, ok := alice.channelState.RevocationStore.(*shachain.IndependentStore)
	require.True(t, ok, "expected an IndependentStore")

	producer := newRandomSecretProducer(bob.channelState.RevocationProducer)
	bob.channelState.RevocationProducer = producer

	withHTLCs(t, alice, bob)

	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x5c}
	addSplice(t, alice, bob, keys, txid, aliceDelta, bobDelta)
	exchangeSpliceCommitments(t, alice, bob, txid)

	return alice, bob, txid, producer
}

// requireLogSecret checks that a revocation log entry carries the secret Bob
// revealed for its commitment number.
func requireLogSecret(t *testing.T, rl *channeldb.RevocationLog,
	producer *randomSecretProducer, height uint64) {

	t.Helper()

	want, err := producer.AtIndex(height)
	require.NoError(t, err)

	var got [32]byte
	rl.CommitSecret.WhenSome(
		func(r tlv.RecordT[tlv.TlvType65537, [32]byte]) {
			got = r.Val
		},
	)
	require.True(t, rl.CommitSecret.IsSome(), "height %d", height)
	require.Equal(t, [32]byte(*want), got, "height %d", height)
}

// TestIndependentSecretsSpliceBreach is TestSpliceBreachAfterLock on a
// channel with option_independent_secrets: every revoked commitment of
// Bob's can be punished, on the splice's funding while it was pending, on
// the old funding at the lock, and on the new one after it, including after
// a restart. While a splice is pending, Alice logs Bob's revoked commitments
// on its funding in the splice's own log, which must carry the secret too.
func TestIndependentSecretsSpliceBreach(t *testing.T) {
	t.Parallel()

	alice, bob, txid, producer := independentSplicedPair(
		t, 100_000_000, 0,
	)

	type revoked struct {
		height uint64
		tx     *wire.MsgTx
		bob    lnwire.MilliSatoshi
		alice  lnwire.MilliSatoshi
	}
	capture := func(c *commitment) revoked {
		return revoked{c.height, c.txn, c.ourBalance, c.theirBalance}
	}

	// Pending: Bob's commitment at his current height, on both fundings.
	p, err := bob.pendingFunding(txid)
	require.NoError(t, err)
	h := bob.commitChains.Local.tail().height
	pendingOld := capture(bob.commitChains.Local.tail())
	pendingNew := capture(p.local[h])

	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	forceBatchTransition(t, alice, bob)

	// Bob's revoked commitment on the splice's funding is in the splice's
	// log, with the random secret he revealed for it.
	rl, err := alice.channelState.FetchSpliceRevocationLog(txid, h)
	require.NoError(t, err)
	requireLogSecret(t, rl, producer, h)

	// Bob's commitment on the old funding when the splice locks.
	atLock := capture(bob.commitChains.Local.tail())
	scid := lnwire.ShortChannelID{BlockHeight: 900_300}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))

	// After the lock, on the new funding.
	afterLock := capture(bob.commitChains.Local.tail())
	require.NoError(t, ForceStateTransition(alice, bob))
	require.NoError(t, ForceStateTransition(alice, bob))

	all := []revoked{pendingOld, pendingNew, atLock, afterLock}
	for _, r := range all {
		requireBreachFor(t, alice, r.height, r.tx, r.bob, r.alice)
	}

	// And after a restart, from the database alone.
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	for _, r := range all {
		requireBreachFor(t, alice, r.height, r.tx, r.bob, r.alice)
	}
}

// TestIndependentSecretsSpliceTower is TestSpliceTowerRetributions on a
// channel with option_independent_secrets: a watchtower backup can be built
// for every commitment of Bob's at a revoked height, the one in the
// channel's own log and one per splice log holding the height, each with the
// secret Bob revealed.
func TestIndependentSecretsSpliceTower(t *testing.T) {
	t.Parallel()

	alice, bob, txid, producer := independentSplicedPair(
		t, 100_000_000, 0,
	)

	h := bob.commitChains.Local.tail().height

	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	forceBatchTransition(t, alice, bob)

	atLockHeight := bob.commitChains.Local.tail().height
	scid := lnwire.ShortChannelID{BlockHeight: 900_300}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))
	require.NoError(t, ForceStateTransition(alice, bob))
	require.NoError(t, ForceStateTransition(alice, bob))

	for _, height := range []uint64{h, atLockHeight} {
		secret, err := producer.AtIndex(height)
		require.NoError(t, err)
		point := input.ComputeCommitmentPoint(secret[:])

		splices, err := alice.channelState.SpliceRevocationsAt(height)
		require.NoError(t, err)
		require.NotEmpty(t, splices, "height %d", height)

		for _, splice := range append(
			[]chainhash.Hash{{}}, splices...,
		) {
			br, err := NewTowerBreachRetribution(
				alice.channelState, height, splice,
				fn.None[AuxLeafStore](),
				fn.None[AuxContractResolver](),
			)
			require.NoError(t, err, "height %d splice %v",
				height, splice)
			require.True(t, point.IsEqual(br.KeyRing.CommitPoint),
				"height %d splice %v: wrong commitment point",
				height, splice)
		}
	}
}

// TestIndependentSecretsLogWithoutSecret checks the fallback for a revocation
// log entry without the secret, such as a splice's entry written by a
// version that didn't store it there: the secret is taken from the channel's
// own log at the same commitment number, since one secret revokes the
// commitment with that number on every funding output.
func TestIndependentSecretsLogWithoutSecret(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(
		t, independentSecretsChanType(true),
	)
	require.NoError(t, err)

	producer := newRandomSecretProducer(bob.channelState.RevocationProducer)
	bob.channelState.RevocationProducer = producer
	for i := 0; i < 2; i++ {
		require.NoError(t, sendAndSettle(t, alice, bob, i))
	}

	const height = 2
	want, err := producer.AtIndex(height)
	require.NoError(t, err)

	got, err := revokedCommitSecret(
		alice.channelState, &channeldb.RevocationLog{}, height,
	)
	require.NoError(t, err)
	require.Equal(t, *want, *got)

	// A number that was never revoked has no secret anywhere.
	_, err = revokedCommitSecret(
		alice.channelState, &channeldb.RevocationLog{}, 1000,
	)
	require.ErrorContains(t, err, "no per-commitment secret")
}
