//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"errors"
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
			// There is no default rate on purpose: a wrong one
			// loses money on every swap and does it quietly.
			name: "no rate",
			edit: func(c *Config) { c.FixedRate = 0 },
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

	if r.rate.BaseSpread != 0.05 {
		t.Errorf("rate base spread %g, wanted the operator's 0.05",
			r.rate.BaseSpread)
	}
	if r.inventory.BaseSpread != 0.05 {
		t.Errorf("inventory base spread %g, wanted 0.05",
			r.inventory.BaseSpread)
	}
	if r.rate.MaxSpread < r.rate.BaseSpread {
		t.Error("the rate policy cannot widen past the posted fee")
	}
	if r.inventory.MaxSpread < r.inventory.BaseSpread {
		t.Error("the inventory policy cannot widen past the posted fee")
	}

	// A spread above the defaults' ceiling must raise the ceiling, not be
	// silently clamped down to it.
	c.Spread = 0.1
	r = c.resolve()
	if r.rate.MaxSpread < 0.1 || r.inventory.MaxSpread < 0.1 {
		t.Errorf("a large spread was clamped: rate %g, inventory %g",
			r.rate.MaxSpread, r.inventory.MaxSpread)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("a spread of 0.1 should still validate: %v", err)
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
