package chanacceptor

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAcceptorCommitmentTypeIgnoresUnifiedSigs checks that a channel carrying
// option_unified_sigs is reported by its commitment type, as the same channel
// without it is.
func TestAcceptorCommitmentTypeIgnoresUnifiedSigs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		bits []lnwire.FeatureBit
		want lnrpc.CommitmentType
	}{
		{
			bits: []lnwire.FeatureBit{
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			},
			want: lnrpc.CommitmentType_ANCHORS,
		},
		{
			bits: []lnwire.FeatureBit{
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.ZeroConfRequired,
				lnwire.ScidAliasRequired,
			},
			want: lnrpc.CommitmentType_ANCHORS,
		},
		{
			bits: []lnwire.FeatureBit{
				lnwire.StaticRemoteKeyRequired,
			},
			want: lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		},
	}

	for _, c := range cases {
		plain := lnwire.NewRawFeatureVector(c.bits...)
		require.Equal(t, c.want, acceptorCommitmentType(plain))

		unified := lnwire.NewRawFeatureVector(
			append(c.bits, lnwire.UnifiedSigsRequired)...,
		)
		require.Equal(t, c.want, acceptorCommitmentType(unified),
			"%v", c.bits)

		// The caller's vector is not changed.
		require.True(t, unified.IsSet(lnwire.UnifiedSigsRequired))
	}

	// Something that is no known type is still reported as unknown.
	require.Equal(t, lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
		acceptorCommitmentType(lnwire.NewRawFeatureVector(
			lnwire.AnchorsZeroFeeHtlcTxRequired,
		)))
}
