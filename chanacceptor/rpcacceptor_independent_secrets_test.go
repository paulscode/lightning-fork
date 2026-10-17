package chanacceptor

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAcceptorCommitmentTypeIgnoresIndependentSecrets checks that a channel
// carrying option_independent_secrets, alone or with option_unified_sigs, is
// reported by its commitment type, as the same channel without it is.
func TestAcceptorCommitmentTypeIgnoresIndependentSecrets(t *testing.T) {
	t.Parallel()

	base := []lnwire.FeatureBit{
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
	}

	for _, extra := range [][]lnwire.FeatureBit{
		{lnwire.IndependentSecretsRequired},
		{lnwire.IndependentSecretsRequired, lnwire.UnifiedSigsRequired},
	} {
		bits := append(append([]lnwire.FeatureBit{}, base...), extra...)
		vec := lnwire.NewRawFeatureVector(bits...)
		require.Equal(t, lnrpc.CommitmentType_ANCHORS,
			acceptorCommitmentType(vec), "%v", bits)

		// The caller's vector is not changed.
		require.True(t, vec.IsSet(lnwire.IndependentSecretsRequired))
	}
}
