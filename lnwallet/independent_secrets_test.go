package lnwallet

import (
	"crypto/rand"
	"io"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

// randomSecretProducer is a shachain.Producer for tests that behaves like a
// peer generating its per-commitment secrets independently, for example
// jointly across k-of-n signers. It returns the secrets of an existing
// producer for the first two indexes, whose points were already exchanged when
// the test channel was created, and fresh random secrets after that.
type randomSecretProducer struct {
	base    shachain.Producer
	secrets map[uint64]chainhash.Hash
}

func newRandomSecretProducer(base shachain.Producer) *randomSecretProducer {
	return &randomSecretProducer{
		base:    base,
		secrets: make(map[uint64]chainhash.Hash),
	}
}

// AtIndex returns the secret at index i.
func (p *randomSecretProducer) AtIndex(i uint64) (*chainhash.Hash, error) {
	if i < 2 {
		return p.base.AtIndex(i)
	}

	if h, ok := p.secrets[i]; ok {
		return &h, nil
	}

	var h chainhash.Hash
	if _, err := rand.Read(h[:]); err != nil {
		return nil, err
	}
	p.secrets[i] = h

	return &h, nil
}

// Encode writes the base producer, so the channel state can be persisted.
func (p *randomSecretProducer) Encode(w io.Writer) error {
	return p.base.Encode(w)
}

var _ shachain.Producer = (*randomSecretProducer)(nil)

// unreducedProducer is like randomSecretProducer, but from index 2 on it
// returns secrets of the form s + n, where n is the order of secp256k1. Such a
// secret isn't a valid private key, yet it gives the same point as s once
// reduced modulo n.
type unreducedProducer struct {
	*randomSecretProducer
}

// AtIndex returns the secret at index i.
func (p *unreducedProducer) AtIndex(i uint64) (*chainhash.Hash, error) {
	if i < 2 {
		return p.base.AtIndex(i)
	}

	if h, ok := p.secrets[i]; ok {
		return &h, nil
	}

	// A 120-bit s keeps s + n below 2^256.
	var small [15]byte
	if _, err := rand.Read(small[:]); err != nil {
		return nil, err
	}
	v := new(big.Int).SetBytes(small[:])
	v.Add(v, btcec.S256().N)

	var h chainhash.Hash
	v.FillBytes(h[:])
	p.secrets[i] = h

	return &h, nil
}

// independentSecretsChanType is the commitment type the tests below use, an
// anchors channel, with or without option_independent_secrets.
func independentSecretsChanType(independent bool) channeldb.ChannelType {
	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit
	if independent {
		chanType |= channeldb.IndependentSecretsBit
	}

	return chanType
}

// sendAndSettle has Alice send Bob an HTLC, locks it in, then has Bob settle
// it and locks the settle in. Each call makes both parties revoke two
// commitments.
func sendAndSettle(t *testing.T, alice, bob *LightningChannel, id int) error {
	t.Helper()

	htlc, preimage := createHTLC(id, lnwire.NewMSatFromSatoshis(
		btcutil.Amount(10_000+id),
	))

	if _, err := alice.AddHTLC(htlc, nil); err != nil {
		return err
	}
	if _, err := bob.ReceiveHTLC(htlc); err != nil {
		return err
	}
	if err := ForceStateTransition(alice, bob); err != nil {
		return err
	}

	err := bob.SettleHTLC(preimage, uint64(id), nil, nil, nil)
	if err != nil {
		return err
	}
	err = alice.ReceiveHTLCSettle(preimage, uint64(id))
	if err != nil {
		return err
	}

	return ForceStateTransition(bob, alice)
}

// TestIndependentSecretsAcceptsUnrelatedSecrets shows that on a channel with
// option_independent_secrets a node accepts per-commitment secrets that aren't
// shachain-derived, persists them, and can still build a breach retribution
// for every revoked state, before and after a restart.
func TestIndependentSecretsAcceptsUnrelatedSecrets(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(
		t, independentSecretsChanType(true),
	)
	require.NoError(t, err)

	// Alice keeps Bob's secrets in an IndependentStore.
	_, ok := alice.channelState.RevocationStore.(*shachain.IndependentStore)
	require.True(t, ok, "expected an IndependentStore")

	// From here on Bob's secrets are random.
	producer := newRandomSecretProducer(bob.channelState.RevocationProducer)
	bob.channelState.RevocationProducer = producer

	const rounds = 6
	for i := 0; i < rounds; i++ {
		require.NoError(t, sendAndSettle(t, alice, bob, i))
	}

	// Bob really did reveal random secrets.
	require.GreaterOrEqual(t, len(producer.secrets), 2*rounds-2)

	// Every revoked commitment of Bob's can be punished, using the secret
	// Bob revealed for it.
	assertRetributions := func(alice *LightningChannel) {
		t.Helper()

		tip := alice.channelState.RemoteCommitment.CommitHeight
		require.Greater(t, tip, uint64(4))

		for h := uint64(0); h < tip; h++ {
			br, err := NewBreachRetribution(
				alice.channelState, h, 101, nil,
				fn.Some[AuxLeafStore](&MockAuxLeafStore{}),
				fn.Some[AuxContractResolver](
					&MockAuxContractResolver{},
				),
			)
			require.NoError(t, err, "state %d", h)
			require.Equal(t, h, br.RevokedStateNum)

			secret, err := producer.AtIndex(h)
			require.NoError(t, err)
			point := input.ComputeCommitmentPoint(secret[:])
			require.True(
				t, point.IsEqual(br.KeyRing.CommitPoint),
				"state %d: wrong commitment point", h,
			)
		}
	}
	assertRetributions(alice)

	// The secrets live in the database, not in memory: after a restart
	// Alice still syncs with Bob and can still punish every old state.
	alice, err = restartChannel(alice)
	require.NoError(t, err)

	_, ok = alice.channelState.RevocationStore.(*shachain.IndependentStore)
	require.True(t, ok, "expected an IndependentStore after restart")

	assertNoChanSyncNeeded(t, alice, bob)
	assertRetributions(alice)

	// And the channel keeps working.
	require.NoError(t, sendAndSettle(t, alice, bob, rounds))
	assertRetributions(alice)
}

// TestShachainChannelRejectsUnrelatedSecrets is the control: on a channel
// without option_independent_secrets, the same peer behaviour is refused by
// the shachain revocation store, which is why the channel type is needed.
func TestShachainChannelRejectsUnrelatedSecrets(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(
		t, independentSecretsChanType(false),
	)
	require.NoError(t, err)

	_, ok := alice.channelState.RevocationStore.(*shachain.RevocationStore)
	require.True(t, ok, "expected a shachain RevocationStore")

	bob.channelState.RevocationProducer = newRandomSecretProducer(
		bob.channelState.RevocationProducer,
	)

	var lastErr error
	for i := 0; i < 6 && lastErr == nil; i++ {
		lastErr = sendAndSettle(t, alice, bob, i)
	}
	require.ErrorContains(t, lastErr, "hash isn't derivable")
}

// TestIndependentSecretsRejectsUnreducedSecret checks that on a channel with
// option_independent_secrets a secret must be a valid private key as sent,
// even if it gives the expected point once reduced modulo n.
func TestIndependentSecretsRejectsUnreducedSecret(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(
		t, independentSecretsChanType(true),
	)
	require.NoError(t, err)

	bob.channelState.RevocationProducer = &unreducedProducer{
		newRandomSecretProducer(bob.channelState.RevocationProducer),
	}

	var lastErr error
	for i := 0; i < 4 && lastErr == nil; i++ {
		lastErr = sendAndSettle(t, alice, bob, i)
	}
	require.ErrorContains(t, lastErr, "not a valid private key")
}

// TestIndependentSecretsChanSyncMidRevocation checks that a node can still
// build a channel_reestablish after its revocation store has taken a new
// secret but before the remote commitment has been advanced, which is the
// window inside ReceiveRevocation. channel_reestablish then names the secret
// one before the newest.
func TestIndependentSecretsChanSyncMidRevocation(t *testing.T) {
	t.Parallel()

	alice, bob, err := CreateTestChannels(
		t, independentSecretsChanType(true),
	)
	require.NoError(t, err)

	producer := newRandomSecretProducer(bob.channelState.RevocationProducer)
	bob.channelState.RevocationProducer = producer

	for i := 0; i < 3; i++ {
		require.NoError(t, sendAndSettle(t, alice, bob, i))
	}

	tip := alice.channelState.RemoteCommitment.CommitHeight
	want, err := producer.AtIndex(tip - 1)
	require.NoError(t, err)

	// The store takes the secret for the current remote commitment, as it
	// would first thing on receiving Bob's next revoke_and_ack.
	next, err := producer.AtIndex(tip)
	require.NoError(t, err)
	store := alice.channelState.RevocationStore
	require.NoError(t, store.AddNextEntry(next))

	msg, err := alice.channelState.ChanSyncMsg()
	require.NoError(t, err)
	require.Equal(t, [32]byte(*want), msg.LastRemoteCommitSecret)

	// If the store runs further ahead than the two secrets it keeps in
	// memory, as a copy of the channel refreshed in two steps can see,
	// the secret comes from the revocation log instead.
	afterNext, err := producer.AtIndex(tip + 1)
	require.NoError(t, err)
	require.NoError(t, store.AddNextEntry(afterNext))

	msg, err = alice.channelState.ChanSyncMsg()
	require.NoError(t, err)
	require.Equal(t, [32]byte(*want), msg.LastRemoteCommitSecret)
}

// TestIndependentSecretsBothSidesIndependentProducer runs a channel on which
// both sides generate their secrets with an IndependentProducer, as dev nodes
// with the sender option do: every secret is accepted and stored, the
// producer is rebuilt from the database after a restart so the same secrets
// keep coming, and every revoked state stays punishable.
func TestIndependentSecretsBothSidesIndependentProducer(t *testing.T) {
	t.Parallel()

	// Taproot channels also derive their musig2 nonces from the stored
	// root, which the IndependentProducer keeps.
	taproot := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.SimpleTaprootFeatureBit |
		channeldb.IndependentSecretsBit

	for name, chanType := range map[string]channeldb.ChannelType{
		"anchors": independentSecretsChanType(true),
		"taproot": taproot,
	} {
		t.Run(name, func(t *testing.T) {
			local := channeldb.LocalIndependentSecretsBit
			testBothSidesIndependentProducer(t, chanType|local)
		})
	}
}

func testBothSidesIndependentProducer(t *testing.T,
	chanType channeldb.ChannelType) {

	alice, bob, err := CreateTestChannels(t, chanType)
	require.NoError(t, err)

	for _, c := range []*LightningChannel{alice, bob} {
		producer := c.channelState.RevocationProducer
		_, ok := producer.(*shachain.IndependentProducer)
		require.True(t, ok, "expected an IndependentProducer")
	}

	for i := 0; i < 3; i++ {
		require.NoError(t, sendAndSettle(t, alice, bob, i))
	}

	// Restart both. Each must rebuild its IndependentProducer from the
	// stored root, or it would reveal secrets for points it never sent.
	alice, err = restartChannel(alice)
	require.NoError(t, err)
	bob, err = restartChannel(bob)
	require.NoError(t, err)
	for _, c := range []*LightningChannel{alice, bob} {
		producer := c.channelState.RevocationProducer
		_, ok := producer.(*shachain.IndependentProducer)
		require.True(t, ok, "expected an IndependentProducer "+
			"after restart")
	}
	assertNoChanSyncNeeded(t, alice, bob)

	for i := 3; i < 6; i++ {
		require.NoError(t, sendAndSettle(t, alice, bob, i))
	}

	// Each side can punish every revoked state of the other.
	pairs := [][2]*LightningChannel{{alice, bob}, {bob, alice}}
	for _, pair := range pairs {
		self, peer := pair[0], pair[1]
		tip := self.channelState.RemoteCommitment.CommitHeight
		require.Greater(t, tip, uint64(4))

		for h := uint64(0); h < tip; h++ {
			br, err := NewBreachRetribution(
				self.channelState, h, 101, nil,
				fn.Some[AuxLeafStore](&MockAuxLeafStore{}),
				fn.Some[AuxContractResolver](
					&MockAuxContractResolver{},
				),
			)
			require.NoError(t, err, "state %d", h)

			secret, err := peer.channelState.RevocationProducer.
				AtIndex(h)
			require.NoError(t, err)
			point := input.ComputeCommitmentPoint(secret[:])
			require.True(t, point.IsEqual(br.KeyRing.CommitPoint),
				"state %d", h)
		}
	}
}
