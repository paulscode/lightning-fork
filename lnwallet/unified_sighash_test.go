package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestUnifiedSigHashTypes pins the hash types a channel that negotiated the
// unified signature hash signs with. These are wire-visible: both sides
// compute a digest from them, and the byte appears in the witness on chain,
// so a change here silently breaks every channel with a Core Lightning peer.
//
// The rule is that BOLT 3's hash type for a signature gains SIGHASH_UNIFIED
// (0x20) and nothing else. privkeyio's port derives it the same way, in
// channel_type_sighash(), and applies it in full_channel.c: SIGHASH_ALL for
// the commitment, and SIGHASH_SINGLE|SIGHASH_ANYONECANPAY for second-level
// HTLC transactions on an anchor channel.
func TestUnifiedSigHashTypes(t *testing.T) {
	t.Parallel()

	const (
		anchors = channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit
		unified = channeldb.UnifiedSigsBit
	)

	tests := []struct {
		name     string
		chanType channeldb.ChannelType
		commit   txscript.SigHashType
		htlc     txscript.SigHashType
		sweep    txscript.SigHashType
	}{
		// The sweep column is 0x21 on every row, including the two
		// channels that negotiated nothing. That is not an oversight in
		// the table: sweepSigHash is not channel-scoped at all. It asks
		// input.UnifiedSigHash(), a process-wide switch that is on in a
		// release build, because a signature no peer verifies needs no
		// peer's agreement to opt in and opting it in is what keeps it
		// off the chain that did not upgrade. So a legacy channel's
		// HTLC-timeout carries the peer's legacy hash type next to this
		// node's unified one, and both are valid.
		{
			name:     "plain channel is untouched",
			chanType: channeldb.SingleFunderTweaklessBit,
			commit:   txscript.SigHashAll,
			htlc:     txscript.SigHashAll,
			sweep:    0x21,
		},
		{
			name:     "anchors without unified is untouched",
			chanType: channeldb.SingleFunderTweaklessBit | anchors,
			commit:   txscript.SigHashAll,
			htlc: txscript.SigHashSingle |
				txscript.SigHashAnyOneCanPay,
			sweep: 0x21,
		},
		{
			name:     "unified without anchors",
			chanType: channeldb.SingleFunderTweaklessBit | unified,
			commit:   0x21,
			htlc:     0x21,
			sweep:    0x21,
		},
		{
			// What privkeyio's port negotiates.
			name: "anchors with unified",
			chanType: channeldb.SingleFunderTweaklessBit | anchors |
				unified,
			commit: 0x21,
			htlc:   0xa3,
			sweep:  0x21,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.commit,
				CommitSigHashType(test.chanType),
				"commitment and cooperative close hash type",
			)
			require.Equal(
				t, test.htlc, HtlcSigHashType(test.chanType),
				"second-level HTLC hash type",
			)
			require.Equal(
				t, test.sweep, sweepSigHash(test.chanType),
				"the broadcaster's own hash type",
			)
		})
	}
}

// A second-level HTLC transaction is spent by a 2-of-2 and so carries two
// signatures. On an anchor channel they do not share a hash type: the peer
// pre-signs with SIGHASH_SINGLE|SIGHASH_ANYONECANPAY so that the broadcaster
// can attach fees, and the broadcaster signs its own half with SIGHASH_ALL so
// that the transaction it finally publishes cannot be altered around it.
//
// Nothing else pins this. HtlcSigHashType is checked above, but it supplies
// only the byte appended to the *peer's* signature; the broadcaster's comes
// from sweepSigHash. Making the two agree would look like a tidy-up, would
// break no test without this one, and would produce malleable HTLC-timeout
// transactions.
//
// Confirmed on chain rather than reasoned about: an HTLC offered by this node
// over a channel with an unmodified privkeyio build, held until it timed out,
// puts `<> <remotehtlcsig 0xa3> <localhtlcsig 0x21> <> <script>` in the
// witness. See the lab's htlc-sighash scenario.
func TestSecondLevelHtlcCarriesTwoDifferentHashTypes(t *testing.T) {
	t.Parallel()

	// Stated rather than assumed: sweepSigHash reads this, so the value
	// below is only 0x21 while it is on.
	require.True(t, input.UnifiedSigHash(),
		"a release build signs its own half with the opt-in")

	anchored := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit |
		channeldb.UnifiedSigsBit

	peers := HtlcSigHashType(anchored)
	ours := sweepSigHash(anchored)

	require.Equal(t, txscript.SigHashType(0xa3), peers,
		"the signature sent to the peer")
	require.Equal(t, txscript.SigHashType(0x21), ours,
		"the signature this node adds when it broadcasts")
	require.NotEqual(t, peers, ours,
		"these are deliberately different; making them equal makes the "+
			"HTLC-timeout transaction malleable")

	// Both still carry the opt-in, which is what keeps either of them from
	// being replayable on the chain that did not upgrade.
	require.NotZero(t, peers&txscript.SigHashUnified)
	require.NotZero(t, ours&txscript.SigHashUnified)
}

// TestUnifiedSigHashComposition checks the two derived values against the
// constants they are built from, so that a change to either the BOLT 3 base
// or the opt-in bit shows up as a failure here rather than as a channel that
// cannot be opened.
func TestUnifiedSigHashComposition(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, txscript.SigHashType(0x20), txscript.SigHashUnified,
		"the opt-in bit is 0x20",
	)
	require.Equal(
		t, txscript.SigHashType(0x21),
		txscript.SigHashAll|txscript.SigHashUnified,
	)
	require.Equal(
		t, txscript.SigHashType(0xa3),
		txscript.SigHashSingle|txscript.SigHashAnyOneCanPay|
			txscript.SigHashUnified,
	)

	// A channel that did not negotiate it must be bit-for-bit what it was
	// before this feature existed.
	plain := channeldb.SingleFunderTweaklessBit
	require.False(t, plain.HasUnifiedSigs())
	require.Zero(t, CommitSigHashType(plain)&txscript.SigHashUnified)
	require.Zero(t, HtlcSigHashType(plain)&txscript.SigHashUnified)
}
