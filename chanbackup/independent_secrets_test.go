package chanbackup

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/stretchr/testify/require"
)

// TestSingleIgnoresIndependentSecrets checks that a channel with
// option_independent_secrets backs up exactly like the same channel without
// it. A static channel backup only lets the node ask its peer to force close,
// after which it never receives a revocation again, so the add-on has nothing
// to restore and the backup format stays unchanged.
func TestSingleIgnoresIndependentSecrets(t *testing.T) {
	t.Parallel()

	keyRing := &lnencrypt.MockKeyRing{}

	channel, err := genRandomOpenChannelShell()
	require.NoError(t, err)

	base := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit

	pack := func(chanType channeldb.ChannelType) Single {
		channel.ChanType = chanType
		single := NewSingle(channel, nil)

		var b bytes.Buffer
		require.NoError(t, single.PackToWriter(&b, keyRing))

		var unpacked Single
		require.NoError(t, unpacked.UnpackFromReader(
			bytes.NewReader(b.Bytes()), keyRing,
		))

		return unpacked
	}

	for _, extra := range []channeldb.ChannelType{
		0, channeldb.UnifiedSigsBit,
	} {
		plain := pack(base | extra)
		independent := pack(
			base | extra | channeldb.IndependentSecretsBit,
		)

		require.Equal(t, plain.Version, independent.Version)
		require.Equal(t, plain.UnifiedSigs, independent.UnifiedSigs)
		anchors := SingleBackupVersion(
			AnchorsZeroFeeHtlcTxCommitVersion,
		)
		require.Equal(t, anchors, independent.Version)
	}
}
