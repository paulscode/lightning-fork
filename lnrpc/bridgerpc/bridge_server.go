//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// quoteTimeout bounds a quote, which talks to both nodes.
//
// Generous, because it covers decoding on two nodes and creating a hold
// invoice, and the caller's own deadline ends their wait sooner. What it
// exists for is the swap: the work has to finish or fail rather than be
// abandoned halfway.
const quoteTimeout = 60 * time.Second

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize this as the name of the
	// config file that we need.
	subServerName = "BridgeRPC"
)

var (
	// macPermissions maps RPC calls to the permissions they require.
	//
	// Quoting a swap commits this node's own liquidity on both chains, so
	// it needs "offchain write" rather than an invoice permission: the
	// caller is not asking to be paid, they are asking this node to pay
	// someone on their behalf and to hold funds against it. A macaroon that
	// can only read must not be able to start one.
	macPermissions = map[string][]bakery.Op{
		"/bridgerpc.Bridge/Quote": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/bridgerpc.Bridge/LookupSwap": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/bridgerpc.Bridge/Status": {{
			Entity: "offchain",
			Action: "read",
		}},
	}
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	BridgeServer
}

// Server is a sub-server of the main RPC server: it serves the bridge API.
type Server struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedBridgeServer

	cfg *Config

	// mu guards svc, which Start publishes and the methods read.
	mu sync.RWMutex

	// local is this node, as both halves of a swap.
	local *Local

	// remote is the SHA256 node, and conn the connection to it.
	remote *Remote
	conn   *grpc.ClientConn

	// svc is the running bridge. Nil while the bridge is disabled or
	// before Start has wired it, which every method checks: a quote
	// answered without it would be a promise with nothing behind it.
	//
	// Read through service, never directly.
	svc *service
}

// service is the running bridge, or nil if it is not up.
func (s *Server) service() *service {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.svc
}

// A compile time check to ensure that Server fully implements the
// BridgeServer gRPC service.
var _ BridgeServer = (*Server)(nil)

// New returns a new instance of the bridgerpc Bridge sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	return &Server{
		cfg:   cfg,
		local: NewLocal(cfg.Deps),
	}, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	if atomic.AddInt32(&s.started, 1) != 1 {
		return nil
	}

	if !s.cfg.Enabled {
		// Registered but not serving. The methods refuse with a
		// reason, which is a better answer than an unimplemented
		// error: an operator who has not turned this on should be told
		// so, not left wondering whether their build has it.
		log.Infof("Bridge is compiled in but not enabled")

		return nil
	}

	conn, dialErr := dialSHA256Node(s.cfg)
	if dialErr != nil {
		return dialErr
	}
	s.conn = conn
	s.remote = NewRemote(conn)

	// Both nodes have to answer before anything is served. Dialling
	// succeeds against a node that is not there, so a wrong address, a
	// wrong macaroon or a node that is down has to be found here rather
	// than by a swap that has already accepted someone's money. That is a
	// refusal to start: a bridge that cannot reach one of its two nodes
	// cannot honour a quote, and starting anyway advertises one.
	//
	// Being behind the chain is deliberately not part of this. Every node
	// is behind for a while after it starts, so refusing on that would
	// make the daemon unbootable on every restart, and this one runs
	// inside the node it is checking. It is already refused where it
	// counts: no swap is sized against a height that may be stale. Say so
	// and carry on.
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	local, err := s.local.Reachable(ctx)
	if err != nil {
		_ = conn.Close()

		return fmt.Errorf("the bridge cannot use this node: %w", err)
	}
	remote, err := s.remote.Reachable(ctx)
	if err != nil {
		_ = conn.Close()

		return fmt.Errorf("the bridge cannot use the SHA256 node at "+
			"%s: %w", s.cfg.SHA256RPCHost, err)
	}
	if !local.SyncedToChain || !remote.SyncedToChain {
		log.Infof("Bridge will refuse to quote until both nodes catch "+
			"up (this node synced=%v at height %d, SHA256 node "+
			"synced=%v at height %d)", local.SyncedToChain,
			local.Height, remote.SyncedToChain, remote.Height)
	}

	svc, err := newService(s.cfg, s.local, s.remote)
	if err != nil {
		_ = conn.Close()

		return err
	}
	// Started before it is published, so a caller that reaches Quote the
	// instant the RPC server opens cannot find a service whose context is
	// not set yet.
	svc.start()

	s.mu.Lock()
	s.svc = svc
	s.mu.Unlock()

	log.Infof("Bridge is up, serving %d direction(s) through the SHA256 "+
		"node at %s", len(svc.sides), s.cfg.SHA256RPCHost)

	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	// Swaps in flight are waited for before the connection they are
	// talking over is closed. The node tears down the invoice registry and
	// the router after this returns, and a swap still driving would find
	// them gone mid-HTLC.
	if svc := s.service(); svc != nil {
		svc.stop()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}

	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until
// this is called, each sub-server won't be able to have requests routed
// towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterBridgeServer(grpcServer, r)

	log.Debugf("Bridge RPC server successfully registered with root " +
		"gRPC server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterBridgeHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Bridge REST server "+
			"with root REST server: %v", err)

		return err
	}

	log.Debugf("Bridge REST server successfully registered with " +
		"root REST server")

	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for
// all methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(
	configRegistry lnrpc.SubServerConfigDispatcher) (lnrpc.SubServer,
	lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.BridgeServer = subServer

	return subServer, macPermissions, nil
}

// errDisabled is what every method answers with while the operator has not
// turned the bridge on.
//
// FailedPrecondition rather than Unimplemented, because the difference matters
// to whoever is calling: Unimplemented says "this build cannot do that", and
// this build can.
func errDisabled() error {
	return status.Error(codes.FailedPrecondition, "the bridge is not "+
		"enabled on this node; set bridgerpc.enabled to offer swaps")
}

// Quote asks what a swap would cost and creates the hold invoice to pay for it.
//
// The swap begins being driven before this returns. That is deliberate: the
// hold invoice exists the moment the quote does, so somebody can pay it
// immediately, and a swap nobody is watching between the quote and the first
// poll is a swap whose incoming HTLC could lock in unobserved.
func (s *Server) Quote(ctx context.Context, req *QuoteRequest) (*QuoteResponse,
	error) {

	svc := s.service()
	if !s.cfg.Enabled || svc == nil {
		return nil, errDisabled()
	}

	// A quote taken while the node is going down creates a hold invoice
	// that will not be driven until the next start. The journal means it
	// is picked up rather than lost, but promising a swap on the way out
	// is still worse than declining one.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		return nil, status.Error(codes.Unavailable, "the bridge is "+
			"shutting down and is not taking new swaps")
	}
	if req.GetInvoice() == "" {
		return nil, status.Error(codes.InvalidArgument, "no invoice "+
			"to pay")
	}

	// Deliberately not the caller's context.
	//
	// Quoting creates a hold invoice on this node and records the swap.
	// A caller who hangs up partway through would otherwise abandon it
	// between those two, leaving the node holding an invoice the journal
	// has no record of. The timeout is what bounds this instead, and the
	// caller's own deadline still ends their wait.
	ctx, cancel := context.WithTimeout(svc.ctx, quoteTimeout)
	defer cancel()

	sd, _, err := svc.route(ctx, req.GetInvoice())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	q, err := sd.quoter.Quote(ctx, req.GetInvoice())
	if err != nil {
		// A refusal is the bridge declining to promise something, not
		// a fault: too large, too small, not enough left on the paying
		// side, or a chain it cannot currently measure. The caller
		// gets the reason.
		if errors.Is(err, quote.ErrRefused) {
			return nil, status.Error(
				codes.FailedPrecondition, err.Error(),
			)
		}

		return nil, status.Error(codes.Internal, err.Error())
	}

	svc.drive(sd, q.Hash)

	return &QuoteResponse{
		HoldInvoice:  q.HoldInvoice,
		Hash:         q.Hash[:],
		IncomingMsat: q.IncomingMsat,
		OutgoingMsat: q.OutgoingMsat,
		Rate:         q.Rate,
		Spread:       q.Spread,
		CltvDelta:    q.CLTVDelta,
		Direction:    sd.name,
		ExpiresAt:    q.Expires.Unix(),
	}, nil
}

// LookupSwap reports what the bridge believes about a swap.
func (s *Server) LookupSwap(ctx context.Context, req *LookupSwapRequest) (
	*Swap, error) {

	svc := s.service()
	if !s.cfg.Enabled || svc == nil {
		return nil, errDisabled()
	}
	if len(req.GetHash()) != len(node.Hash{}) {
		return nil, status.Errorf(codes.InvalidArgument, "the hash "+
			"must be %d bytes, got %d", len(node.Hash{}),
			len(req.GetHash()))
	}

	var hash node.Hash
	copy(hash[:], req.GetHash())

	rec, err := svc.journal.Get(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "no swap "+
				"with hash %x", hash)
		}

		return nil, status.Error(codes.Internal, err.Error())
	}

	out := &Swap{
		Hash:         rec.Hash[:],
		State:        rec.State.String(),
		IncomingMsat: rec.IncomingMsat,
		OutgoingMsat: rec.OutgoingMsat,
	}

	// Only once there is one. An all-zero preimage settles nothing, and
	// returning it would look like proof the destination was paid.
	if rec.Preimage != (node.Preimage{}) {
		out.Preimage = rec.Preimage[:]
	}

	return out, nil
}

// Status reports whether this node is in a position to serve swaps.
//
// It answers even while the bridge is disabled or refusing everything, which
// is the case it exists for: "why is this not working" must be answerable
// without reading logs.
func (s *Server) Status(ctx context.Context, _ *StatusRequest) (
	*StatusResponse, error) {

	resp := &StatusResponse{Enabled: s.cfg.Enabled}

	if !s.cfg.Enabled {
		resp.Refusals = append(resp.Refusals, "the bridge is not "+
			"enabled on this node")

		return resp, nil
	}

	// Whether each node can be used at all comes first, and runs even when
	// the bridge failed to start, because an unsynced or unreachable node
	// is frequently the reason it did.
	if err := s.local.Check(ctx); err != nil {
		resp.Refusals = append(resp.Refusals, err.Error())
	}
	if s.remote != nil {
		if err := s.remote.Check(ctx); err != nil {
			resp.Refusals = append(resp.Refusals, err.Error())
		}
	}

	svc := s.service()
	if svc == nil {
		resp.Refusals = append(resp.Refusals, "the bridge is enabled "+
			"but did not start; see the node's log")

		return resp, nil
	}

	for _, sd := range svc.sides {
		resp.Directions = append(resp.Directions, sd.name)
	}
	for _, name := range svc.disabled {
		resp.Refusals = append(resp.Refusals, name+" is configured "+
			"but not enabled")
	}
	resp.SwapsInFlight = uint32(svc.active())

	// A chain the bridge cannot currently measure is a chain it cannot
	// size an HTLC against, so every swap touching it is refused. That
	// takes a few blocks from each chain after a restart, which looks
	// identical to being broken unless it is said.
	if _, err := svc.spacing(false)(ctx); err != nil {
		resp.Refusals = append(resp.Refusals, "still measuring block "+
			"spacing, which takes a few blocks from each chain: "+
			err.Error())
	}

	resp.Refusals = append(resp.Refusals, svc.liquidityRefusals(ctx)...)

	return resp, nil
}
