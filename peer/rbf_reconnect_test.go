package peer

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
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

// reconnectBalances is what the observer reports as the final balances.
type reconnectBalances = fn.Option[chancloser.ShutdownBalances]

func (*reconnectObserver) FinalBalances() reconnectBalances {
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

// reconnectFeeMachine starts a restored close whose offers are recorded by
// fees, and makes it the peer's closer for the channel.
func reconnectFeeMachine(t *testing.T, p *Brontide, point wire.OutPoint,
	initial chancloser.RbfState,
	fees *reconnectFeeEstimator) *chancloser.RbfChanCloser {

	t.Helper()
	machine := reconnectMachine(t, initial, &chancloser.Environment{
		ChainParams:    p.cfg.Wallet.Cfg.NetParams,
		ChanPoint:      point,
		ChanObserver:   &reconnectObserver{},
		FeeEstimator:   fees,
		DefaultFeeRate: 1,
	})
	p.activeChanCloses.Store(
		lnwire.NewChanIDFromOutPoint(point), makeRbfCloser(machine),
	)

	return machine
}

// newFeeBumpRequest returns an RPC fee bump for the channel.
func newFeeBumpRequest(ctx context.Context, point *wire.OutPoint,
	fee chainfee.SatPerKWeight) *htlcswitch.ChanClose {

	req := newTestCloseRequest(point, 10)
	req.Ctx = ctx
	req.CloseType = contractcourt.CloseRegular
	req.TargetFeePerKw = fee

	return req
}

// startFeeBump starts a fee bump and checks that it returns at once: the
// wait runs on a goroutine of its own, and the outcome reaches the caller
// through the request.
func startFeeBump(t *testing.T, p *Brontide, point wire.OutPoint,
	req *htlcswitch.ChanClose) {

	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- p.startRbfChanCloser(newRPCShutdownInit(req), point)
	}()
	err, recvErr := fn.RecvOrTimeout(result, timeout)
	require.NoError(t, recvErr, "fee bump did not return at once")
	require.NoError(t, err)
}

// closeReqErr waits for the error a close request ends with.
func closeReqErr(t *testing.T, req *htlcswitch.ChanClose) error {
	t.Helper()
	err, recvErr := fn.RecvOrTimeout(req.ErrChan(t.Context()), timeout)
	require.NoError(t, recvErr, "close request was not answered")

	return err
}

// requireNoCloseReqErr checks that a close request has not ended with an
// error.
func requireNoCloseReqErr(t *testing.T, req *htlcswitch.ChanClose) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), shortTimeout)
	defer cancel()
	require.ErrorIs(t, <-req.ErrChan(ctx), context.DeadlineExceeded)
}

// sendLinkFailure queues a link failure for the peer's channel manager.
func sendLinkFailure(t *testing.T, p *Brontide, failure linkFailureReport) {
	t.Helper()
	select {
	case p.linkFailures.ChanIn() <- failure:
	case <-time.After(timeout):
		t.Fatalf("channel manager did not take the link failure")
	}
}

// completeHandshake takes a restored close through the rest of the shutdown
// handshake. The local balance is zero, so every offer fails as unpayable.
func completeHandshake(t *testing.T, machine *chancloser.RbfChanCloser,
	flushing bool) {

	t.Helper()
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
			machine := reconnectFeeMachine(
				t, p, point, reconnectState(t, flushing), fees,
			)
			req := newFeeBumpRequest(t.Context(), &point, 5000)
			startFeeBump(t, p, point, req)

			// The bump waits for the handshake: no offer yet.
			requireNoCloseReqErr(t, req)
			require.Empty(t, fees.fees)

			completeHandshake(t, machine, flushing)
			initialFee, err := fn.RecvOrTimeout(fees.fees, timeout)
			require.NoError(t, err)
			require.EqualValues(t, 250, initialFee)
			requestedFee, err := fn.RecvOrTimeout(
				fees.fees, timeout,
			)
			require.NoError(t, err)
			require.Equal(t, req.TargetFeePerKw, requestedFee)
		})
	}
}

// TestRbfReconnectCancellation releases a pending RPC on caller cancellation,
// peer shutdown, the closer stopping (as retirement stops it), or on-chain
// confirmation.
func TestRbfReconnectCancellation(t *testing.T) {
	for _, reason := range []string{
		"caller", "peer", "closer", "confirmed",
	} {
		t.Run(reason, func(t *testing.T) {
			h, err := createTestPeerWithChannel(t, noUpdate)
			require.NoError(t, err)
			p := h.peer
			point := h.channel.ChannelPoint()
			machine := reconnectMachine(
				t, reconnectState(t, false),
				&chancloser.Environment{ChanPoint: point},
			)
			p.activeChanCloses.Store(
				lnwire.NewChanIDFromOutPoint(point),
				makeRbfCloser(machine),
			)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := newFeeBumpRequest(ctx, &point, 5000)
			startFeeBump(t, p, point, req)
			requireNoCloseReqErr(t, req)

			switch reason {
			case "caller":
				cancel()
			case "peer":
				p.Disconnect(context.Canceled)
			case "closer":
				machine.Stop()
			case "confirmed":
				machine.SendEvent(
					t.Context(), &chancloser.SpendEvent{
						Tx: wire.NewMsgTx(2),
					},
				)
			}
			err = closeReqErr(t, req)
			require.Error(t, err)
			switch reason {
			case "caller":
				require.ErrorIs(t, err, context.Canceled)
			case "closer":
				require.ErrorIs(
					t, err,
					protofsm.ErrStateMachineShutdown,
				)
			case "confirmed":
				require.ErrorContains(
					t, err, "already confirmed",
				)
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
	fees := &reconnectFeeEstimator{
		fees: make(chan chainfee.SatPerKWeight, 4),
	}
	machine := reconnectMachine(t, &chancloser.ClosingNegotiation{
		PeerState: lntypes.Dual[chancloser.AsymmetricPeerState]{
			Local: &chancloser.CloseErr{
				CloseChannelTerms: terms,
				Party:             lntypes.Local,
				ErrState: chancloser.NewErrStateCantPayForFee(
					0, 1000,
				),
			},
			Remote: &chancloser.RemoteCloseStart{
				CloseChannelTerms: terms,
			},
		},
		CloseChannelTerms: terms,
	}, &chancloser.Environment{ChanPoint: point, FeeEstimator: fees})
	p.activeChanCloses.Store(
		lnwire.NewChanIDFromOutPoint(point), makeRbfCloser(machine),
	)
	req := newFeeBumpRequest(t.Context(), &point, 5000)
	startFeeBump(t, p, point, req)
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
		p.activeChanCloses.Store(
			lnwire.NewChanIDFromOutPoint(point),
			makeRbfCloser(machine),
		)
		result := make(chan error, 1)
		go func() {
			result <- p.startRbfChanCloser(shutdownInit{}, point)
		}()
		err, recvErr := fn.RecvOrTimeout(result, shortTimeout)
		require.NoError(t, recvErr)
		require.NoError(t, err)
	}
}

// TestRbfReconnectAlreadyConfirmed refuses a fee bump for a close that has
// confirmed, and says so at once.
func TestRbfReconnectAlreadyConfirmed(t *testing.T) {
	h, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	p := h.peer
	point := h.channel.ChannelPoint()
	machine := reconnectMachine(
		t, &chancloser.CloseFin{ConfirmedTx: wire.NewMsgTx(2)},
		&chancloser.Environment{ChanPoint: point},
	)
	p.activeChanCloses.Store(
		lnwire.NewChanIDFromOutPoint(point), makeRbfCloser(machine),
	)
	req := newFeeBumpRequest(t.Context(), &point, 5000)
	require.ErrorContains(
		t, p.startRbfChanCloser(newRPCShutdownInit(req), point),
		"already confirmed",
	)
}

// TestRbfReconnectWaitKeepsManagerFree sends a fee bump through the channel
// manager while the restored shutdown is still pending, then checks that a
// second request and a link failure for the same peer are still handled and
// that the waiting caller is told the link failed.
func TestRbfReconnectWaitKeepsManagerFree(t *testing.T) {
	h, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	p := h.peer
	point := h.channel.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(point)

	rbfBits := lnwire.NewRawFeatureVector(lnwire.RbfCoopCloseOptional)
	p.cfg.Features = lnwire.NewFeatureVector(rbfBits, lnwire.Features)
	p.remoteFeatures = lnwire.NewFeatureVector(rbfBits, lnwire.Features)
	require.True(t, p.rbfCoopCloseAllowed(h.channel.ChanType()))

	machine := reconnectMachine(t, reconnectState(t, false),
		&chancloser.Environment{ChanPoint: point})
	p.activeChanCloses.Store(chanID, makeRbfCloser(machine))

	req := newFeeBumpRequest(t.Context(), &point, 5000)
	p.HandleLocalCloseChanReqs(req)
	requireNoCloseReqErr(t, req)

	// A second request reaches the channel manager only if the first is
	// not holding it.
	second := newFeeBumpRequest(t.Context(), &point, 6000)
	sent := make(chan struct{})
	go func() {
		p.HandleLocalCloseChanReqs(second)
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(timeout):
		t.Fatalf("channel manager did not take a second request " +
			"while a fee bump waits")
	}

	sendLinkFailure(t, p, linkFailureReport{chanPoint: point})
	require.Eventually(t, func() bool {
		_, ok := p.activeChannels.Load(chanID)
		return !ok
	}, timeout, 10*time.Millisecond, "link failure not handled")

	require.ErrorIs(
		t, closeReqErr(t, req), ErrCoopCloseAbortedByLinkFailure,
	)
	require.Error(t, closeReqErr(t, second))
}

// TestRbfReconnectConcurrentBumps sends two fee bumps while the restored
// shutdown is pending. Both must reach the close, and the close must still
// run afterwards. The local balance is zero, so each offer fails as unpayable
// and its caller is told so.
func TestRbfReconnectConcurrentBumps(t *testing.T) {
	h, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	p := h.peer
	point := h.channel.ChannelPoint()
	fees := &reconnectFeeEstimator{
		fees: make(chan chainfee.SatPerKWeight, 4),
	}
	machine := reconnectFeeMachine(
		t, p, point, reconnectState(t, false), fees,
	)

	first := newFeeBumpRequest(t.Context(), &point, 5000)
	second := newFeeBumpRequest(t.Context(), &point, 6000)
	startFeeBump(t, p, point, first)
	startFeeBump(t, p, point, second)
	completeHandshake(t, machine, false)

	initialFee, err := fn.RecvOrTimeout(fees.fees, timeout)
	require.NoError(t, err)
	require.EqualValues(t, 250, initialFee)

	got := make(map[chainfee.SatPerKWeight]bool)
	for range 2 {
		fee, err := fn.RecvOrTimeout(fees.fees, timeout)
		require.NoError(t, err)
		got[fee] = true
	}
	require.Equal(t, map[chainfee.SatPerKWeight]bool{
		5000: true, 6000: true,
	}, got)

	// The close is still running, and each caller is answered by the
	// outcome of its own offer.
	_, err = machine.CurrentState()
	require.NoError(t, err)
	require.ErrorContains(t, closeReqErr(t, first), "cannot pay")
	require.ErrorContains(t, closeReqErr(t, second), "cannot pay")
}

// TestRbfFeeBumpSlots checks that a channel's fee bumps take turns, that a
// bump waiting for its turn can be cancelled, and that channels do not share
// a turn.
func TestRbfFeeBumpSlots(t *testing.T) {
	var slots rbfFeeBumpSlots
	chanA := lnwire.ChannelID{1}
	chanB := lnwire.ChannelID{2}

	release, err := slots.acquire(t.Context(), chanA)
	require.NoError(t, err)

	// Another channel has a turn of its own.
	releaseB, err := slots.acquire(t.Context(), chanB)
	require.NoError(t, err)
	releaseB()

	// A second bump on the same channel waits, and can be cancelled.
	ctx, cancel := context.WithTimeout(t.Context(), shortTimeout)
	defer cancel()
	_, err = slots.acquire(ctx, chanA)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// Once the turn is given back, the next bump takes it.
	got := make(chan error, 1)
	go func() {
		release2, err := slots.acquire(t.Context(), chanA)
		if err == nil {
			release2()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatalf("second bump took a turn that was not free")
	case <-time.After(shortTimeout):
	}
	release()
	err, recvErr := fn.RecvOrTimeout(got, timeout)
	require.NoError(t, recvErr)
	require.NoError(t, err)

	// A channel's slot is dropped once no bump uses it.
	slots.mu.Lock()
	defer slots.mu.Unlock()
	require.Empty(t, slots.slots)
}
