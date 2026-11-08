//go:build !dev && !integration

// Release builds only, like the unified signature hash tests: a spliceable
// channel has option_unified_sigs, which development builds never sign with.

package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// spliceChanTypes are the channel types a splice applies to: unified
// signatures, with and without anchors.
var spliceChanTypes = map[string]channeldb.ChannelType{
	"tweakless": channeldb.SingleFunderTweaklessBit |
		channeldb.UnifiedSigsBit,
	"anchors": channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit |
		channeldb.UnifiedSigsBit,
}

// withHTLCs locks in HTLCs both ways, one of them below Bob's dust limit, so
// commitments carry HTLC outputs and a trimmed one.
func withHTLCs(t *testing.T, alice, bob *LightningChannel) {
	t.Helper()

	amounts := []btcutil.Amount{150_000, 900, 220_000}
	for i, amt := range amounts {
		htlc, _ := createHTLC(i, lnwire.NewMSatFromSatoshis(amt))
		addAndReceiveHTLC(t, alice, bob, htlc, nil)
	}
	htlc, _ := createHTLC(0, lnwire.NewMSatFromSatoshis(310_000))
	addAndReceiveHTLC(t, bob, alice, htlc, nil)

	// Bob's HTLC reaches his own commitment in the second round.
	require.NoError(t, ForceStateTransition(alice, bob))
	require.NoError(t, ForceStateTransition(alice, bob))
	require.True(t, alice.quiescent())
	require.True(t, bob.quiescent())
}

// TestRebuildCommitmentIdentity checks the rebuild every pending splice's
// commitments come from: on the current funding with no deltas it gives back
// exactly the commitment it started from, on both chains, for both sides.
func TestRebuildCommitmentIdentity(t *testing.T) {
	t.Parallel()

	for name, chanType := range spliceChanTypes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			alice, bob, err := CreateTestChannels(t, chanType)
			require.NoError(t, err)
			withHTLCs(t, alice, bob)

			for _, lc := range []*LightningChannel{alice, bob} {
				local := lc.commitChains.Local.tail()
				keyRing, err := lc.localKeyRing(local.height)
				require.NoError(t, err)
				got, err := lc.rebuildCommitment(
					local, keyRing,
					lc.currentCommitFunding(),
				)
				require.NoError(t, err)
				require.Equal(t, local.txn.TxHash(),
					got.txn.TxHash())
				require.Equal(t, local.ourBalance,
					got.ourBalance)
				require.Equal(t, local.theirBalance,
					got.theirBalance)
				require.Equal(t, local.fee, got.fee)
				require.Len(t, got.txn.TxOut,
					len(local.txn.TxOut))

				remote := lc.commitChains.Remote.tail()
				keyRing, err = lc.remoteKeyRing()
				require.NoError(t, err)
				got, err = lc.rebuildCommitment(
					remote, keyRing,
					lc.currentCommitFunding(),
				)
				require.NoError(t, err)
				require.Equal(t, remote.txn.TxHash(),
					got.txn.TxHash())
			}
		})
	}
}

// spliceKeys are fresh funding keys for a splice, which each side's signer
// can sign with.
type spliceKeys struct {
	alice, bob *btcec.PrivateKey
}

func newSpliceKeys(t *testing.T, alice, bob *LightningChannel) spliceKeys {
	t.Helper()

	var k spliceKeys
	var err error
	k.alice, err = btcec.NewPrivateKey()
	require.NoError(t, err)
	k.bob, err = btcec.NewPrivateKey()
	require.NoError(t, err)

	aliceSigner := alice.Signer.(*input.MockSigner)
	aliceSigner.Privkeys = append(aliceSigner.Privkeys, k.alice)
	bobSigner := bob.Signer.(*input.MockSigner)
	bobSigner.Privkeys = append(bobSigner.Privkeys, k.bob)

	return k
}

// addSplice adds the same pending splice to both channels: Alice's and
// Bob's balances move by aliceDelta and bobDelta.
func addSplice(t *testing.T, alice, bob *LightningChannel, keys spliceKeys,
	txid chainhash.Hash, aliceDelta, bobDelta btcutil.Amount) wire.OutPoint {

	t.Helper()

	op := wire.OutPoint{Hash: txid, Index: 1}
	capacity := alice.channelState.Capacity + aliceDelta + bobDelta

	require.NoError(t, alice.AddPendingFunding(PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:   keychain.KeyDescriptor{PubKey: keys.alice.PubKey()},
		RemoteKey:  keys.bob.PubKey(),
		LocalDelta: aliceDelta, RemoteDelta: bobDelta,
	}))
	require.NoError(t, bob.AddPendingFunding(PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:   keychain.KeyDescriptor{PubKey: keys.bob.PubKey()},
		RemoteKey:  keys.alice.PubKey(),
		LocalDelta: bobDelta, RemoteDelta: aliceDelta,
	}))

	return op
}

// exchangeSpliceCommitments runs the splice's own commitment_signed both
// ways.
func exchangeSpliceCommitments(t *testing.T, alice, bob *LightningChannel,
	txid chainhash.Hash) {

	t.Helper()

	sigs, err := alice.SignSpliceCommitment(ctxb, txid)
	require.NoError(t, err)
	require.NoError(t, bob.ReceiveSpliceCommitment(txid, sigs))

	sigs, err = bob.SignSpliceCommitment(ctxb, txid)
	require.NoError(t, err)
	require.NoError(t, alice.ReceiveSpliceCommitment(txid, sigs))
}

// requireSpends checks with the script engine that tx spends the pending
// funding output of lc's splice txid.
func requireSpends(t *testing.T, lc *LightningChannel, txid chainhash.Hash,
	tx *wire.MsgTx) {

	t.Helper()

	p, err := lc.pendingFunding(txid)
	require.NoError(t, err)
	require.Equal(t, p.OutPoint, tx.TxIn[0].PreviousOutPoint)

	fetcher := txscript.NewCannedPrevOutputFetcher(
		p.fundingOutput.PkScript, p.fundingOutput.Value,
	)
	vm, err := txscript.NewEngine(
		p.fundingOutput.PkScript, tx, 0, txscript.StandardVerifyFlags,
		nil, txscript.NewTxSigHashes(tx, fetcher),
		p.fundingOutput.Value, fetcher,
	)
	require.NoError(t, err)
	require.NoError(t, vm.Execute())
}

// outputValues returns a commitment's outputs by script.
func outputValues(tx *wire.MsgTx) map[string]int64 {
	m := make(map[string]int64)
	for _, out := range tx.TxOut {
		m[string(out.PkScript)] = out.Value
	}

	return m
}

// TestSpliceCommitmentExchange checks the splice's own commitments: each
// side signs the other's current commitment on the new funding, the result
// spends the new funding output under the chain's rules, and the balances on
// it are the current ones moved by the splice.
func TestSpliceCommitmentExchange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		aliceDelta, bobDelta btcutil.Amount
	}{
		{"alice adds funds", 100_000_000, 0},
		{"bob adds funds", 0, 75_000_000},
		{"alice moves funds out", -40_000_000, 0},
		{"bob moves nearly all out", 0, -490_000_000},
	}

	for name, chanType := range spliceChanTypes {
		for _, tc := range tests {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				alice, bob, err := CreateTestChannels(
					t, chanType,
				)
				require.NoError(t, err)
				withHTLCs(t, alice, bob)

				keys := newSpliceKeys(t, alice, bob)
				txid := chainhash.Hash{0x5a, byte(len(name))}
				addSplice(
					t, alice, bob, keys, txid,
					tc.aliceDelta, tc.bobDelta,
				)
				exchangeSpliceCommitments(t, alice, bob, txid)

				for _, side := range []struct {
					lc                *LightningChannel
					ownDelta, peerDel btcutil.Amount
				}{
					{alice, tc.aliceDelta, tc.bobDelta},
					{bob, tc.bobDelta, tc.aliceDelta},
				} {
					tx, err := side.lc.SignedSpliceCommitment(
						txid,
					)
					require.NoError(t, err)
					requireSpends(t, side.lc, txid, tx)

					// The same commitment as on the
					// current funding, but for the two
					// balance outputs.
					current := side.lc.commitChains.Local.
						tail()
					requireMovedBalances(
						t, current, tx, side.ownDelta,
						side.peerDel,
						keys.alice.PubKey(),
						keys.bob.PubKey(),
					)
				}
			})
		}
	}
}

// requireMovedBalances checks that tx is the commitment current on another
// funding with our and the peer's balance outputs moved by the deltas, every
// HTLC output the same, and the anchors, if any, each locked to one of the
// other funding's keys.
func requireMovedBalances(t *testing.T, current *commitment, tx *wire.MsgTx,
	ourDelta, theirDelta btcutil.Amount,
	fundingKeys ...*btcec.PublicKey) {

	t.Helper()

	split := func(tx *wire.MsgTx) (map[string]int64, []string) {
		others := make(map[string]int64)
		var anchors []string
		for _, out := range tx.TxOut {
			if out.Value == int64(AnchorSize) {
				anchors = append(anchors, string(out.PkScript))
				continue
			}
			others[string(out.PkScript)] = out.Value
		}

		return others, anchors
	}
	before, beforeAnchors := split(current.txn)
	after, afterAnchors := split(tx)
	require.Equal(t, len(before), len(after))
	require.Equal(t, len(beforeAnchors), len(afterAnchors))

	var moved int
	for script, v := range before {
		w, ok := after[script]
		require.True(t, ok, "output gone")
		if v != w {
			moved++
		}
	}
	require.LessOrEqual(t, moved, 2)

	if len(afterAnchors) > 0 {
		allowed := make(map[string]bool)
		for _, key := range fundingKeys {
			script, err := input.CommitScriptAnchor(key)
			require.NoError(t, err)
			pkScript, err := input.WitnessScriptHash(script)
			require.NoError(t, err)
			allowed[string(pkScript)] = true
		}
		for _, a := range afterAnchors {
			require.True(t, allowed[a], "anchor not on the "+
				"splice's funding keys")
		}
	}

	require.Equal(t, totalOut(current.txn)+int64(ourDelta+theirDelta),
		totalOut(tx))
	require.Equal(t, current.txn.LockTime, tx.LockTime)
	require.Equal(t, current.txn.TxIn[0].Sequence, tx.TxIn[0].Sequence)
}

// totalOut sums a transaction's outputs.
func totalOut(tx *wire.MsgTx) int64 {
	var total int64
	for _, out := range tx.TxOut {
		total += out.Value
	}

	return total
}

// TestSpliceCommitmentRefused checks what the splice's own commitments
// refuse: a splice the other side sees differently, a forged signature, an
// unknown splice, updates in flight, a capacity that doesn't add up, and a
// splice taking more than a balance.
func TestSpliceCommitmentRefused(t *testing.T) {
	t.Parallel()

	chanType := spliceChanTypes["anchors"]

	t.Run("different deltas", func(t *testing.T) {
		t.Parallel()

		alice, bob, err := CreateTestChannels(t, chanType)
		require.NoError(t, err)
		keys := newSpliceKeys(t, alice, bob)
		txid := chainhash.Hash{0x61}
		op := wire.OutPoint{Hash: txid, Index: 1}
		capacity := alice.channelState.Capacity + 100_000

		require.NoError(t, alice.AddPendingFunding(PendingFunding{
			OutPoint: op, Capacity: capacity,
			LocalKey: keychain.KeyDescriptor{
				PubKey: keys.alice.PubKey(),
			},
			RemoteKey:  keys.bob.PubKey(),
			LocalDelta: 100_000,
		}))
		// Bob thinks the funds are his.
		require.NoError(t, bob.AddPendingFunding(PendingFunding{
			OutPoint: op, Capacity: capacity,
			LocalKey: keychain.KeyDescriptor{
				PubKey: keys.bob.PubKey(),
			},
			RemoteKey:  keys.alice.PubKey(),
			LocalDelta: 100_000,
		}))

		sigs, err := alice.SignSpliceCommitment(ctxb, txid)
		require.NoError(t, err)
		var sigErr *InvalidCommitSigError
		require.ErrorAs(t, bob.ReceiveSpliceCommitment(txid, sigs),
			&sigErr)
	})

	t.Run("forged and misdirected signatures", func(t *testing.T) {
		t.Parallel()

		alice, bob, err := CreateTestChannels(t, chanType)
		require.NoError(t, err)
		withHTLCs(t, alice, bob)
		keys := newSpliceKeys(t, alice, bob)
		txid := chainhash.Hash{0x62}
		addSplice(t, alice, bob, keys, txid, 50_000, 0)

		sigs, err := alice.SignSpliceCommitment(ctxb, txid)
		require.NoError(t, err)

		// The commitment signature for the current funding instead.
		current, err := alice.SignNextCommitmentForTest()
		require.NoError(t, err)
		misdirected := *sigs
		misdirected.CommitSig = current
		var sigErr *InvalidCommitSigError
		require.ErrorAs(t, bob.ReceiveSpliceCommitment(
			txid, &misdirected,
		), &sigErr)

		// An HTLC signature swapped for another.
		require.GreaterOrEqual(t, len(sigs.HtlcSigs), 2)
		swapped := *sigs
		swapped.HtlcSigs = append([]lnwire.Sig(nil), sigs.HtlcSigs...)
		swapped.HtlcSigs[0], swapped.HtlcSigs[1] =
			swapped.HtlcSigs[1], swapped.HtlcSigs[0]
		require.Error(t, bob.ReceiveSpliceCommitment(txid, &swapped))

		// One HTLC signature missing.
		short := *sigs
		short.HtlcSigs = sigs.HtlcSigs[1:]
		require.Error(t, bob.ReceiveSpliceCommitment(txid, &short))

		// Nothing was recorded by the refusals.
		_, err = bob.SignedSpliceCommitment(txid)
		require.Error(t, err)

		// The real one passes.
		require.NoError(t, bob.ReceiveSpliceCommitment(txid, sigs))
	})

	t.Run("unknown splice", func(t *testing.T) {
		t.Parallel()

		alice, _, err := CreateTestChannels(t, chanType)
		require.NoError(t, err)
		_, err = alice.SignSpliceCommitment(ctxb, chainhash.Hash{1})
		require.ErrorIs(t, err, ErrUnknownFunding)
	})

	t.Run("updates in flight", func(t *testing.T) {
		t.Parallel()

		alice, bob, err := CreateTestChannels(t, chanType)
		require.NoError(t, err)
		keys := newSpliceKeys(t, alice, bob)
		txid := chainhash.Hash{0x63}
		addSplice(t, alice, bob, keys, txid, 50_000, 0)

		htlc, _ := createHTLC(0, 1_000_000)
		addAndReceiveHTLC(t, alice, bob, htlc, nil)

		_, err = alice.SignSpliceCommitment(ctxb, txid)
		require.ErrorIs(t, err, ErrNotQuiescent)
		require.ErrorIs(t, bob.ReceiveSpliceCommitment(
			txid, &CommitSigs{},
		), ErrNotQuiescent)
	})

	t.Run("capacity mismatch and limits", func(t *testing.T) {
		t.Parallel()

		alice, bob, err := CreateTestChannels(t, chanType)
		require.NoError(t, err)
		keys := newSpliceKeys(t, alice, bob)

		f := PendingFunding{
			OutPoint: wire.OutPoint{Hash: chainhash.Hash{0x64}},
			Capacity: alice.channelState.Capacity + 99_999,
			LocalKey: keychain.KeyDescriptor{
				PubKey: keys.alice.PubKey(),
			},
			RemoteKey:  keys.bob.PubKey(),
			LocalDelta: 100_000,
		}
		require.Error(t, alice.AddPendingFunding(f))

		f.Capacity++
		f.RemoteKey = nil
		require.Error(t, alice.AddPendingFunding(f))

		f.RemoteKey = keys.bob.PubKey()
		f.OutPoint = alice.channelState.CurrentFundingOutpoint()
		require.Error(t, alice.AddPendingFunding(f))

		f.OutPoint = wire.OutPoint{Hash: chainhash.Hash{0x64}}
		require.NoError(t, alice.AddPendingFunding(f))
		require.Error(t, alice.AddPendingFunding(f), "twice")
		require.Len(t, alice.PendingFundings(), 1)
	})

	t.Run("more than the balance", func(t *testing.T) {
		t.Parallel()

		alice, bob, err := CreateTestChannels(t, chanType)
		require.NoError(t, err)
		keys := newSpliceKeys(t, alice, bob)
		txid := chainhash.Hash{0x65}

		// Bob has 5 BTC; a splice can't take 5.1 out of it.
		addSplice(t, alice, bob, keys, txid, 0, -510_000_000)
		_, err = alice.SignSpliceCommitment(ctxb, txid)
		require.ErrorIs(t, err, ErrBelowChanReserve)
	})
}

// SignNextCommitmentForTest returns a commitment signature over the current
// funding's remote tail, for tests that need a valid signature for the wrong
// funding.
func (lc *LightningChannel) SignNextCommitmentForTest() (lnwire.Sig, error) {
	lc.Lock()
	defer lc.Unlock()

	tail := lc.commitChains.Remote.tail()
	lc.signDesc.SigHashes = input.NewTxSigHashesV0Only(tail.txn)
	raw, err := lc.Signer.SignOutputRaw(tail.txn, lc.signDesc)
	if err != nil {
		return lnwire.Sig{}, err
	}

	return lnwire.NewSigFromSignature(raw)
}

// forceBatchTransition is ForceStateTransition for channels with pending
// splices: every commitment goes out as a batch.
func forceBatchTransition(t *testing.T, a, b *LightningChannel) {
	t.Helper()

	aCommit, err := a.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.NoError(t, b.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, aCommit.SpliceSigs,
	))
	bRev, _, _, err := b.RevokeCurrentCommitment()
	require.NoError(t, err)
	bCommit, err := b.SignNextCommitment(ctxb)
	require.NoError(t, err)
	_, _, err = a.ReceiveRevocation(bRev)
	require.NoError(t, err)
	require.NoError(t, a.ReceiveNewCommitmentBatch(
		bCommit.CommitSigs, bCommit.SpliceSigs,
	))
	aRev, _, _, err := a.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = b.ReceiveRevocation(aRev)
	require.NoError(t, err)
}

// splicedPair returns two channels with HTLCs and a pending splice whose own
// commitments have been exchanged.
func splicedPair(t *testing.T, aliceDelta,
	bobDelta btcutil.Amount) (*LightningChannel, *LightningChannel,
	chainhash.Hash) {

	t.Helper()

	alice, bob, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)
	withHTLCs(t, alice, bob)

	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x5c}
	addSplice(t, alice, bob, keys, txid, aliceDelta, bobDelta)
	exchangeSpliceCommitments(t, alice, bob, txid)

	return alice, bob, txid
}

// TestSpliceBatchTransitions checks state transitions while a splice is
// pending: every commitment is signed and checked on both fundings, the
// commitment on the splice's funding follows the channel (a new HTLC shows
// up on it), the peer's revoked commitments on it are logged with their own
// txid and output indexes, and all of it survives restarts at rest and with
// a commitment in flight.
func TestSpliceBatchTransitions(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)

	before, err := bob.SignedSpliceCommitment(txid)
	require.NoError(t, err)

	// A new HTLC from Alice, locked in on both fundings.
	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)

	// Bob's commitment on the splice's funding at his current height:
	// what Alice will log once he revokes it.
	bobHeight := bob.commitChains.Local.tail().height
	p, err := bob.pendingFunding(txid)
	require.NoError(t, err)
	bobRevoked := p.local[bobHeight]
	require.NotNil(t, bobRevoked)

	forceBatchTransition(t, alice, bob)
	forceBatchTransition(t, alice, bob)

	after, err := bob.SignedSpliceCommitment(txid)
	require.NoError(t, err)
	requireSpends(t, bob, txid, after)
	require.Len(t, after.TxOut, len(before.TxOut)+1)

	tx, err := alice.SignedSpliceCommitment(txid)
	require.NoError(t, err)
	requireSpends(t, alice, txid, tx)

	// Alice's log of Bob's revoked commitment on the splice's funding.
	rl, err := alice.channelState.FetchSpliceRevocationLog(
		txid, bobHeight,
	)
	require.NoError(t, err)
	require.Equal(t, [32]byte(bobRevoked.txn.TxHash()), rl.CommitTxHash.Val)
	ours := bobRevoked.txn.TxOut[rl.OurOutputIndex.Val]
	require.Equal(t, int64(bobRevoked.theirBalance.ToSatoshis()),
		ours.Value)
	theirs := bobRevoked.txn.TxOut[rl.TheirOutputIndex.Val]
	require.Equal(t, int64(bobRevoked.ourBalance.ToSatoshis()),
		theirs.Value)

	// It isn't the channel's own entry at that height.
	own, _, err := alice.channelState.FindPreviousState(bobHeight)
	require.NoError(t, err)
	require.NotEqual(t, own.CommitTxHash.Val, rl.CommitTxHash.Val)

	// Restart both at rest: the same commitments come back.
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	bob, err = restartChannel(bob)
	require.NoError(t, err)
	again, err := bob.SignedSpliceCommitment(txid)
	require.NoError(t, err)
	require.Equal(t, after.TxHash(), again.TxHash())

	// Restart Alice with her commitment in flight.
	htlc, _ = createHTLC(4, lnwire.NewMSatFromSatoshis(120_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	aCommit, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.Len(t, aCommit.SpliceSigs, 1)

	alice, err = restartChannel(alice)
	require.NoError(t, err)

	require.NoError(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, aCommit.SpliceSigs,
	))
	bRev, _, _, err := bob.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = alice.ReceiveRevocation(bRev)
	require.NoError(t, err)
	forceBatchTransition(t, alice, bob)

	final, err := bob.SignedSpliceCommitment(txid)
	require.NoError(t, err)
	requireSpends(t, bob, txid, final)
	require.Len(t, final.TxOut, len(after.TxOut)+1)
}

// TestSpliceBatchRefused checks that a commitment batch must name exactly
// the pending splices, each once, with valid signatures, and that no
// revocation goes out without the next commitment signed on every funding.
func TestSpliceBatchRefused(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 0, 60_000_000)

	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	aCommit, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.Len(t, aCommit.SpliceSigs, 1)
	member := aCommit.SpliceSigs[0]
	require.Equal(t, txid, member.FundingTxid)

	// The current funding's signatures alone.
	require.Error(t, bob.ReceiveNewCommitment(aCommit.CommitSigs))

	// A member for a splice Bob doesn't know.
	unknown := member
	unknown.FundingTxid = chainhash.Hash{0xdd}
	require.ErrorIs(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, []SpliceCommitSigs{unknown},
	), ErrUnknownFunding)

	// The same splice twice, or an extra member.
	require.Error(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, []SpliceCommitSigs{member, member},
	))
	require.Error(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, []SpliceCommitSigs{member, unknown},
	))

	// Nothing was accepted: no revocation can go out.
	_, _, _, err = bob.RevokeCurrentCommitment()
	require.Error(t, err)

	// The splice member signed for the current funding instead.
	forged := member
	forgedSigs := *member.CommitSigs
	forgedSigs.CommitSig = aCommit.CommitSig
	forged.CommitSigs = &forgedSigs
	var sigErr *InvalidCommitSigError
	require.ErrorAs(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, []SpliceCommitSigs{forged},
	), &sigErr)

	_, _, _, err = bob.RevokeCurrentCommitment()
	require.Error(t, err)
}

// TestSpliceBatchTwoPending checks a batch with two pending splices (the
// shape replacement attempts take): both are signed, a batch naming one of
// them twice is refused, and both commitments follow the channel.
func TestSpliceBatchTwoPending(t *testing.T) {
	t.Parallel()

	alice, bob, first := splicedPair(t, 100_000_000, 0)

	keys := newSpliceKeys(t, alice, bob)
	second := chainhash.Hash{0x5d}
	addSplice(t, alice, bob, keys, second, 90_000_000, 0)
	exchangeSpliceCommitments(t, alice, bob, second)

	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	aCommit, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.Len(t, aCommit.SpliceSigs, 2)

	twice := []SpliceCommitSigs{aCommit.SpliceSigs[0], aCommit.SpliceSigs[0]}
	require.ErrorContains(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, twice,
	), "twice")

	// In either order, the right members pass.
	reversed := []SpliceCommitSigs{aCommit.SpliceSigs[1], aCommit.SpliceSigs[0]}
	require.NoError(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, reversed,
	))
	bRev, _, _, err := bob.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = alice.ReceiveRevocation(bRev)
	require.NoError(t, err)
	forceBatchTransition(t, bob, alice)

	for _, txid := range []chainhash.Hash{first, second} {
		for _, lc := range []*LightningChannel{alice, bob} {
			tx, err := lc.SignedSpliceCommitment(txid)
			require.NoError(t, err)
			requireSpends(t, lc, txid, tx)
		}
	}

	// Members swapped between the two splices. Last: a signature that
	// doesn't verify ends the channel's updates, as on any channel.
	htlc, _ = createHTLC(4, lnwire.NewMSatFromSatoshis(100_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	aCommit, err = alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	swapped := []SpliceCommitSigs{{
		FundingTxid: aCommit.SpliceSigs[0].FundingTxid,
		CommitSigs:  aCommit.SpliceSigs[1].CommitSigs,
	}, {
		FundingTxid: aCommit.SpliceSigs[1].FundingTxid,
		CommitSigs:  aCommit.SpliceSigs[0].CommitSigs,
	}}
	var sigErr *InvalidCommitSigError
	require.ErrorAs(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, swapped,
	), &sigErr)
}

// TestSpliceAvailableBalance checks that while a splice is pending, what we
// can send is the least on any funding: moving funds out lowers it by the
// amount, adding funds doesn't raise it, and the peer's splice doesn't change
// ours.
func TestSpliceAvailableBalance(t *testing.T) {
	t.Parallel()

	const out = btcutil.Amount(300_000_000)

	alice, bob, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)
	withHTLCs(t, alice, bob)
	aliceBefore := alice.AvailableBalance()
	bobBefore := bob.AvailableBalance()

	keys := newSpliceKeys(t, alice, bob)
	addSplice(t, alice, bob, keys, chainhash.Hash{0x71}, -out, 0)
	require.Equal(t, aliceBefore-lnwire.NewMSatFromSatoshis(out),
		alice.AvailableBalance())
	require.Equal(t, bobBefore, bob.AvailableBalance())

	// A second, adding funds, changes nothing.
	keys = newSpliceKeys(t, alice, bob)
	addSplice(t, alice, bob, keys, chainhash.Hash{0x72}, 0, 50_000_000)
	require.Equal(t, aliceBefore-lnwire.NewMSatFromSatoshis(out),
		alice.AvailableBalance())
	require.Equal(t, bobBefore, bob.AvailableBalance())
}

// TestSpliceSanity checks that an HTLC must fit on every funding: one that
// fits on the current funding but not on a pending splice-out's is refused
// by the sender and by the receiver.
func TestSpliceSanity(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)
	withHTLCs(t, alice, bob)

	// Alice moves out all but 2,000,000 sats of what she can send.
	avail := alice.AvailableBalance().ToSatoshis()
	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x73}
	addSplice(t, alice, bob, keys, txid, -(avail - 2_000_000), 0)
	exchangeSpliceCommitments(t, alice, bob, txid)

	left := alice.AvailableBalance().ToSatoshis()
	require.InDelta(t, 2_000_000, float64(left), 1)

	// Too much for the splice's funding, fine for the current one.
	big, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(left+100_000))
	_, err = alice.AddHTLC(big, nil)
	require.ErrorIs(t, err, ErrBelowChanReserve)

	// Bob refuses it too if Alice sends it anyway.
	_, err = bob.ReceiveHTLC(big)
	require.ErrorIs(t, err, ErrBelowChanReserve)

	// What fits on every funding goes through and locks in.
	small, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(left-100_000))
	addAndReceiveHTLC(t, alice, bob, small, nil)
	forceBatchTransition(t, alice, bob)
	forceBatchTransition(t, alice, bob)

	tx, err := bob.SignedSpliceCommitment(txid)
	require.NoError(t, err)
	requireSpends(t, bob, txid, tx)
}

// requireCommitSpends checks with the script engine that lc's own signed
// commitment spends its current funding output.
func requireCommitSpends(t *testing.T, lc *LightningChannel) {
	t.Helper()

	tx, err := lc.getSignedCommitTx()
	require.NoError(t, err)
	require.Equal(t, lc.channelState.CurrentFundingOutpoint(),
		tx.TxIn[0].PreviousOutPoint)

	out := lc.fundingOutput
	fetcher := txscript.NewCannedPrevOutputFetcher(out.PkScript, out.Value)
	vm, err := txscript.NewEngine(
		out.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, fetcher), out.Value, fetcher,
	)
	require.NoError(t, err)
	require.NoError(t, vm.Execute())
}

// TestSpliceLock checks completing a splice: the channel moves to the new
// funding output with its commitments, updates go on without batches, the
// channel's own commitment spends the new output, and all of it survives a
// restart. The lock is taken in the middle of a state transition, with a
// commitment in flight on both chains.
func TestSpliceLock(t *testing.T) {
	t.Parallel()

	const added = btcutil.Amount(100_000_000)

	alice, bob, txid := splicedPair(t, added, 0)
	capacity := alice.channelState.Capacity + added
	p, err := alice.pendingFunding(txid)
	require.NoError(t, err)
	newFunding := p.OutPoint
	scid := lnwire.ShortChannelID{BlockHeight: 900_100, TxIndex: 4}

	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	forceBatchTransition(t, alice, bob)

	// Alice signs; Bob has it but hasn't revoked; then both lock.
	htlc, _ = createHTLC(4, lnwire.NewMSatFromSatoshis(150_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	aCommit, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.NoError(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, aCommit.SpliceSigs,
	))

	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))

	for _, lc := range []*LightningChannel{alice, bob} {
		require.Empty(t, lc.PendingFundings())
		require.Equal(t, newFunding,
			lc.channelState.CurrentFundingOutpoint())
		require.Equal(t, capacity, lc.channelState.Capacity)
		require.Equal(t, capacity, lc.Capacity)
		require.Equal(t, int64(capacity), lc.fundingOutput.Value)
	}

	// The transition finishes, on the new funding alone.
	bRev, _, _, err := bob.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = alice.ReceiveRevocation(bRev)
	require.NoError(t, err)
	require.NoError(t, ForceStateTransition(bob, alice))

	bCommit, err := bob.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.Empty(t, bCommit.SpliceSigs)
	require.NoError(t, alice.ReceiveNewCommitment(bCommit.CommitSigs))
	aRev, _, _, err := alice.RevokeCurrentCommitment()
	require.NoError(t, err)
	_, _, err = bob.ReceiveRevocation(aRev)
	require.NoError(t, err)
	require.NoError(t, ForceStateTransition(alice, bob))

	requireCommitSpends(t, alice)
	requireCommitSpends(t, bob)

	// Both restart: the new funding and commitments come back.
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	bob, err = restartChannel(bob)
	require.NoError(t, err)
	require.Equal(t, newFunding, alice.channelState.CurrentFundingOutpoint())
	require.Empty(t, alice.PendingFundings())
	requireCommitSpends(t, alice)
	requireCommitSpends(t, bob)

	htlc, _ = createHTLC(5, lnwire.NewMSatFromSatoshis(90_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	require.NoError(t, ForceStateTransition(alice, bob))
	require.NoError(t, ForceStateTransition(alice, bob))
	requireCommitSpends(t, alice)
	requireCommitSpends(t, bob)

	// The old funding is kept for reorgs.
	prev, err := alice.channelState.PreviousFundings()
	require.NoError(t, err)
	require.Len(t, prev, 1)
	require.Equal(t, alice.channelState.FundingOutpoint, prev[0].OutPoint)
	require.Equal(t, newFunding, prev[0].LockedTo)
}

// TestSpliceLockRefused checks that a splice can't lock before its
// commitments were exchanged, or under a txid that isn't pending.
func TestSpliceLockRefused(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)
	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x74}
	addSplice(t, alice, bob, keys, txid, 10_000_000, 0)

	scid := lnwire.ShortChannelID{BlockHeight: 900_100}
	require.Error(t, alice.LockPendingFunding(txid, scid))
	require.ErrorIs(t, alice.LockPendingFunding(chainhash.Hash{1}, scid),
		ErrUnknownFunding)

	// Signed by us only: not enough either.
	_, err = alice.SignSpliceCommitment(ctxb, txid)
	require.NoError(t, err)
	require.Error(t, alice.LockPendingFunding(txid, scid))
	require.Len(t, alice.PendingFundings(), 1)
}

// TestSpliceRestoreFailure checks a channel whose pending splice can't be
// restored: it still loads and can be closed on its current funding, but
// every update is refused.
func TestSpliceRestoreFailure(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)

	// The stored commitment on the splice's funding no longer matches.
	err := bob.channelState.UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.LocalCommit.CommitTx.TxOut[0].Value++
			return nil
		})
	require.NoError(t, err)

	bob, err = restartChannel(bob)
	require.NoError(t, err)
	require.Error(t, bob.SpliceRestoreError())
	require.Empty(t, bob.PendingFundings())

	requireCommitSpends(t, bob)

	_, err = bob.SignNextCommitment(ctxb)
	require.Error(t, err)
	aCommit, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.Error(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, aCommit.SpliceSigs,
	))
	require.Error(t, bob.AddPendingFunding(PendingFunding{}))
}

// TestSpliceNoUpdatesBeforeExchange checks that no commitment moves between
// adding a splice and exchanging its own commitments, which are signed at
// the current heights: signing and accepting are refused without changing
// anything, and work once the exchange is done.
func TestSpliceNoUpdatesBeforeExchange(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)
	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x75}
	addSplice(t, alice, bob, keys, txid, 10_000_000, 0)

	htlc, _ := createHTLC(0, lnwire.NewMSatFromSatoshis(100_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)

	_, err = alice.SignNextCommitment(ctxb)
	require.ErrorIs(t, err, ErrSpliceNotExchanged)
	require.ErrorIs(t, bob.ReceiveNewCommitmentBatch(
		&CommitSigs{}, nil,
	), ErrSpliceNotExchanged)

	// A splice can't be added with updates in flight either.
	keys2 := newSpliceKeys(t, alice, bob)
	require.ErrorIs(t, alice.AddPendingFunding(PendingFunding{
		OutPoint: wire.OutPoint{Hash: chainhash.Hash{0x76}},
		Capacity: alice.channelState.Capacity + 1,
		LocalKey: keychain.KeyDescriptor{
			PubKey: keys2.alice.PubKey(),
		},
		RemoteKey: keys2.bob.PubKey(), LocalDelta: 1,
	}), ErrNotQuiescent)
}

// TestSpliceRemove checks dropping a pending splice: its record goes, the
// channel updates without it, and a batch member the peer still sends for it
// counts as obsolete.
func TestSpliceRemove(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 0, -40_000_000)

	require.NoError(t, alice.RemovePendingFunding(txid))
	require.NoError(t, bob.RemovePendingFunding(txid))
	require.ErrorIs(t, alice.RemovePendingFunding(txid), ErrUnknownFunding)
	require.True(t, alice.IsObsoleteFunding(txid))
	require.Empty(t, alice.PendingFundings())

	// Retired, with Bob's commitment on it still to be revoked and
	// logged: a reorganisation could confirm the splice yet.
	records, err := alice.channelState.SpliceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.True(t, records[0].Retired)
	require.Nil(t, records[0].LocalCommit)
	require.Nil(t, records[0].Negotiation)
	dropHeight := records[0].RemoteTail.CommitHeight

	// Bob's splice-out no longer limits what he can send (his HTLC id
	// 0 is taken by withHTLCs).
	htlc, _ := createHTLC(1, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, bob, alice, htlc, nil)
	require.NoError(t, ForceStateTransition(bob, alice))
	require.NoError(t, ForceStateTransition(bob, alice))

	// Bob revoked it: logged on the splice's funding, the record gone,
	// the splice still known.
	_, err = alice.channelState.FetchSpliceRevocationLog(txid, dropHeight)
	require.NoError(t, err)
	records, err = alice.channelState.SpliceRecords()
	require.NoError(t, err)
	require.Empty(t, records)
	known, err := alice.channelState.DroppedSplice(txid)
	require.NoError(t, err)
	require.Equal(t, txid, known.Txid())
	require.Nil(t, known.RemoteTail)

	// After a restart too.
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	require.Empty(t, alice.PendingFundings())
	require.NoError(t, alice.SpliceRestoreError())
}

// TestSpliceRestartLockForceClose checks a quiet channel waiting for its
// splice to confirm: both restart, the splice locks before any further
// update, and each side can still force close from its commitment on the new
// funding, HTLC signatures included.
func TestSpliceRestartLockForceClose(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)

	var err error
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	bob, err = restartChannel(bob)
	require.NoError(t, err)

	scid := lnwire.ShortChannelID{BlockHeight: 900_200}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))

	for _, lc := range []*LightningChannel{alice, bob} {
		var nonDust int
		for _, h := range lc.channelState.LocalCommitment.Htlcs {
			if h.OutputIndex >= 0 {
				nonDust++
				require.NotEmpty(t, h.Signature)
			}
		}
		require.Positive(t, nonDust)

		summary, err := lc.ForceClose()
		require.NoError(t, err)
		require.Equal(t, lc.channelState.CurrentFundingOutpoint(),
			summary.CloseTx.TxIn[0].PreviousOutPoint)
	}
}

// requireBreachFor checks that Alice's breach retribution for Bob's revoked
// commitment tx at height names that transaction, and that the outputs it
// sweeps are Bob's and Alice's balances on it.
func requireBreachFor(t *testing.T, alice *LightningChannel, height uint64,
	tx *wire.MsgTx, bobBalance, aliceBalance lnwire.MilliSatoshi) {

	t.Helper()

	br, err := NewBreachRetribution(
		alice.channelState, height, 1, tx,
		fn.None[AuxLeafStore](), fn.None[AuxContractResolver](),
	)
	require.NoError(t, err)
	require.Equal(t, tx.TxHash(), br.BreachTxHash)
	require.Equal(t, int64(aliceBalance.ToSatoshis()),
		tx.TxOut[br.LocalOutpoint.Index].Value)
	require.Equal(t, int64(bobBalance.ToSatoshis()),
		tx.TxOut[br.RemoteOutpoint.Index].Value)
}

// TestSpliceBreachAfterLock checks that every revoked commitment of Bob's
// can be punished after a splice locked: on the new funding from while the
// splice was pending, on the old funding from before the lock (a reorg can
// bring it back), including the ones still unrevoked at the lock, and on the
// new funding after it.
func TestSpliceBreachAfterLock(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)

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

	// Bob's commitment on the old funding when the splice locks.
	atLock := capture(bob.commitChains.Local.tail())
	scid := lnwire.ShortChannelID{BlockHeight: 900_300}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))

	// After the lock, on the new funding.
	afterLock := capture(bob.commitChains.Local.tail())
	require.NoError(t, ForceStateTransition(alice, bob))
	require.NoError(t, ForceStateTransition(alice, bob))

	for _, r := range []revoked{pendingOld, pendingNew, atLock, afterLock} {
		requireBreachFor(t, alice, r.height, r.tx, r.bob, r.alice)
	}

	// The old funding's commitments at the lock are all logged: nothing
	// left to wait for.
	require.Empty(t, alice.retiredFundings)
	records, err := alice.channelState.SpliceRecords()
	require.NoError(t, err)
	require.Empty(t, records)

	// And after a restart.
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	requireBreachFor(t, alice, atLock.height, atLock.tx, atLock.bob,
		atLock.alice)
}

// TestSpliceTowerRetributions checks a watchtower gets every commitment of
// Bob's a breach could publish: at each revoked height, the one in the
// channel's own log and one per splice log holding the height, each built
// from its own entry. Taken after the lock and further updates, when the
// channel's current funding is the splice's: the channel's own log must not
// be read through it.
func TestSpliceTowerRetributions(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)
	oldTxid := alice.channelState.FundingOutpoint.Hash

	// While pending: Bob's commitment at his current height on both.
	p, err := bob.pendingFunding(txid)
	require.NoError(t, err)
	h := bob.commitChains.Local.tail().height
	pendingOld := bob.commitChains.Local.tail().txn
	pendingNew := p.local[h].txn

	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	forceBatchTransition(t, alice, bob)

	// At the lock, revoked after it: the same height on the old funding
	// and, swapped in by the lock, on the new.
	atLockHeight := bob.commitChains.Local.tail().height
	atLock := bob.commitChains.Local.tail().txn
	scid := lnwire.ShortChannelID{BlockHeight: 900_300}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))
	atLockNew := bob.commitChains.Local.tail().txn
	require.NotEqual(t, atLock.TxHash(), atLockNew.TxHash())

	// After the lock, on the new funding only.
	require.NoError(t, ForceStateTransition(alice, bob))
	laterHeight := bob.commitChains.Local.tail().height
	later := bob.commitChains.Local.tail().txn
	require.NoError(t, ForceStateTransition(alice, bob))

	// What the tower gets at a height: the txids of the commitments.
	towerTxids := func(height uint64) []chainhash.Hash {
		t.Helper()

		var txids []chainhash.Hash
		build := func(splice chainhash.Hash) {
			br, err := NewTowerBreachRetribution(
				alice.channelState, height, splice,
				fn.None[AuxLeafStore](),
				fn.None[AuxContractResolver](),
			)
			require.NoError(t, err)
			txids = append(txids, br.BreachTxHash)
		}
		build(chainhash.Hash{})
		splices, err := alice.channelState.SpliceRevocationsAt(height)
		require.NoError(t, err)
		for _, s := range splices {
			build(s)
		}

		return txids
	}

	require.ElementsMatch(t, []chainhash.Hash{
		pendingOld.TxHash(), pendingNew.TxHash(),
	}, towerTxids(h))
	require.ElementsMatch(t, []chainhash.Hash{
		atLock.TxHash(), atLockNew.TxHash(),
	}, towerTxids(atLockHeight))
	require.Equal(t, []chainhash.Hash{later.TxHash()},
		towerTxids(laterHeight))

	// The old funding's entry at the lock is in its own log.
	splices, err := alice.channelState.SpliceRevocationsAt(atLockHeight)
	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{oldTxid}, splices)
}

// TestSpliceAnchorResolutions checks the anchors of the commitments on a
// pending splice's funding output are offered too, each on its own
// commitment and signed with the splice's funding key, and none once the
// splice locked.
func TestSpliceAnchorResolutions(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)

	res, err := alice.NewAnchorResolutions()
	require.NoError(t, err)
	require.NotNil(t, res.Local)
	require.Len(t, res.Splices, 1)
	splice := res.Splices[0]
	require.NotNil(t, splice.Local)
	require.NotNil(t, splice.Remote)

	p, err := alice.pendingFunding(txid)
	require.NoError(t, err)
	localTx := p.local[alice.commitChains.Local.tail().height].txn
	remoteTx := p.remote[alice.commitChains.Remote.tail().height].txn
	require.Equal(t, localTx.TxHash(), splice.Local.CommitAnchor.Hash)
	require.Equal(t, remoteTx.TxHash(), splice.Remote.CommitAnchor.Hash)
	require.NotEqual(t, res.Local.CommitAnchor.Hash,
		splice.Local.CommitAnchor.Hash)

	// The anchor is ours on that commitment, by the splice's key.
	require.True(t, p.LocalKey.PubKey.IsEqual(
		splice.Local.AnchorSignDescriptor.KeyDesc.PubKey,
	))
	out := localTx.TxOut[splice.Local.CommitAnchor.Index]
	require.Equal(t, splice.Local.AnchorSignDescriptor.Output.PkScript,
		out.PkScript)

	scid := lnwire.ShortChannelID{BlockHeight: 900_300}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))
	res, err = alice.NewAnchorResolutions()
	require.NoError(t, err)
	require.Empty(t, res.Splices)
	require.Equal(t, localTx.TxHash(), res.Local.CommitAnchor.Hash)
}

// TestSpliceLockRetiresSibling checks that locking one transaction of a
// splice retires another signed one, a replacement that lost, rather than
// deleting it: the peer's commitment on it is logged once revoked, and the
// splice stays known, since a reorganisation could still confirm it.
func TestSpliceLockRetiresSibling(t *testing.T) {
	t.Parallel()

	alice, bob, first := splicedPair(t, 100_000_000, 0)
	keys := newSpliceKeys(t, alice, bob)
	second := chainhash.Hash{0x5d}
	addSplice(t, alice, bob, keys, second, 90_000_000, 0)
	exchangeSpliceCommitments(t, alice, bob, second)

	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, alice.LockPendingFunding(first, scid))
	require.NoError(t, bob.LockPendingFunding(first, scid))

	retired, err := alice.channelState.DroppedSplice(second)
	require.NoError(t, err)
	require.True(t, retired.Retired)
	require.NotNil(t, retired.RemoteTail)
	height := retired.RemoteTail.CommitHeight

	htlc, _ := createHTLC(3, lnwire.NewMSatFromSatoshis(400_000))
	addAndReceiveHTLC(t, alice, bob, htlc, nil)
	require.NoError(t, ForceStateTransition(alice, bob))
	require.NoError(t, ForceStateTransition(bob, alice))

	_, err = alice.channelState.FetchSpliceRevocationLog(second, height)
	require.NoError(t, err)
	known, err := alice.channelState.DroppedSplice(second)
	require.NoError(t, err)
	require.Nil(t, known.RemoteTail)
}

// TestSpliceObsoleteAfterRestart checks that a splice transaction dropped at
// the lock, a replacement that lost, is still obsolete after a restart: the
// peer's batch signed before it locked, or retransmitted, may name it, and
// a member for a funding neither pending nor obsolete fails the batch.
func TestSpliceObsoleteAfterRestart(t *testing.T) {
	t.Parallel()

	alice, bob, first := splicedPair(t, 100_000_000, 0)
	keys := newSpliceKeys(t, alice, bob)
	second := chainhash.Hash{0x5d}
	addSplice(t, alice, bob, keys, second, 90_000_000, 0)
	exchangeSpliceCommitments(t, alice, bob, second)

	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, alice.LockPendingFunding(first, scid))
	require.True(t, alice.IsObsoleteFunding(second))

	alice, err := restartChannel(alice)
	require.NoError(t, err)
	require.True(t, alice.IsObsoleteFunding(second))

	// A dropped one too.
	alice2, bob2, txid := splicedPair(t, 0, -40_000_000)
	_ = bob2
	require.NoError(t, alice2.RemovePendingFunding(txid))
	alice2, err = restartChannel(alice2)
	require.NoError(t, err)
	require.True(t, alice2.IsObsoleteFunding(txid))
}

// TestSpliceReserveOnCandidateOnly checks a commitment that keeps the
// reserve on the current funding but not on a pending splice's, where the
// opener's balance is lower: the opener raising its fee after moving most of
// its balance out, having checked only the current funding, as the Core
// Lightning port does. Refused with ErrSpliceReserve, which the link answers
// with a disconnection rather than a force close.
func TestSpliceReserveOnCandidateOnly(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)

	// Alice, the opener, moves out all but a little above the reserve
	// Bob holds her to.
	reserve := bob.channelState.RemoteChanCfg.ChanReserve
	balance := alice.channelState.LocalCommitment.LocalBalance.ToSatoshis()
	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x77}
	addSplice(t, alice, bob, keys, txid, -(balance - reserve - 20_000), 0)
	exchangeSpliceCommitments(t, alice, bob, txid)

	// Her own check, against the current funding only, as the port's.
	alice.channelState.LocalChanCfg.ChanReserve = 0
	alice.channelState.RemoteChanCfg.ChanReserve = 0
	fee := alice.channelState.LocalCommitment.FeePerKw * 20
	require.NoError(t, alice.UpdateFee(chainfee.SatPerKWeight(fee)))
	require.NoError(t, bob.ReceiveUpdateFee(chainfee.SatPerKWeight(fee)))
	commit, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)

	err = bob.ReceiveNewCommitmentBatch(commit.CommitSigs, commit.SpliceSigs)
	require.ErrorIs(t, err, ErrSpliceReserve)
	require.ErrorIs(t, err, ErrBelowChanReserve)
}

// TestSpliceFeeUnaffordableOnCandidateOnly checks a fee the opener can pay
// on the current funding but not at all on a pending splice's, its balance
// there below the commitment fee: refused with ErrSpliceReserve too, so the
// link disconnects rather than force closing, the peer (the Core Lightning
// port) having checked only the current funding.
func TestSpliceFeeUnaffordableOnCandidateOnly(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(t, spliceChanTypes["anchors"])
	require.NoError(t, err)

	reserve := bob.channelState.RemoteChanCfg.ChanReserve
	balance := alice.channelState.LocalCommitment.LocalBalance.ToSatoshis()
	keys := newSpliceKeys(t, alice, bob)
	txid := chainhash.Hash{0x78}
	addSplice(t, alice, bob, keys, txid, -(balance - reserve - 2_000), 0)
	exchangeSpliceCommitments(t, alice, bob, txid)

	// Bob's check of Alice's next commitment at a fee her balance on the
	// splice, reserve + 2,000 sat, can't pay, though hers on the current
	// funding can.
	theirs := bob.channelState.LocalCommitment.RemoteBalance
	ours := bob.channelState.LocalCommitment.LocalBalance
	const weight = lntypes.WeightUnit(1_124)
	fee := chainfee.SatPerKWeight(
		(reserve + 2_000 + 10_000) * 1_000 / 1_124,
	)
	require.Less(t, lnwire.NewMSatFromSatoshis(fee.FeeForWeight(weight)),
		theirs)
	err = bob.checkPendingFundingBalances(
		ours, theirs, ours, theirs, weight, fee, NoBuffer,
		lenientNone,
	)
	require.ErrorIs(t, err, ErrSpliceReserve)
	require.ErrorIs(t, err, ErrBelowChanReserve)
}

// TestSpliceRestoreFailureRevocation checks a channel whose pending splices
// couldn't be restored after it signed a commitment: the peer's revocation
// is refused too, and so is revoking our own, so no state moves. Accepted,
// the splices' logs missed the height and their records fell behind the
// chain, frozen for good.
func TestSpliceRestoreFailureRevocation(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)

	htlc, _ := createHTLC(0, lnwire.NewMSatFromSatoshis(100_000))
	index, err := bob.AddHTLC(htlc, nil)
	require.NoError(t, err)
	htlc.ID = index
	_, err = alice.ReceiveHTLC(htlc)
	require.NoError(t, err)
	bCommit, err := bob.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.NoError(t, alice.ReceiveNewCommitmentBatch(
		bCommit.CommitSigs, bCommit.SpliceSigs,
	))
	rev, _, _, err := alice.RevokeCurrentCommitment()
	require.NoError(t, err)

	err = bob.channelState.UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.LocalCommit.CommitTx.TxOut[0].Value++
			return nil
		})
	require.NoError(t, err)
	bob, err = restartChannel(bob)
	require.NoError(t, err)
	require.Error(t, bob.SpliceRestoreError())

	_, _, err = bob.ReceiveRevocation(rev)
	require.ErrorIs(t, err, bob.SpliceRestoreError())
	_, _, _, err = bob.RevokeCurrentCommitment()
	require.Error(t, err)
}

// TestSpliceChanSyncOweRevocationAndCommitment checks a reconnection on which
// we owe both our revocation and our commitment while a splice is pending:
// the peer added an HTLC and signed, we revoked and stopped before signing,
// and the revocation was lost. Our commitment goes out in its batch, a
// commitment_signed per funding. Alone, the peer fails the channel (the Core
// Lightning port does, as BOLT 2 asks).
func TestSpliceChanSyncOweRevocationAndCommitment(t *testing.T) {
	t.Parallel()

	alice, bob, txid := splicedPair(t, 100_000_000, 0)

	htlc, _ := createHTLC(0, lnwire.NewMSatFromSatoshis(100_000))
	index, err := alice.AddHTLC(htlc, nil)
	require.NoError(t, err)
	htlc.ID = index
	_, err = bob.ReceiveHTLC(htlc)
	require.NoError(t, err)
	aCommit, err := alice.SignNextCommitment(ctxb)
	require.NoError(t, err)
	require.NoError(t, bob.ReceiveNewCommitmentBatch(
		aCommit.CommitSigs, aCommit.SpliceSigs,
	))
	_, _, _, err = bob.RevokeCurrentCommitment()
	require.NoError(t, err)

	aliceSync, err := alice.channelState.ChanSyncMsg()
	require.NoError(t, err)
	msgs, _, _, err := bob.ProcessChanSyncMsg(ctxb, aliceSync)
	require.NoError(t, err)

	require.Len(t, msgs, 4)
	require.IsType(t, &lnwire.RevokeAndAck{}, msgs[0])
	batch, ok := msgs[1].(*lnwire.StartBatch)
	require.True(t, ok, "%T", msgs[1])
	require.EqualValues(t, 2, batch.BatchSize)
	var named []chainhash.Hash
	for _, m := range msgs[2:] {
		sig, ok := m.(*lnwire.CommitSig)
		require.True(t, ok, "%T", m)
		sig.FundingTxid.WhenSome(func(h chainhash.Hash) {
			named = append(named, h)
		})
	}
	require.Contains(t, named, txid)

	// Alice takes them as they come.
	rev := msgs[0].(*lnwire.RevokeAndAck)
	_, _, err = alice.ReceiveRevocation(rev)
	require.NoError(t, err)
	main := msgs[2].(*lnwire.CommitSig)
	var spliceSigs []SpliceCommitSigs
	for _, m := range msgs[3:] {
		sig := m.(*lnwire.CommitSig)
		spliceSigs = append(spliceSigs, SpliceCommitSigs{
			FundingTxid: sig.FundingTxid.UnsafeFromSome(),
			CommitSigs: &CommitSigs{
				CommitSig: sig.CommitSig, HtlcSigs: sig.HtlcSigs,
			},
		})
	}
	require.NoError(t, alice.ReceiveNewCommitmentBatch(&CommitSigs{
		CommitSig: main.CommitSig, HtlcSigs: main.HtlcSigs,
	}, spliceSigs))
}

// TestApplySpliceReserves checks a spliced channel's reserves: unchanged at
// the lock, as the peer's running channel process keeps what it started
// with; then, applied on both sides as their processes start again, 1% of
// the new capacity for each, the same on both sides. A channel never spliced
// keeps those of its opening.
func TestApplySpliceReserves(t *testing.T) {
	t.Parallel()

	const added = btcutil.Amount(100_000_000)
	alice, bob, txid := splicedPair(t, added, 0)
	before := alice.channelState.LocalChanCfg.ChanReserve

	alice.ApplySpliceReserves()
	require.Equal(t, before, alice.channelState.LocalChanCfg.ChanReserve)

	scid := lnwire.ShortChannelID{BlockHeight: 900_100, TxIndex: 4}
	require.NoError(t, alice.LockPendingFunding(txid, scid))
	require.NoError(t, bob.LockPendingFunding(txid, scid))
	require.Equal(t, before, alice.channelState.LocalChanCfg.ChanReserve)

	alice.ApplySpliceReserves()
	bob.ApplySpliceReserves()
	want := alice.channelState.Capacity / 100
	for _, lc := range []*LightningChannel{alice, bob} {
		require.Equal(t, want, lc.channelState.LocalChanCfg.ChanReserve)
		require.Equal(t, want, lc.channelState.RemoteChanCfg.ChanReserve)
	}
}
