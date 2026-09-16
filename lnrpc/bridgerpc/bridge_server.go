//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"sync/atomic"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

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

	// local is this node, as both halves of a swap.
	local *Local
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
		// Registered but not serving. The methods below refuse, which
		// is a better answer than an unimplemented error: an operator
		// who has not turned this on should be told so, not left
		// wondering whether their build has it.
		log.Infof("Bridge is compiled in but not enabled")

		return nil
	}

	log.Infof("Bridge starting")

	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
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
func (s *Server) Quote(_ context.Context, _ *QuoteRequest) (*QuoteResponse,
	error) {

	if !s.cfg.Enabled {
		return nil, errDisabled()
	}

	// The swap logic, the rate oracle and the Bitcoin-side connection are
	// not wired up yet. Refusing is the only honest answer: a quote
	// commits this node's money, and there is nothing here that could
	// honour one.
	return nil, status.Error(codes.Unimplemented, "quoting is not wired "+
		"up yet")
}

// LookupSwap reports what the bridge believes about a swap.
func (s *Server) LookupSwap(_ context.Context, _ *LookupSwapRequest) (*Swap,
	error) {

	if !s.cfg.Enabled {
		return nil, errDisabled()
	}

	return nil, status.Error(codes.Unimplemented, "swap lookup is not "+
		"wired up yet")
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

	// Whether this node can be used at all is worth reporting before
	// anything about the other side: an unsynced node refuses every swap,
	// and the reason belongs here rather than in a log.
	if err := s.local.Check(ctx); err != nil {
		resp.Refusals = append(resp.Refusals, err.Error())
	}

	resp.Refusals = append(resp.Refusals, "the Bitcoin side is not wired "+
		"up yet, so no direction can be served")

	return resp, nil
}
