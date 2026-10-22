package contractcourt

import (
	"crypto/rand"
	"io"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	lnmock "github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

// randomSecretProducer is a shachain.Producer that behaves like a peer
// generating its per-commitment secrets independently. It returns the base
// producer's secrets for the first two indexes, whose points were exchanged
// when the test channel was created, and random secrets after that.
type randomSecretProducer struct {
	base    shachain.Producer
	secrets map[uint64]chainhash.Hash
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

// Encode writes the base producer.
func (p *randomSecretProducer) Encode(w io.Writer) error {
	return p.base.Encode(w)
}

// TestChainWatcherBreachIndependentSecrets checks that the chain watcher
// detects and builds a retribution for the broadcast of a revoked commitment
// on a channel with option_independent_secrets, whose remote secrets have no
// shachain relationship. The watcher's copy of the channel is loaded from the
// database before the channel is updated, as the chain arbitrator's is, so its
// in-memory revocation store is stale and the secret has to come from the
// revocation log.
func TestChainWatcherBreachIndependentSecrets(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit |
		channeldb.IndependentSecretsBit

	aliceChannel, bobChannel, err := lnwallet.CreateTestChannels(
		t, chanType,
	)
	require.NoError(t, err)

	producer := &randomSecretProducer{
		base:    bobChannel.State().RevocationProducer,
		secrets: make(map[uint64]chainhash.Hash),
	}
	bobChannel.State().RevocationProducer = producer

	watcherState, err := aliceChannel.State().Db.FetchChannel(
		aliceChannel.State().FundingOutpoint,
	)
	require.NoError(t, err)

	// Advance the channel a few states, keeping each of Bob's commitments
	// before it's revoked.
	var revoked []*wire.MsgTx
	for i := 0; i < 5; i++ {
		revoked = append(
			revoked, bobChannel.State().LocalCommitment.CommitTx,
		)
		addFakeHTLC(t, 10_000, uint64(i), aliceChannel, bobChannel)
		require.NoError(t, lnwallet.ForceStateTransition(
			aliceChannel, bobChannel,
		))
	}

	confRegistered := make(chan struct{}, 1)
	aliceNotifier := &lnmock.ChainNotifier{
		SpendChan:      make(chan *chainntnfs.SpendDetail, 1),
		EpochChan:      make(chan *chainntnfs.BlockEpoch),
		ConfChan:       make(chan *chainntnfs.TxConfirmation, 1),
		ConfRegistered: confRegistered,
	}
	retributions := make(chan *lnwallet.BreachRetribution, 1)
	aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
		chanState:           watcherState,
		notifier:            aliceNotifier,
		signer:              aliceChannel.Signer,
		extractStateNumHint: lnwallet.GetStateNumHint,
		chanCloseConfs:      fn.Some(uint32(1)),
		contractBreach: func(r *lnwallet.BreachRetribution) error {
			retributions <- r
			return nil
		},
		auxLeafStore: fn.Some[lnwallet.AuxLeafStore](
			&lnwallet.MockAuxLeafStore{},
		),
		auxResolver: fn.Some[lnwallet.AuxContractResolver](
			&lnwallet.MockAuxContractResolver{},
		),
	})
	require.NoError(t, err)
	require.NoError(t, aliceChainWatcher.Start())
	defer aliceChainWatcher.Stop()

	mockBeat := &chainio.MockBlockbeat{}
	mockBeat.On("logger").Return(log)
	mockBeat.On("Height").Return(int32(1)).Maybe()
	mockBeat.On("NotifyBlockProcessed",
		nil, aliceChainWatcher.quit).Return().Maybe()

	chanEvents := aliceChainWatcher.SubscribeChannelEvents()

	// Bob broadcasts his revoked commitment for state 2, whose secret was
	// random.
	const breachedState = 2
	breachTx := revoked[breachedState]
	breachTxHash := breachTx.TxHash()
	select {
	case aliceNotifier.SpendChan <- &chainntnfs.SpendDetail{
		SpenderTxHash:  &breachTxHash,
		SpendingTx:     breachTx,
		SpendingHeight: 100,
	}:
	case <-time.After(time.Second):
		t.Fatalf("unable to send spend details")
	}

	select {
	case aliceChainWatcher.BlockbeatChan <- mockBeat:
	case <-time.After(time.Second):
		t.Fatalf("unable to send blockbeat")
	}

	var retribution *lnwallet.BreachRetribution
	select {
	case retribution = <-retributions:
	case <-time.After(15 * time.Second):
		t.Fatalf("breach not detected")
	}
	require.EqualValues(t, breachedState, retribution.RevokedStateNum)

	// The retribution uses the secret Bob revealed for that state.
	secret, err := producer.AtIndex(breachedState)
	require.NoError(t, err)
	point := input.ComputeCommitmentPoint(secret[:])
	require.True(t, point.IsEqual(retribution.KeyRing.CommitPoint))

	select {
	case <-chanEvents.ContractBreach:
	case <-time.After(15 * time.Second):
		t.Fatalf("didn't receive contract breach event")
	}
}
