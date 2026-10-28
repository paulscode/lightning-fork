//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
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

// While it starts, a SHA256 node that runs out of time over GetInfo is said to
// be slow, with why, not a node that does not answer: waited on as starting
// when this node runs it, and retried at the usual pace when it does not.
func TestStartSaysTheSHA256NodeIsSlow(t *testing.T) {
	slow := fmt.Errorf("SHA256 node: reading the SHA256 node's tip: %w",
		status.Error(codes.DeadlineExceeded,
			"context deadline exceeded"))

	srv, _, err := New(&Config{
		Enabled: true, ToSHA256: true, SHA256RPCHost: "x:10009",
	})
	require.NoError(t, err)
	err = srv.slowSha256(slow)
	require.NotErrorIs(t, err, errSha256Pending)
	require.ErrorContains(t, err, "the bridge cannot use the SHA256 "+
		"node at x:10009 yet: the SHA256 Lightning node did not "+
		"answer GetInfo within 1m0s; lnd answers it only after "+
		"asking its chain node (bitcoind)")

	require.ErrorIs(t, err, slow)

	sup, _ := newTestSupervisor(t, &fakeSha256Node{})
	srv.sup = sup
	err = srv.slowSha256(slow)
	require.ErrorIs(t, err, errSha256Pending)
	state, detail := sup.report()
	require.Equal(t, sha256Starting, state)
	require.Contains(t, detail, "is running but did not answer GetInfo")
}

// The waits on the SHA256 node's GetInfo while the bridge starts are a minute
// each, and end when the bridge stops, so stopping is no slower for them.
func TestStartGivesGetInfoAMinuteUntilStop(t *testing.T) {
	main := &deadlineMain{fakeMain: &fakeMain{
		info: &lnrpc.GetInfoResponse{BlockHeight: 1},
	}}
	r := remote(nil, nil, nil)
	r.main = main

	_, err := r.Reachable(context.Background())
	require.NoError(t, err)
	require.Greater(t, main.left, 55*time.Second)

	main.left = 0
	_ = r.CheckChain(context.Background(), "regtest", "")
	require.Greater(t, main.left, 55*time.Second)

	srv, _, err := New(&Config{Enabled: true, ToSHA256: true})
	require.NoError(t, err)
	srv.quit = make(chan struct{})
	ctx, cancel := srv.untilQuit(time.Hour)
	defer cancel()
	close(srv.quit)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end with the bridge")
	}
}

// deadlineMain records how long the caller gave GetInfo.
type deadlineMain struct {
	*fakeMain
	left time.Duration
}

func (d *deadlineMain) GetInfo(ctx context.Context, in *lnrpc.GetInfoRequest,
	opts ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	if dl, ok := ctx.Deadline(); ok {
		d.left = time.Until(dl)
	}

	return d.fakeMain.GetInfo(ctx, in, opts...)
}

// Status says how often the bridge really tries again: every ten seconds
// while the SHA256 node is on its way, which the platforms' health checks read
// as starting.
func TestStatusSaysHowOftenItTries(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	s.setStartErr(fmt.Errorf("%w: the SHA256 Lightning node is slow",
		errSha256Pending))
	resp, err := s.Status(context.Background(), &StatusRequest{})
	require.NoError(t, err)
	require.Contains(t, strings.Join(resp.Refusals, "\n"),
		"cannot start, and tries again every 10s: the SHA256 "+
			"Lightning node is not ready yet: the SHA256 "+
			"Lightning node is slow")

	s.setStartErr(errors.New("broken"))
	resp, err = s.Status(context.Background(), &StatusRequest{})
	require.NoError(t, err)
	require.Contains(t, strings.Join(resp.Refusals, "\n"),
		"tries again every 1m0s: broken")
}

// A connected SHA256 node that runs out of time over GetInfo is said to be
// slow, with why, in the summary and the refusals both, and asked once: the
// platforms' health checks give Status 25 seconds.
func TestStatusSaysTheSHA256NodeIsSlow(t *testing.T) {
	t.Parallel()

	main := &countingMain{fakeMain: &fakeMain{
		infoErr: status.Error(codes.DeadlineExceeded,
			"context deadline exceeded"),
	}}
	r := remote(nil, nil, nil)
	r.main = main
	s := serverWith(t, true, &fakeNode{synced: true})
	s.mu.Lock()
	s.remote = r
	s.mu.Unlock()

	resp, err := s.Status(context.Background(), &StatusRequest{})
	require.NoError(t, err)
	slow := "the SHA256 Lightning node did not answer GetInfo within " +
		"10s; lnd answers it only after asking its chain node " +
		"(bitcoind), which may be busy, restarting or overloaded"
	require.Equal(t, slow, resp.Sha256Node.GetDetail())
	require.Contains(t, resp.Refusals, slow)
	require.Equal(t, 1, main.infos)
}

// countingMain counts GetInfo calls.
type countingMain struct {
	*fakeMain
	infos int
}

func (c *countingMain) GetInfo(ctx context.Context, in *lnrpc.GetInfoRequest,
	opts ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	c.infos++

	return c.fakeMain.GetInfo(ctx, in, opts...)
}
