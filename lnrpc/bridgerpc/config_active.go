//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"errors"
	"fmt"
	"time"

	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/inventory"
	"github.com/paulscode/lightning-fork-bridge/margin"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/rate"
)

// ErrConfig is returned for a configuration that cannot be used. It is
// separate from an operational failure because the two need different
// responses: this one needs an edit, not a retry.
var ErrConfig = errors.New("the bridge configuration cannot be used")

// Config is the primary configuration struct for the bridge RPC server. It
// contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
//
// Only the settings an operator actually knows are here. The numbers that
// decide whether a swap is safe (how long an HTLC must outlive the outgoing
// leg, how far a chain may surge, how stale a price may be) are derived or
// defaulted, because they are interdependent in ways that are easy to get
// wrong and expensive to get wrong. Where one is exposed at all it is under
// Advanced, and Validate checks the combination rather than each in turn.
type Config struct {
	// Enabled turns the bridge on. It is off by default and stays off
	// until an operator says otherwise: a node carrying this code is not
	// the same thing as a node offering to swap other people's money.
	Enabled bool `long:"enabled" description:"Offer cross-chain swaps between this chain and the SHA256 chain. Requires a node there and funded channels on both sides."`

	// SHA256RPCHost is the Lightning node on the SHA256 chain, as
	// host:port.
	//
	// A Lightning node, not a chain node: it talks to whatever SHA256 node
	// the operator already runs. Nothing here needs a second copy of a
	// chain.
	//
	// Named for the proof of work rather than for a currency. lnd's own
	// bitcoin.* options already mean the chain this node follows, which
	// here is the BLAKE2b one, so bitcoin.rpchost would be ambiguous in
	// the same config file as well as taking a side on a question this
	// code has no need to answer.
	SHA256RPCHost string `long:"sha256.rpchost" description:"The Lightning node on the SHA256 chain to bridge through, as host:port. This is a Lightning node, not a second chain node."`

	// SHA256TLSCertPath is that node's TLS certificate.
	SHA256TLSCertPath string `long:"sha256.tlscertpath" description:"Path to the SHA256 node's TLS certificate."`

	// SHA256MacaroonPath is a macaroon for that node.
	SHA256MacaroonPath string `long:"sha256.macaroonpath" description:"Path to a macaroon for the SHA256 node. It needs invoice and offchain write."`

	// Journal is where swaps are recorded.
	//
	// It holds preimages and the state of anything in flight, so it must be
	// covered by whatever backs up this node: losing it while a swap is
	// running loses the record of an HTLC that is still out there.
	Journal string `long:"journal" description:"Path to the swap journal. Defaults to bridge/swaps.journal under the network directory. Back this up: it records swaps in flight."`

	// ToSHA256 serves swaps that pay out on the SHA256 chain.
	ToSHA256 bool `long:"tosha256" description:"Serve swaps that receive on this chain and pay out on the SHA256 chain. Needs outbound capacity there."`

	// ToBLAKE2b serves swaps that pay out on this chain.
	ToBLAKE2b bool `long:"toblake2b" description:"Serve swaps that receive on the SHA256 chain and pay out on this chain. Needs outbound capacity here."`

	// FixedRate is what the operator will trade at, in outgoing units per
	// incoming unit for the toSHA256 direction: SHA256 coin per BLAKE2b
	// coin.
	//
	// There is deliberately no default. A wrong rate loses money on every
	// swap and does it quietly, so the bridge refuses to guess one; for a
	// market this thin an operator's own posted rate is the price, and it
	// has to be their number.
	FixedRate float64 `long:"fixedrate" description:"What you will trade at, as SHA256 coin per BLAKE2b coin. There is no default: a wrong rate loses money silently, so this must be set deliberately."`

	// Spread is the fraction charged on top of the rate.
	Spread float64 `long:"spread" description:"The fraction you keep, on top of the rate. Routing fees come out of this. Default 0.01 (1%)."`

	// MaxSwapMsat caps a single swap, in SHA256 millisatoshis.
	//
	// The same chain either way round, because an operator thinks in one
	// unit. The swap packages bound the outgoing leg in the units of
	// the chain that leg is on, which is a different unit per direction,
	// so this is converted for the direction that pays in BLAKE2b coin. One
	// number used raw for both would cap two different amounts of value:
	// at any plausible rate, a couple of hundred times apart.
	MaxSwapMsat uint64 `long:"maxswapmsat" description:"The most a single swap may be, in SHA256 millisatoshis. The same value is applied in both directions, converted at your rate."`

	// MinSwapMsat floors a single swap, in SHA256 millisatoshis.
	MinSwapMsat uint64 `long:"minswapmsat" description:"The least a single swap may be, in SHA256 millisatoshis. The same value is applied in both directions, converted at your rate."`

	// OutgoingCLTVLimit caps the total CLTV of the outgoing route, in
	// blocks of the outgoing chain.
	//
	// It has to cover the destination's own final hop delta plus every hop
	// in between. The swap packages default it to 80, which is what a
	// stock lnd asks for on the final hop alone, so a bridge using that
	// number cannot pay an ordinary invoice at all. Raising it is not
	// free: the incoming leg has to outlive the whole budget, so a larger
	// one demands more incoming CLTV, and past a point no incoming CLTV
	// within the cap is enough. Validate checks that combination.
	OutgoingCLTVLimit uint32 `long:"outgoingcltvlimit" description:"The most CLTV a payout route may use, in blocks of the outgoing chain. Must cover the destination's final hop delta plus the hops before it."`

	// InventoryTargetMsat is the working balance each paying side is sized
	// against, in millisatoshis of that side's chain.
	//
	// Zero means derive it from what that side actually holds when the
	// bridge starts. That is the useful default: the shipped one is a
	// mainnet-sized half a bitcoin, and on a node holding less than its
	// floor the bridge refuses every swap while reporting only that it is
	// below a number the operator never chose.
	InventoryTargetMsat uint64 `long:"inventorytargetmsat" description:"The working balance each paying side is priced against. Zero derives it from what that side holds at startup."`

	// InventoryFloorMsat is how much of that is held back for swaps
	// already in flight. Zero derives it alongside the target.
	InventoryFloorMsat uint64 `long:"inventoryfloormsat" description:"How much of the working balance is reserved for swaps already in flight. Zero derives it from the target."`

	// Deps is what the node provides.
	Deps *Deps
}

// DefaultFloorFraction is the share of the working balance held back for swaps
// already in flight, when the bounds are derived rather than configured. It is
// the ratio the shipped defaults use.
const DefaultFloorFraction = 10

// DefaultOutgoingCLTVLimit is the route budget when the operator names none.
//
// A stock lnd asks for 80 blocks on the final hop, so the budget has to be
// comfortably above that to leave room for the hops before it. This is roughly
// that plus three ordinary hops, and it stays well inside what the incoming
// leg can be asked to outlive.
const DefaultOutgoingCLTVLimit = 200

// maxSpread bounds what an operator may post as a fee.
//
// The limit is not a judgement about what a bridge may charge; it is where the
// derived policies stop making sense, since the inventory policy's widest
// spread is a multiple of the posted one and a spread of 1 is not a quote. It
// doubles as a units check, which is the likelier mistake.
const maxSpread = 0.2

// DefaultJournalName is where the journal lives under the network directory
// when the operator names no path.
const DefaultJournalName = "bridge/swaps.journal"

// resolved is a Config with every derived value filled in.
//
// It is separate from Config so that the defaults are applied once, in one
// place, rather than at each use where one could be forgotten. Nothing reads
// the policies off Config directly.
type resolved struct {
	quote     quote.Policy
	margin    margin.Policy
	inventory inventory.Policy
	rate      rate.Policy
	b2bChain  chainrate.Params
	shaChain  chainrate.Params
}

// resolve applies the defaults and the operator's overrides.
func (c *Config) resolve() resolved {
	r := resolved{
		quote:     quote.DefaultPolicy,
		margin:    margin.DefaultPolicy,
		inventory: inventory.DefaultPolicy,
		rate:      rate.DefaultPolicy,
		b2bChain:  chainrate.BlakeParams,
		shaChain:  chainrate.BitcoinParams,
	}

	r.quote.OutgoingCLTVLimit = DefaultOutgoingCLTVLimit
	if c.OutgoingCLTVLimit != 0 {
		r.quote.OutgoingCLTVLimit = c.OutgoingCLTVLimit
	}

	if c.InventoryTargetMsat != 0 {
		r.inventory.TargetOutgoingMsat = c.InventoryTargetMsat
		r.inventory.FloorOutgoingMsat =
			c.InventoryTargetMsat / DefaultFloorFraction
	}
	if c.InventoryFloorMsat != 0 {
		r.inventory.FloorOutgoingMsat = c.InventoryFloorMsat
	}

	if c.MaxSwapMsat != 0 {
		r.quote.MaxSwapMsat = c.MaxSwapMsat
	}
	if c.MinSwapMsat != 0 {
		r.quote.MinSwapMsat = c.MinSwapMsat
	}
	if c.Spread > 0 {
		// The operator's spread is a floor, not a ceiling: the rate
		// oracle widens for volatility and the inventory policy widens
		// for how drained the paying side is, and both are reasons to
		// charge more than the posted fee rather than less.
		//
		// Both policies are scaled by the same factor rather than
		// having one field overwritten, because their fields are not
		// independent. The inventory policy requires its discount to
		// be no larger than its base spread and no larger than what a
		// rebalance costs, and its maximum spread to cover that cost.
		// Setting the base alone breaks all three at once for any
		// spread below the default, which is a configuration that
		// looks reasonable field by field and refuses every swap.
		// Scaling preserves the ratios the defaults were chosen with.
		scale := c.Spread / inventory.DefaultPolicy.BaseSpread
		r.inventory.BaseSpread *= scale
		r.inventory.MaxSpread *= scale
		r.inventory.MaxDiscount *= scale
		r.inventory.RebalanceCost *= scale

		rateScale := c.Spread / rate.DefaultPolicy.BaseSpread
		r.rate.BaseSpread *= rateScale
		r.rate.MaxSpread *= rateScale
	}

	return r
}

// Validate checks that this configuration would actually serve swaps.
//
// It checks the combination rather than each number in turn, because the ones
// that matter are only wrong together. Writing an example configuration for the
// standalone daemon produced one that refused every swap while every individual
// value looked reasonable, and a later pass found a second pair with the same
// shape. Both are checked here.
//
// Called before anything is served, so that an operator learns at startup
// rather than from a swap that silently never happens.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	if !c.ToSHA256 && !c.ToBLAKE2b {
		return fmt.Errorf("%w: the bridge is enabled but neither "+
			"direction is, so it would refuse every swap; set "+
			"bridgerpc.tosha256, bridgerpc.toblake2b, or both",
			ErrConfig)
	}
	if c.SHA256RPCHost == "" {
		return fmt.Errorf("%w: no Lightning node on the SHA256 chain; "+
			"set bridgerpc.sha256.rpchost to the node that holds "+
			"your channels there", ErrConfig)
	}
	if c.SHA256MacaroonPath == "" {
		return fmt.Errorf("%w: no macaroon for the SHA256 node; set "+
			"bridgerpc.sha256.macaroonpath", ErrConfig)
	}
	if c.FixedRate <= 0 {
		return fmt.Errorf("%w: no rate; set bridgerpc.fixedrate to "+
			"what you will trade at, in SHA256 coin per BLAKE2b "+
			"coin. There is no default because a wrong one loses "+
			"money on every swap and does it quietly", ErrConfig)
	}
	if c.Spread < 0 {
		return fmt.Errorf("%w: a negative spread (%g) pays people to "+
			"use the bridge", ErrConfig, c.Spread)
	}
	if c.Spread >= maxSpread {
		// Far more likely to be a unit mistake than an intention. The
		// spread is a fraction, so a one percent fee is 0.01, and
		// someone who meant that and typed 1 would otherwise have
		// configured a bridge that keeps everything.
		return fmt.Errorf("%w: a spread of %g means %.0f%%, which is "+
			"almost certainly not what was meant: this is a "+
			"fraction, so one percent is 0.01. The most this "+
			"accepts is %g", ErrConfig, c.Spread, c.Spread*100,
			maxSpread)
	}

	r := c.resolve()

	if r.quote.MaxSwapMsat < r.quote.MinSwapMsat {
		return fmt.Errorf("%w: the most a swap may be (%d msat) is "+
			"below the least (%d msat), so every swap is refused",
			ErrConfig, r.quote.MaxSwapMsat, r.quote.MinSwapMsat)
	}
	if err := r.quote.Valid(); err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if !r.margin.Valid() {
		return fmt.Errorf("%w: the margin policy is unusable", ErrConfig)
	}
	if err := r.inventory.Valid(); err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if !r.rate.Valid() {
		return fmt.Errorf("%w: the rate policy is unusable", ErrConfig)
	}
	if !r.b2bChain.Valid() || !r.shaChain.Valid() {
		return fmt.Errorf("%w: a chain observer is unusable", ErrConfig)
	}

	// The spread the operator asked for must be one the bridge can
	// actually charge. Under it, every swap loses the difference.
	if c.Spread > 0 && c.Spread < r.quote.FeeFraction {
		return fmt.Errorf("%w: a spread of %g is below the %g this "+
			"bridge budgets for routing fees, so it would lose "+
			"money on every swap", ErrConfig, c.Spread,
			r.quote.FeeFraction)
	}

	return c.checkReachable(r)
}

// checkReachable is the case that produced a configuration refusing every swap
// while every number in it looked fine.
//
// The outgoing CLTV budget, the settlement allowance and the incoming chain's
// surge factor combine into a CLTV the incoming leg must carry. Planning for
// the full surge this chain has shown, with a budget that suits ordinary
// destinations, needs thousands of blocks against a cap in the low thousands,
// and refuses everything.
//
// It asks margin.RequiredIncomingBlocks rather than recomputing the rule.
// A second implementation that drifted from the real one would either pass a
// configuration that refuses every swap or refuse one that works, and the
// whole point of checking at startup is to be right about it. What is
// approximated here is only the input: at startup no block has been observed,
// so the bounds each chain would show while running exactly at its target
// spacing stand in.
//
// That makes this an optimistic check. A chain running slower than target
// needs more incoming blocks than this predicts, and the run-time check in
// margin is what catches that, per swap, with observed numbers. Passing here
// means "this cannot be dismissed on the numbers", not "this will always
// quote".
func (c *Config) checkReachable(r resolved) error {
	for _, dir := range []struct {
		name string

		enabled bool

		// in is the chain the payer pays on, out the chain the bridge
		// pays on. The CLTV budget is spent on the outgoing chain and
		// has to be outlived on the incoming one.
		in, out chainrate.Params
	}{
		{"toSHA256", c.ToSHA256, r.b2bChain, r.shaChain},
		{"toBLAKE2b", c.ToBLAKE2b, r.shaChain, r.b2bChain},
	} {
		if !dir.enabled {
			continue
		}

		inBounds, err := atTargetSpacing(dir.in)
		if err != nil {
			return fmt.Errorf("%w: %s incoming chain: %w",
				ErrConfig, dir.name, err)
		}
		outBounds, err := atTargetSpacing(dir.out)
		if err != nil {
			return fmt.Errorf("%w: %s outgoing chain: %w",
				ErrConfig, dir.name, err)
		}

		_, err = margin.RequiredIncomingBlocks(inBounds, margin.Leg{
			Bounds: outBounds,
			Blocks: uint32(r.quote.OutgoingCLTVLimit),
		}, r.margin)
		if err != nil {
			return fmt.Errorf("%w: %s would refuse every swap: "+
				"%w. Lower the outgoing CLTV budget (%d "+
				"blocks), shorten the settlement allowance "+
				"(%v), or raise the incoming block cap (%d)",
				ErrConfig, dir.name, err,
				r.quote.OutgoingCLTVLimit,
				r.margin.Settlement, r.margin.MaxIncomingBlocks)
		}
	}

	return nil
}

// atTargetSpacing is the bounds a chain would show while running exactly at
// its target: as fast as its surge factor allows, as slow as its stall factor
// does.
func atTargetSpacing(p chainrate.Params) (margin.Bounds, error) {
	if p.TargetSpacing <= 0 || p.SurgeFactor <= 0 || p.StallFactor <= 0 {
		return margin.Bounds{}, errors.New("the chain parameters " +
			"describe no usable block spacing")
	}

	b := margin.Bounds{
		Fast: time.Duration(float64(p.TargetSpacing) / p.SurgeFactor),
		Slow: time.Duration(float64(p.TargetSpacing) * p.StallFactor),
	}
	if !b.Valid() {
		return margin.Bounds{}, fmt.Errorf("the chain parameters give "+
			"unusable bounds %v..%v", b.Fast, b.Slow)
	}

	return b, nil
}
