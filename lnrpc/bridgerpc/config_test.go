//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/margin"
	"github.com/paulscode/lightning-fork-bridge/quote"
)

// usable is a configuration that should serve swaps, which the refusal tests
// vary one field at a time from. If this ever stops validating, the defaults
// have drifted into something that refuses everything, which is exactly the
// failure this file exists to catch.
func usable() Config {
	return Config{
		Enabled:            true,
		SHA256RPCHost:      "127.0.0.1:10010",
		SHA256MacaroonPath: "/tmp/admin.macaroon",
		ToSHA256:           true,
		ToBLAKE2b:          true,
		RateSource:         RateSourceFixed,
		FixedRate:          0.00308078,
		Spread:             0.01,
	}
}

func TestDefaultsServeSwaps(t *testing.T) {
	t.Parallel()

	c := usable()
	if err := c.Validate(); err != nil {
		t.Fatalf("the shipped defaults refuse every swap: %v", err)
	}
}

// A disabled bridge is never misconfigured: an operator who has not turned it
// on must not be stopped from starting their node.
func TestDisabledIsNeverInvalid(t *testing.T) {
	t.Parallel()

	c := Config{Enabled: false}
	if err := c.Validate(); err != nil {
		t.Errorf("an empty disabled config should be fine: %v", err)
	}
}

func TestValidateRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{
			name: "no direction enabled",
			edit: func(c *Config) {
				c.ToSHA256, c.ToBLAKE2b = false, false
			},
			want: "neither direction",
		},
		{
			name: "no SHA256 node",
			edit: func(c *Config) { c.SHA256RPCHost = "" },
			want: "rpchost",
		},
		{
			name: "no macaroon",
			edit: func(c *Config) { c.SHA256MacaroonPath = "" },
			want: "macaroonpath",
		},
		{
			name: "a rate that is not a number",
			edit: func(c *Config) { c.FixedRate = math.NaN() },
			want: "fixedrate",
		},
		{
			name: "an infinite rate",
			edit: func(c *Config) { c.FixedRate = math.Inf(1) },
			want: "fixedrate",
		},
		{
			name: "a negative rate",
			edit: func(c *Config) { c.FixedRate = -1 },
			want: "fixedrate",
		},
		{
			name: "a negative spread pays people to use the bridge",
			edit: func(c *Config) { c.Spread = -0.01 },
			want: "negative spread",
		},
		{
			// Below the routing budget the bridge loses the
			// difference on every swap, by construction.
			name: "a spread under the routing budget",
			edit: func(c *Config) {
				c.Spread = quote.DefaultPolicy.FeeFraction / 2
			},
			want: "lose money on every swap",
		},
		{
			name: "the maximum swap is below the minimum",
			edit: func(c *Config) {
				c.MinSwapMsat = 10_000_000
				c.MaxSwapMsat = 1_000_000
			},
			want: "below the least",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			c := usable()
			test.edit(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("wanted a refusal")
			}
			if !errors.Is(err, ErrConfig) {
				t.Errorf("should be an ErrConfig: %v", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("the refusal should say %q: %v",
					test.want, err)
			}
		})
	}
}

// The case that produced a configuration refusing every swap while every
// number in it looked reasonable. It must be caught at startup, and the
// message must name a knob to turn, because the numbers give no hint alone.
func TestReachabilityIsCheckedAndExplained(t *testing.T) {
	t.Parallel()

	// A settlement allowance long enough that no incoming CLTV within the
	// cap can outlive the outgoing leg.
	c := usable()
	r := c.resolve()
	r.margin.Settlement = 30 * 24 * time.Hour

	err := c.checkReachable(r)
	if err == nil {
		t.Fatal("a configuration that refuses every swap validated")
	}
	for _, want := range []string{
		"refuse every swap", "CLTV budget", "settlement allowance",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
}

// The check must agree with the function that actually decides, not
// approximate it: a second implementation that drifted would pass a
// configuration that refuses every swap, or refuse one that works.
func TestReachabilityAgreesWithMargin(t *testing.T) {
	t.Parallel()

	c := usable()
	r := c.resolve()

	in, err := atTargetSpacing(r.b2bChain)
	if err != nil {
		t.Fatal(err)
	}
	out, err := atTargetSpacing(r.shaChain)
	if err != nil {
		t.Fatal(err)
	}

	_, marginErr := margin.RequiredIncomingBlocks(in, margin.Leg{
		Bounds: out,
		Blocks: uint32(r.quote.OutgoingCLTVLimit),
	}, r.margin)

	ourErr := c.checkReachable(r)
	if (marginErr == nil) != (ourErr == nil) {
		t.Errorf("margin says %v but the config check says %v",
			marginErr, ourErr)
	}
}

// The bounds stood in for at startup must be usable ones, or the check would
// refuse every configuration for the wrong reason.
func TestAtTargetSpacing(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]chainrate.Params{
		"blake2b": chainrate.BlakeParams,
		"bitcoin": chainrate.BitcoinParams,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b, err := atTargetSpacing(p)
			if err != nil {
				t.Fatal(err)
			}
			if !b.Valid() {
				t.Fatalf("unusable bounds %+v", b)
			}
			if b.Fast > p.TargetSpacing {
				t.Errorf("the fast bound %v is slower than "+
					"the target %v", b.Fast,
					p.TargetSpacing)
			}
			if b.Slow < p.TargetSpacing {
				t.Errorf("the slow bound %v is faster than "+
					"the target %v", b.Slow,
					p.TargetSpacing)
			}
		})
	}

	for name, p := range map[string]chainrate.Params{
		"no spacing":      {SurgeFactor: 2, StallFactor: 2},
		"no surge factor": {TargetSpacing: time.Minute, StallFactor: 2},
		"no stall factor": {TargetSpacing: time.Minute, SurgeFactor: 2},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()

			if _, err := atTargetSpacing(p); err == nil {
				t.Error("wanted a refusal")
			}
		})
	}
}

// The swap packages default the route budget to 80 blocks, which is exactly
// what a stock lnd asks for on the final hop alone. A bridge using that number
// refuses every ordinary invoice, which the lab found by trying one.
func TestTheRouteBudgetCanPayAStockNodesInvoice(t *testing.T) {
	t.Parallel()

	const stockFinalHopDelta = 80

	c := usable()
	r := c.resolve()

	if r.quote.OutgoingCLTVLimit <= stockFinalHopDelta {
		t.Errorf("the route budget is %d blocks against a final hop "+
			"delta of %d, which leaves nothing for the hops "+
			"between and refuses every ordinary invoice",
			r.quote.OutgoingCLTVLimit, stockFinalHopDelta)
	}

	// And it still has to be a budget the incoming leg can outlive.
	if err := c.Validate(); err != nil {
		t.Errorf("the default route budget does not validate: %v", err)
	}
}

// Raising the budget is not free: the incoming leg has to outlive it, and past
// a point no incoming CLTV within the cap is enough. That has to be refused at
// startup rather than discovered per swap.
func TestARouteBudgetTooLargeToOutliveIsRefused(t *testing.T) {
	t.Parallel()

	c := usable()
	c.OutgoingCLTVLimit = 5000

	err := c.Validate()
	if err == nil {
		t.Fatal("a route budget nothing can outlive was accepted")
	}
	if !strings.Contains(err.Error(), "refuse every swap") {
		t.Errorf("the refusal should say what it costs: %v", err)
	}
}

// The operator's spread is a floor, not a ceiling: the oracle widens for
// volatility and the inventory policy for how drained the paying side is, and
// both are reasons to charge more than the posted fee rather than less.
func TestSpreadIsAFloorNotACeiling(t *testing.T) {
	t.Parallel()

	c := usable()
	c.Spread = 0.05
	r := c.resolve()

	if r.feeToSHA256 != 0.05 || r.feeToB2B != 0.05 {
		t.Errorf("fees %g/%g, wanted the operator's 0.05 both ways",
			r.feeToSHA256, r.feeToB2B)
	}
	inv := r.inventoryFor(r.feeToSHA256)
	if inv.BaseSpread != 0.05 {
		t.Errorf("inventory base spread %g, wanted 0.05", inv.BaseSpread)
	}
	if inv.MaxSpread < inv.BaseSpread {
		t.Error("the inventory policy cannot widen past the posted fee")
	}

	// A spread above the defaults' ceiling must raise the ceiling, not be
	// silently clamped down to it.
	c.Spread = 0.1
	r = c.resolve()
	if inv := r.inventoryFor(r.feeToSHA256); inv.MaxSpread < 0.1 {
		t.Errorf("a large spread was clamped: inventory %g", inv.MaxSpread)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("a spread of 0.1 should still validate: %v", err)
	}
}

// The fees: 1.5% each way by default, spread for both, and a per-direction fee
// over either.
func TestFeesPerDirection(t *testing.T) {
	t.Parallel()

	c := usable()
	c.Spread = 0
	r := c.resolve()
	if r.feeToSHA256 != DefaultFee || r.feeToB2B != DefaultFee {
		t.Errorf("default fees %g/%g, wanted %g", r.feeToSHA256,
			r.feeToB2B, DefaultFee)
	}

	c.Spread = 0.012
	c.FeeToBLAKE2b = 0.008
	r = c.resolve()
	if r.feeToSHA256 != 0.012 || r.feeToB2B != 0.008 {
		t.Errorf("fees %g/%g, wanted 0.012/0.008", r.feeToSHA256,
			r.feeToB2B)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("per-direction fees refused: %v", err)
	}

	for _, bad := range []struct {
		name string
		set  func(*Config)
	}{
		{"negative", func(c *Config) { c.FeeToSHA256 = -0.01 }},
		{"a percentage typed as a whole number", func(c *Config) {
			c.FeeToBLAKE2b = 1.5
		}},
		{"under the routing budget", func(c *Config) {
			c.FeeToSHA256 = 0.002
		}},
		{"an unknown rate source", func(c *Config) {
			c.RateSource = "coinbase"
		}},
	} {
		c := usable()
		bad.set(&c)
		if err := c.Validate(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: accepted (%v)", bad.name, err)
		}
	}
}

// The discounted direction never goes below its routing budget: at the most
// discount, the default discount took a 1% fee to 0.2%, under the 0.3%
// budget, and the quote refused the swaps that would have refilled the bridge.
func TestTheDiscountStaysAboveTheRoutingBudget(t *testing.T) {
	t.Parallel()

	for _, fee := range []float64{0.004, 0.01, DefaultFee, 0.05, 0.19} {
		c := usable()
		c.Spread = fee
		r := c.resolve()
		inv := r.inventoryFor(fee)
		if err := inv.Valid(); err != nil {
			t.Errorf("fee %g: %v", fee, err)
		}
		least := inv.BaseSpread - inv.MaxDiscount
		if least <= r.quote.FeeFraction {
			t.Errorf("fee %g discounts to %g, not above the %g "+
				"routing budget", fee, least, r.quote.FeeFraction)
		}
	}
}

// The source: the market by default, the operator's own on request.
func TestTheRateSourceDefaultsToTheMarket(t *testing.T) {
	t.Parallel()

	c := usable()
	c.RateSource = ""
	if got := c.resolve().rateSource; got != RateSourceNeoxa {
		t.Errorf("default source %q", got)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("a fixed rate beside the default source must not stop "+
			"the node: %v", err)
	}
	c.RateSource = RateSourceFixed
	if got := c.resolve().rateSource; got != RateSourceFixed {
		t.Errorf("source %q", got)
	}
}

// Scaling one field of the inventory policy and leaving the rest breaks three
// of its invariants at once for any spread below the default. Every spread the
// config accepts has to produce a policy the inventory package will take.
func TestEveryAcceptedSpreadProducesAUsablePolicy(t *testing.T) {
	t.Parallel()

	for _, spread := range []float64{
		0.003, 0.005, 0.01, 0.02, 0.05, 0.1, 0.19,
	} {
		c := usable()
		c.Spread = spread

		if err := c.Validate(); err != nil {
			t.Errorf("a spread of %g was refused: %v", spread, err)

			continue
		}

		r := c.resolve()
		if err := r.inventory.Valid(); err != nil {
			t.Errorf("a spread of %g validated but gives an "+
				"unusable inventory policy: %v", spread, err)
		}
		if !r.rate.Valid() {
			t.Errorf("a spread of %g gives an unusable rate policy",
				spread)
		}
	}
}

// A spread is a fraction. Someone who means one percent and types 1 has
// configured a bridge that keeps everything, so that must be refused with the
// units spelled out rather than accepted.
func TestAnImplausibleSpreadIsRefusedAsAUnitsMistake(t *testing.T) {
	t.Parallel()

	for _, spread := range []float64{0.25, 1, 5} {
		c := usable()
		c.Spread = spread

		err := c.Validate()
		if err == nil {
			t.Errorf("a spread of %g was accepted", spread)

			continue
		}
		if !strings.Contains(err.Error(), "fraction") {
			t.Errorf("the refusal should explain the units: %v",
				err)
		}
	}
}

// The default route budgets are the largest the margin policy carries within
// the incoming cap at target spacing: one block more and the direction would
// refuse every swap.
func TestTheCLTVBudgetsAreTheLargestThatFit(t *testing.T) {
	t.Parallel()

	for _, dir := range []struct {
		name   string
		budget uint32
		enable func(*Config)
	}{
		{"toSHA256", DefaultOutgoingCLTVLimitToSHA256, func(c *Config) {
			c.ToSHA256, c.ToBLAKE2b = true, false
		}},
		{"toBLAKE2b", DefaultOutgoingCLTVLimitToBLAKE2b, func(c *Config) {
			c.ToSHA256, c.ToBLAKE2b = false, true
		}},
	} {
		c := usable()
		dir.enable(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: the default budget %d refuses: %v", dir.name,
				dir.budget, err)
		}
		c.OutgoingCLTVLimit = dir.budget + 1
		if err := c.Validate(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %d blocks still fit, so %d is not the largest",
				dir.name, dir.budget+1, dir.budget)
		}
	}
	if DefaultMaxIncomingBlocks+payerRouteAllowance != 2016 {
		t.Error("the incoming cap no longer leaves the payer's route " +
			"inside lnd's 2016 blocks")
	}
}
