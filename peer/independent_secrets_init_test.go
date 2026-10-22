package peer

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestInitRefusesIndependentSecretsEvenBit checks that a peer setting the
// even bit of option_independent_secrets in init, where it isn't valid, is
// refused whether or not this node offers the add-on, and that the optional
// bit is accepted.
func TestInitRefusesIndependentSecretsEvenBit(t *testing.T) {
	t.Parallel()

	init := func(bit lnwire.FeatureBit) *lnwire.Init {
		return &lnwire.Init{
			Networks:       []chainhash.Hash{{}},
			GlobalFeatures: lnwire.NewRawFeatureVector(),
			Features: lnwire.NewRawFeatureVector(
				lnwire.DataLossProtectRequired,
				lnwire.ExplicitChannelTypeOptional, bit,
			),
		}
	}

	for _, offered := range []bool{false, true} {
		local := lnwire.NewRawFeatureVector(
			lnwire.DataLossProtectRequired,
			lnwire.ExplicitChannelTypeOptional,
		)
		if offered {
			local.Set(lnwire.IndependentSecretsOptional)
		}

		p := createTestPeer(t).peer
		p.cfg.Features = lnwire.NewFeatureVector(local, lnwire.Features)
		require.ErrorContains(t,
			p.handleInitMsg(init(lnwire.IndependentSecretsRequired)),
			"even bit", "offered=%v", offered)

		p = createTestPeer(t).peer
		p.cfg.Features = lnwire.NewFeatureVector(local, lnwire.Features)
		require.NoError(t,
			p.handleInitMsg(init(lnwire.IndependentSecretsOptional)),
			"offered=%v", offered)
	}
}
