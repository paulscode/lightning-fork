package lnwallet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// deriving lists the calls that turn a channel type into a hash type. A
// signing site must use one of these rather than naming a hash type itself.
var deriving = map[string]struct{}{
	"CommitSigHashType": {},
	"HtlcSigHashType":   {},
	"sweepSigHash":      {},
}

// derivedLocals are variables this package assigns from one of the above and
// then passes to a sign descriptor. TestSigHashLocalsAreDerived checks that
// they really are assigned that way, so naming them here is not a loophole.
var derivedLocals = map[string]struct{}{
	"sigHashType": {},
}

// Every signature this node makes for a channel takes its hash type from the
// channel type, never from the signing site. That is what lets one channel be
// bound to this chain and another not, and it is the only way a spend made
// long after a channel closes still signs the way the channel agreed.
//
// This is a source-level test, which is unusual, and it is here because the
// behavioural tests cannot see the problem. Forcing the funding-time
// commitment signature to a literal SIGHASH_ALL leaves every test in this
// repository passing: two nodes of the same build agree on the wrong value and
// never notice. What it produces is a channel whose *first* commitment is not
// bound to this chain, which is the one that gets broadcast if the channel is
// funded and never used, and a node that cannot open channels with Core
// Lightning at all.
//
// The funding path is the one that gets missed, because it signs the first
// commitment somewhere other than where every later commitment is signed.
// lightning-blake2b/bolts#1 calls this out by name: "MUST apply this to every
// signature it makes for the channel, including the first commitment
// signature exchanged while funding, which some implementations sign on a path
// separate from later commitments", and "MUST take the hash type from the
// channel type rather than from each signing site". This test is that rule.
//
// A new signing path is the real risk, which is why the test looks at every
// HashType in these files rather than at the handful that exist today.
func TestSigningSitesTakeTheHashTypeFromTheChannel(t *testing.T) {
	t.Parallel()

	// commitment.go is where the deriving functions live, so it is the one
	// place a literal hash type belongs.
	for _, file := range []string{"wallet.go", "channel.go"} {
		file := file
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, nil, 0)
			require.NoError(t, err)

			var found int
			ast.Inspect(f, func(n ast.Node) bool {
				kv, ok := n.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "HashType" {
					return true
				}

				found++
				where := fset.Position(kv.Pos())
				require.True(t, derivesFromChannel(kv.Value),
					"%v: HashType is not taken from the "+
						"channel type. A signature "+
						"that names its own hash type "+
						"is not bound to the channel "+
						"that agreed it, and no "+
						"behavioural test in this "+
						"repository can see the "+
						"difference", where)

				return true
			})

			// If the field is ever renamed, this test would pass by
			// inspecting nothing at all.
			require.NotZero(t, found, "no HashType assignments "+
				"found in %v; has the field been renamed?",
				file)
		})
	}
}

// derivesFromChannel reports whether a HashType value comes from the channel
// type rather than being named at the signing site.
func derivesFromChannel(v ast.Expr) bool {
	switch e := v.(type) {
	case *ast.CallExpr:
		fn, ok := e.Fun.(*ast.Ident)
		if !ok {
			return false
		}
		_, ok = deriving[fn.Name]

		return ok

	case *ast.Ident:
		_, ok := derivedLocals[e.Name]

		return ok
	}

	return false
}

// The locals allowed above have to earn it. Each must be assigned from one of
// the deriving calls and from nothing else, or naming it in derivedLocals
// would be a way to smuggle a literal hash type past the test above.
func TestSigHashLocalsAreDerived(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "channel.go", nil, 0)
	require.NoError(t, err)

	assignments := make(map[string]int)
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}

		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if _, ok := derivedLocals[id.Name]; !ok {
				continue
			}
			if i >= len(as.Rhs) {
				continue
			}

			assignments[id.Name]++
			require.True(t, derivesFromChannel(as.Rhs[i]),
				"%v: %v is assigned something other than a "+
					"hash type derived from the channel",
				fset.Position(as.Pos()), id.Name)
		}

		return true
	})

	for name := range derivedLocals {
		require.NotZero(t, assignments[name], "%v is allowed as a "+
			"derived local but is never assigned in channel.go, so "+
			"the allowance is stale", name)
	}
}
