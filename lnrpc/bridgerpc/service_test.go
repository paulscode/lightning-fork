//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/inventory"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

// serviceFor builds a service over a fake local node, with the journal in a
// temporary directory. The SHA256 side is a Remote over fakes.
func serviceFor(t *testing.T, cfg Config, f *fakeNode, r *Remote) *service {
	t.Helper()

	cfg.Journal = filepath.Join(t.TempDir(), "swaps.journal")

	svc, err := newService(&cfg, NewLocal(f.deps()), r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.close)

	return svc
}

// Both directions must be wired the right way round. Getting this backwards
// would have each swap priced against the chain it is not on, and would spend
// the wrong side's liquidity.
func TestDirectionsAreWiredTheRightWayRound(t *testing.T) {
	t.Parallel()

	cfg := usable()
	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))

	if len(svc.sides) != 2 {
		t.Fatalf("wired %d directions, wanted 2", len(svc.sides))
	}

	byName := map[string]*side{}
	for _, sd := range svc.sides {
		byName[sd.name] = sd
	}

	toSHA256, ok := byName["toSHA256"]
	if !ok {
		t.Fatal("toSHA256 was not wired")
	}
	if toSHA256.invert {
		t.Error("toSHA256 should quote the posted rate directly")
	}
	if toSHA256.dir != inventory.Draining {
		t.Error("toSHA256 spends the SHA256 side, so it drains")
	}

	toBLAKE2b, ok := byName["toBLAKE2b"]
	if !ok {
		t.Fatal("toBLAKE2b was not wired")
	}
	if !toBLAKE2b.invert {
		t.Error("toBLAKE2b pays in BLAKE2b coin, so it quotes the reciprocal")
	}
	if toBLAKE2b.dir != inventory.Replenishing {
		t.Error("toBLAKE2b puts back what the other direction spends")
	}
}

// A direction that is configured off must be named, so an operator is sent to
// the line they changed rather than to their nodes.
func TestADisabledDirectionIsNamed(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.ToBLAKE2b = false

	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))

	if len(svc.sides) != 1 {
		t.Fatalf("wired %d directions, wanted 1", len(svc.sides))
	}

	_, _, err := svc.route(context.Background(), "lnbcrt1payme")
	if err == nil {
		t.Fatal("routing should fail when no node can decode")
	}
	if !strings.Contains(err.Error(), "toBLAKE2b") {
		t.Errorf("the refusal should name the disabled direction: %v",
			err)
	}
}

// The rate is posted one way round. Quoting the same number in both directions
// would price one of them upside down.
func TestTheReciprocalDirectionInvertsTheRate(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.FixedRate = 0.004

	f := &fakeNode{synced: true, balance: 100_000_000_000}
	svc := serviceFor(t, cfg, f, remote(nil, nil, nil))

	var forward, reverse *side
	for _, sd := range svc.sides {
		if sd.invert {
			reverse = sd
		} else {
			forward = sd
		}
	}

	// Both price against the local fake's balance so the inventory half is
	// the same and only the inversion differs.
	forward.balance = f.deps().ChannelBalance
	reverse.balance = f.deps().ChannelBalance

	fwd, err := svc.pricer(forward)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rev, err := svc.pricer(reverse)(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if fwd.OutgoingPerIncoming != 0.004 {
		t.Errorf("forward rate %g, wanted the posted 0.004",
			fwd.OutgoingPerIncoming)
	}
	if want := 1 / 0.004; rev.OutgoingPerIncoming != want {
		t.Errorf("reverse rate %g, wanted %g",
			rev.OutgoingPerIncoming, want)
	}
}

// The spread charged is the larger of what the operator posted and what the
// inventory policy asks for. With a fixed rate there is no volatility to widen
// for, so the inventory half is the only thing that can.
func TestADrainedSideIsQuotedWider(t *testing.T) {
	t.Parallel()

	cfg := usable()

	full := &fakeNode{
		synced: true, balance: inventory.DefaultPolicy.TargetOutgoingMsat,
	}
	drained := &fakeNode{
		synced:  true,
		balance: inventory.DefaultPolicy.FloorOutgoingMsat + 1,
	}

	spreadWith := func(f *fakeNode) float64 {
		t.Helper()

		svc := serviceFor(t, cfg, f, remote(nil, nil, nil))
		for _, sd := range svc.sides {
			if sd.invert {
				continue
			}
			sd.balance = f.deps().ChannelBalance

			r, err := svc.pricer(sd)(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			return r.Spread
		}
		t.Fatal("no forward direction")

		return 0
	}

	wide, narrow := spreadWith(drained), spreadWith(full)
	if wide <= narrow {
		t.Errorf("a drained side was quoted at %g and a full one at "+
			"%g; the drained one should be wider", wide, narrow)
	}
	if narrow < cfg.Spread {
		t.Errorf("a full side was quoted at %g, under the posted %g",
			narrow, cfg.Spread)
	}
}

// Both chains have to be measurable before anything can be sized, and the
// refusal has to say which one is missing rather than failing anonymously.
func TestSpacingRefusesUntilBothChainsAreMeasured(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	if _, err := svc.spacing(false)(context.Background()); err == nil {
		t.Fatal("spacing should refuse with no blocks observed")
	}

	// Feed one chain only. The refusal must now name the other.
	now := time.Now()
	for i := range 200 {
		svc.b2bChain.Add(chainrate.Block{
			Height: int32(800_000 + i),
			Time:   now.Add(time.Duration(i) * 10 * time.Minute),
		})
	}

	_, err := svc.spacing(false)(context.Background())
	if err == nil {
		t.Fatal("spacing should still refuse with one chain missing")
	}
	if !strings.Contains(err.Error(), "SHA256") {
		t.Errorf("the refusal should name the missing chain: %v", err)
	}
}

// Headroom must subtract what is already promised to swaps that have not
// finished. Quoting against the raw balance would promise the same funds
// twice.
func TestHeadroomSubtractsWhatIsAlreadyCommitted(t *testing.T) {
	t.Parallel()

	f := &fakeNode{synced: true, balance: 10_000_000}
	svc := serviceFor(t, usable(), f, remote(nil, nil, nil))

	sd := svc.sides[0]
	sd.balance = f.deps().ChannelBalance

	ctx := context.Background()

	got, err := svc.headroom(sd)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10_000_000 {
		t.Errorf("with nothing committed, headroom is %d, wanted the "+
			"whole balance", got)
	}

	// A swap that has been quoted but not paid is already spoken for: the
	// payer can fund it at any moment, and between "they might" and "they
	// did" there is no chance to re-decide.
	now := time.Now()
	if err := svc.journal.Put(ctx, store.Record{
		Hash: [32]byte{1}, State: swap.Quoted, OutgoingCLTVLimit: 40,
		Invoice: "lnbcrt1payme", IncomingMsat: 4_000_000,
		OutgoingMsat: 4_000_000, Rate: 0.003, Spread: 0.01,
		Created: now, Updated: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err = svc.headroom(sd)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 6_000_000 {
		t.Errorf("headroom is %d with 4,000,000 msat committed "+
			"against a 10,000,000 msat balance, wanted 6,000,000: "+
			"quoting against the raw balance promises the same "+
			"funds twice", got)
	}

	// Committed past the balance is no headroom, not a negative one that
	// would wrap into an enormous allowance.
	if err := svc.journal.Put(ctx, store.Record{
		Hash: [32]byte{2}, State: swap.Funded, OutgoingCLTVLimit: 40,
		Invoice: "lnbcrt1payme2", IncomingMsat: 20_000_000,
		OutgoingMsat: 20_000_000, Rate: 0.003, Spread: 0.01,
		Created: now, Updated: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err = svc.headroom(sd)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("headroom is %d when more is committed than held, "+
			"wanted 0", got)
	}
}

// A balance the paying node cannot report is not zero headroom, it is an
// unknown one, and quoting against a guess would commit money on no grounds.
func TestHeadroomFailsRatherThanGuessing(t *testing.T) {
	t.Parallel()

	f := &fakeNode{synced: true}
	f.balanceErr = context.DeadlineExceeded

	svc := serviceFor(t, usable(), f, remote(nil, nil, nil))
	sd := svc.sides[0]
	sd.balance = f.deps().ChannelBalance

	if _, err := svc.headroom(sd)(context.Background()); err == nil {
		t.Fatal("an unreadable balance should refuse, not read as zero")
	}
}
