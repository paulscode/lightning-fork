package peer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/stretchr/testify/require"
)

// turnFeeEstimator makes every offer payable.
type turnFeeEstimator struct{}

func (turnFeeEstimator) EstimateFee(channeldb.ChannelType, *wire.TxOut,
	*wire.TxOut, []*wire.TxOut, chainfee.SatPerKWeight) btcutil.Amount {

	return 1000
}

// turnCloseSigner signs every offer with a throwaway key. It reports each
// offer it is asked to sign, then waits for the gate before it signs.
type turnCloseSigner struct {
	key     *btcec.PrivateKey
	signing chan struct{}
	gate    chan struct{}
}

func (s *turnCloseSigner) CreateCloseProposal(btcutil.Amount, []byte,
	[]byte, ...lnwallet.ChanCloseOpt) (input.Signature, *wire.MsgTx,
	btcutil.Amount, error) {

	s.signing <- struct{}{}
	<-s.gate

	sig := ecdsa.Sign(s.key, chainhash.HashB([]byte("close")))

	return sig, wire.NewMsgTx(2), 9000, nil
}

func (s *turnCloseSigner) CompleteCooperativeClose(input.Signature,
	input.Signature, []byte, []byte, btcutil.Amount,
	...lnwallet.ChanCloseOpt) (*wire.MsgTx, btcutil.Amount, error) {

	return wire.NewMsgTx(2), 9000, nil
}

// turnDaemon accepts the closer's messages and does nothing else.
type turnDaemon struct{}

func (turnDaemon) SendMessages(btcec.PublicKey, []lnwire.Message) error {
	return nil
}

func (turnDaemon) BroadcastTransaction(*wire.MsgTx, string) error {
	return nil
}

func (turnDaemon) RegisterConfirmationsNtfn(*chainhash.Hash, []byte, uint32,
	uint32, ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	return &chainntnfs.ConfirmationEvent{}, nil
}

func (turnDaemon) RegisterSpendNtfn(*wire.OutPoint, []byte,
	uint32) (*chainntnfs.SpendEvent, error) {

	return &chainntnfs.SpendEvent{}, nil
}

// turnErrors records the error that stops the closer.
type turnErrors struct {
	errs chan error
}

func (e *turnErrors) ReportError(err error) {
	e.errs <- err
}

// TestRbfFeeBumpTurnHeldUntilOfferTaken checks that a fee bump keeps its turn
// until the close has taken its offer. The signer holds the close in the
// middle of the offer: the offer is sent, but the state does not show it yet,
// so another bump must not get the turn.
func TestRbfFeeBumpTurnHeldUntilOfferTaken(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	h, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	p := h.peer
	point := h.channel.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(point)

	terms := &chancloser.CloseChannelTerms{
		ShutdownScripts: chancloser.ShutdownScripts{
			LocalDeliveryScript:  genScript(t, p2wshAddress),
			RemoteDeliveryScript: genScript(t, p2wshAddress),
		},
		ShutdownBalances: chancloser.ShutdownBalances{
			LocalBalance:  10_000_000,
			RemoteBalance: 10_000_000,
		},
	}
	signer := &turnCloseSigner{
		key:     key,
		signing: make(chan struct{}, 1),
		gate:    make(chan struct{}),
	}
	reporter := &turnErrors{errs: make(chan error, 1)}
	machine := protofsm.NewStateMachine(chancloser.RbfChanCloserCfg{
		ErrorReporter: reporter,
		Daemon:        turnDaemon{},
		InitialState: &chancloser.ClosingNegotiation{
			PeerState: lntypes.Dual[chancloser.AsymmetricPeerState]{
				Local: &chancloser.LocalCloseStart{
					CloseChannelTerms: terms,
				},
				Remote: &chancloser.RemoteCloseStart{
					CloseChannelTerms: terms,
				},
			},
			CloseChannelTerms: terms,
		},
		Env: &chancloser.Environment{
			ChainParams:  p.cfg.Wallet.Cfg.NetParams,
			ChanPeer:     *key.PubKey(),
			ChanPoint:    point,
			ChanID:       chanID,
			FeeEstimator: turnFeeEstimator{},
			CloseSigner:  signer,
		},
	})
	machine.Start(t.Context())
	t.Cleanup(machine.Stop)
	p.activeChanCloses.Store(chanID, makeRbfCloser(&machine))

	// Let the signer go before the close is stopped, also when the test
	// fails early: a close held in the signer cannot stop.
	var openGate sync.Once
	releaseSigner := func() {
		openGate.Do(func() { close(signer.gate) })
	}
	t.Cleanup(releaseSigner)

	req := newFeeBumpRequest(t.Context(), &point, 5000)
	startFeeBump(t, p, point, req)

	// The close is in the middle of the offer.
	select {
	case <-signer.signing:
	case <-time.After(timeout):
		t.Fatalf("the offer did not reach the signer")
	}

	// The bump still has the turn.
	ctx, cancel := context.WithTimeout(t.Context(), shortTimeout)
	defer cancel()
	_, err = p.rbfFeeBumps.acquire(ctx, chanID)
	require.ErrorIs(
		t, err, context.DeadlineExceeded,
		"turn given up before the close took the offer",
	)

	// Once the close has taken the offer, the turn is free again.
	releaseSigner()
	ctx, cancel = context.WithTimeout(t.Context(), timeout)
	defer cancel()
	release, err := p.rbfFeeBumps.acquire(ctx, chanID)
	require.NoError(t, err)
	release()

	state, err := machine.CurrentState()
	require.NoError(t, err)
	negotiation, ok := state.(*chancloser.ClosingNegotiation)
	require.True(t, ok)
	require.IsType(
		t, &chancloser.LocalOfferSent{},
		negotiation.PeerState.GetForParty(lntypes.Local),
	)
	require.Empty(t, reporter.errs)
	require.Eventually(t, func() bool {
		p.rbfFeeBumps.mu.Lock()
		defer p.rbfFeeBumps.mu.Unlock()

		return len(p.rbfFeeBumps.slots) == 0
	}, timeout, 10*time.Millisecond, "slot kept after the last bump")
}
