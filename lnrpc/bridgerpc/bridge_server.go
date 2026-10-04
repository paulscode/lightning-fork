//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
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
		"/bridgerpc.Bridge/Info": {{
			Entity: "offchain",
			Action: "read",
		}},
		// Changing the rate changes what every later swap costs, and a
		// wrong one pays out the SHA256 node's funds for next to
		// nothing. Paying permission is not enough for that: wallets
		// and apps hold it, and none of them should be able to set the
		// price this node sells at. Making macaroons is a permission
		// only the operator's own (admin) macaroon carries.
		// Participants' macaroons are scoped to the payer calls by URI
		// and never reach this.
		// The SHA256 node's seed controls its funds, so only a
		// macaroon that could already move this node's may see it.
		"/bridgerpc.Bridge/ExportSha256Seed": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}, {
			Entity: "macaroon",
			Action: "generate",
		}, {
			Entity: "signer",
			Action: "generate",
		}},
		"/bridgerpc.Bridge/SetRate": {{
			Entity: "offchain",
			Action: "write",
		}, {
			Entity: "macaroon",
			Action: "generate",
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

	// startErr is why the bridge is enabled and not up, while it keeps
	// trying. Guarded by mu.
	startErr error

	// sup prepares the SHA256 node when this node runs it for the bridge
	// (bridgerpc.sha256.supervised), and is nil otherwise.
	sup *supervisor

	// rateMu serialises SetRate with the service coming up; see connect.
	rateMu sync.Mutex

	// info is the last Info answer and when it was made, shared for
	// infoFresh. Guarded by infoMu, which also lets one caller at a time
	// make a new one.
	infoMu sync.Mutex
	info   *InfoResponse
	infoAt time.Time

	// quit ends the retry loop, and wg waits for it.
	quit chan struct{}
	wg   sync.WaitGroup
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
	s := &Server{
		cfg:   cfg,
		local: NewLocal(cfg.Deps),
	}
	if cfg.Enabled && cfg.Supervised {
		s.sup = newSupervisor(cfg)
	}

	return s, macPermissions, nil
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

	// A bridge that cannot come up must not keep the node from coming up.
	// lnd aborts its whole start when a sub-server's Start fails, and the
	// usual reason this one would is the SHA256 node being down or
	// misconfigured: taking the operator's own Lightning node offline over
	// that would be the wrong way round. So try now, and if that fails,
	// say why in Status and the log and keep trying in the background.
	//
	// The first try runs in the background too: reaching a SHA256 node that
	// does not answer takes the dial timeout and the chain check's, and
	// the node's own start should not wait on either.
	s.quit = make(chan struct{})
	s.setStartErr(errStarting)
	s.wg.Add(1)
	go s.retry()

	return nil
}

// errStarting is why the bridge is not up before its first try has finished.
var errStarting = errors.New("still connecting to both nodes")

// retryInterval is how often a bridge that could not start tries again.
const retryInterval = time.Minute

// pendingRetryInterval is how often it tries while a supervised SHA256 node is
// on its way up. Those steps follow one another within seconds, and a minute
// between each would make a first start take several for no reason.
const pendingRetryInterval = 10 * time.Second

// retry brings the bridge up, trying at once and then every retryInterval
// until it is up or the node stops.
func (s *Server) retry() {
	defer s.wg.Done()

	for first := true; ; first = false {
		err := s.connect()
		s.setStartErr(err)

		wait := retryInterval
		pending := errors.Is(err, errSha256Pending)
		if pending {
			wait = pendingRetryInterval
		}
		switch {
		case err == nil:
			return
		case pending:
			log.Infof("Bridge waiting for its SHA256 node: %v", err)
		case first:
			log.Warnf("Bridge did not start, retrying every %v: %v",
				retryInterval, err)
		default:
			log.Debugf("Bridge still cannot start: %v", err)
		}

		select {
		case <-s.quit:
			return
		case <-time.After(wait):
		}
	}
}

// setStartErr records why the bridge is not up, or clears it.
func (s *Server) setStartErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.startErr = err
}

// connect dials the SHA256 node, checks both nodes, and starts the bridge. On
// any failure nothing is left running or open.
func (s *Server) connect() error {
	// A supervised node is brought to where it can be used first: wallet
	// created from the derived seed, macaroon baked, identity checked.
	// Until that holds there is nothing to dial.
	if s.sup != nil {
		// Ends with the node too: preparing can wait minutes for a
		// node catching up, and stopping this one must not wait on it.
		ctx, cancel := context.WithTimeout(
			context.Background(), sha256ReadyWait+dialTimeout,
		)
		done := make(chan struct{})
		go func() {
			select {
			case <-s.quit:
				cancel()
			case <-done:
			}
		}()
		err := s.sup.prepare(ctx)
		close(done)
		cancel()
		if err != nil {
			return err
		}
	}

	conn, dialErr := dialSHA256Node(s.cfg)
	if dialErr != nil {
		return dialErr
	}
	remoteNode := NewRemote(conn)

	// Both nodes have to answer before anything is served. Dialling
	// succeeds against a node that is not there, so a wrong address, a
	// wrong macaroon or a node that is down has to be found here rather
	// than by a swap that has already accepted someone's money. A bridge
	// that cannot reach one of its two nodes cannot honour a quote, and
	// serving anyway advertises one.
	//
	// Being behind the chain is deliberately not part of this. Every node
	// is behind for a while after it starts, so refusing on that would
	// keep the bridge down on every restart, and this one runs inside the
	// node it is checking. It is already refused where it counts: no swap
	// is sized against a height that may be stale. Say so and carry on.
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	local, err := s.local.Reachable(ctx)
	if err != nil {
		_ = conn.Close()

		return fmt.Errorf("the bridge cannot use this node: %w", err)
	}
	remote, err := remoteNode.Reachable(ctx)
	if err != nil {
		_ = conn.Close()

		return fmt.Errorf("the bridge cannot use the SHA256 node at "+
			"%s: %w", s.cfg.SHA256RPCHost, err)
	}
	var network string
	if s.cfg.Deps != nil {
		network = s.cfg.Deps.Network
	}
	if err := remoteNode.CheckChain(ctx, network,
		s.local.NodeKey()); err != nil {

		_ = conn.Close()

		return fmt.Errorf("the bridge will not use the SHA256 node at "+
			"%s: %w", s.cfg.SHA256RPCHost, err)
	}
	if s.cfg.Deps != nil && s.cfg.Deps.Blake2bActivation != nil {
		height, hash, strict, err := s.cfg.Deps.Blake2bActivation(ctx)
		if err == nil {
			err = remoteNode.CheckNotBlake2b(
				ctx, height, hash, strict,
			)
		}
		if err != nil {
			_ = conn.Close()

			return fmt.Errorf("the bridge will not use the SHA256 "+
				"node at %s: %w", s.cfg.SHA256RPCHost, err)
		}
	}
	// Channels a restored node left in its channel backup come back now
	// that it is known to follow the SHA256 chain. In the background:
	// a peer that cannot be reached only delays its own channel, and is
	// no reason to keep the bridge down.
	if s.sup != nil {
		s.sup.startRestore(s.quit)
	}
	if !local.SyncedToChain || !remote.SyncedToChain {
		log.Infof("Bridge will refuse to quote until both nodes catch "+
			"up (this node synced=%v at height %d, SHA256 node "+
			"synced=%v at height %d)", local.SyncedToChain,
			local.Height, remote.SyncedToChain, remote.Height)
	}

	// rateMu is held from reading the rate file until the service that
	// read it is published, so a rate set in between cannot be written to
	// the file after it was read and then be lost to the running service.
	s.rateMu.Lock()
	svc, err := newService(s.cfg, s.local, remoteNode)
	if err != nil {
		s.rateMu.Unlock()
		_ = conn.Close()

		return err
	}
	// Started before it is published, so a caller that reaches Quote the
	// instant it is published cannot find a service whose context is not
	// set yet.
	svc.start()

	s.mu.Lock()
	s.conn, s.remote, s.svc = conn, remoteNode, svc
	s.mu.Unlock()
	s.rateMu.Unlock()

	log.Infof("Bridge is up, serving %d direction(s) through the SHA256 "+
		"node at %s", len(svc.sides), s.cfg.SHA256RPCHost)

	return nil
}

// remoteNode is the SHA256 node once the bridge has connected, or nil.
func (s *Server) remoteNode() *Remote {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.remote
}

// rebuildForRate brings the bridge up again once a rate has been set for the
// first time, so the direction held back for want of one starts.
//
// Safe to do with no thought for swaps: the bridge that is replaced had no rate
// for its whole life, so it never quoted one. Anything it resumed from the
// journal is waited for by stop and picked up again by the new one, as at any
// restart. It runs in the background so SetRate answers at once.
func (s *Server) rebuildForRate() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.mu.Lock()
		svc, conn := s.svc, s.conn
		s.svc, s.remote, s.conn = nil, nil, nil
		s.mu.Unlock()
		s.setStartErr(errStarting)

		if svc != nil {
			svc.stop()
		}
		if conn != nil {
			_ = conn.Close()
		}
		if atomic.LoadInt32(&s.shutdown) != 0 {
			return
		}

		log.Infof("Bridge restarting now that a rate is set, to bring " +
			"up toBLAKE2b")
		s.wg.Add(1)
		go s.retry()
	}()
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	// The retry loop first, so it cannot start a bridge while this one is
	// being stopped.
	if s.quit != nil {
		close(s.quit)
	}
	s.wg.Wait()

	// Swaps in flight are waited for before the connection they are
	// talking over is closed. The node tears down the invoice registry and
	// the router after this returns, and a swap still driving would find
	// them gone mid-HTLC.
	if svc := s.service(); svc != nil {
		svc.stop()
	}
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn != nil {
		_ = conn.Close()
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
	return coded(codes.FailedPrecondition, "disabled", "the bridge is "+
		"not enabled on this node; set bridgerpc.enabled to offer swaps")
}

// notServing is the answer while the bridge cannot serve: disabled, or
// enabled and not up yet. The second is temporary, and says so: a payer reads
// "disabled" as for good.
func (s *Server) notServing() error {
	if !s.cfg.Enabled {
		return errDisabled()
	}

	return coded(codes.Unavailable, "unavailable", "the bridge is "+
		"starting, or cannot reach one of its nodes yet and tries "+
		"again every minute; lncli bridge status says why")
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
		return nil, s.notServing()
	}

	// A quote taken while the node is going down creates a hold invoice
	// that will not be driven until the next start. The journal means it
	// is picked up rather than lost, but promising a swap on the way out
	// is still worse than declining one.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		return nil, coded(codes.Unavailable, "unavailable", "the "+
			"bridge is shutting down and is not taking new swaps")
	}
	if req.GetInvoice() == "" {
		return nil, coded(codes.InvalidArgument, "invalid_request",
			"no invoice to pay")
	}

	// Read from the caller's context before it is replaced below.
	who := participantOf(ctx)

	// Deliberately not the caller's context.
	//
	// Quoting creates a hold invoice on this node and records the swap.
	// A caller who hangs up partway through would otherwise abandon it
	// between those two, leaving the node holding an invoice the journal
	// has no record of. The timeout is what bounds this instead, and the
	// caller's own deadline still ends their wait.
	ctx, cancel := context.WithTimeout(svc.ctx, quoteTimeout)
	defer cancel()

	sd, dec, err := svc.route(ctx, req.GetInvoice())
	if err != nil {
		return nil, refusal(err)
	}

	q, err := sd.quoter.QuoteFor(ctx, who, req.GetInvoice())
	if err != nil {
		// The record exists and the hold invoice may: drive it, so it
		// either catches up or expires instead of holding its amount
		// against the headroom until a restart.
		if isUntracked(err) {
			svc.drive(sd, dec.Hash)
		}

		// A refusal is the bridge declining to promise something, not
		// a fault: too large, too small, not enough left on the paying
		// side, or a chain it cannot currently measure. The caller
		// gets the reason, with a code to branch on.
		return nil, refusal(err)
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
		return nil, s.notServing()
	}
	if len(req.GetHash()) != len(node.Hash{}) {
		return nil, coded(codes.InvalidArgument, "invalid_request",
			fmt.Sprintf("the hash must be %d bytes, got %d",
				len(node.Hash{}), len(req.GetHash())))
	}

	var hash node.Hash
	copy(hash[:], req.GetHash())

	notFound := coded(codes.NotFound, "not_found",
		fmt.Sprintf("no swap with hash %x", hash))
	rec, err := svc.journal.Get(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFound
		}

		return nil, coded(codes.Internal, "internal", err.Error())
	}

	// A participant sees their own swaps only: another's amounts, and its
	// preimage, are that participant's proof of payment, not theirs. Told
	// apart from a hash that was never quoted by nothing at all.
	if who := participantOf(ctx); who != "" && rec.Participant != who {
		return nil, notFound
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

	// The SHA256 node first and whatever the bridge's own state: it is
	// the usual reason the bridge is not up.
	resp.Sha256Node = s.sha256Summary(ctx)

	// Whether each node can be used at all comes first, and runs even when
	// the bridge failed to start, because an unsynced or unreachable node
	// is frequently the reason it did.
	if err := s.local.Check(ctx); err != nil {
		resp.Refusals = append(resp.Refusals, err.Error())
	}
	if remote := s.remoteNode(); remote != nil {
		if err := remote.Check(ctx); err != nil {
			resp.Refusals = append(resp.Refusals, err.Error())
		}
	}

	svc := s.service()
	if svc == nil {
		s.mu.RLock()
		why := s.startErr
		s.mu.RUnlock()

		msg := "the bridge is enabled but has not started"
		if errors.Is(why, errStarting) {
			msg = "the bridge is enabled and still connecting to " +
				"both nodes"
		} else if why != nil {
			msg = fmt.Sprintf("the bridge is enabled but cannot "+
				"start, and tries again every %v: %v",
				retryInterval, why)
		}
		resp.Refusals = append(resp.Refusals, msg)

		return resp, nil
	}

	for _, sd := range svc.sides {
		resp.Directions = append(resp.Directions, sd.name)
	}
	for _, name := range svc.disabled {
		resp.Refusals = append(resp.Refusals, name+" is configured "+
			"but not enabled")
	}
	if svc.heldForRate {
		resp.Refusals = append(resp.Refusals, "toBLAKE2b starts once "+
			"a rate is set: its swap bounds are converted from "+
			"SHA256 coin at the rate in force when it starts")
	}
	resp.SwapsInFlight = uint32(svc.active())

	rate, setAt := svc.rates.current()
	resp.Rate = rate
	if !setAt.IsZero() {
		resp.RateSetAt = setAt.Unix()
	}
	if exp := svc.rates.expiresAt(); !exp.IsZero() {
		resp.RateExpiresAt = exp.Unix()
	}
	if _, _, err := svc.rates.usable(); err != nil {
		resp.Refusals = append(resp.Refusals, err.Error())
	}

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
	resp.NeedsOperator = svc.needsOperator(ctx)

	return resp, nil
}

// Info reports what the bridge would charge now, per direction, without
// creating or reserving anything.
//
// It runs every check a quote would, short of creating one, so that "open"
// means what a payer needs it to: a quote asked for now could succeed. A
// direction that is shut says why, with the code a quote would have failed
// with.
func (s *Server) Info(ctx context.Context, _ *InfoRequest) (*InfoResponse,
	error) {

	svc := s.service()
	if !s.cfg.Enabled || svc == nil {
		return nil, s.notServing()
	}

	// The answer is the same for every caller and reads balances on both
	// nodes, so it is shared for a few seconds: however often anyone asks,
	// the nodes are asked at most that often. Info has no per-participant
	// limit of its own, and needs none with this.
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	if s.info != nil && time.Since(s.infoAt) < infoFresh {
		return proto.Clone(s.info).(*InfoResponse), nil
	}

	ctx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()

	resp := &InfoResponse{
		Version: PayerAPIVersion,
		Node:    s.local.NodeKey(),
	}
	for _, sd := range svc.sides {
		resp.Directions = append(resp.Directions,
			svc.directionInfo(ctx, sd))
	}
	s.info, s.infoAt = resp, time.Now()

	return proto.Clone(resp).(*InfoResponse), nil
}

// infoTimeout bounds Info. It reads balances on both nodes, and a payer is
// waiting on it to show a price.
const infoTimeout = 15 * time.Second

// infoFresh is how long one Info answer is given to every caller.
const infoFresh = 5 * time.Second

// SetRate changes the rate the bridge trades at.
//
// It works while the bridge is enabled and not up, too, by writing the rate
// file the bridge reads when it comes up: an unreadable rate file is one of
// the things that keeps it down, and setting the rate is the way out.
func (s *Server) SetRate(_ context.Context, req *SetRateRequest) (
	*SetRateResponse, error) {

	if !s.cfg.Enabled {
		return nil, errDisabled()
	}

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	rates := &ratebook{
		path: filepath.Join(filepath.Dir(s.cfg.Journal), rateFileName),
		now:  time.Now,
	}
	if svc := s.service(); svc != nil {
		rates = svc.rates
	} else if err := os.MkdirAll(filepath.Dir(rates.path),
		0700); err != nil {

		return nil, coded(codes.Internal, "internal", err.Error())
	}

	r, at, err := rates.set(req.GetRate(), s.cfg.FixedRate)
	if err != nil {
		if errors.Is(err, ErrConfig) {
			return nil, coded(codes.InvalidArgument,
				"invalid_request", err.Error())
		}

		return nil, coded(codes.Internal, "internal", err.Error())
	}

	// A new price is news at once, not in a few seconds.
	s.infoMu.Lock()
	s.info = nil
	s.infoMu.Unlock()

	if svc := s.service(); svc != nil && svc.heldForRate {
		s.rebuildForRate()
	}

	log.Infof("Bridge rate set to %g SHA256 coin per BLAKE2b coin", r)

	return &SetRateResponse{Rate: r, RateSetAt: at.Unix()}, nil
}
