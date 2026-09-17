package bridgerpc

import (
	"os/exec"
	"strings"
	"testing"
)

// The bridge repository ships its own generated gRPC client, built from the
// same .proto files lnd uses. Linking it into this binary panics at startup,
// before any of it runs:
//
//	panic: proto: file "lightning.proto" is already registered
//	        previously from: ".../lightning-fork-bridge/lndrpc/lnrpc"
//	        currently from:  "github.com/lightningnetwork/lnd/lnrpc"
//
// protobuf's registry keys on the proto file path, so two packages registering
// lightning.proto cannot coexist. The whole integration rests on importing only
// the bridge's decision packages, which carry no generated client, and reaching
// nodes through lnd's own.
//
// That holds today by construction and nothing else enforces it. One import
// added in the wrong place, by someone who reasonably wants the client that is
// already written, turns every build of this sub-server into a node that dies
// at startup.
//
// Note what actually happens when the rule is broken: any test binary linking
// this package panics at init, before this check gets to run, so the panic
// above is the first thing anyone sees. That is a loud enough failure, and it
// names the two packages. What this test adds is the untagged case, which no
// panic covers because the module is not linked there at all, and a place to
// write down why the rule exists so that whoever hits the panic can find the
// reasoning instead of working around it.
var forbidden = []string{
	// Carries the generated client.
	"github.com/paulscode/lightning-fork-bridge/lndrpc",

	// The out-of-process adapters, which import it.
	"github.com/paulscode/lightning-fork-bridge/lnd",

	// The standalone daemon's wiring, which imports both.
	"github.com/paulscode/lightning-fork-bridge/bridged",
}

func TestNoGeneratedClientFromTheBridgeModule(t *testing.T) {
	t.Parallel()

	// Both configurations: untagged builds must not reach the bridge
	// module at all, and tagged ones must reach only its decision
	// packages.
	for _, tags := range []string{"", "bridgerpc"} {
		name := "untagged"
		if tags != "" {
			name = tags
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			args := []string{"list", "-deps"}
			if tags != "" {
				args = append(args, "-tags", tags)
			}
			args = append(args, ".")

			out, err := exec.Command("go", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("go %s: %v\n%s",
					strings.Join(args, " "), err, out)
			}

			for _, dep := range strings.Fields(string(out)) {
				for _, bad := range forbidden {
					if dep == bad ||
						strings.HasPrefix(dep, bad+"/") {

						t.Errorf("%s reaches %s, "+
							"which registers "+
							"lightning.proto and "+
							"will panic this "+
							"node at startup",
							name, dep)
					}
				}
			}
		})
	}
}

// An untagged build must not reach the bridge module at all.
//
// Stronger than the list above and worth asserting separately: the design
// claim is that a node which will never bridge carries none of this, not
// merely that it avoids three named packages. It is also the claim that keeps
// the module out of everyone else's dependency graph, which is the reason the
// sub-server is behind a tag in the first place.
func TestUntaggedBuildsCarryNoneOfTheBridgeModule(t *testing.T) {
	t.Parallel()

	const module = "github.com/paulscode/lightning-fork-bridge"

	for _, pkg := range []string{".", "../../cmd/lnd"} {
		out, err := exec.Command("go", "list", "-deps", pkg).
			CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
		}

		for _, dep := range strings.Fields(string(out)) {
			if dep == module || strings.HasPrefix(dep, module+"/") {
				t.Errorf("an untagged build of %s reaches %s, "+
					"so every node carries the bridge "+
					"module whether or not it will ever "+
					"bridge", pkg, dep)
			}
		}
	}
}

// And a tagged build must reach only the packages that decide things, never
// one that registers a proto file.
func TestTaggedBuildsReachOnlyTheDecisionPackages(t *testing.T) {
	t.Parallel()

	const module = "github.com/paulscode/lightning-fork-bridge"

	// The swap logic, and nothing that carries a generated client.
	want := map[string]bool{
		"chainrate": true, "driver": true, "inventory": true,
		"margin": true, "node": true, "quote": true, "rate": true,
		"runner": true, "store": true, "swap": true,
	}

	out, err := exec.Command(
		"go", "list", "-deps", "-tags", "bridgerpc", "../../cmd/lnd",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}

	for _, dep := range strings.Fields(string(out)) {
		if !strings.HasPrefix(dep, module+"/") {
			continue
		}

		name := strings.TrimPrefix(dep, module+"/")
		if !want[name] {
			t.Errorf("a tagged build reaches %s, which is not one "+
				"of the packages that decide things; if it "+
				"carries generated protobuf code this node "+
				"will panic at startup", dep)
		}
	}
}
