package feature

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestNoIndependentSecrets checks that the feature manager advertises
// option_independent_secrets in init and node_announcement unless told not
// to (lnd tells it not to unless protocol.independent-secrets is set), and
// only as optional, so peers without it still open ordinary channels.
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

// TestCustomFeaturesRefuseKnownBits checks that custom-init and
// custom-nodeann can't set either bit of option_independent_secrets: the odd
// one would turn the add-on on with its option off, and the even one isn't
// valid outside channel_type. Other bits lnd defines are taken as before,
// unless already set.
func TestCustomFeaturesRefuseKnownBits(t *testing.T) {
	t.Parallel()

	custom := func(set Set, bits ...lnwire.FeatureBit) error {
		_, err := NewManager(Config{
			NoIndependentSecrets: true,
			CustomFeatures: map[Set][]lnwire.FeatureBit{
				set: bits,
			},
		})

		return err
	}

	for _, set := range []Set{SetInit, SetNodeAnn} {
		for _, bit := range []lnwire.FeatureBit{
			lnwire.IndependentSecretsOptional,
			lnwire.IndependentSecretsRequired,
		} {
			require.ErrorIs(t, custom(set, bit),
				lnwire.ErrFeatureStandard,
				"set %v, bit %v", set, bit)
		}

		// Bits lnd defines but leaves unset, which earlier releases
		// took, and an unknown bit.
		for _, bit := range []lnwire.FeatureBit{
			lnwire.InitialRoutingSync,
			lnwire.AnchorsOptional,
			lnwire.PaymentMetadataOptional,
			lnwire.Bolt11BlindedPathsOptional,
			lnwire.FeatureBit(333),
		} {
			require.NoError(t, custom(set, bit),
				"set %v, bit %v", set, bit)
		}

		// One already set is refused, as before.
		err := custom(set, lnwire.DataLossProtectRequired)
		require.ErrorContains(t, err, "already set")
		require.NotErrorIs(t, err, lnwire.ErrFeatureStandard)
	}
}

// TestValidateRoutableRefusesIndependentSecretsEvenBit checks that a node
// announcing the even bit of option_independent_secrets, which is only valid in
// channel_type, is refused for routing as if the bit were unknown, while one
// announcing the optional bit is not.
func TestValidateRoutableRefusesIndependentSecretsEvenBit(t *testing.T) {
	t.Parallel()

	announced := func(bit lnwire.FeatureBit) *lnwire.FeatureVector {
		return lnwire.NewFeatureVector(lnwire.NewRawFeatureVector(
			lnwire.ExplicitChannelTypeOptional, bit,
		), lnwire.Features)
	}

	even := announced(lnwire.IndependentSecretsRequired)
	require.NoError(t, ValidateRequired(even), "lnd knows the bit")
	err := ValidateRoutable(even)
	require.Error(t, err)
	require.Contains(t, err.Error(),
		fmt.Sprintf("%d", lnwire.IndependentSecretsRequired))

	require.NoError(t, ValidateRoutable(
		announced(lnwire.IndependentSecretsOptional),
	))
	require.Error(t, ValidateRoutable(lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(100), lnwire.Features,
	)), "an unknown required bit is still refused")
}
