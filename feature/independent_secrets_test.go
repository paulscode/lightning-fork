package feature

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestNoIndependentSecrets checks that option_independent_secrets is
// advertised in init and node_announcement by default, as optional so peers
// without it still open ordinary channels, and not at all when the operator
// turns it off.
func TestNoIndependentSecrets(t *testing.T) {
	t.Parallel()

	for _, off := range []bool{false, true} {
		m, err := NewManager(Config{NoIndependentSecrets: off})
		require.NoError(t, err)

		for _, set := range []Set{SetInit, SetNodeAnn} {
			fv := m.Get(set)
			require.Equal(t, !off,
				fv.IsSet(lnwire.IndependentSecretsOptional),
				"set %v, NoIndependentSecrets=%v", set, off)
			require.False(t,
				fv.IsSet(lnwire.IndependentSecretsRequired))
			require.NoError(t, ValidateDeps(fv), "set %v", set)
		}
	}
}

// TestIndependentSecretsDependsOnChannelType pins the dependency on explicit
// channel type negotiation, the only way the add-on is ever agreed.
func TestIndependentSecretsDependsOnChannelType(t *testing.T) {
	t.Parallel()

	vec := func(bits ...lnwire.FeatureBit) *lnwire.FeatureVector {
		return lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(bits...), lnwire.Features,
		)
	}

	err := ValidateDeps(vec(lnwire.IndependentSecretsOptional))
	require.ErrorContains(t, err, "missing feature dependency")

	require.NoError(t, ValidateDeps(vec(
		lnwire.IndependentSecretsOptional,
		lnwire.ExplicitChannelTypeOptional,
	)))
}
