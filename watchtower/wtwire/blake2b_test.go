package wtwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestInitSetsBlake2b checks that every Init this build makes says it follows
// the BLAKE2b rules, without touching the caller's vector.
func TestInitSetsBlake2b(t *testing.T) {
	t.Parallel()

	genesis := *chaincfg.MainNetParams.GenesisHash

	caller := lnwire.NewRawFeatureVector(AltruistSessionsRequired)
	init := NewInitMessage(caller, genesis)

	require.True(t, init.ConnFeatures.IsSet(Blake2bRequired))
	require.True(t, init.ConnFeatures.IsSet(AltruistSessionsRequired))
	require.False(t, caller.IsSet(Blake2bRequired),
		"the caller's vector is left as it was")

	require.True(t, NewInitMessage(nil, genesis).ConnFeatures.IsSet(
		Blake2bRequired,
	))

	// And it survives the wire.
	var b bytes.Buffer
	require.NoError(t, init.Encode(&b, 0))
	var decoded Init
	require.NoError(t, decoded.Decode(&b, 0))
	require.True(t, decoded.ConnFeatures.IsSet(Blake2bRequired))
	require.Equal(t, genesis, decoded.ChainHash)
}

// TestCheckRemoteInitBlake2b checks the tower handshake in both directions: a
// peer of this build must set either form of the bit, and an implementation
// without these rules refuses ours at the unknown even bit. Both chains share
// the genesis hash, so the chain hash check alone would pass.
func TestCheckRemoteInitBlake2b(t *testing.T) {
	t.Parallel()

	genesis := *chaincfg.MainNetParams.GenesisHash
	ours := NewInitMessage(
		lnwire.NewRawFeatureVector(AltruistSessionsRequired), genesis,
	)

	remote := func(bits ...lnwire.FeatureBit) *Init {
		return &Init{
			ConnFeatures: lnwire.NewRawFeatureVector(bits...),
			ChainHash:    genesis,
		}
	}

	// Another tower or client of this build.
	require.NoError(t, ours.CheckRemoteInit(
		NewInitMessage(nil, genesis), FeatureNames,
	))

	// The odd form is enough.
	require.NoError(t, ours.CheckRemoteInit(
		remote(AltruistSessionsRequired, Blake2bOptional), FeatureNames,
	))

	// A stock tower: same genesis hash, no bit.
	require.ErrorIs(t, ours.CheckRemoteInit(
		remote(AltruistSessionsRequired), FeatureNames,
	), ErrNotBlake2b)

	// A stock implementation, which knows neither the bit nor its name,
	// refuses ours before anything else.
	stockNames := make(map[lnwire.FeatureBit]string)
	for bit, name := range FeatureNames {
		if bit != Blake2bRequired && bit != Blake2bOptional {
			stockNames[bit] = name
		}
	}
	stock := &Init{
		ConnFeatures: lnwire.NewRawFeatureVector(
			AltruistSessionsRequired,
		),
		ChainHash: genesis,
	}
	err := stock.CheckRemoteInit(ours, stockNames)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotBlake2b)

	// The chain hash is still checked first.
	other := NewInitMessage(nil, *chaincfg.TestNet3Params.GenesisHash)
	var unknown *ErrUnknownChainHash
	require.ErrorAs(t, ours.CheckRemoteInit(other, FeatureNames), &unknown)
}
