//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
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
	// configured one. The rate is posted as SHA256 coin per BLAKE2b coin, so the
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

	// paysBLAKE2b is whether this direction pays out on the BLAKE2b chain,
	// which is how an invoice is matched to its direction.
	paysBLAKE2b bool

	// finishing is set on a direction configured off that was built only
	// to finish the swaps of its own still in the journal. It quotes
	// nothing and is not offered.
	finishing bool
}

// quoting is whether this direction takes new swaps.
func (sd *side) quoting() bool {
	return !sd.finishing
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

	// rates is the rate in force, which the operator can change while the
	// bridge runs.
	rates *ratebook

	// own is both nodes' identity keys, so neither can be paid through the
	// bridge.
	own []string

	// positionOf reads what the bridge holds on the SHA256 chain, which is
	// what both directions are priced against. A field so it can be
	// replaced in tests, like each side's balance.
	positionOf func(context.Context) (uint64, error)

	// stopped holds swaps the runner stopped driving because the bridge
	// must not decide them alone, by hash, with why. In memory: a restart
	// drives them again, and if the reason stands it is found again.
	stoppedMu sync.Mutex
	stopped   map[node.Hash]string

	// sides holds whichever directions are enabled, in the order a quote
	// request tries them.
	sides []*side

	// disabled names directions that are configured off, so an invoice for
	// one can be refused by name. "No enabled direction can pay this"
	// sends an operator looking at their nodes; "toBLAKE2b is configured
	// but not enabled" sends them to the line they changed.
	disabled []string

	// heldForRate is set when toBLAKE2b is enabled but was not built,
	// because there was no rate to convert its bounds at.
	heldForRate bool

	// b2bChain and shaChain measure how fast each chain is running, which
	// is what turns a wall-clock safety margin into a number of blocks.
	// Guarded because the poller writes them and quotes read them.
	chainMu  sync.Mutex
	b2bChain *chainrate.Observer
	shaChain *chainrate.Observer

	// sideOf remembers which direction each swap belongs to, so that what
	// is already committed can be counted per side.
	//
	// It is not in the journal, so it is rebuilt by asking the paying node
	// to decode the invoice, and cached because that is a round trip and
	// this is asked on every quote.
	sideMu sync.Mutex
	sideOf map[node.Hash]string

	// compactEvery is how often the journal is rewritten. Zero means
	// DefaultCompactInterval; a test sets it low so that the scheduling
	// can be observed rather than only the compaction.
	compactEvery time.Duration

	// ctx is the bridge's own lifetime, which a swap started by an RPC
	// must outlive: the caller may hang up the moment after they pay, and
	// the HTLC does not go away with their connection.
	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup

	closeOnce sync.Once
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
	if s.rates, err = openRatebook(filepath.Dir(cfg.Journal),
		cfg.FixedRate, s.res.rateMaxAge, nil); err != nil {

		s.close()

		return nil, err
	}

	s.own = []string{local.NodeKey()}
	if remote != nil {
		s.positionOf = remote.Balance

		ctx, cancel := context.WithTimeout(context.Background(),
			dialTimeout)
		key, err := remote.NodeKey(ctx)
		cancel()
		if err != nil {
			s.close()

			return nil, err
		}
		s.own = append(s.own, key)
	}

	if s.b2bChain, err = chainrate.New(s.res.b2bChain); err != nil {
		s.close()

		return nil, fmt.Errorf("the BLAKE2b chain observer: %w", err)
	}
	if s.shaChain, err = chainrate.New(s.res.shaChain); err != nil {
		s.close()

		return nil, fmt.Errorf("the SHA256 chain observer: %w", err)
	}

	// A direction turned off with swaps unfinished is built all the same,
	// to finish them, and quotes nothing: left undriven, a payer's HTLC
	// could expire after the bridge had paid out, or a paid one never be
	// claimed. Only resume reaches it, by the journal's own invoices.
	finishOff := false
	if !cfg.ToSHA256 || !cfg.ToBLAKE2b {
		pending, err := s.journal.Pending(context.Background())
		if err != nil {
			s.close()

			return nil, fmt.Errorf("reading the swap journal: %w", err)
		}
		finishOff = len(pending) > 0
	}

	// toSHA256 receives here and pays on the SHA256 chain, so it drains the
	// SHA256 side. toBLAKE2b puts back what the other one spends.
	if cfg.ToSHA256 || finishOff {
		sd := s.build(
			"toSHA256", local, remote, remote.Balance,
			inventory.Draining, false,
		)
		sd.finishing = !cfg.ToSHA256
		s.sides = append(s.sides, sd)
	}
	if !cfg.ToSHA256 {
		s.disabled = append(s.disabled, "toSHA256")
	}
	if !cfg.ToBLAKE2b && finishOff {
		sd := s.build(
			"toBLAKE2b", remote, local, local.Balance,
			inventory.Replenishing, true,
		)
		sd.finishing = true
		s.sides = append(s.sides, sd)
	}
	if cfg.ToBLAKE2b {
		// Its swap bounds are configured in SHA256 coin and converted
		// at the rate in force now, once (see inBLAKE2bMsat). With no
		// rate they stay in the wrong unit, so it is held: nothing is
		// quoted without a rate anyway, and SetRate rebuilds the
		// bridge the first time, converting them.
		//
		// Built all the same. Swaps already in the journal (from
		// before a configured rate was dropped, say) need their side
		// to be driven: a payer's HTLC to settle or refund, a paid one
		// to claim. Leaving them undriven until a rate is set could
		// let an incoming HTLC expire after the bridge had paid out.
		if r, _ := s.rates.current(); !usableRate(r) {
			s.heldForRate = true
		}
		// The rate is posted as SHA256 coin per BLAKE2b coin, so the
		// direction that pays out in BTCB2 quotes its reciprocal.
		s.sides = append(s.sides, s.build(
			"toBLAKE2b", remote, local, local.Balance,
			inventory.Replenishing, true,
		))
	} else {
		s.disabled = append(s.disabled, "toBLAKE2b")
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
		// The direction that inverts the rate is the one paying in
		// BTCB2.
		paysBLAKE2b: invert,
	}

	// The swap bounds are configured in SHA256 millisatoshis and applied
	// to the outgoing leg, which is BTCB2 on this direction. One number
	// used raw for both would cap two different amounts of value.
	policy := s.res.quote
	if invert {
		policy.MinSwapMsat = s.inBLAKE2bMsat(policy.MinSwapMsat)
		policy.MaxSwapMsat = s.inBLAKE2bMsat(policy.MaxSwapMsat)
	}

	sd.quoter = &quote.Quoter{
		In: sd.in, Out: sd.out, Store: s.journal,
		Price:       s.pricer(sd),
		Spacing:     quote.Rates(s.spacing(invert)),
		Room:        s.headroom(sd),
		Margin:      s.res.margin,
		Policy:      policy,
		PaysBLAKE2b: sd.paysBLAKE2b,
		Own:         s.own,
		Limits:      s.res.limits,
	}
	sd.runner = &runner.Runner{
		Driver: &driver.Driver{
			In: sd.in, Out: sd.out, Store: s.journal,
			Policy: s.res.margin,
		},
		Store:       s.journal,
		Rates:       runner.Rates(s.spacing(invert)),
		Log:         bridgeLogger(name),
		FundedGrace: s.res.fundedGrace,
	}

	return sd
}

// inBLAKE2bMsat converts an amount of SHA256 millisatoshis into BLAKE2b ones at the
// posted rate, which is quoted as Bitcoin per BTCB2.
//
// Saturating rather than wrapping: a cap that overflowed to a small number
// would refuse everything, and one that wrapped to a huge number would cap
// nothing at all, which is the worse of the two.
func (s *service) inBLAKE2bMsat(btcMsat uint64) uint64 {
	// The rate in force when the bridge started. The bounds are caps, set
	// once; a rate changed later moves what they are worth in BTCB2, but
	// re-deriving them under quotes in flight would race those quotes.
	rate, _ := s.rates.current()
	if btcMsat == 0 || rate <= 0 {
		return btcMsat
	}

	converted := float64(btcMsat) / rate
	if converted >= math.MaxUint64 {
		return math.MaxUint64
	}

	return uint64(converted)
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
		posted, at, err := s.rates.usable()
		if err != nil {
			return rate.Reading{}, fmt.Errorf("%w: %w", quote.ErrRefused,
				err)
		}

		r := rate.Reading{
			OutgoingPerIncoming: posted,
			At:                  at,

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

		// Both directions price against what the bridge holds on the
		// SHA256 chain. That is the position the inventory policy
		// describes: toSHA256 spends it and toBLAKE2b puts it back.
		// Pricing toBLAKE2b against its own BTCB2 balance instead would
		// offer its biggest discount when the BTCB2 side was nearly
		// empty, the opposite of what that side needs.
		held, err := s.position(ctx)
		if err != nil {
			return rate.Reading{}, fmt.Errorf("%w: %w", quote.ErrRefused,
				err)
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

// position is what the bridge holds on the SHA256 chain: the outgoing
// balance of the side that pays there.
func (s *service) position(ctx context.Context) (uint64, error) {
	if s.positionOf == nil {
		return 0, errors.New("no SHA256 node to read the position from")
	}

	return s.positionOf(ctx)
}

// spacing reports both chains' current block rates, incoming first.
//
// invert names which chain is which: the toSHA256 direction receives on
// BLAKE2b, and toBLAKE2b receives on the SHA256 chain.
func (s *service) spacing(invert bool) func(context.Context) (driver.Rates,
	error) {

	return func(context.Context) (driver.Rates, error) {
		s.chainMu.Lock()
		defer s.chainMu.Unlock()

		now := time.Now()
		lf, err := s.b2bChain.Estimate(now)
		if err != nil {
			return driver.Rates{}, fmt.Errorf("the BLAKE2b "+
				"chain's spacing: %w", err)
		}
		btc, err := s.shaChain.Estimate(now)
		if err != nil {
			return driver.Rates{}, fmt.Errorf("the SHA256 "+
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

	// Every swap ever driven was remembered and nothing forgot any of
	// them, so the map grew for the life of the process. Pruning here
	// rather than when a swap finishes because this is the one place that
	// already knows the whole unfinished set, and a swap that leaves it
	// has by definition stopped being a commitment.
	s.forgetAllBut(pending)

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

// forgetAllBut drops remembered swaps that are no longer unfinished.
func (s *service) forgetAllBut(pending []store.Record) {
	s.sideMu.Lock()
	defer s.sideMu.Unlock()

	if len(s.sideOf) == 0 {
		return
	}

	keep := make(map[node.Hash]struct{}, len(pending))
	for _, rec := range pending {
		keep[rec.Hash] = struct{}{}
	}

	for hash := range s.sideOf {
		if _, ok := keep[hash]; !ok {
			delete(s.sideOf, hash)
		}
	}
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
//
// The journal stays referenced once closed: a closed journal still answers
// from its index, and a caller that read the service just before it was
// stopped (the drain's watcher, LookupSwap) must not find it gone.
func (s *service) close() {
	s.closeOnce.Do(func() {
		if s.journal != nil {
			_ = s.journal.Close()
		}
	})
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

		// The position, not this side's own paying balance: both
		// directions price against what is held on the SHA256 chain.
		held, err := s.position(ctx)
		if err != nil {
			log.Warnf("Bridge could not read the SHA256 balance to "+
				"size %s inventory, so it keeps the default: %v",
				sd.name, err)

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
		// The configured minimum, which is in SHA256 millisatoshis like
		// the position it is compared with.
		floor := s.res.quote.MinSwapMsat
		if held < floor {
			log.Debugf("Bridge cannot pay a swap on %s yet (%d "+
				"msat against a %d msat minimum), so it will "+
				"refuse that direction until the paying node "+
				"has outbound capacity", sd.name, held, floor)

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

// liquidityRefusals names the directions that cannot currently pay.
//
// A bridge with no outbound capacity on a side is up, synced, correctly
// configured and refuses every swap that way. That is the most common thing to
// be wrong, it is guaranteed to be wrong on a freshly created node, and
// nothing else here would say so: the nodes answer, the chains measure, and
// the refusal only appears per quote as a number the operator has to
// interpret.
//
// Bounded, because this is the request that has to answer when things are
// wrong: a node that accepts the connection and then says nothing must not
// hang the one call asking why.
func (s *service) liquidityRefusals(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var out []string
	for _, sd := range s.sides {
		if !sd.quoting() {
			continue
		}
		held, err := sd.balance(ctx)
		if err != nil {
			out = append(out, fmt.Sprintf("%s cannot read what "+
				"the paying node can send, so it will refuse "+
				"every swap: %v", sd.name, err))

			continue
		}

		// The same floor sizing uses: below one swap there is nothing
		// to serve, whatever the inventory policy would say.
		if floor := sd.quoter.Policy.MinSwapMsat; held < floor {
			out = append(out, fmt.Sprintf("%s has %d msat of "+
				"outbound capacity against a %d msat minimum "+
				"swap, so it will refuse every swap until the "+
				"paying node has more", sd.name, held, floor))
		}
	}

	return out
}

// directionInfo is what one direction would charge now, and whether a quote
// could succeed, without creating or reserving anything.
//
// The checks are the ones a quote runs, in the same order, through the same
// functions: the price (which includes the rate's age and how drained the
// position is), the chains' block rates, and what is left to commit. A second
// set of rules here that drifted from the quote's would report a direction as
// open that then refuses, or the other way round.
func (s *service) directionInfo(ctx context.Context, sd *side) *DirectionInfo {
	policy := sd.quoter.Policy
	out := &DirectionInfo{
		Name:    sd.name,
		MinMsat: policy.MinSwapMsat,
		MaxMsat: policy.MaxSwapMsat,
	}
	if _, setAt := s.rates.current(); !setAt.IsZero() {
		out.RateSetAt = setAt.Unix()
	}

	shut := func(err error) *DirectionInfo {
		out.Open = false
		out.RefusalCode = string(quote.CodeOf(err))
		out.Refusal = err.Error()

		return out
	}

	reading, err := sd.quoter.Price(ctx)
	if err != nil {
		return shut(err)
	}
	out.Rate = reading.OutgoingPerIncoming
	out.Spread = reading.Spread

	if _, err := sd.quoter.Spacing(ctx); err != nil {
		return shut(fmt.Errorf("%w: %w", quote.ErrRefused, err))
	}

	room, err := sd.quoter.Room(ctx)
	if err != nil {
		return shut(fmt.Errorf("%w: %w", quote.ErrRefused, err))
	}
	// What can be quoted now is also bounded by what is left: a payer
	// shown the policy's maximum would be refused anything above this.
	if room < out.MaxMsat {
		out.MaxMsat = room
	}
	if room < policy.MinSwapMsat {
		return shut(fmt.Errorf("%w: %w: %d msat left against a %d msat "+
			"minimum", quote.ErrRefused, quote.ErrNoLiquidity, room,
			policy.MinSwapMsat))
	}

	out.Open = true

	return out
}

// markStopped records a swap the runner gave up on, for the operator to see.
func (s *service) markStopped(hash node.Hash, why string) {
	s.stoppedMu.Lock()
	defer s.stoppedMu.Unlock()

	if s.stopped == nil {
		s.stopped = map[node.Hash]string{}
	}
	s.stopped[hash] = why
}

// needsOperator lists the swaps a person has to look at: those that ended
// lost, and those the runner stopped driving. The bridge keeps quoting
// either way; this is so they are seen rather than found in a log.
func (s *service) needsOperator(ctx context.Context) []string {
	var out []string

	lost, err := s.journal.Where(ctx, func(r store.Record) bool {
		return r.State == swap.Lost
	})
	if err != nil {
		out = append(out, "the journal cannot be read: "+err.Error())
	}
	for _, r := range lost {
		out = append(out, fmt.Sprintf("%x lost: paid out, and the "+
			"payment coming in could not be claimed", r.Hash))
	}

	s.stoppedMu.Lock()
	defer s.stoppedMu.Unlock()
	for hash, why := range s.stopped {
		out = append(out, fmt.Sprintf("%x stopped: %s", hash, why))
	}
	sort.Strings(out)

	return out
}
