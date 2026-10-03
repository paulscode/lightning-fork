//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/rate"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

// ---------------------------------------------------------------------------
// The rate book: set at runtime, kept across restarts, refused when stale.
// ---------------------------------------------------------------------------

func TestRatebookStartsFromTheConfiguration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0.004, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := b.usable()
	if err != nil || r != 0.004 {
		t.Fatalf("rate %g (%v), wanted the configured 0.004", r, err)
	}
	if _, err := os.Stat(filepath.Join(dir, rateFileName)); err != nil {
		t.Errorf("the rate was not recorded: %v", err)
	}
}

// A rate set while running survives a restart, so an operator who changed it
// is not silently reverted by the next start.
func TestARateSetAtRuntimeSurvivesARestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0.004, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.set(0.0048, 0.004); err != nil {
		t.Fatal(err)
	}

	again, err := openRatebook(dir, 0.004, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := again.current(); r != 0.0048 {
		t.Errorf("after a restart the rate is %g, wanted the 0.0048 "+
			"set at runtime", r)
	}
}

// An edit to the configuration since is the newer decision and wins.
func TestAnEditedConfigurationWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0.004, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.set(0.0048, 0.004); err != nil {
		t.Fatal(err)
	}

	again, err := openRatebook(dir, 0.005, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := again.current(); r != 0.005 {
		t.Errorf("rate %g, wanted the newly configured 0.005", r)
	}
}

// Past its age a rate refuses as stale, which a payer sees as
// price_unavailable rather than as a quote at last week's price.
func TestAStaleRateRefuses(t *testing.T) {
	t.Parallel()

	clock := time.Now()
	b, err := openRatebook(t.TempDir(), 0.004, time.Hour,
		func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(59 * time.Minute)
	if _, _, err := b.usable(); err != nil {
		t.Fatalf("inside its age the rate was refused: %v", err)
	}

	clock = clock.Add(2 * time.Minute)
	_, _, err = b.usable()
	if !errors.Is(err, rate.ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if quote.CodeOf(err) != quote.CodePriceUnavailable {
		t.Errorf("code %q", quote.CodeOf(err))
	}

	// Setting it again is the cure.
	if _, _, err := b.set(0.0041, 0.004); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.usable(); err != nil {
		t.Errorf("a freshly set rate was refused: %v", err)
	}
}

func TestANonsenseRateIsRefused(t *testing.T) {
	t.Parallel()

	b, err := openRatebook(t.TempDir(), 0.004, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []float64{0, -1} {
		if _, _, err := b.set(bad, 0.004); err == nil {
			t.Errorf("a rate of %g was accepted", bad)
		}
	}
	if r, _ := b.current(); r != 0.004 {
		t.Errorf("a refused rate changed the one in force to %g", r)
	}
}

// A file that cannot be read is not a reason to trade at a guess, nor to fall
// back to the configuration without saying so.
func TestAnUnreadableRateFileStopsTheBridge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, rateFileName)
	for _, body := range []string{"not json", `{"rate":0,"set_at":` +
		`"2026-10-02T00:00:00Z","configured":0.004}`} {

		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := openRatebook(dir, 0.004, time.Hour, nil); err == nil {
			t.Errorf("opened over %q", body)
		}
	}
}

// A rate that could not be recorded is not in force: a restart would bring the
// old one back without a word.
func TestARateThatCannotBeRecordedIsNotInForce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0.004, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.path = filepath.Join(dir, "missing", rateFileName)

	if _, _, err := b.set(0.009, 0.004); err == nil {
		t.Fatal("a rate that could not be saved was reported set")
	}
	if r, _ := b.current(); r != 0.004 {
		t.Errorf("rate %g is in force though it was not saved", r)
	}
}

// ---------------------------------------------------------------------------
// Who is asking.
// ---------------------------------------------------------------------------

// macaroonFor bakes a macaroon under a given root key id, in lnd's format, and
// returns a context carrying it as the RPC server would receive it.
func macaroonFor(t *testing.T, rootKeyID string) context.Context {
	t.Helper()

	id, err := proto.Marshal(&lnrpc.MacaroonId{
		Nonce: []byte("nonce"), StorageId: []byte(rootKeyID),
	})
	if err != nil {
		t.Fatal(err)
	}
	mac, err := macaroon.New([]byte("root key"),
		append([]byte{byte(bakery.LatestVersion)}, id...), "lnd",
		macaroon.LatestVersion)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mac.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("macaroon", hex.EncodeToString(raw)))
}

func TestParticipantIsTheMacaroonsRootKey(t *testing.T) {
	t.Parallel()

	if got := participantOf(context.Background()); got != "" {
		t.Errorf("no macaroon (a node without them) read as %q, "+
			"wanted the operator", got)
	}
	if got := participantOf(macaroonFor(t, "0")); got != "" {
		t.Errorf("the node's own root key read as %q, wanted the "+
			"operator", got)
	}
	if got := participantOf(macaroonFor(t, "17")); got != "17" {
		t.Errorf("participant %q, wanted 17", got)
	}

	// Unreadable is limited, not taken for the operator.
	garbage := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("macaroon", "zz"))
	if got := participantOf(garbage); got != unknownParticipant {
		t.Errorf("an unreadable macaroon read as %q", got)
	}
}

// ---------------------------------------------------------------------------
// Refusals reach the payer as codes and statuses it can act on.
// ---------------------------------------------------------------------------

func TestRefusalsCarryTheirCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		code codes.Code
		name quote.Code
	}{
		{quote.ErrTooLarge, codes.FailedPrecondition, quote.CodeTooLarge},
		{quote.ErrWrongChain, codes.FailedPrecondition,
			quote.CodeNoDirection},
		{quote.ErrAlreadyPaid, codes.AlreadyExists,
			quote.CodeAlreadyPaid},
		{quote.ErrInProgress, codes.AlreadyExists, quote.CodeInProgress},
		{quote.ErrLimit, codes.ResourceExhausted, quote.CodeLimit},
		{rate.ErrStale, codes.FailedPrecondition,
			quote.CodePriceUnavailable},
		{quote.ErrRefused, codes.Unavailable, quote.CodeUnavailable},
		{quote.ErrUntracked, codes.Internal, quote.CodeInternal},
		{errors.New("anything"), codes.Internal, quote.CodeInternal},
	}
	for _, c := range cases {
		err := refusal(c.err)
		if status.Code(err) != c.code {
			t.Errorf("%v: status %v, want %v", c.err, status.Code(err),
				c.code)
		}
		msg := status.Convert(err).Message()
		if !strings.HasPrefix(msg, string(c.name)+": ") {
			t.Errorf("%v: message %q should start with %q", c.err, msg,
				c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// Direction by chain, and the adapters that report it.
// ---------------------------------------------------------------------------

// TestRouteGoesByTheChainBit: both nodes read every invoice, so routing has to
// follow option_blake2b. This is the regression for routing by "first node
// that can decode it", which sent BLAKE2b invoices to the SHA256 side.
func TestRouteGoesByTheChainBit(t *testing.T) {
	t.Parallel()

	svc := sidedService(t, 10_000_000)

	for invoice, want := range map[string]string{
		"sideA-payme": "toSHA256", "sideB-payme": "toBLAKE2b",
	} {
		sd, dec, err := svc.route(context.Background(), invoice)
		if err != nil {
			t.Fatalf("%s: %v", invoice, err)
		}
		if sd.name != want {
			t.Errorf("%s went to %s, wanted %s", invoice, sd.name,
				want)
		}
		if dec.BLAKE2b != sd.paysBLAKE2b {
			t.Errorf("%s: a side was given an invoice for the "+
				"other chain", invoice)
		}
	}

	// With the direction that would pay it switched off, the reason is the
	// chain, which is what a payer is told.
	only := sidedService(t, 10_000_000)
	only.sides = only.sides[:1]
	_, _, err := only.route(context.Background(), "sideB-payme")
	if quote.CodeOf(err) != quote.CodeNoDirection {
		t.Errorf("code %q (%v), want no_direction", quote.CodeOf(err),
			err)
	}
	_, _, err = only.route(context.Background(), "garbage")
	if quote.CodeOf(err) != quote.CodeInvalidInvoice {
		t.Errorf("code %q (%v), want invalid_invoice", quote.CodeOf(err),
			err)
	}
}

func TestLocalDecodeReadsTheChainBit(t *testing.T) {
	t.Parallel()

	h := hash8(9)
	for name, c := range map[string]struct {
		bits []lnwire.FeatureBit
		want bool
	}{
		"bitcoin":  {nil, false},
		"required": {[]lnwire.FeatureBit{lnwire.Blake2bRequired}, true},
		"optional": {[]lnwire.FeatureBit{lnwire.Blake2bOptional}, true},
	} {
		t.Run(name, func(t *testing.T) {
			l := decodingAs(&zpay32.Invoice{
				PaymentHash: &h, MilliSat: msat(150_000),
				Timestamp: time.Now(),
				Features: lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(c.bits...),
					lnwire.Features,
				),
			}, nil)

			got, err := l.Decode(context.Background(), "lnbc1x")
			if err != nil {
				t.Fatal(err)
			}
			if got.BLAKE2b != c.want {
				t.Errorf("BLAKE2b %v, want %v", got.BLAKE2b, c.want)
			}
		})
	}
}

// A stock lnd lists bits it does not know, marked unknown. That is how the
// SHA256 node can tell an invoice is for the other chain.
func TestRemoteDecodeReadsTheChainBit(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		features map[uint32]*lnrpc.Feature
		want     bool
	}{
		"bitcoin":  {map[uint32]*lnrpc.Feature{9: {}}, false},
		"required": {map[uint32]*lnrpc.Feature{512: {}}, true},
		"optional": {map[uint32]*lnrpc.Feature{513: {}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			m := &fakeMain{payReq: &lnrpc.PayReq{
				PaymentHash: hex.EncodeToString(make([]byte, 32)),
				NumMsat:     1000, CltvExpiry: 40,
				Timestamp: time.Now().Unix(), Expiry: 3600,
				Features: c.features,
			}}
			got, err := remote(m, nil, nil).Decode(
				context.Background(), "lnbc1x",
			)
			if err != nil {
				t.Fatal(err)
			}
			if got.BLAKE2b != c.want {
				t.Errorf("BLAKE2b %v, want %v", got.BLAKE2b, c.want)
			}
		})
	}
}

// deletingMain records DeleteCanceledInvoice.
type deletingMain struct {
	fakeMain

	deleted []string
}

func (d *deletingMain) DeleteCanceledInvoice(_ context.Context,
	in *lnrpc.DelCanceledInvoiceReq, _ ...grpc.CallOption) (
	*lnrpc.DelCanceledInvoiceResp, error) {

	d.deleted = append(d.deleted, in.GetInvoiceHash())

	return &lnrpc.DelCanceledInvoiceResp{}, nil
}

// TestRemoteForgetsOnlyACancelledInvoice: deleting anything else would erase
// the record of an HTLC that is held or was paid.
func TestRemoteForgetsOnlyACancelledInvoice(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := node.Hash{7}

	for _, st := range []lnrpc.Invoice_InvoiceState{
		lnrpc.Invoice_OPEN, lnrpc.Invoice_ACCEPTED, lnrpc.Invoice_SETTLED,
	} {
		m := &deletingMain{}
		r := remote(&m.fakeMain, &fakeInvoices{
			lookup: &lnrpc.Invoice{State: st},
		}, nil)
		r.main = m
		if err := r.ForgetInvoice(ctx, h); err == nil {
			t.Errorf("%v was forgotten", st)
		}
		if len(m.deleted) != 0 {
			t.Errorf("%v: delete was called", st)
		}
	}

	m := &deletingMain{}
	r := remote(&m.fakeMain, &fakeInvoices{
		lookup: &lnrpc.Invoice{State: lnrpc.Invoice_CANCELED},
	}, nil)
	r.main = m
	if err := r.ForgetInvoice(ctx, h); err != nil {
		t.Fatal(err)
	}
	if len(m.deleted) != 1 || m.deleted[0] != hex.EncodeToString(h[:]) {
		t.Errorf("deleted %v", m.deleted)
	}

	// A hash the node never saw is already forgotten.
	unknown := remote(nil, &fakeInvoices{
		lookupErr: status.Error(codes.NotFound, "no invoice"),
	}, nil)
	if err := unknown.ForgetInvoice(ctx, h); err != nil {
		t.Errorf("an unknown hash was not counted as forgotten: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Info and SetRate, through the server.
// ---------------------------------------------------------------------------

// measuredServer is a running bridge whose chains can be measured and whose
// sides hold balance, so Info can answer as it would in service.
func measuredServer(t *testing.T, balance uint64) (*Server, *service) {
	t.Helper()

	const spacing = 10 * time.Minute

	now := time.Now()
	f := &fakeNode{synced: true, height: 800_000, blockTime: now,
		balance: balance}
	deps := f.deps()
	deps.BlockAt = func(_ context.Context, h int32) (BlockInfo, error) {
		back := time.Duration(800_000-h) * spacing

		return BlockInfo{
			Height: h, Time: now.Add(-back), SyncedToChain: true,
		}, nil
	}
	main := &fakeMain{info: &lnrpc.GetInfoResponse{
		BlockHeight: 700_000, SyncedToChain: true,
		BestHeaderTimestamp: now.Unix(),
		IdentityPubkey:      "03" + strings.Repeat("22", 32),
	}}
	chain := &fakeChain{at: func(h int32) (int64, error) {
		back := time.Duration(700_000-h) * spacing

		return now.Add(-back).Unix(), nil
	}}

	cfg := usable()
	cfg.Deps = deps
	svc := serviceFor(t, cfg, f, remoteWithChain(main, chain))
	svc.local = NewLocal(deps)
	svc.positionOf = func(context.Context) (uint64, error) {
		return balance, nil
	}
	for _, sd := range svc.sides {
		sd.balance = svc.positionOf
	}
	svc.backfill(context.Background())

	srv := &Server{cfg: &cfg, local: svc.local, svc: svc}

	return srv, svc
}

func TestInfoReportsAPriceWithoutQuoting(t *testing.T) {
	t.Parallel()

	srv, svc := measuredServer(t, 100_000_000_000)

	resp, err := srv.Info(context.Background(), &InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Version != PayerAPIVersion {
		t.Errorf("version %d", resp.Version)
	}
	if resp.Node != svc.local.NodeKey() || resp.Node == "" {
		t.Errorf("node %q, want this node's key", resp.Node)
	}
	if len(resp.Directions) != len(svc.sides) {
		t.Fatalf("%d directions, want %d", len(resp.Directions),
			len(svc.sides))
	}
	for _, d := range resp.Directions {
		if !d.Open {
			t.Errorf("%s closed: %s %s", d.Name, d.RefusalCode,
				d.Refusal)
		}
		if d.Rate <= 0 || d.Spread <= 0 {
			t.Errorf("%s: rate %g spread %g", d.Name, d.Rate, d.Spread)
		}
		if d.MinMsat == 0 || d.MaxMsat < d.MinMsat {
			t.Errorf("%s: bounds %d..%d", d.Name, d.MinMsat, d.MaxMsat)
		}
	}

	// Nothing was created or recorded.
	pending, err := svc.journal.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("Info recorded %d swaps", len(pending))
	}
}

// A direction that could not quote says why, with the code a quote would
// have failed with.
func TestInfoNamesWhyADirectionIsShut(t *testing.T) {
	t.Parallel()

	srv, svc := measuredServer(t, 1)
	resp, err := srv.Info(context.Background(), &InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Directions {
		if d.Open {
			t.Errorf("%s open with 1 msat to pay with", d.Name)
		}
		if d.RefusalCode != string(quote.CodeNoLiquidity) {
			t.Errorf("%s: code %q (%s)", d.Name, d.RefusalCode,
				d.Refusal)
		}
	}

	// A stale rate shuts every direction with the price code.
	srv, svc = measuredServer(t, 100_000_000_000)
	svc.rates.maxAge = time.Nanosecond
	time.Sleep(time.Millisecond)
	resp, err = srv.Info(context.Background(), &InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Directions {
		if d.RefusalCode != string(quote.CodePriceUnavailable) {
			t.Errorf("%s with a stale rate: code %q", d.Name,
				d.RefusalCode)
		}
	}
}

func TestSetRateChangesWhatInfoReports(t *testing.T) {
	t.Parallel()

	srv, _ := measuredServer(t, 100_000_000_000)
	ctx := context.Background()

	set, err := srv.SetRate(ctx, &SetRateRequest{Rate: 0.00483})
	if err != nil {
		t.Fatal(err)
	}
	if set.Rate != 0.00483 || set.RateSetAt == 0 {
		t.Errorf("set %+v", set)
	}

	resp, err := srv.Info(ctx, &InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Directions {
		want := 0.00483
		if d.Name == "toBLAKE2b" {
			want = 1 / 0.00483
		}
		if d.Rate != want {
			t.Errorf("%s rate %g, want %g", d.Name, d.Rate, want)
		}
		if d.RateSetAt != set.RateSetAt {
			t.Errorf("%s set at %d, want %d", d.Name, d.RateSetAt,
				set.RateSetAt)
		}
	}

	st, err := srv.Status(ctx, &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Rate != 0.00483 || st.RateExpiresAt == 0 {
		t.Errorf("status rate %g expires %d", st.Rate, st.RateExpiresAt)
	}

	if _, err := srv.SetRate(ctx, &SetRateRequest{Rate: -1}); status.Code(
		err) != codes.InvalidArgument {

		t.Errorf("a negative rate: %v", err)
	}
}

// SetRate changes what every later swap costs, so it needs write, and the
// payer calls need only what they do.
func TestThePayerCallsAndSetRateHaveTheRightPermissions(t *testing.T) {
	t.Parallel()

	for uri, action := range map[string]string{
		"/bridgerpc.Bridge/Info":       "read",
		"/bridgerpc.Bridge/LookupSwap": "read",
		"/bridgerpc.Bridge/Quote":      "write",
		"/bridgerpc.Bridge/SetRate":    "write",
	} {
		ops := macPermissions[uri]
		if len(ops) != 1 || ops[0].Entity != "offchain" ||
			ops[0].Action != action {

			t.Errorf("%s requires %+v, want offchain:%s", uri, ops,
				action)
		}
	}
}

// Info and SetRate refuse with the disabled code while the bridge is off.
func TestInfoAndSetRateWhileDisabled(t *testing.T) {
	t.Parallel()

	s := serverWith(t, false, &fakeNode{synced: true})
	ctx := context.Background()

	_, err := s.Info(ctx, &InfoRequest{})
	if !strings.HasPrefix(status.Convert(err).Message(), "disabled: ") {
		t.Errorf("Info: %v", err)
	}
	_, err = s.SetRate(ctx, &SetRateRequest{Rate: 1})
	if !strings.HasPrefix(status.Convert(err).Message(), "disabled: ") {
		t.Errorf("SetRate: %v", err)
	}
}

// TestTheSHA256NodeIsCheckedForWhatItIs: both chains run lnd and report the
// same chain and network names, so a misconfigured address pointing at another
// Lightning Fork node would answer every call. option_blake2b is what gives it
// away, and the network and identity are checked as well.
func TestTheSHA256NodeIsCheckedForWhatItIs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	chains := []*lnrpc.Chain{{Chain: "bitcoin", Network: "mainnet"}}
	stock := &lnrpc.GetInfoResponse{
		IdentityPubkey: "03aa", Chains: chains,
		Features: map[uint32]*lnrpc.Feature{9: {}, 15: {}},
	}

	if err := remote(&fakeMain{info: stock}, nil, nil).CheckChain(ctx,
		"mainnet", "02bb"); err != nil {

		t.Fatalf("a stock lnd on the same network was refused: %v", err)
	}

	fork := proto.Clone(stock).(*lnrpc.GetInfoResponse)
	fork.Features[512] = &lnrpc.Feature{Name: "blake2b"}
	err := remote(&fakeMain{info: fork}, nil, nil).CheckChain(ctx, "mainnet",
		"02bb")
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(),
		"option_blake2b") {

		t.Errorf("a Lightning Fork node was accepted as the SHA256 one: %v",
			err)
	}

	if err := remote(&fakeMain{info: stock}, nil, nil).CheckChain(ctx,
		"regtest", "02bb"); !errors.Is(err, ErrConfig) {

		t.Errorf("a node on another network was accepted: %v", err)
	}

	if err := remote(&fakeMain{info: stock}, nil, nil).CheckChain(ctx,
		"mainnet", "03AA"); !errors.Is(err, ErrConfig) {

		t.Errorf("this node itself was accepted as the SHA256 one: %v", err)
	}
}

// TestABridgeThatCannotStartDoesNotStopTheNode: lnd aborts its whole start
// when a sub-server's Start fails, so a SHA256 node that is down or
// misconfigured must leave the bridge refusing and retrying, never the
// operator's own node offline.
func TestABridgeThatCannotStartDoesNotStopTheNode(t *testing.T) {
	t.Parallel()

	srv, _, err := New(&Config{
		Enabled: true, ToSHA256: true, FixedRate: 0.004,
		SHA256RPCHost:      "127.0.0.1:1",
		SHA256MacaroonPath: filepath.Join(t.TempDir(), "missing.macaroon"),
		Deps:               (&fakeNode{synced: true}).deps(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start failed, which would stop the node: %v", err)
	}

	st, err := srv.Status(context.Background(), &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var said bool
	for _, r := range st.Refusals {
		if strings.Contains(r, "cannot start") &&
			strings.Contains(r, "tries again") {

			said = true
		}
	}
	if !said {
		t.Errorf("Status does not say why the bridge is down: %v",
			st.Refusals)
	}
	if _, err := srv.Quote(context.Background(),
		&QuoteRequest{Invoice: "lnbc1x"}); err == nil {

		t.Error("a bridge that is not up quoted")
	}

	done := make(chan struct{})
	go func() {
		_ = srv.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not end the retry loop")
	}
}

// TestStatusNamesSwapsThatNeedTheOperator: a lost swap is terminal and the
// runner stops on one it must not decide alone; both are for a person, and a
// log line is not where a person looks.
func TestStatusNamesSwapsThatNeedTheOperator(t *testing.T) {
	t.Parallel()

	srv, svc := measuredServer(t, 100_000_000_000)
	ctx := context.Background()
	now := time.Now()

	err := svc.journal.Put(ctx, store.Record{
		Hash: [32]byte{0xaa}, State: swap.Lost, OutgoingCLTVLimit: 40,
		Invoice: "lnbc1lost", IncomingMsat: 1, OutgoingMsat: 1,
		Created: now, Updated: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.markStopped([32]byte{0xbb}, "the outgoing payment has no record")

	st, err := srv.Status(ctx, &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.NeedsOperator) != 2 {
		t.Fatalf("needs operator %v, wanted the lost and the stopped swap",
			st.NeedsOperator)
	}
	joined := strings.Join(st.NeedsOperator, "\n")
	if !strings.Contains(joined, "aa00") || !strings.Contains(joined,
		"lost") || !strings.Contains(joined, "bb00") {

		t.Errorf("needs operator %v", st.NeedsOperator)
	}
}
