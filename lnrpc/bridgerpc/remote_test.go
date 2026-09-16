//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/paulscode/lightning-fork-bridge/node"
)

// The fakes below embed the generated interfaces and override only the calls
// this adapter makes, so the compiler keeps them in step as lnd's own RPC
// surface grows.

type fakeMain struct {
	lnrpc.LightningClient

	info    *lnrpc.GetInfoResponse
	infoErr error

	payReq    *lnrpc.PayReq
	payReqErr error
}

func (f *fakeMain) GetInfo(context.Context, *lnrpc.GetInfoRequest,
	...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	return f.info, f.infoErr
}

func (f *fakeMain) DecodePayReq(context.Context, *lnrpc.PayReqString,
	...grpc.CallOption) (*lnrpc.PayReq, error) {

	return f.payReq, f.payReqErr
}

type fakeInvoices struct {
	invoicesrpc.InvoicesClient

	addResp *invoicesrpc.AddHoldInvoiceResp
	addErr  error

	lookup    *lnrpc.Invoice
	lookupErr error

	settleErr error
	cancelErr error

	settles, cancels int
}

func (f *fakeInvoices) AddHoldInvoice(_ context.Context,
	_ *invoicesrpc.AddHoldInvoiceRequest, _ ...grpc.CallOption) (
	*invoicesrpc.AddHoldInvoiceResp, error) {

	return f.addResp, f.addErr
}

func (f *fakeInvoices) LookupInvoiceV2(context.Context,
	*invoicesrpc.LookupInvoiceMsg, ...grpc.CallOption) (*lnrpc.Invoice,
	error) {

	return f.lookup, f.lookupErr
}

func (f *fakeInvoices) SettleInvoice(context.Context,
	*invoicesrpc.SettleInvoiceMsg, ...grpc.CallOption) (
	*invoicesrpc.SettleInvoiceResp, error) {

	f.settles++

	return &invoicesrpc.SettleInvoiceResp{}, f.settleErr
}

func (f *fakeInvoices) CancelInvoice(context.Context,
	*invoicesrpc.CancelInvoiceMsg, ...grpc.CallOption) (
	*invoicesrpc.CancelInvoiceResp, error) {

	f.cancels++

	return &invoicesrpc.CancelInvoiceResp{}, f.cancelErr
}

// fakeStream replays updates and then an error, which is how a real one ends.
type fakeStream struct {
	grpc.ClientStream

	updates []*lnrpc.Payment
	err     error
	i       int
}

func (s *fakeStream) Recv() (*lnrpc.Payment, error) {
	if s.i < len(s.updates) {
		u := s.updates[s.i]
		s.i++

		return u, nil
	}
	if s.err != nil {
		return nil, s.err
	}

	return nil, io.EOF
}

type fakeRouter struct {
	routerrpc.RouterClient

	send    *fakeStream
	sendErr error

	track    *fakeStream
	trackErr error
}

func (f *fakeRouter) SendPaymentV2(context.Context,
	*routerrpc.SendPaymentRequest, ...grpc.CallOption) (
	routerrpc.Router_SendPaymentV2Client, error) {

	if f.sendErr != nil {
		return nil, f.sendErr
	}

	return f.send, nil
}

func (f *fakeRouter) TrackPaymentV2(context.Context,
	*routerrpc.TrackPaymentRequest, ...grpc.CallOption) (
	routerrpc.Router_TrackPaymentV2Client, error) {

	if f.trackErr != nil {
		return nil, f.trackErr
	}

	return f.track, nil
}

func remote(m *fakeMain, i *fakeInvoices, r *fakeRouter) *Remote {
	if m == nil {
		m = &fakeMain{}
	}
	if i == nil {
		i = &fakeInvoices{}
	}
	if r == nil {
		r = &fakeRouter{}
	}

	return &Remote{main: m, invoices: i, router: r}
}

// A node still catching up must refuse to report a height.
func TestRemoteBlockHeightRefusesWhileSyncing(t *testing.T) {
	t.Parallel()

	r := remote(&fakeMain{info: &lnrpc.GetInfoResponse{
		BlockHeight: 800_000, SyncedToChain: false,
	}}, nil, nil)

	got, err := r.BlockHeight(context.Background())
	if !errors.Is(err, ErrNotSynced) {
		t.Fatalf("wanted ErrNotSynced, got %v (height %d)", err, got)
	}
	if got != 0 {
		t.Errorf("a refused height should be zero, got %d", got)
	}
}

func TestRemoteBlockHeightWhenSynced(t *testing.T) {
	t.Parallel()

	r := remote(&fakeMain{info: &lnrpc.GetInfoResponse{
		BlockHeight: 800_000, SyncedToChain: true,
	}}, nil, nil)

	got, err := r.BlockHeight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 800_000 {
		t.Errorf("got %d, wanted 800000", got)
	}
}

// Idempotency must come from the invoice's state, not the error text: a node
// that rephrases its error must not turn a settled swap into one that retries
// forever, and a settle that did not settle must still fail.
func TestRemoteSettleIsIdempotentByState(t *testing.T) {
	t.Parallel()

	var pre node.Preimage
	pre[0] = 0x33

	t.Run("already settled is success", func(t *testing.T) {
		t.Parallel()

		inv := &fakeInvoices{
			settleErr: status.Error(
				codes.Unknown, "wrapped in new wording",
			),
			lookup: &lnrpc.Invoice{State: lnrpc.Invoice_SETTLED},
		}

		if err := remote(nil, inv, nil).SettleInvoice(
			context.Background(), pre,
		); err != nil {

			t.Fatalf("settling an already settled invoice: %v", err)
		}
		if inv.settles != 1 {
			t.Errorf("asked %d times, wanted once", inv.settles)
		}
	})

	t.Run("not settled is still a failure", func(t *testing.T) {
		t.Parallel()

		inv := &fakeInvoices{
			settleErr: status.Error(
				codes.Unknown, "invoice already settled",
			),
			lookup: &lnrpc.Invoice{State: lnrpc.Invoice_ACCEPTED},
		}

		if err := remote(nil, inv, nil).SettleInvoice(
			context.Background(), pre,
		); err == nil {

			t.Fatal("a settle that did not settle reported success")
		}
	})
}

func TestRemoteCancelIsIdempotentByState(t *testing.T) {
	t.Parallel()

	h := node.Hash(hash8(7))

	inv := &fakeInvoices{
		cancelErr: status.Error(codes.Unknown, "some other wording"),
		lookup:    &lnrpc.Invoice{State: lnrpc.Invoice_CANCELED},
	}
	if err := remote(nil, inv, nil).CancelInvoice(
		context.Background(), h,
	); err != nil {

		t.Fatalf("cancelling an already cancelled invoice: %v", err)
	}

	inv2 := &fakeInvoices{
		cancelErr: status.Error(codes.Unknown, "invoice already canceled"),
		lookup:    &lnrpc.Invoice{State: lnrpc.Invoice_ACCEPTED},
	}
	if err := remote(nil, inv2, nil).CancelInvoice(
		context.Background(), h,
	); err == nil {

		t.Fatal("a cancel that did not cancel reported success")
	}
}

// An unknown hash is its own answer, not a generic failure.
func TestRemoteLookupInvoiceUnknownHash(t *testing.T) {
	t.Parallel()

	inv := &fakeInvoices{
		lookupErr: status.Error(codes.NotFound, "no such invoice"),
	}

	_, err := remote(nil, inv, nil).LookupInvoice(
		context.Background(), node.Hash(hash8(8)),
	)
	if !errors.Is(err, node.ErrUnknownHash) {
		t.Fatalf("wanted ErrUnknownHash, got %v", err)
	}
}

func TestRemoteDecode(t *testing.T) {
	t.Parallel()

	h := hash8(9)
	hexHash := hex.EncodeToString(h[:])
	created := time.Now().Truncate(time.Second)

	t.Run("carries the hash, amount and wall-clock expiry", func(t *testing.T) {
		t.Parallel()

		r := remote(&fakeMain{payReq: &lnrpc.PayReq{
			PaymentHash: hexHash, NumMsat: 150_000,
			CltvExpiry: 80, Timestamp: created.Unix(),
			Expiry: 600, Destination: "02aa", Description: "swap",
		}}, nil, nil)

		got, err := r.Decode(context.Background(), "lnbc1payme")
		if err != nil {
			t.Fatal(err)
		}
		if got.Hash != node.Hash(h) {
			t.Errorf("hash %x, wanted %x", got.Hash, h)
		}
		if got.AmountMsat != 150_000 {
			t.Errorf("amount %d, wanted 150000", got.AmountMsat)
		}
		if got.CLTVDelta != 80 {
			t.Errorf("CLTV delta %d, wanted 80", got.CLTVDelta)
		}
		if want := created.Add(600 * time.Second); !got.Expiry.Equal(want) {
			t.Errorf("expiry %v, wanted %v", got.Expiry, want)
		}
	})

	refusals := []struct {
		name string
		req  *lnrpc.PayReq
	}{
		{
			name: "no amount",
			req: &lnrpc.PayReq{
				PaymentHash: hexHash, NumMsat: 0,
			},
		},
		{
			name: "a negative amount",
			req: &lnrpc.PayReq{
				PaymentHash: hexHash, NumMsat: -1,
			},
		},
		{
			name: "a short payment hash",
			req: &lnrpc.PayReq{
				PaymentHash: "abcd", NumMsat: 1000,
			},
		},
		{
			name: "a payment hash that is not hex",
			req: &lnrpc.PayReq{
				PaymentHash: strings.Repeat("z", 64),
				NumMsat:     1000,
			},
		},
		{
			name: "a negative CLTV delta",
			req: &lnrpc.PayReq{
				PaymentHash: hexHash, NumMsat: 1000,
				CltvExpiry: -1,
			},
		},
	}

	for _, test := range refusals {
		t.Run("refuses "+test.name, func(t *testing.T) {
			t.Parallel()

			r := remote(&fakeMain{payReq: test.req}, nil, nil)
			if _, err := r.Decode(
				context.Background(), "lnbc1payme",
			); err == nil {

				t.Fatal("wanted a refusal")
			}
		})
	}

	t.Run("refuses an empty invoice without asking the node", func(t *testing.T) {
		t.Parallel()

		if _, err := remote(nil, nil, nil).Decode(
			context.Background(), "",
		); err == nil {

			t.Fatal("wanted a refusal")
		}
	})
}

// A stream that dies must never read as failure. The HTLC is out there whatever
// this call saw, and concluding failure would cancel the incoming claim.
func TestRemotePayReportsInFlightWhenTheStreamDies(t *testing.T) {
	t.Parallel()

	req := node.PayRequest{
		Invoice: "lnbc1payme", CLTVLimit: 80, Timeout: time.Minute,
	}

	for _, test := range []struct {
		name   string
		stream *fakeStream
	}{
		{
			name:   "it ends before saying anything",
			stream: &fakeStream{},
		},
		{
			name: "it breaks mid-payment",
			stream: &fakeStream{
				err: errors.New("connection reset"),
			},
		},
		{
			name: "the deadline passes after an in-flight update",
			stream: &fakeStream{
				updates: []*lnrpc.Payment{
					{Status: lnrpc.Payment_IN_FLIGHT},
				},
				err: context.DeadlineExceeded,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := remote(nil, nil, &fakeRouter{
				send: test.stream,
			}).Pay(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != node.PaymentInFlight {
				t.Errorf("got %v, wanted in flight", got.State)
			}
		})
	}
}

func TestRemotePayTerminalStates(t *testing.T) {
	t.Parallel()

	pre := strings.Repeat("cd", 32)
	req := node.PayRequest{
		Invoice: "lnbc1payme", CLTVLimit: 80, Timeout: time.Minute,
	}

	t.Run("succeeded", func(t *testing.T) {
		t.Parallel()

		got, err := remote(nil, nil, &fakeRouter{
			send: &fakeStream{updates: []*lnrpc.Payment{
				{Status: lnrpc.Payment_IN_FLIGHT},
				{
					Status:          lnrpc.Payment_SUCCEEDED,
					PaymentPreimage: pre,
				},
			}},
		}).Pay(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != node.PaymentSucceeded {
			t.Fatalf("got %v, wanted succeeded", got.State)
		}
		if got.Preimage == (node.Preimage{}) {
			t.Error("a success arrived with no preimage")
		}
	})

	t.Run("failed", func(t *testing.T) {
		t.Parallel()

		got, err := remote(nil, nil, &fakeRouter{
			send: &fakeStream{updates: []*lnrpc.Payment{
				{Status: lnrpc.Payment_FAILED},
			}},
		}).Pay(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != node.PaymentFailed {
			t.Errorf("got %v, wanted failed", got.State)
		}
	})
}

// The bridge's contract refuses these, each a way of losing money.
func TestRemotePayRefusesUnboundedRequests(t *testing.T) {
	t.Parallel()

	good := node.PayRequest{
		Invoice: "lnbc1payme", CLTVLimit: 80, Timeout: time.Minute,
	}

	for _, test := range []struct {
		name string
		req  node.PayRequest
	}{
		{"no invoice", node.PayRequest{CLTVLimit: 80, Timeout: time.Minute}},
		{"no CLTV limit", node.PayRequest{
			Invoice: "lnbc1payme", Timeout: time.Minute,
		}},
		{"no timeout", node.PayRequest{
			Invoice: "lnbc1payme", CLTVLimit: 80,
		}},
	} {
		t.Run("refuses "+test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := remote(nil, nil, nil).Pay(
				context.Background(), test.req,
			); err == nil {

				t.Fatal("wanted a refusal")
			}
		})
	}

	if _, err := remote(nil, nil, &fakeRouter{
		send: &fakeStream{updates: []*lnrpc.Payment{
			{Status: lnrpc.Payment_FAILED},
		}},
	}).Pay(context.Background(), good); err != nil {

		t.Errorf("a well-formed request was refused: %v", err)
	}
}

// "Never sent" is how the driver tells an unpaid swap from one it lost track
// of, so NotFound must come back as none rather than as an error.
func TestRemoteLookupPaymentNotFoundIsAnAnswer(t *testing.T) {
	t.Parallel()

	notFound := status.Error(codes.NotFound, "payment isn't initiated")

	t.Run("on opening the stream", func(t *testing.T) {
		t.Parallel()

		got, err := remote(nil, nil, &fakeRouter{
			trackErr: notFound,
		}).LookupPayment(context.Background(), node.Hash(hash8(1)))
		if err != nil {
			t.Fatal(err)
		}
		if got.State != node.PaymentNone {
			t.Errorf("got %v, wanted none", got.State)
		}
	})

	t.Run("on the first receive", func(t *testing.T) {
		t.Parallel()

		got, err := remote(nil, nil, &fakeRouter{
			track: &fakeStream{err: notFound},
		}).LookupPayment(context.Background(), node.Hash(hash8(1)))
		if err != nil {
			t.Fatal(err)
		}
		if got.State != node.PaymentNone {
			t.Errorf("got %v, wanted none", got.State)
		}
	})
}

// Any other stream failure is a real error: the bridge must not read "I could
// not ask" as "it was never sent".
func TestRemoteLookupPaymentDistinguishesUnreachableFromUnknown(t *testing.T) {
	t.Parallel()

	_, err := remote(nil, nil, &fakeRouter{
		trackErr: status.Error(codes.Unavailable, "node is down"),
	}).LookupPayment(context.Background(), node.Hash(hash8(1)))
	if err == nil {
		t.Fatal("an unreachable node must not read as never sent")
	}
}

func TestRemoteAddHoldInvoiceRefusesUnpricedRequests(t *testing.T) {
	t.Parallel()

	good := node.HoldInvoice{
		Hash: hash8(1), AmountMsat: 1000, CLTVDelta: 400,
		Expiry: time.Minute,
	}

	for _, test := range []struct {
		name string
		req  node.HoldInvoice
	}{
		{"no amount", node.HoldInvoice{CLTVDelta: 400, Expiry: time.Minute}},
		{"no CLTV delta", node.HoldInvoice{
			AmountMsat: 1000, Expiry: time.Minute,
		}},
		{"no expiry", node.HoldInvoice{
			AmountMsat: 1000, CLTVDelta: 400,
		}},
	} {
		t.Run("refuses "+test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := remote(nil, nil, nil).AddHoldInvoice(
				context.Background(), test.req,
			); err == nil {

				t.Fatal("wanted a refusal")
			}
		})
	}

	inv := &fakeInvoices{addResp: &invoicesrpc.AddHoldInvoiceResp{
		PaymentRequest: "lnbc1held",
	}}
	got, err := remote(nil, inv, nil).AddHoldInvoice(
		context.Background(), good,
	)
	if err != nil {
		t.Fatalf("a well-formed request was refused: %v", err)
	}
	if got != "lnbc1held" {
		t.Errorf("got %q, wanted the payment request", got)
	}
}
