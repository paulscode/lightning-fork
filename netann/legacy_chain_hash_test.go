package netann

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/legacychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// sign returns a wire signature by key over the double hash of data.
func sign(t *testing.T, key *btcec.PrivateKey, data []byte) lnwire.Sig {
	t.Helper()

	sig, err := lnwire.NewSigFromSignature(
		ecdsa.Sign(key, chainhash.DoubleHashB(data)),
	)
	require.NoError(t, err)

	return sig
}

func newKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return key
}

// signedAnn1 returns a channel announcement whose four signatures were made
// with its chain_hash set to signedUnder, and which then carries current.
func signedAnn1(t *testing.T, signedUnder,
	current chainhash.Hash) *lnwire.ChannelAnnouncement1 {

	t.Helper()

	keys := [4]*btcec.PrivateKey{
		newKey(t), newKey(t), newKey(t), newKey(t),
	}

	a := &lnwire.ChannelAnnouncement1{
		ChainHash:      signedUnder,
		ShortChannelID: lnwire.NewShortChanIDFromInt(1 << 40),
		Features:       lnwire.NewRawFeatureVector(),
	}
	copy(a.NodeID1[:], keys[0].PubKey().SerializeCompressed())
	copy(a.NodeID2[:], keys[1].PubKey().SerializeCompressed())
	copy(a.BitcoinKey1[:], keys[2].PubKey().SerializeCompressed())
	copy(a.BitcoinKey2[:], keys[3].PubKey().SerializeCompressed())

	data, err := a.DataToSign()
	require.NoError(t, err)

	a.NodeSig1 = sign(t, keys[0], data)
	a.NodeSig2 = sign(t, keys[1], data)
	a.BitcoinSig1 = sign(t, keys[2], data)
	a.BitcoinSig2 = sign(t, keys[3], data)

	a.ChainHash = current

	return a
}

// signedUpdate1 does the same for a channel update.
func signedUpdate1(t *testing.T, key *btcec.PrivateKey, signedUnder,
	current chainhash.Hash) *lnwire.ChannelUpdate1 {

	t.Helper()

	u := &lnwire.ChannelUpdate1{
		ChainHash:       signedUnder,
		ShortChannelID:  lnwire.NewShortChanIDFromInt(1 << 40),
		Timestamp:       1,
		MessageFlags:    lnwire.ChanUpdateRequiredMaxHtlc,
		TimeLockDelta:   80,
		HtlcMinimumMsat: 1000,
		HtlcMaximumMsat: 1000000,
	}

	data, err := u.DataToSign()
	require.NoError(t, err)

	u.Signature = sign(t, key, data)
	u.ChainHash = current

	return u
}

// TestLegacyChainHashGossip checks that gossip signed under the chain_hash a
// network used to advertise verifies under the one it advertises now, and
// that nothing wider is accepted.
func TestLegacyChainHashGossip(t *testing.T) {
	t.Parallel()

	current := *chaincfg.MainNetParams.GenesisHash
	legacy, ok := legacychain.For(current)
	require.True(t, ok)

	other := chainhash.Hash{0x01}
	otherNet := *chaincfg.RegressionNetParams.GenesisHash
	otherLegacy, ok := legacychain.For(otherNet)
	require.True(t, ok)

	t.Run("announcement", func(t *testing.T) {
		t.Parallel()

		// Signed and carried under the current hash: the ordinary case.
		require.NoError(t, validateChannelAnn1(
			signedAnn1(t, current, current),
		))

		// Signed under the withdrawn hash, carried under the current.
		require.NoError(t, validateChannelAnn1(
			signedAnn1(t, legacy, current),
		))

		// Signed under anything else.
		require.Error(t, validateChannelAnn1(
			signedAnn1(t, other, current),
		))

		// Signed under another network's withdrawn hash.
		require.Error(t, validateChannelAnn1(
			signedAnn1(t, otherLegacy, current),
		))

		// Carried under the withdrawn hash itself: that is the message
		// as signed, so it verifies. Whether it is for this chain is
		// the gossiper's chain_hash check, not a signature question.
		require.NoError(t, validateChannelAnn1(
			signedAnn1(t, legacy, legacy),
		))

		// A signature over the withdrawn hash does not stretch to a
		// message that differs anywhere else.
		a := signedAnn1(t, legacy, current)
		a.ShortChannelID = lnwire.NewShortChanIDFromInt(2 << 40)
		require.Error(t, validateChannelAnn1(a))
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()

		key := newKey(t)
		pub := key.PubKey()

		require.NoError(t, verifyChannelUpdate1Signature(
			signedUpdate1(t, key, current, current), pub,
		))
		require.NoError(t, verifyChannelUpdate1Signature(
			signedUpdate1(t, key, legacy, current), pub,
		))
		require.Error(t, verifyChannelUpdate1Signature(
			signedUpdate1(t, key, other, current), pub,
		))
		require.Error(t, verifyChannelUpdate1Signature(
			signedUpdate1(t, key, otherLegacy, current), pub,
		))

		// Someone else's key, under either hash.
		require.Error(t, verifyChannelUpdate1Signature(
			signedUpdate1(t, key, legacy, current),
			newKey(t).PubKey(),
		))

		u := signedUpdate1(t, key, legacy, current)
		u.BaseFee = 7777
		require.Error(t, verifyChannelUpdate1Signature(u, pub))
	})
}

// TestSignedUnderLegacyChainHash checks the question the gossiper asks before
// passing an announcement on: do its signatures hold only under the withdrawn
// chain_hash? And that the strict check refuses exactly those.
func TestSignedUnderLegacyChainHash(t *testing.T) {
	t.Parallel()

	current := *chaincfg.MainNetParams.GenesisHash
	legacy, ok := legacychain.For(current)
	require.True(t, ok)

	// Signed under the current hash: an ordinary announcement.
	fresh := signedAnn1(t, current, current)
	require.False(t, SignedUnderLegacyChainHash(fresh))
	require.NoError(t, ValidateChannelAnnStrict(fresh))

	// Signed under the withdrawn hash, carried under the current one, as
	// this fork relays those made by releases up to .9.
	old := signedAnn1(t, legacy, current)
	require.True(t, SignedUnderLegacyChainHash(old))
	require.Error(t, ValidateChannelAnnStrict(old))
	require.NoError(t, validateChannelAnn1(old),
		"still accepted into this node's own graph")

	// Signed under an unrelated value: neither, and invalid.
	bogus := signedAnn1(t, chainhash.Hash{0x01}, current)
	require.False(t, SignedUnderLegacyChainHash(bogus))
	require.Error(t, ValidateChannelAnnStrict(bogus))

	// A network with no withdrawn value has nothing to fall back to.
	none := signedAnn1(t, chainhash.Hash{0x02}, chainhash.Hash{0x03})
	require.False(t, SignedUnderLegacyChainHash(none))

	// One tampered signature: not legacy either, whatever hash it names.
	tampered := signedAnn1(t, legacy, current)
	tampered.NodeSig2 = fresh.NodeSig2
	require.False(t, SignedUnderLegacyChainHash(tampered))
}
