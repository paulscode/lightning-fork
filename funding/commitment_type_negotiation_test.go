package funding

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestCommitmentTypeNegotiation tests all of the possible paths of a channel
// commitment type negotiation.
func TestCommitmentTypeNegotiation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name              string
		channelFeatures   *lnwire.RawFeatureVector
		localFeatures     *lnwire.RawFeatureVector
		remoteFeatures    *lnwire.RawFeatureVector
		expectsCommitType lnwallet.CommitmentType
		expectsChanType   *lnwire.ChannelType
		zeroConf          bool
		scidAlias         bool
		expectsErr        error
	}{
		{
			name: "explicit missing remote negotiation feature",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			//nolint:ll
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType:   nil,
			expectsErr:        nil,
		},
		{
			name: "explicit missing remote commitment feature",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsErr: errUnsupportedChannelType,
		},
		{
			name: "explicit zero-conf script enforced",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.ScriptEnforcedLeaseRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeScriptEnforcedLease,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ZeroConfRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.ScriptEnforcedLeaseRequired,
				),
			),
			zeroConf:   true,
			expectsErr: nil,
		},
		{
			name: "explicit zero-conf anchors",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ZeroConfRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			zeroConf:   true,
			expectsErr: nil,
		},
		{
			name: "explicit scid-alias script enforced",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.ScriptEnforcedLeaseRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeScriptEnforcedLease,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ScidAliasRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.ScriptEnforcedLeaseRequired,
				),
			),
			scidAlias:  true,
			expectsErr: nil,
		},
		{
			name: "explicit scid-alias anchors",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ScidAliasRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			scidAlias:  true,
			expectsErr: nil,
		},
		{
			name: "explicit anchors",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			expectsErr: nil,
		},
		{
			name: "explicit tweakless",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeTweakless,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.StaticRemoteKeyRequired,
				),
			),
			expectsErr: nil,
		},
		{
			name:            "explicit legacy",
			channelFeatures: lnwire.NewRawFeatureVector(),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeLegacy,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(),
			),
			expectsErr: nil,
		},
		// Both sides signal the explicit chan type bit, so we expect
		// that we return the corresponding chan type feature bits,
		// even though we didn't set a desired channel type.
		{
			name:            "default explicit anchors",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			expectsErr: nil,
		},
		{
			name:            "implicit tweakless",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeTweakless,
			expectsChanType:   nil,
			expectsErr:        nil,
		},
		{
			name:            "implicit legacy",
			channelFeatures: nil,
			localFeatures:   lnwire.NewRawFeatureVector(),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeLegacy,
			expectsChanType:   nil,
			expectsErr:        nil,
		},

		// Test cases for final taproot channels with explicit
		// negotiation.
		{
			name: "explicit simple taproot final only",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsRequiredFinal,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeSimpleTaprootFinal, //nolint:ll
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.SimpleTaprootChannelsRequiredFinal, //nolint:ll
				),
			),
			expectsErr: nil,
		},
		{
			name: "explicit simple taproot final with scid alias",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsRequiredFinal,
				lnwire.ScidAliasRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ScidAliasOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ScidAliasOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeSimpleTaprootFinal, //nolint:ll
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.SimpleTaprootChannelsRequiredFinal, //nolint:ll
					lnwire.ScidAliasRequired,
				),
			),
			scidAlias:  true,
			expectsErr: nil,
		},
		{
			name: "explicit simple taproot final with zero conf",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsRequiredFinal,
				lnwire.ZeroConfRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ZeroConfOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ZeroConfOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeSimpleTaprootFinal, //nolint:ll
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.SimpleTaprootChannelsRequiredFinal, //nolint:ll
					lnwire.ZeroConfRequired,
				),
			),
			zeroConf:   true,
			expectsErr: nil,
		},
		{
			name: "explicit simple taproot final with scid alias " +
				"and zero conf",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsRequiredFinal,
				lnwire.ScidAliasRequired,
				lnwire.ZeroConfRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ScidAliasOptional,
				lnwire.ZeroConfOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ScidAliasOptional,
				lnwire.ZeroConfOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeSimpleTaprootFinal, //nolint:ll
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.SimpleTaprootChannelsRequiredFinal, //nolint:ll
					lnwire.ScidAliasRequired,
					lnwire.ZeroConfRequired,
				),
			),
			scidAlias:  true,
			zeroConf:   true,
			expectsErr: nil,
		},
		{
			name: "explicit simple taproot final missing " +
				"remote support",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsRequiredFinal,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalStaging,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsErr: errUnsupportedChannelType,
		},

		// Test cases for implicit negotiation ignoring taproot feature
		// bits. Taproot channels require an explicit channel type.
		{
			//nolint:ll
			name:            "implicit anchors preferred over taproot",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.SimpleTaprootChannelsOptionalStaging,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.SimpleTaprootChannelsOptionalStaging,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, //nolint:ll
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			expectsErr: nil,
		},
		{
			//nolint:ll
			name:            "implicit ignores staging taproot without anchors",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.SimpleTaprootChannelsOptionalStaging,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalStaging,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeLegacy,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(),
			),
			expectsErr: nil,
		},
		{
			//nolint:ll
			name:            "implicit ignores final taproot without anchors",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsOptionalFinal,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeLegacy,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(),
			),
			expectsErr: nil,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		ok := t.Run(testCase.name, func(t *testing.T) {
			localFeatures := lnwire.NewFeatureVector(
				testCase.localFeatures, lnwire.Features,
			)
			remoteFeatures := lnwire.NewFeatureVector(
				testCase.remoteFeatures, lnwire.Features,
			)

			var channelType *lnwire.ChannelType
			if testCase.channelFeatures != nil {
				channelType = new(lnwire.ChannelType)
				*channelType = lnwire.ChannelType(
					*testCase.channelFeatures,
				)
			}

			lChan, lCommit, err := negotiateCommitmentType(
				channelType, localFeatures, remoteFeatures,
			)

			var (
				localZc    bool
				localScid  bool
				remoteZc   bool
				remoteScid bool
			)

			if lChan != nil {
				localFv := lnwire.RawFeatureVector(*lChan)
				localZc = localFv.IsSet(
					lnwire.ZeroConfRequired,
				)
				localScid = localFv.IsSet(
					lnwire.ScidAliasRequired,
				)
			}

			require.Equal(t, testCase.zeroConf, localZc)
			require.Equal(t, testCase.scidAlias, localScid)
			require.Equal(t, testCase.expectsErr, err)

			rChan, rCommit, err := negotiateCommitmentType(
				channelType, remoteFeatures, localFeatures,
			)

			if rChan != nil {
				remoteFv := lnwire.RawFeatureVector(*rChan)
				remoteZc = remoteFv.IsSet(
					lnwire.ZeroConfRequired,
				)
				remoteScid = remoteFv.IsSet(
					lnwire.ScidAliasRequired,
				)
			}

			require.Equal(t, testCase.zeroConf, remoteZc)
			require.Equal(t, testCase.scidAlias, remoteScid)
			require.Equal(t, testCase.expectsErr, err)

			if testCase.expectsErr != nil {
				return
			}

			require.Equal(
				t, testCase.expectsCommitType, lCommit,
				testCase.name,
			)
			require.Equal(
				t, testCase.expectsCommitType, rCommit,
				testCase.name,
			)

			require.Equal(
				t, testCase.expectsChanType, lChan,
				testCase.name,
			)
			require.Equal(
				t, testCase.expectsChanType, rChan,
				testCase.name,
			)
		})
		if !ok {
			return
		}
	}
}

// TestUnifiedSigsNegotiation covers the unified signature hash as a channel
// type dimension of its own. It is orthogonal to the commitment type: it
// changes the digest both parties sign, not the transactions, so it is taken
// off before the commitment type is decided and the caller keeps it on the
// type it returns.
func TestUnifiedSigsNegotiation(t *testing.T) {
	t.Parallel()

	var (
		anchorsUnified = lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
			lnwire.UnifiedSigsRequired,
		))
		taprootUnified = lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.SimpleTaprootChannelsRequiredFinal,
			lnwire.UnifiedSigsRequired,
		))
		// ExplicitChannelTypeOptional on both sides is what makes the
		// negotiation explicit; without it a requested type is only
		// compared against what implicit negotiation would have
		// picked, and no type is returned.
		bothSupport = lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.ExplicitChannelTypeOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.UnifiedSigsOptional,
				lnwire.SimpleTaprootChannelsOptionalFinal,
			), lnwire.Features,
		)
		noUnified = lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.ExplicitChannelTypeOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			), lnwire.Features,
		)
	)

	t.Run("both sides support it", func(t *testing.T) {
		t.Parallel()

		chanType, commitType, err := negotiateCommitmentType(
			&anchorsUnified, bothSupport, bothSupport,
		)
		require.NoError(t, err)
		require.Equal(
			t, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			commitType,
		)

		// The bit stays on the type that gets echoed back and
		// persisted; stripping it is only for deciding the commitment
		// type.
		features := lnwire.RawFeatureVector(*chanType)
		require.True(t, features.IsSet(lnwire.UnifiedSigsRequired))
	})

	t.Run("peer cannot do it", func(t *testing.T) {
		t.Parallel()

		_, _, err := negotiateCommitmentType(
			&anchorsUnified, bothSupport, noUnified,
		)
		require.ErrorIs(t, err, errUnsupportedChannelType)
	})

	t.Run("refused on taproot", func(t *testing.T) {
		t.Parallel()

		// The taproot commitment signature is a MuSig2 partial
		// signature over a BIP341 digest. Opting that in is a wire
		// change rather than a different hash type, so agreeing to it
		// would mean two sides signing different digests.
		_, _, err := negotiateCommitmentType(
			&taprootUnified, bothSupport, bothSupport,
		)
		require.ErrorIs(t, err, errUnsupportedChannelType)
	})

	t.Run("proposed implicitly when both support it", func(t *testing.T) {
		t.Parallel()

		chanType, _, err := negotiateCommitmentType(
			nil, bothSupport, bothSupport,
		)
		require.NoError(t, err)

		features := lnwire.RawFeatureVector(*chanType)
		require.True(t, features.IsSet(lnwire.UnifiedSigsRequired))
	})

	t.Run("not proposed to a peer without it", func(t *testing.T) {
		t.Parallel()

		// This is the case that matters for interop: an lnd that does
		// not know the bit must be offered an ordinary channel, not a
		// type it will refuse.
		chanType, _, err := negotiateCommitmentType(
			nil, bothSupport, noUnified,
		)
		require.NoError(t, err)

		features := lnwire.RawFeatureVector(*chanType)
		require.False(t, features.IsSet(lnwire.UnifiedSigsRequired))
	})

	// The rest of these are about a type the caller named. Asking for
	// "anchors" is asking for the shape of the transactions; which chain
	// their signatures are bound to is not part of that choice, and was
	// silently being dropped.
	anchorsPlain := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
	))

	t.Run("a named type still gets the bit", func(t *testing.T) {
		t.Parallel()

		// Before this was fixed, `--channel_type anchors` between two
		// of these nodes opened a channel that closed with `01 01` in
		// the witness, while the default for that same commitment type
		// closed with `21 21`. Nothing reported the difference.
		chanType, commitType, err := negotiateCommitmentType(
			&anchorsPlain, bothSupport, bothSupport,
		)
		require.NoError(t, err)
		require.Equal(
			t, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			commitType,
		)

		features := lnwire.RawFeatureVector(*chanType)
		require.True(t, features.IsSet(lnwire.UnifiedSigsRequired),
			"a channel opened by naming its commitment type is "+
				"not bound to this chain, while the same type "+
				"chosen by default is")
	})

	t.Run("a named type, peer cannot do it", func(t *testing.T) {
		t.Parallel()

		// Adding the bit must not turn a channel that would have
		// opened into an error. A peer that cannot do it gets the
		// plain type.
		chanType, commitType, err := negotiateCommitmentType(
			&anchorsPlain, bothSupport, noUnified,
		)
		require.NoError(t, err)
		require.Equal(
			t, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			commitType,
		)

		features := lnwire.RawFeatureVector(*chanType)
		require.False(t, features.IsSet(lnwire.UnifiedSigsRequired))
	})

	t.Run("a named taproot type does not get it", func(t *testing.T) {
		t.Parallel()

		taprootPlain := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.SimpleTaprootChannelsRequiredFinal,
		))

		// Adding it here would refuse the channel outright, since
		// taproot plus the bit is an error. Asking for a taproot
		// channel must still give one.
		chanType, commitType, err := negotiateCommitmentType(
			&taprootPlain, bothSupport, bothSupport,
		)
		require.NoError(t, err)
		require.Equal(
			t, lnwallet.CommitmentTypeSimpleTaprootFinal,
			commitType,
		)

		features := lnwire.RawFeatureVector(*chanType)
		require.False(t, features.IsSet(lnwire.UnifiedSigsRequired))
	})

	t.Run("the tweakless fall-back carries it too", func(t *testing.T) {
		t.Parallel()

		// A channel reached by falling back needs binding to this
		// chain exactly as much as one reached directly. Only the
		// anchors branch used to add the bit.
		noAnchors := lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.ExplicitChannelTypeOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.UnifiedSigsOptional,
			), lnwire.Features,
		)

		chanType, commitType, err := negotiateCommitmentType(
			nil, noAnchors, noAnchors,
		)
		require.NoError(t, err)
		require.Equal(t, lnwallet.CommitmentTypeTweakless, commitType)

		features := lnwire.RawFeatureVector(*chanType)
		require.True(t, features.IsSet(lnwire.UnifiedSigsRequired))
	})
}
