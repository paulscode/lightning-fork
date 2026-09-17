//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/paulscode/lightning-fork-bridge/swap"
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

	// mu guards the inventory policy below.
	//
	// The poll loop derives it from a real balance while a quote is
	// pricing against it, so this is two goroutines on one struct. Without
	// the lock a quote can read a new target beside an old floor, which is
	// not a policy anyone chose, and the number it produces prices real
	// money.
	mu sync.RWMutex

	// inventory prices how drained this side is. It is per side because
	// the two sides hold different amounts, on chains whose units are not
	// the same: one policy for both would price one of them against the
	// other's balance.
	//
	// Read through policy and written through setPolicy. Nothing touches
	// it directly.
	inventory inventory.Policy

	// sized is set once the inventory bounds have been derived from a
	// real balance, so it is done once rather than tracking the balance.
	sized bool
}

// policy is this side's inventory policy, copied under the lock.
//
// A copy rather than a pointer: the caller prices against a whole policy, and
// one that changed halfway through would mix two.
func (sd *side) policy() inventory.Policy {
	sd.mu.RLock()
	defer sd.mu.RUnlock()

	return sd.inventory
}

// setPolicy replaces the inventory policy and marks the side sized.
func (sd *side) setPolicy(p inventory.Policy) {
	sd.mu.Lock()
	defer sd.mu.Unlock()

	sd.inventory = p
	sd.sized = true
}

// isSized reports whether the bounds have been derived from a real balance.
func (sd *side) isSized() bool {
	sd.mu.RLock()
	defer sd.mu.RUnlock()

	return sd.sized
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

	// sideOf remembers which direction each swap belongs to, so that what
	// is already committed can be counted per side.
	//
	// It is not in the journal, so it is rebuilt by asking the paying node
	// to decode the invoice, and cached because that is a round trip and
	// this is asked on every quote.
	sideMu sync.Mutex
	sideOf map[node.Hash]string

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

	// The journal's directory is created rather than required. It defaults
	// to a subdirectory of the network directory that nothing else makes,
	// so on a fresh node the first start would otherwise fail on a
	// directory the operator never asked for and cannot be expected to
	// know about.
	if dir := filepath.Dir(cfg.Journal); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("creating the swap journal's "+
				"directory %s: %w", dir, err)
		}
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
		invert: invert, inventory: s.res.inventory,
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

		pos, err := sd.policy().Spread(inventory.State{
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
// quoting against it would promise the same funds twice.
//
// What is promised has to be counted per direction. The journal holds both,
// and the two chains' millisatoshis are not the same unit, so summing all of
// it and subtracting from one side's balance compares quantities that do not
// mean the same thing. It is wrong in both directions and unsafe in one: the
// chain whose unit is numerically larger has its commitments under-counted,
// and the bridge promises more of it than it has left.
func (s *service) headroom(sd *side) quote.Headroom {
	return func(ctx context.Context) (uint64, error) {
		held, err := sd.balance(ctx)
		if err != nil {
			return 0, err
		}

		promised, err := s.committedOn(ctx, sd)
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

// committedOn is what this direction has already promised to pay out, in the
// units of the chain it pays on.
//
// Only the states store.Committed counts, and for the same reason: a swap
// merely quoted has no HTLC yet, but a payer can fund it at any moment, and
// between "they might" and "they did" there is no chance to re-decide.
func (s *service) committedOn(ctx context.Context, sd *side) (uint64, error) {
	pending, err := s.journal.Pending(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading pending swaps: %w", err)
	}

	var total uint64
	for _, rec := range pending {
		switch rec.State {
		case swap.Quoted, swap.Offered, swap.Funded, swap.Paying:
		default:
			continue
		}

		name, err := s.sideNameOf(ctx, rec.Hash, rec.Invoice)
		if err != nil {
			// A swap that cannot be attributed is counted against
			// every side. Leaving it out of the one it belongs to
			// would let the bridge promise those funds twice, and
			// over-counting only refuses swaps it might have
			// served.
			total += rec.OutgoingMsat

			continue
		}
		if name != sd.name {
			continue
		}

		// Saturating, because an overflow here would report a small
		// commitment for an enormous one, in the direction that
		// oversells.
		if total+rec.OutgoingMsat < total {
			return 0, errors.New("committed amounts overflow")
		}
		total += rec.OutgoingMsat
	}

	return total, nil
}

// sideNameOf is which direction a swap belongs to, remembered once.
func (s *service) sideNameOf(ctx context.Context, hash node.Hash,
	invoice string) (string, error) {

	s.sideMu.Lock()
	name, ok := s.sideOf[hash]
	s.sideMu.Unlock()

	if ok {
		return name, nil
	}

	sd, _, err := s.route(ctx, invoice)
	if err != nil {
		return "", err
	}
	s.remember(hash, sd)

	return sd.name, nil
}

// remember records which direction a swap belongs to.
func (s *service) remember(hash node.Hash, sd *side) {
	s.sideMu.Lock()
	defer s.sideMu.Unlock()

	if s.sideOf == nil {
		s.sideOf = make(map[node.Hash]string)
	}
	s.sideOf[hash] = sd.name
}

// close releases what newService opened. Safe to call twice.
func (s *service) close() {
	if s.journal != nil {
		_ = s.journal.Close()
		s.journal = nil
	}
}

// sizeInventory derives each side's working balance from what it actually
// holds, unless the operator named one.
//
// The shipped default is a mainnet-sized half a bitcoin with a tenth of it
// held back. On a node holding less than that floor the bridge refuses every
// swap, and says only that it is below a number the operator never chose,
// which is the least useful way to be right. What the side holds now is a far
// better estimate of what it will hold, and an operator who knows better can
// still say so.
//
// Run once at startup rather than per quote: the target is what the position is
// priced against, and one that moved with the balance would price a drained
// side as though it were full.
func (s *service) sizeInventory(ctx context.Context) {
	if s.cfg.InventoryTargetMsat != 0 {
		return
	}

	for _, sd := range s.sides {
		if sd.isSized() {
			continue
		}

		held, err := sd.balance(ctx)
		if err != nil {
			log.Warnf("Bridge could not read the %s paying "+
				"balance to size its inventory, so it keeps "+
				"the default: %v", sd.name, err)

			continue
		}
		// A balance too small to fund one swap is not a working
		// balance, and sizing against it would call the side fully
		// stocked while it can pay nothing, quoting at the base spread
		// on a position that deserves the widest. Below this the
		// default stands and the read is retried, because the operator
		// may be about to fund it.
		//
		// Retrying also covers the case that made this necessary: a
		// peer whose link was not up at startup reads as zero, and
		// treating that as the answer for the life of the process
		// would refuse the direction thereafter.
		if held < s.res.quote.MinSwapMsat {
			log.Debugf("Bridge cannot pay a swap on %s yet (%d "+
				"msat against a %d msat minimum), so it will "+
				"refuse that direction until the paying node "+
				"has outbound capacity", sd.name, held,
				s.res.quote.MinSwapMsat)

			continue
		}

		// Built whole and installed in one write, so a quote pricing
		// concurrently sees either the old policy or the new one and
		// never a mixture of the two.
		sized := sd.policy()
		sized.TargetOutgoingMsat = held
		sized.FloorOutgoingMsat = held / DefaultFloorFraction
		if s.cfg.InventoryFloorMsat != 0 {
			sized.FloorOutgoingMsat = s.cfg.InventoryFloorMsat
		}

		// A policy derived from a balance still has to be one the
		// package will take: the discount and rebalance cost were
		// scaled against the operator's spread, and the floor has to
		// stay below the target.
		if err := sized.Valid(); err != nil {
			log.Warnf("Bridge could not size %s inventory from a "+
				"balance of %d msat, so it keeps the default: "+
				"%v", sd.name, held, err)

			continue
		}

		sd.setPolicy(sized)

		log.Infof("Bridge sized %s against %d msat of paying "+
			"balance, holding back %d msat for swaps in flight",
			sd.name, sized.TargetOutgoingMsat,
			sized.FloorOutgoingMsat)
	}
}
