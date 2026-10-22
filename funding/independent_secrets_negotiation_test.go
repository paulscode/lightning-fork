package funding

import (
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

// TestIndependentSecretsNegotiation covers option_independent_secrets as a
// channel type add-on. Like option_unified_sigs it isn't a commitment shape,
// so it's proposed whenever both peers signal it, taken off before the
// commitment type is decided, and kept on the type that's persisted.
func TestIndependentSecretsNegotiation(t *testing.T) {
	t.Parallel()

	features := func(bits ...lnwire.FeatureBit) *lnwire.FeatureVector {
		base := []lnwire.FeatureBit{
			lnwire.ExplicitChannelTypeOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.SimpleTaprootChannelsOptionalFinal,
		}

		return lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(append(base, bits...)...),
			lnwire.Features,
		)
	}

	var (
		both = features(
			lnwire.IndependentSecretsOptional,
			lnwire.UnifiedSigsOptional,
		)
		noIndependent = features(lnwire.UnifiedSigsOptional)

		anchors = lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
		))
		anchorsIndependent = lnwire.ChannelType(
			*lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.IndependentSecretsRequired,
			),
		)
		taprootIndependent = lnwire.ChannelType(
			*lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsRequiredFinal,
				lnwire.IndependentSecretsRequired,
			),
		)
	)

	isSet := func(chanType *lnwire.ChannelType) bool {
		raw := lnwire.RawFeatureVector(*chanType)
		return raw.IsSet(lnwire.IndependentSecretsRequired)
	}

	t.Run("proposed implicitly when both support it", func(t *testing.T) {
		t.Parallel()

		chanType, commitType, err := negotiateCommitmentType(
			nil, both, both,
		)
		require.NoError(t, err)
		require.Equal(
			t, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			commitType,
		)
		require.True(t, isSet(chanType))

		// It rides alongside option_unified_sigs.
		raw := lnwire.RawFeatureVector(*chanType)
		require.True(t, raw.IsSet(lnwire.UnifiedSigsRequired))
	})

	t.Run("not proposed to a peer without it", func(t *testing.T) {
		t.Parallel()

		chanType, _, err := negotiateCommitmentType(
			nil, both, noIndependent,
		)
		require.NoError(t, err)
		require.False(t, isSet(chanType))
	})

	t.Run("accepted explicitly when both support it", func(t *testing.T) {
		t.Parallel()

		chanType, commitType, err := negotiateCommitmentType(
			&anchorsIndependent, both, both,
		)
		require.NoError(t, err)
		require.Equal(
			t, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			commitType,
		)
		require.True(t, isSet(chanType))
	})

	t.Run("refused explicitly by a peer without it", func(t *testing.T) {
		t.Parallel()

		_, _, err := negotiateCommitmentType(
			&anchorsIndependent, both, noIndependent,
		)
		require.ErrorIs(t, err, errUnsupportedChannelType)
	})

	t.Run("allowed on taproot", func(t *testing.T) {
		t.Parallel()

		// Unlike option_unified_sigs, nothing about signing changes,
		// so taproot channels can carry it.
		chanType, commitType, err := negotiateCommitmentType(
			&taprootIndependent, both, both,
		)
		require.NoError(t, err)
		require.Equal(
			t, lnwallet.CommitmentTypeSimpleTaprootFinal,
			commitType,
		)
		require.True(t, isSet(chanType))
	})

	t.Run("added to a type the funder names", func(t *testing.T) {
		t.Parallel()

		chanType := funderChannelType(&anchors, both, both)
		require.True(t, isSet(chanType))

		chanType = funderChannelType(&anchors, both, noIndependent)
		require.False(t, isSet(chanType))
	})
}

// TestIndependentSecretsFundingFlow runs the funding flow between two funding
// managers and checks that, when option_independent_secrets is negotiated,
// both sides persist the channel type bit and keep the remote party's secrets
// in an IndependentStore, and that neither does otherwise.
func TestIndependentSecretsFundingFlow(t *testing.T) {
	t.Parallel()

	base := []lnwire.FeatureBit{
		lnwire.ExplicitChannelTypeOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.SimpleTaprootChannelsOptionalStaging,
	}
	with := append(
		append([]lnwire.FeatureBit{}, base...),
		lnwire.IndependentSecretsOptional,
	)
	taproot := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	))

	testCases := []struct {
		name        string
		aliceBits   []lnwire.FeatureBit
		bobBits     []lnwire.FeatureBit
		chanType    *lnwire.ChannelType
		independent bool

		// aliceSender turns on the dev option that makes Alice
		// generate her own secrets without a shachain.
		aliceSender bool
	}{
		{
			name:        "alice sends independent secrets",
			aliceBits:   with,
			bobBits:     with,
			independent: true,
			aliceSender: true,
		},
		{
			// Taproot nonces derive from the same root, so they
			// survive the change of producer.
			name:        "alice sends independent secrets on taproot",
			aliceBits:   with,
			bobBits:     with,
			chanType:    &taproot,
			independent: true,
			aliceSender: true,
		},
		{
			// The dev option only applies where the peer stores
			// every secret.
			name:        "sender option without the add-on",
			aliceBits:   with,
			bobBits:     base,
			independent: false,
			aliceSender: true,
		},
		{
			name:        "both signal it",
			aliceBits:   with,
			bobBits:     with,
			independent: true,
		},
		{
			name:        "bob doesn't signal it",
			aliceBits:   with,
			bobBits:     base,
			independent: false,
		},
		{
			name:        "explicit taproot channel",
			aliceBits:   with,
			bobBits:     with,
			chanType:    &taproot,
			independent: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			alice, bob := setupFundingManagers(t)
			t.Cleanup(func() {
				tearDownFundingManagers(t, alice, bob)
			})

			alice.localFeatures = tc.aliceBits
			alice.remoteFeatures = tc.bobBits
			bob.localFeatures = tc.bobBits
			bob.remoteFeatures = tc.aliceBits

			aliceCfg := &alice.fundingMgr.cfg.Wallet.Cfg
			aliceCfg.IndependentSecretsSender = tc.aliceSender

			updateChan := make(chan *lnrpc.OpenStatusUpdate)
			fundChannel(
				t, alice, bob, btcutil.Amount(500000), 0,
				false, 0, 0, 1, updateChan, false,
				tc.chanType,
			)

			for _, node := range []*testNode{alice, bob} {
				db := node.fundingMgr.cfg.Wallet.Cfg.Database
				pending, err := db.FetchPendingChannels()
				require.NoError(t, err)
				require.Len(t, pending, 1)

				channel := pending[0]
				store := channel.RevocationStore
				_, isIndependent :=
					store.(*shachain.IndependentStore)
				chanType := channel.ChanType
				require.Equal(
					t, tc.independent,
					chanType.HasIndependentSecrets(),
				)
				require.Equal(t, tc.independent, isIndependent)

				// Only Alice, and only on an independent
				// channel, generates her secrets without a
				// shachain.
				sender := node == alice && tc.aliceSender &&
					tc.independent
				producer := channel.RevocationProducer
				_, isProducer :=
					producer.(*shachain.IndependentProducer)
				require.Equal(t, sender, isProducer)
				require.Equal(t, sender,
					chanType.HasLocalIndependentSecrets())
			}
		})
	}
}

// TestIndependentSecretsUnproposedEchoRefused checks that a funder that did
// not propose option_independent_secrets refuses an accept_channel whose
// channel_type adds it: the funder would keep requiring shachain secrets while
// the peer believed it could generate them freely.
func TestIndependentSecretsUnproposedEchoRefused(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() {
		tearDownFundingManagers(t, alice, bob)
	})

	// Alice signals the add-on but Bob doesn't, so Alice proposes a type
	// without it.
	base := []lnwire.FeatureBit{
		lnwire.ExplicitChannelTypeOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
	}
	with := append(
		append([]lnwire.FeatureBit{}, base...),
		lnwire.IndependentSecretsOptional,
	)
	alice.localFeatures, alice.remoteFeatures = with, base
	bob.localFeatures, bob.remoteFeatures = base, with

	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error, 1)
	alice.fundingMgr.InitFundingWorkflow(&InitFundingMsg{
		Peer:            bob,
		TargetPubkey:    bob.privKey.PubKey(),
		ChainHash:       *fundingNetParams.GenesisHash,
		LocalFundingAmt: 500000,
		Updates:         updateChan,
		Err:             errChan,
	})

	var openChannel *lnwire.OpenChannel
	select {
	case msg := <-alice.msgChan:
		var ok bool
		openChannel, ok = msg.(*lnwire.OpenChannel)
		require.True(t, ok, "expected OpenChannel, got %T", msg)
	case err := <-errChan:
		t.Fatalf("error init funding workflow: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("alice did not send OpenChannel")
	}
	require.NotNil(t, openChannel.ChannelType)
	proposed := lnwire.RawFeatureVector(*openChannel.ChannelType)
	require.False(t, proposed.IsSet(lnwire.IndependentSecretsRequired))

	bob.fundingMgr.ProcessFundingMsg(openChannel, alice)
	accept := assertFundingMsgSent(
		t, bob.msgChan, "AcceptChannel",
	).(*lnwire.AcceptChannel)

	// Bob's reply now adds the add-on to the type Alice proposed.
	withBit := proposed.Clone()
	withBit.Set(lnwire.IndependentSecretsRequired)
	chanType := lnwire.ChannelType(*withBit)
	accept.ChannelType = &chanType

	alice.fundingMgr.ProcessFundingMsg(accept, bob)

	// Alice fails the flow: Bob gets an error, and the caller gets the
	// reason.
	assertFundingMsgSent(t, alice.msgChan, "Error")
	select {
	case err := <-errChan:
		require.True(t, strings.Contains(
			err.Error(), "channel type mismatch",
		), err.Error())
	case <-time.After(5 * time.Second):
		t.Fatalf("funding flow didn't fail")
	}
}
