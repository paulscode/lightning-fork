//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/driver"
	"github.com/paulscode/lightning-fork-bridge/inventory"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/rate"
	"github.com/paulscode/lightning-fork-bridge/runner"
	"github.com/paulscode/lightning-fork-bridge/store"
)

// ErrNoDirection is returned when no enabled direction can pay an invoice.
var ErrNoDirection = errors.New("no enabled direction can pay this invoice")

// side is one direction of the bridge, fully wired.
type side struct {
	name string

	// in receives, out pays. Which node is which is the direction.
	in  node.Incoming
	out node.Outgoing

	// balance reports what the paying node can still send. That is the
	// side the bridge's own money leaves from, and so the side the spread
	// is priced against.
	balance func(context.Context) (uint64, error)

	quoter *quote.Quoter
	runner *runner.Runner

	// dir is what this direction does to the position, which is what the
	// inventory policy prices.
	dir inventory.Direction

	// invert is set on the direction whose rate is the reciprocal of the
	// configured one. The rate is posted as BTC per BTCB2, so the
	// direction paying out in BTCB2 uses one over it.
	invert bool
}

// service is the bridge, running inside the node.
//
// It mirrors what the standalone daemon does, with two differences that follow
// from living here: the local node is reached in process rather than dialled,
// and there is no HTTP surface because the sub-server is the surface.
type service struct {
	cfg *Config
	res resolved

	local  *Local
	remote *Remote

	journal *store.Journal

	// sides holds whichever directions are enabled, in the order a quote
	// request tries them.
	sides []*side

	// disabled names directions that are configured off, so an invoice for
	// one can be refused by name. "No enabled direction can pay this"
	// sends an operator looking at their nodes; "toBlake2b is configured
	// but not enabled" sends them to the line they changed.
	disabled []string

	// lfChain and btcChain measure how fast each chain is running, which
	// is what turns a wall-clock safety margin into a number of blocks.
	// Guarded because the poller writes them and quotes read them.
	chainMu  sync.Mutex
	lfChain  *chainrate.Observer
	btcChain *chainrate.Observer

	// ctx is the bridge's own lifetime, which a swap started by an RPC
	// must outlive: the caller may hang up the moment after they pay, and
	// the HTLC does not go away with their connection.
	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup
}

// newService wires everything. It does not start the pollers: start does that,
// so a caller can build a service to check a configuration without it
// beginning to quote.
func newService(cfg *Config, local *Local, remote *Remote) (*service, error) {
	s := &service{
		cfg: cfg, res: cfg.resolve(), local: local, remote: remote,
	}

	var err error
	if s.journal, err = store.Open(cfg.Journal); err != nil {
		return nil, fmt.Errorf("opening the swap journal %s: %w",
			cfg.Journal, err)
	}
	if s.lfChain, err = chainrate.New(s.res.lfChain); err != nil {
		s.close()

		return nil, fmt.Errorf("the BLAKE2b chain observer: %w", err)
	}
	if s.btcChain, err = chainrate.New(s.res.btcChain); err != nil {
		s.close()

		return nil, fmt.Errorf("the Bitcoin chain observer: %w", err)
	}

	// toBitcoin receives here and pays on Bitcoin, so it drains the
	// Bitcoin side. toBlake2b puts back what the other one spends.
	if cfg.ToBitcoin {
		s.sides = append(s.sides, s.build(
			"toBitcoin", local, remote, remote.Balance,
			inventory.Draining, false,
		))
	} else {
		s.disabled = append(s.disabled, "toBitcoin")
	}
	if cfg.ToBlake2b {
		// The rate is posted as BTC per BTCB2, so the direction that
		// pays out in BTCB2 quotes its reciprocal.
		s.sides = append(s.sides, s.build(
			"toBlake2b", remote, local, local.Balance,
			inventory.Replenishing, true,
		))
	} else {
		s.disabled = append(s.disabled, "toBlake2b")
	}

	return s, nil
}

// build wires one direction. in receives and out pays.
func (s *service) build(name string, in node.Incoming, out node.Outgoing,
	balance func(context.Context) (uint64, error),
	dir inventory.Direction, invert bool) *side {

	sd := &side{
		name: name, in: in, out: out, balance: balance, dir: dir,
		invert: invert,
	}

	sd.quoter = &quote.Quoter{
		In: sd.in, Out: sd.out, Store: s.journal,
		Price:   s.pricer(sd),
		Spacing: quote.Rates(s.spacing(invert)),
		Room:    s.headroom(sd),
		Margin:  s.res.margin,
		Policy:  s.res.quote,
	}
	sd.runner = &runner.Runner{
		Driver: &driver.Driver{
			In: sd.in, Out: sd.out, Store: s.journal,
			Policy: s.res.margin,
		},
		Store: s.journal,
		Rates: runner.Rates(s.spacing(invert)),
		Log:   bridgeLogger(name),
	}

	return sd
}

// pricer combines the posted rate with what the paying side is holding.
//
// The spread charged is the larger of the operator's posted one and what the
// inventory policy asks for given how drained the paying side is. Both are
// reasons to charge more; taking the larger charges enough for whichever risk
// is bigger, and taking the posted one alone would underprice a nearly empty
// side.
//
// With a fixed rate there is no volatility measurement to widen for, which is
// why the inventory half matters more here than it does with a live feed.
func (s *service) pricer(sd *side) quote.Pricer {
	return func(ctx context.Context) (rate.Reading, error) {
		r := rate.Reading{
			OutgoingPerIncoming: s.cfg.FixedRate,
			At:                  time.Now(),

			// One source, and it is the operator. Worth carrying
			// honestly rather than inflating: nothing
			// cross-checked this number.
			Sources: 1,
			Spread:  s.res.rate.BaseSpread,
		}

		if sd.invert {
			if r.OutgoingPerIncoming <= 0 {
				return rate.Reading{}, fmt.Errorf("%w: cannot "+
					"invert a rate of %g", quote.ErrRefused,
					r.OutgoingPerIncoming)
			}
			r.OutgoingPerIncoming = 1 / r.OutgoingPerIncoming
		}

		held, err := sd.balance(ctx)
		if err != nil {
			return rate.Reading{}, err
		}

		pos, err := s.res.inventory.Spread(inventory.State{
			OutgoingMsat: held, At: time.Now(),
		}, sd.dir, time.Now())
		if err != nil {
			return rate.Reading{}, err
		}
		if pos.Spread > r.Spread {
			r.Spread = pos.Spread
		}

		return r, nil
	}
}

// spacing reports both chains' current block rates, incoming first.
//
// invert names which chain is which: the toBitcoin direction receives on
// BLAKE2b, and toBlake2b receives on Bitcoin.
func (s *service) spacing(invert bool) func(context.Context) (driver.Rates,
	error) {

	return func(context.Context) (driver.Rates, error) {
		s.chainMu.Lock()
		defer s.chainMu.Unlock()

		now := time.Now()
		lf, err := s.lfChain.Estimate(now)
		if err != nil {
			return driver.Rates{}, fmt.Errorf("the BLAKE2b "+
				"chain's spacing: %w", err)
		}
		btc, err := s.btcChain.Estimate(now)
		if err != nil {
			return driver.Rates{}, fmt.Errorf("the Bitcoin "+
				"chain's spacing: %w", err)
		}

		in, out := lf.Bounds, btc.Bounds
		if invert {
			in, out = btc.Bounds, lf.Bounds
		}

		return driver.Rates{
			Incoming: in, Outgoing: out, As: time.Now(),
		}, nil
	}
}

// headroom is how much the bridge may still commit on the paying side.
//
// The balance alone is not the answer: what has already been promised to swaps
// that have not finished is still on the node's books but is spoken for, and
// quoting against it would promise the same funds twice. store.Committed is
// that second part, and it is the one that is easy to forget.
func (s *service) headroom(sd *side) quote.Headroom {
	return func(ctx context.Context) (uint64, error) {
		held, err := sd.balance(ctx)
		if err != nil {
			return 0, err
		}

		promised, err := store.Committed(ctx, s.journal)
		if err != nil {
			return 0, fmt.Errorf("reading what is already "+
				"committed: %w", err)
		}
		if promised >= held {
			return 0, nil
		}

		return held - promised, nil
	}
}

// close releases what newService opened. Safe to call twice.
func (s *service) close() {
	if s.journal != nil {
		_ = s.journal.Close()
		s.journal = nil
	}
}
