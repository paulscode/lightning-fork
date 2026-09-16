package lnd

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc/bridgerpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/zpay32"
)

// bridgeDeps is what the bridge sub-server gets from the node: the local half
// of a cross-chain swap, wired in process.
//
// Dialling this node's own RPC server from inside it would mean an operator
// supplying a macaroon and a TLS path for their own node, and the sub-server
// waiting for that server to come up before it could work. Going straight to
// the registry and the router removes both.
func (s *server) bridgeDeps(
	routerBackend *routerrpc.RouterBackend) *bridgerpc.Deps {

	return &bridgerpc.Deps{
		AddHoldInvoice: s.addBridgeHoldInvoice,

		LookupInvoice: func(ctx context.Context, hash [32]byte) (
			bridgerpc.InvoiceStatus, bool, error) {

			inv, err := s.invoices.LookupInvoice(ctx, hash)
			if err != nil {
				if errors.Is(err, invoices.ErrInvoiceNotFound) {
					return bridgerpc.InvoiceStatus{}, false,
						nil
				}

				return bridgerpc.InvoiceStatus{}, false, err
			}

			return bridgerpc.TranslateInvoice(&inv), true, nil
		},

		SettleInvoice: func(ctx context.Context, pre [32]byte) error {
			return s.invoices.SettleHodlInvoice(
				ctx, lntypes.Preimage(pre),
			)
		},

		CancelInvoice: func(ctx context.Context, hash [32]byte) error {
			return s.invoices.CancelInvoice(ctx, lntypes.Hash(hash))
		},

		DecodeInvoice: func(_ context.Context,
			invoice string) (*zpay32.Invoice, error) {

			return zpay32.Decode(
				invoice, s.cfg.ActiveNetParams.Params,
			)
		},

		PayInvoice: func(ctx context.Context, req bridgerpc.PayRequest) (
			bridgerpc.PaymentStatus, error) {

			return s.payBridgeInvoice(ctx, routerBackend, req)
		},

		LookupPayment: s.lookupBridgePayment,

		BlockHeight: s.bridgeBlockHeight,
	}
}

// addBridgeHoldInvoice creates a hold invoice on this node.
//
// It is the same path lnrpc/invoicesrpc's AddHoldInvoice takes, with the
// request built here rather than unmarshalled from the wire.
//
// No route hints are attached. That is correct while the bridge is its own
// users' LSP, which is the topology this is built for, and wrong the moment it
// is not: a payer with no public path to this node needs a hint to find one.
// Adding them is a change here plus a source of channel information, not a
// change anywhere above.
func (s *server) addBridgeHoldInvoice(ctx context.Context,
	req bridgerpc.HoldInvoiceRequest) (string, error) {

	hash := lntypes.Hash(req.Hash)

	// The bridge's own adapter rejects these before they reach here. They
	// are checked again because this is the boundary where a mistake stops
	// being recoverable: an invoice for any amount lets the payer choose
	// what the bridge is paid, and one with the node's default CLTV will
	// not outlive the outgoing leg.
	if req.AmountMsat == 0 {
		return "", fmt.Errorf("bridge hold invoice %x: no amount", hash)
	}
	if req.CLTVDelta == 0 {
		return "", fmt.Errorf("bridge hold invoice %x: no CLTV delta",
			hash)
	}
	if req.Expiry <= 0 {
		return "", fmt.Errorf("bridge hold invoice %x: no expiry", hash)
	}

	addInvoiceCfg := &invoicesrpc.AddInvoiceConfig{
		AddInvoice:        s.invoices.AddInvoice,
		IsChannelActive:   s.htlcSwitch.HasActiveLink,
		ChainParams:       s.cfg.ActiveNetParams.Params,
		NodeSigner:        s.nodeSigner,
		DefaultCLTVExpiry: s.cfg.Bitcoin.TimeLockDelta,
		ChanDB:            s.chanStateDB,
		Graph:             s.v1Graph,
		GenInvoiceFeatures: func() *lnwire.FeatureVector {
			return s.featureMgr.Get(feature.SetInvoice)
		},
		GenAmpInvoiceFeatures: func() *lnwire.FeatureVector {
			return s.featureMgr.Get(feature.SetInvoiceAmp)
		},
		GetAlias: s.aliasMgr.GetPeerAlias,
	}

	value := lnwire.MilliSatoshi(req.AmountMsat)
	data := &invoicesrpc.AddInvoiceData{
		Memo:        req.Memo,
		Hash:        &hash,
		Value:       value,
		Expiry:      int64(req.Expiry.Seconds()),
		CltvExpiry:  uint64(req.CLTVDelta),
		HodlInvoice: true,

		// The preimage is deliberately absent: it belongs to whoever
		// issued the invoice on the other chain, and the entire point
		// of the swap is that this node cannot claim without being
		// handed it.
		Preimage: nil,
	}

	_, dbInvoice, err := invoicesrpc.AddInvoice(ctx, addInvoiceCfg, data)
	if err != nil {
		return "", err
	}

	return string(dbInvoice.PaymentRequest), nil
}

// payBridgeInvoice sends a payment and blocks until it resolves.
//
// The intent is built by the router backend's own ExtractIntent rather than
// assembled here, so the bridge gets exactly the rules an RPC client gets: the
// invoice's expiry is validated, a payment address or blinded paths are
// required, the amount comes from the invoice, and whether it may be split is
// decided by the invoice's own features. A second reading of a payment request
// that disagreed by one field would have the bridge hold one hash while the
// node paid another, and the whole security of a swap is that those are equal.
func (s *server) payBridgeInvoice(ctx context.Context,
	routerBackend *routerrpc.RouterBackend,
	req bridgerpc.PayRequest) (bridgerpc.PaymentStatus, error) {

	if req.Invoice == "" {
		return bridgerpc.PaymentStatus{}, errors.New("bridge payment: " +
			"no invoice to pay")
	}
	if req.CLTVLimit == 0 {
		return bridgerpc.PaymentStatus{}, errors.New("bridge payment: " +
			"no CLTV limit; the incoming leg is sized against " +
			"this and an unbounded route could outlive it")
	}
	if req.CLTVLimit > math.MaxInt32 {
		return bridgerpc.PaymentStatus{}, fmt.Errorf("bridge payment: "+
			"CLTV limit %d does not fit", req.CLTVLimit)
	}
	if req.Timeout <= 0 {
		return bridgerpc.PaymentStatus{}, errors.New("bridge payment: " +
			"no timeout; an attempt that never gives up holds the " +
			"incoming HTLC until it expires")
	}

	intent, err := routerBackend.ExtractIntent(
		&routerrpc.SendPaymentRequest{
			PaymentRequest: req.Invoice,
			TimeoutSeconds: int32(req.Timeout.Seconds()),
			FeeLimitMsat:   int64(req.MaxFeeMsat),
			CltvLimit:      int32(req.CLTVLimit),
		},
	)
	if err != nil {
		return bridgerpc.PaymentStatus{}, err
	}

	preimage, route, err := s.chanRouter.SendPayment(ctx, intent)
	if err != nil {
		// Not a failure, and this is the distinction the whole design
		// turns on. The router gives up on its own deadline, but an
		// HTLC it already sent is out there regardless, and concluding
		// failure here would cancel the incoming claim against a
		// payment that may still settle. Report what is actually
		// known: the node has a record and its outcome is not decided.
		//
		// The bridge resolves this by looking the payment up, which is
		// the only thing that can answer it.
		srvrLog.Debugf("Bridge payment did not resolve in time or "+
			"failed to dispatch, reporting in flight: %v", err)

		return bridgerpc.PaymentStatus{Known: true, InFlight: true}, nil
	}

	status := bridgerpc.PaymentStatus{
		Known: true, Settled: true, Preimage: preimage,
	}
	if route != nil {
		status.FeeMsat = uint64(route.TotalFees())
	}

	return status, nil
}

// lookupBridgePayment reports what became of a payment, keeping "never seen"
// distinct from "failed".
//
// Those two are the same shape to most callers and must not be here. After a
// restart the bridge asks this about every unfinished swap, and the answer
// decides whether it may cancel the incoming claim. Reading "I have no record"
// as "it failed" would cancel a claim against an HTLC that is still in flight.
func (s *server) lookupBridgePayment(ctx context.Context, hash [32]byte) (
	bridgerpc.PaymentStatus, error) {

	payment, err := s.controlTower.FetchPayment(ctx, lntypes.Hash(hash))
	if err != nil {
		// Never initiated is an answer, not a failure: it is how the
		// bridge tells a swap it never paid out from one it paid and
		// lost track of, and the difference decides whether it may
		// cancel the incoming claim.
		if errors.Is(err, paymentsdb.ErrPaymentNotInitiated) {
			return bridgerpc.TranslatePayment(nil), nil
		}

		return bridgerpc.PaymentStatus{}, err
	}

	return bridgerpc.TranslatePayment(payment), nil
}

// bridgeBlockHeight is the tip this node sees, and whether it has caught up.
//
// The height comes from the chain backend rather than from the wallet's view,
// so it is the freshest number available, and the sync flag says whether this
// node's own view has caught up with it. The bridge refuses to size anything
// while that is false: every margin decision is a comparison against this
// height, and a stale one says an HTLC has more time left than it does, which
// is the direction that pays out against a claim that can no longer be
// collected.
func (s *server) bridgeBlockHeight(_ context.Context) (int32, bool, error) {
	_, bestHeight, err := s.cc.ChainIO.GetBestBlock()
	if err != nil {
		return 0, false, fmt.Errorf("reading the best block: %w", err)
	}

	walletSynced, _, err := s.cc.Wallet.IsSynced()
	if err != nil {
		return bestHeight, false, fmt.Errorf("reading wallet sync "+
			"state: %w", err)
	}
	if !walletSynced {
		return bestHeight, false, nil
	}

	// The router validates each block's channels when it is not assuming
	// them valid, so it can lag the wallet under load. Waiting for it too
	// matches what GetInfo reports as synced_to_chain, which is what the
	// out-of-process adapter checks, and the two must not disagree about
	// whether this node is ready to price a swap.
	if !s.cfg.Routing.AssumeChannelValid {
		if uint32(bestHeight) != s.graphBuilder.SyncedHeight() {
			return bestHeight, false, nil
		}
	}

	return bestHeight, true, nil
}
