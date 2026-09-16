package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/channeldb"
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
	}{
		{
			name:     "plain channel is untouched",
			chanType: channeldb.SingleFunderTweaklessBit,
			commit:   txscript.SigHashAll,
			htlc:     txscript.SigHashAll,
		},
		{
			name:     "anchors without unified is untouched",
			chanType: channeldb.SingleFunderTweaklessBit | anchors,
			commit:   txscript.SigHashAll,
			htlc: txscript.SigHashSingle |
				txscript.SigHashAnyOneCanPay,
		},
		{
			name:     "unified without anchors",
			chanType: channeldb.SingleFunderTweaklessBit | unified,
			commit:   0x21,
			htlc:     0x21,
		},
		{
			// What privkeyio's port negotiates.
			name: "anchors with unified",
			chanType: channeldb.SingleFunderTweaklessBit | anchors |
				unified,
			commit: 0x21,
			htlc:   0xa3,
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
		})
	}
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
