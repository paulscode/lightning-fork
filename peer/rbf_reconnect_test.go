package peer

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/stretchr/testify/require"
)

// reconnectFeeEstimator records offers while keeping the local balance too
// small to sign one. This tests the requested fee without mocking the FSM.
type reconnectFeeEstimator struct {
	fees chan chainfee.SatPerKWeight
}

func (f *reconnectFeeEstimator) EstimateFee(_ channeldb.ChannelType,
	_, _ *wire.TxOut, _ []*wire.TxOut,
	fee chainfee.SatPerKWeight) btcutil.Amount {

	f.fees <- fee
	return 1000
}

type reconnectObserver struct {
	chancloser.ChanStateObserver
}

func (*reconnectObserver) FinalBalances() fn.Option[chancloser.ShutdownBalances] {
	return fn.None[chancloser.ShutdownBalances]()
}

func (*reconnectObserver) DisableIncomingAdds() error {
	return nil
}

func reconnectState(t *testing.T, flushing bool) chancloser.RbfState {
	t.Helper()
	scripts := chancloser.ShutdownScripts{
		LocalDeliveryScript:  genScript(t, p2wshAddress),
		RemoteDeliveryScript: genScript(t, p2wshAddress),
	}
	if flushing {
		return &chancloser.ChannelFlushing{
			ShutdownScripts: scripts,
			IdealFeeRate:    fn.Some(chainfee.SatPerVByte(1)),
		}
	}
	return &chancloser.ShutdownPending{
		ShutdownScripts: scripts,
		IdealFeeRate:    fn.Some(chainfee.SatPerVByte(1)),
	}
}

func reconnectMachine(t *testing.T, initial chancloser.RbfState,
	env *chancloser.Environment) *chancloser.RbfChanCloser {

	t.Helper()
	machine := protofsm.NewStateMachine(chancloser.RbfChanCloserCfg{
		InitialState: initial,
		Env:          env,
	})
	machine.Start(t.Context())
	t.Cleanup(machine.Stop)
	return &machine
}

// TestRbfReconnectFeeBump retains the RPC across both handshake phases and
// verifies that the actual offer uses its fee, not the restored fee estimate.
func TestRbfReconnectFeeBump(t *testing.T) {
	for _, flushing := range []bool{false, true} {
		name := "shutdown pending"
		if flushing {
			name = "channel flushing"
		}
		t.Run(name, func(t *testing.T) {
			h, err := createTestPeerWithChannel(t, noUpdate)
			require.NoError(t, err)
			p := h.peer
			point := h.channel.ChannelPoint()
			fees := &reconnectFeeEstimator{
				fees: make(chan chainfee.SatPerKWeight, 4),
			}
			machine := reconnectMachine(t, reconnectState(t, flushing),
				&chancloser.Environment{
					ChainParams:    p.cfg.Wallet.Cfg.NetParams,
					ChanPoint:      point,
					ChanObserver:   &reconnectObserver{},
					FeeEstimator:   fees,
					DefaultFeeRate: 1,
				})
			p.activeChanCloses.Store(
				lnwire.NewChanIDFromOutPoint(point),
				makeRbfCloser(machine),
			)
			req := newTestCloseRequest(&point, 10)
			req.Ctx = t.Context()
			req.TargetFeePerKw = 5000
			result := make(chan error, 1)
			go func() {
				result <- p.startRbfChanCloser(newRPCShutdownInit(req), point)
			}()
			select {
			case err := <-result:
				t.Fatalf("fee bump returned before handshake completed: %v", err)
			case <-time.After(shortTimeout):
			}

			if !flushing {
				machine.SendEvent(t.Context(), &chancloser.ShutdownReceived{
					ShutdownScript: genScript(t, p2wshAddress),
				})
			}
			machine.SendEvent(t.Context(), &chancloser.ChannelFlushed{
				ShutdownBalances: chancloser.ShutdownBalances{
					RemoteBalance: 10_000_000,
				},
			})
			err, recvErr := fn.RecvOrTimeout(result, timeout)
			require.NoError(t, recvErr)
			require.NoError(t, err)
			initialFee, err := fn.RecvOrTimeout(fees.fees, timeout)
			require.NoError(t, err)
			require.EqualValues(t, 250, initialFee)
			requestedFee, err := fn.RecvOrTimeout(fees.fees, timeout)
			require.NoError(t, err)
			require.Equal(t, req.TargetFeePerKw, requestedFee)
		})
	}
}

// TestRbfReconnectCancellation releases a pending RPC on caller cancellation,
// peer shutdown, closer replacement, or on-chain confirmation.
func TestRbfReconnectCancellation(t *testing.T) {
	for _, reason := range []string{"caller", "peer", "closer", "confirmed"} {
		t.Run(reason, func(t *testing.T) {
			h, err := createTestPeerWithChannel(t, noUpdate)
			require.NoError(t, err)
			p := h.peer
			point := h.channel.ChannelPoint()
			machine := reconnectMachine(t, reconnectState(t, false),
				&chancloser.Environment{ChanPoint: point})
			p.activeChanCloses.Store(
				lnwire.NewChanIDFromOutPoint(point), makeRbfCloser(machine),
			)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := newTestCloseRequest(&point, 10)
			req.Ctx = ctx
			req.TargetFeePerKw = 5000
			result := make(chan error, 1)
			go func() {
				result <- p.startRbfChanCloser(newRPCShutdownInit(req), point)
			}()
			select {
			case err := <-result:
				t.Fatalf("fee bump returned before cancellation: %v", err)
			case <-time.After(shortTimeout):
			}
			switch reason {
			case "caller":
				cancel()
			case "peer":
				p.Disconnect(context.Canceled)
			case "closer":
				machine.Stop()
			case "confirmed":
				machine.SendEvent(t.Context(), &chancloser.SpendEvent{
					Tx: wire.NewMsgTx(2),
				})
			}
			err, recvErr := fn.RecvOrTimeout(result, timeout)
			require.NoError(t, recvErr)
			require.Error(t, err)
			if reason == "caller" {
				require.ErrorIs(t, err, context.Canceled)
			}
			if reason == "closer" {
				require.ErrorIs(t, err, protofsm.ErrStateMachineShutdown)
			}
			if reason == "confirmed" {
				require.ErrorContains(t, err, "already confirmed")
			}
		})
	}
}

// TestRbfReconnectRejectedOffer permits another attempt after a benign fee
// error instead of waiting forever for a ClosePending state that cannot arrive.
func TestRbfReconnectRejectedOffer(t *testing.T) {
	h, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	p := h.peer
	point := h.channel.ChannelPoint()
	terms := &chancloser.CloseChannelTerms{}
	fees := &reconnectFeeEstimator{fees: make(chan chainfee.SatPerKWeight, 4)}
	machine := reconnectMachine(t, &chancloser.ClosingNegotiation{
		PeerState: lntypes.Dual[chancloser.AsymmetricPeerState]{
			Local: &chancloser.CloseErr{
				CloseChannelTerms: terms,
				Party:             lntypes.Local,
				ErrState:          chancloser.NewErrStateCantPayForFee(0, 1000),
			},
			Remote: &chancloser.RemoteCloseStart{CloseChannelTerms: terms},
		},
		CloseChannelTerms: terms,
	}, &chancloser.Environment{ChanPoint: point, FeeEstimator: fees})
	p.activeChanCloses.Store(lnwire.NewChanIDFromOutPoint(point), makeRbfCloser(machine))
	ctx, cancel := context.WithTimeout(t.Context(), shortTimeout)
	defer cancel()
	req := newTestCloseRequest(&point, 10)
	req.Ctx = ctx
	req.TargetFeePerKw = 5000
	result := make(chan error, 1)
	go func() {
		result <- p.startRbfChanCloser(newRPCShutdownInit(req), point)
	}()
	err, recvErr := fn.RecvOrTimeout(result, timeout)
	require.NoError(t, recvErr)
	require.NoError(t, err)
	fee, err := fn.RecvOrTimeout(fees.fees, timeout)
	require.NoError(t, err)
	require.Equal(t, req.TargetFeePerKw, fee)
}

// TestRbfReconnectRecoveryDoesNotWait preserves nonblocking startup when an
// RPC has already started the restored handshake.
func TestRbfReconnectRecoveryDoesNotWait(t *testing.T) {
	for _, flushing := range []bool{false, true} {
		h, err := createTestPeerWithChannel(t, noUpdate)
		require.NoError(t, err)
		p := h.peer
		point := h.channel.ChannelPoint()
		machine := reconnectMachine(t, reconnectState(t, flushing),
			&chancloser.Environment{ChanPoint: point})
		p.activeChanCloses.Store(lnwire.NewChanIDFromOutPoint(point), makeRbfCloser(machine))
		result := make(chan error, 1)
		go func() {
			result <- p.startRbfChanCloser(shutdownInit{}, point)
		}()
		err, recvErr := fn.RecvOrTimeout(result, shortTimeout)
		require.NoError(t, recvErr)
		require.NoError(t, err)
	}
}

func TestRbfReconnectAlreadyConfirmed(t *testing.T) {
	h, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	p := h.peer
	point := h.channel.ChannelPoint()
	machine := reconnectMachine(t, &chancloser.CloseFin{ConfirmedTx: wire.NewMsgTx(2)},
		&chancloser.Environment{ChanPoint: point})
	p.activeChanCloses.Store(lnwire.NewChanIDFromOutPoint(point), makeRbfCloser(machine))
	req := newTestCloseRequest(&point, 10)
	req.Ctx = t.Context()
	req.TargetFeePerKw = 5000
	require.ErrorContains(t, p.startRbfChanCloser(newRPCShutdownInit(req), point),
		"already confirmed")
}
