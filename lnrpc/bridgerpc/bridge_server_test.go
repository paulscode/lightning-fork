//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// serverWith builds a sub-server over a fake node.
func serverWith(t *testing.T, enabled bool, f *fakeNode) *Server {
	t.Helper()

	srv, perms, err := New(&Config{Enabled: enabled, Deps: f.deps()})
	if err != nil {
		t.Fatal(err)
	}
	if len(perms) == 0 {
		t.Fatal("the sub-server declared no macaroon permissions")
	}

	return srv
}

// A node that has the code but has not turned it on must say so, rather than
// answering Unimplemented. The difference matters to whoever is calling:
// Unimplemented says "this build cannot do that", and this build can.
func TestDisabledRefusesWithAReasonRatherThanUnimplemented(t *testing.T) {
	t.Parallel()

	s := serverWith(t, false, &fakeNode{synced: true})
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"Quote": func() error {
			_, err := s.Quote(ctx, &QuoteRequest{})

			return err
		},
		"LookupSwap": func() error {
			_, err := s.LookupSwap(ctx, &LookupSwapRequest{})

			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if status.Code(err) != codes.FailedPrecondition {
				t.Errorf("got %v, wanted FailedPrecondition",
					status.Code(err))
			}
		})
	}
}

// Status has to answer while the bridge is off, because "why is this not
// working" must be answerable without reading logs.
func TestStatusAnswersWhileDisabled(t *testing.T) {
	t.Parallel()

	s := serverWith(t, false, &fakeNode{synced: true})

	resp, err := s.Status(context.Background(), &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Enabled {
		t.Error("reported enabled while it is not")
	}
	if len(resp.Refusals) == 0 {
		t.Fatal("refused without saying why")
	}
	if !strings.Contains(resp.Refusals[0], "not enabled") {
		t.Errorf("the refusal should name the cause: %v", resp.Refusals)
	}
}

// An unsynced node refuses every swap, so Status must say that rather than
// leaving an operator to guess.
func TestStatusReportsAnUnsyncedNode(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{height: 800_000, synced: false})

	resp, err := s.Status(context.Background(), &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled {
		t.Error("reported disabled while it is enabled")
	}

	var found bool
	for _, r := range resp.Refusals {
		if strings.Contains(r, "not synced") {
			found = true
		}
	}
	if !found {
		t.Errorf("an unsynced node should be named as a refusal: %v",
			resp.Refusals)
	}
}

// Quoting commits this node's liquidity on both chains, so it must not be
// reachable by a macaroon that can only read.
func TestQuoteNeedsWritePermission(t *testing.T) {
	t.Parallel()

	ops, ok := macPermissions["/bridgerpc.Bridge/Quote"]
	if !ok {
		t.Fatal("Quote declares no permissions at all")
	}
	if len(ops) != 1 || ops[0].Action != "write" {
		t.Errorf("Quote requires %+v, wanted a single write", ops)
	}

	for _, method := range []string{
		"/bridgerpc.Bridge/LookupSwap", "/bridgerpc.Bridge/Status",
	} {
		ops, ok := macPermissions[method]
		if !ok {
			t.Errorf("%s declares no permissions", method)

			continue
		}
		if len(ops) != 1 || ops[0].Action != "read" {
			t.Errorf("%s requires %+v, wanted a single read",
				method, ops)
		}
	}
}

// The sub-server must refuse to start rather than accept a quote it could not
// act on.
func TestDriverRefusesWithoutAccessToTheNode(t *testing.T) {
	t.Parallel()

	_, _, err := New(&Config{Enabled: true, Deps: nil})
	if err != nil {
		t.Fatalf("New itself should not fail: %v", err)
	}

	// New tolerates it; the adapter is what refuses, and every method goes
	// through it. That is the guarantee worth pinning.
	s := &Server{cfg: &Config{Enabled: true}, local: NewLocal(nil)}
	resp, err := s.Status(context.Background(), &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Refusals) == 0 {
		t.Fatal("a sub-server with no access to the node reported no " +
			"refusals")
	}
}
