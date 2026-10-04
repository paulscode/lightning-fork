//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/rate"
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

	// market follows the market's rate with bridgerpc.ratesource=neoxa,
	// and is nil otherwise. It outlives the services built on it, so a
	// rebuild keeps the readings the median and the breaker need. Started
	// by Start, ended with quit.
	market *feed

	// draining is set by Start when the bridge is off with swaps
	// unfinished; drained once they are done. drainStuck is how many were
	// unfinished when there was no SHA256 node to finish them through.
	// draining is written before any goroutine starts; drained under mu.
	draining   bool
	drained    bool
	drainStuck int

	// journalErr is why the journal could not be read at Start, said in
	// Status (on or off) until a service has opened it. Under mu once
	// RPCs are served.
	journalErr error

	// unfinished is the journal's count of unfinished swaps as last read:
	// at Start, then from the running service. Under mu.
	unfinished int

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
	// Also while off: a bridge turned off with swaps unfinished drains
	// them through the same node (see Start).
	if cfg.Supervised {
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

	// Read here, before anything else has the journal open: opening it
	// repairs a torn tail, which must never race a writer. From now on
	// Status takes the count from the running service's own copy.
	n, journalErr := unfinishedInJournal(s.cfg.Journal)
	if journalErr == nil {
		s.unfinished = n
	} else {
		// Said in Status, on or off, so that what is unfinished reads
		// as unknown rather than as none; cleared once a service has
		// opened the journal.
		s.journalErr = journalErr
	}

	if !s.cfg.Enabled {
		// Turned off with swaps unfinished: they are finished, through
		// the SHA256 node they started on, and nothing new is quoted.
		// A swap cut off after paying out and before claiming the
		// payer's HTLC would cost the operator what was paid. The
		// platforms refuse to turn the bridge off while one is in
		// flight; this covers the one that starts in between, and a
		// configuration edited by hand.
		err := journalErr
		switch {
		case err != nil:
			log.Errorf("Bridge is off and could not read its swap "+
				"journal to see whether anything is unfinished: %v",
				err)
			s.journalErr = err

			return nil

		case n == 0:
			// Registered but not serving. The methods refuse with
			// a reason, which is a better answer than an
			// unimplemented error: an operator who has not turned
			// this on should be told so, not left wondering
			// whether their build has it.
			log.Infof("Bridge is compiled in but not enabled")

			// The SHA256 node it ran goes on running (it may hold
			// channels), and the console manages it with the
			// macaroon baked for it: keep that one there and
			// current, as connecting would.
			if s.sup != nil {
				s.quit = make(chan struct{})
				s.wg.Add(1)
				go s.keepConsoleMacaroon()
			}

			return nil

		case !s.cfg.canReachSha256Node():
			s.drainStuck = n
			log.Errorf("Bridge is off with %d swap(s) unfinished, "+
				"and no SHA256 node is configured to finish "+
				"them through: turn the bridge back on", n)

			return nil
		}

		log.Warnf("Bridge is off but %d swap(s) are unfinished: "+
			"finishing them, and quoting nothing", n)
		s.draining = true
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
	s.startMarket()
	s.setStartErr(errStarting)
	s.wg.Add(1)
	go s.retry()
	if s.draining {
		s.wg.Add(1)
		go s.watchDrain()
	}

	return nil
}

// startMarket starts following the market, when the bridge trades at its rate.
//
// Before the first service comes up rather than with it, so that a bridge
// that takes a while to reach its SHA256 node has a rate by the time it does.
// A reading that arrives while toBLAKE2b is held for want of a rate brings it
// up, as SetRate does with a fixed one.
func (s *Server) startMarket() {
	if s.cfg.resolve().rateSource != RateSourceNeoxa {
		return
	}

	var dial func(string, string, time.Duration) (net.Conn, error)
	if s.cfg.Deps != nil {
		dial = s.cfg.Deps.Dial
	}
	market, err := newFeed(s.cfg.resolve().rate, dial)
	if err != nil {
		// The policy is a fixed one, checked by Validate; a service
		// built without the feed builds its own and says why it has
		// no rate.
		log.Errorf("Bridge cannot follow the market: %v", err)

		return
	}
	market.onReading = func() {
		if svc := s.service(); svc != nil && svc.heldForRate {
			s.rebuild(svc, "now that the market's rate is known, "+
				"to bring up toBLAKE2b")
		}
	}

	s.mu.Lock()
	s.market = market
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		<-s.quit
		cancel()
	}()
	go func() {
		defer s.wg.Done()
		market.run(ctx)
	}()
}

// unfinishedInJournal is how many swaps the journal at path has not finished,
// without starting anything. No journal is none.
func unfinishedInJournal(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	j, err := store.Open(path)
	if err != nil {
		return 0, err
	}
	defer j.Close()

	pending, err := j.Pending(context.Background())

	return len(pending), err
}

// consoleMacaroonInterval is how often a bridge that is off checks the
// console's macaroon for the SHA256 node until it is in place.
var consoleMacaroonInterval = 5 * time.Minute

// keepConsoleMacaroon bakes the console's macaroon for the SHA256 node while
// the bridge is off, once that node exists and answers. It never creates,
// starts or unlocks the node: it only uses the node's own admin macaroon, which
// exists only once the node does.
func (s *Server) keepConsoleMacaroon() {
	defer s.wg.Done()

	for {
		ctx, cancel := context.WithTimeout(context.Background(),
			dialTimeout)
		done := s.sup.ensureOperatorMacaroon(ctx)
		cancel()
		if done {
			return
		}

		select {
		case <-s.quit:
			return
		case <-time.After(consoleMacaroonInterval):
		}
	}
}

// journalRefusal says that the journal could not be read, in the words the
// platforms read as "unfinished is unknown".
func journalRefusal(err error) string {
	return fmt.Sprintf("its swap journal cannot be read, so whether a "+
		"swap is unfinished is not known: %v", err)
}

// unfinishedNow is how many swaps the journal holds unfinished: from the
// running service's journal when there is one, else as last read.
func (s *Server) unfinishedNow(ctx context.Context) uint32 {
	if svc := s.service(); svc != nil {
		if pending, err := svc.journal.Pending(ctx); err == nil {
			s.mu.Lock()
			s.unfinished = len(pending)
			s.mu.Unlock()
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	return uint32(s.unfinished)
}

// drainCheckInterval is how often a draining bridge looks for its last swap
// to have finished.
var drainCheckInterval = 30 * time.Second

// watchDrain stops a draining bridge once nothing in its journal is
// unfinished and nothing is being driven.
func (s *Server) watchDrain() {
	defer s.wg.Done()

	for {
		select {
		case <-s.quit:
			return
		case <-time.After(drainCheckInterval):
		}

		svc := s.service()
		if svc == nil {
			continue
		}
		pending, err := svc.journal.Pending(context.Background())
		if err != nil || len(pending) > 0 || svc.active() > 0 {
			continue
		}

		s.mu.Lock()
		if s.svc != svc {
			s.mu.Unlock()
			continue
		}
		conn := s.conn
		s.svc, s.remote, s.conn = nil, nil, nil
		s.drained = true
		s.unfinished = 0
		s.mu.Unlock()

		svc.stop()
		if conn != nil {
			_ = conn.Close()
		}
		log.Infof("Bridge finished the swaps it had; it is off")

		return
	}
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

	var waited int
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
			// Every ten seconds for as long as it takes, which for a
			// node the platform never starts is for ever: said now
			// and then, not each time.
			if waited%30 == 0 {
				log.Infof("Bridge waiting for its SHA256 node: %v",
					err)
			} else {
				log.Debugf("Bridge waiting for its SHA256 node: %v",
					err)
			}
			waited++
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

	// Taken before dialling, so a certificate renewed in between is seen
	// as a change (one needless rebuild) rather than missed for the life
	// of this connection.
	cert := certFingerprint(s.cfg.SHA256TLSCertPath)
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
	if err := s.checkChainThenRestore(ctx, remoteNode); err != nil {
		_ = conn.Close()

		return fmt.Errorf("the bridge will not use the SHA256 node at "+
			"%s: %w", s.cfg.SHA256RPCHost, err)
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
	cfg := s.cfg
	if s.draining {
		// Every journaled swap needs its direction to be driven,
		// whichever were enabled when the bridge was on.
		c := *s.cfg
		c.ToSHA256, c.ToBLAKE2b = true, true
		cfg = &c
	}
	s.mu.RLock()
	market := s.market
	s.mu.RUnlock()
	svc, err := newServiceWith(cfg, s.local, remoteNode, market)
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
	s.journalErr = nil
	s.mu.Unlock()
	s.rateMu.Unlock()

	log.Infof("Bridge is up, serving %d direction(s) through the SHA256 "+
		"node at %s", len(svc.sides), s.cfg.SHA256RPCHost)

	s.wg.Add(1)
	go s.watchChain(svc, remoteNode, cert)

	return nil
}

// remoteNode is the SHA256 node once the bridge has connected, or nil.
func (s *Server) remoteNode() *Remote {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.remote
}

// rebuild takes down the running bridge and starts it again through the
// retry loop, if svc is still the one running: two callers that saw the same
// service (two SetRate calls at once) rebuild it once, not into two bridges.
func (s *Server) rebuild(svc *service, why string) {
	s.mu.Lock()
	if s.svc != svc || svc == nil {
		s.mu.Unlock()

		return
	}
	conn := s.conn
	// The count as it stands, before the service that knows it goes:
	// Status gives it until the next one is up. A closed journal still
	// answers from its index.
	if pending, err := svc.journal.Pending(context.Background()); err == nil {
		s.unfinished = len(pending)
	}
	s.svc, s.remote, s.conn = nil, nil, nil
	s.mu.Unlock()
	s.setStartErr(errStarting)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		svc.stop()
		if conn != nil {
			_ = conn.Close()
		}
		if atomic.LoadInt32(&s.shutdown) != 0 {
			return
		}

		log.Infof("Bridge restarting %s", why)
		s.wg.Add(1)
		go s.retry()
	}()
}

// checkChainThenRestore checks which chain the SHA256 node follows and, only
// once that has passed, starts restoring any channels a restored supervised
// node left in its channel backup: restoring asks each channel's peer to close
// it, which on the wrong chain would be answered from the wrong chain. In the
// background: a peer that cannot be reached only delays its own channel, and
// is no reason to keep the bridge down.
func (s *Server) checkChainThenRestore(ctx context.Context,
	remoteNode *Remote) error {

	if s.cfg.Deps != nil && s.cfg.Deps.Blake2bActivation != nil {
		height, hash, strict, err := s.cfg.Deps.Blake2bActivation(ctx)
		if err == nil {
			err = remoteNode.CheckNotBlake2b(ctx, height, hash, strict)
		}
		if err != nil {
			return err
		}
	}
	if s.sup != nil {
		s.sup.startRestore(s.quit)
	}

	return nil
}

// recheckChain asks the SHA256 node which chain it follows, as connect does.
// ErrConfig means it is not on the SHA256 chain; anything else is no answer.
func (s *Server) recheckChain(remoteNode *Remote) error {
	if s.cfg.Deps == nil || s.cfg.Deps.Blake2bActivation == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	height, hash, strict, err := s.cfg.Deps.Blake2bActivation(ctx)
	if err != nil {
		return err
	}

	return remoteNode.CheckNotBlake2b(ctx, height, hash, strict)
}

// certFingerprint is the SHA-256 of the file at path, or "" if it cannot be
// read.
func certFingerprint(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// chainRecheckInterval is how often the running bridge asks again which chain
// its SHA256 node follows.
var chainRecheckInterval = 10 * time.Minute

// watchChain asks the SHA256 node which chain it follows, again, every
// chainRecheckInterval while svc is the bridge running. Its chain backend can
// change under a running bridge (Bitcoin Knots upgraded to a BLAKE2b version,
// a node swapped in the platform's settings), and a bridge paying through a
// node on the wrong chain sizes its margins from the wrong heights and pays
// from the wrong coins. A refusal takes the bridge down; it stays down, saying
// why, until the node is back on the SHA256 chain. A node that merely does not
// answer is left to the bridge's own handling.
//
// It also notices the node's TLS certificate changing on disk (lnd renews it
// on expiry): the connection trusts the one it was dialled with, so every
// call would fail until the bridge dialled again, which it then does.
func (s *Server) watchChain(svc *service, remoteNode *Remote, cert string) {
	defer s.wg.Done()

	for {
		select {
		case <-s.quit:
			return
		case <-time.After(chainRecheckInterval):
		}
		if s.service() != svc {
			return
		}
		if now := certFingerprint(s.cfg.SHA256TLSCertPath); cert != "" &&
			now != "" && now != cert {

			log.Infof("Bridge's SHA256 node has a new TLS certificate")
			s.rebuild(svc, "to dial its SHA256 node with its new "+
				"TLS certificate")

			return
		}

		if err := s.recheckChain(remoteNode); errors.Is(err, ErrConfig) {
			log.Errorf("Bridge stopping: %v", err)
			s.rebuild(svc, "to check its SHA256 node again")

			return
		}
	}
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

	sd, dec, err := svc.routeQuote(ctx, req.GetInvoice())
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

	// Answered while draining too: a payer may be waiting on a swap the
	// bridge is finishing.
	svc := s.service()
	if (!s.cfg.Enabled && !s.draining) || svc == nil {
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

	resp := &StatusResponse{
		Enabled:    s.cfg.Enabled,
		Unfinished: s.unfinishedNow(ctx),
	}

	if !s.cfg.Enabled {
		resp.Refusals = append(resp.Refusals, "the bridge is not "+
			"enabled on this node")
		s.mu.RLock()
		draining, drained, stuck := s.draining, s.drained, s.drainStuck
		journalErr := s.journalErr
		s.mu.RUnlock()
		switch {
		case journalErr != nil:
			resp.Refusals = append(resp.Refusals, journalRefusal(journalErr))

		case stuck > 0:
			resp.Refusals = append(resp.Refusals, fmt.Sprintf("%d "+
				"swap(s) are unfinished and no SHA256 node is "+
				"configured to finish them through: turn the "+
				"bridge back on", stuck))

		case draining && !drained:
			resp.Refusals = append(resp.Refusals, "finishing the "+
				"swaps already under way; quoting nothing")
			if svc := s.service(); svc != nil {
				resp.SwapsInFlight = uint32(svc.active())
				// A swap stopped for the operator keeps the
				// drain going until they act on it.
				resp.NeedsOperator = svc.needsOperator(ctx)
			} else {
				s.mu.RLock()
				why := s.startErr
				s.mu.RUnlock()
				if why != nil && !errors.Is(why, errStarting) {
					resp.Refusals = append(resp.Refusals,
						fmt.Sprintf("it cannot start to "+
							"finish them, and tries "+
							"again every %v: %v",
							retryInterval, why))
				}
			}
			resp.Sha256Node = s.sha256Summary(ctx)
		}

		return resp, nil
	}

	// The SHA256 node first and whatever the bridge's own state: it is
	// the usual reason the bridge is not up.
	resp.Sha256Node = s.sha256Summary(ctx)

	s.mu.RLock()
	journalErr := s.journalErr
	s.mu.RUnlock()
	if journalErr != nil {
		resp.Refusals = append(resp.Refusals, journalRefusal(journalErr))
	}

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
		s.reportRate(resp, nil)

		return resp, nil
	}

	for _, sd := range svc.sides {
		if sd.quoting() {
			resp.Directions = append(resp.Directions, sd.name)
		}
	}
	for _, name := range svc.disabled {
		resp.Refusals = append(resp.Refusals, name+" is configured "+
			"but not enabled")
	}
	if svc.heldForRate {
		resp.Refusals = append(resp.Refusals, "toBLAKE2b quotes once "+
			"there is a rate: its swap bounds are converted from "+
			"SHA256 coin at the rate in force then (swaps already "+
			"under way go on meanwhile)")
	}
	resp.SwapsInFlight = uint32(svc.active())

	s.reportRate(resp, svc)

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
		if sd.quoting() {
			resp.Directions = append(resp.Directions,
				svc.directionInfo(ctx, sd))
		}
	}
	s.info, s.infoAt = resp, time.Now()

	return proto.Clone(resp).(*InfoResponse), nil
}

// reportRate fills in Status's rate and fees: from the running service, or,
// while it is not up, from the market the server follows, so that an operator
// waiting on the SHA256 node can see whether the market can be read too.
func (s *Server) reportRate(resp *StatusResponse, svc *service) {
	res := s.cfg.resolve()
	resp.RateSource = res.rateSource
	resp.FeeToSha256, resp.FeeToBlake2B = res.feeToSHA256, res.feeToB2B

	var prices priceSource
	var market *feed
	switch {
	case svc != nil:
		prices, market = svc.prices, svc.market

	default:
		s.mu.RLock()
		market = s.market
		s.mu.RUnlock()
		if market == nil {
			// The operator's own rate is read with the service.
			return
		}
		prices = market
	}

	rate, setAt := prices.last()
	resp.Rate = rate
	if !setAt.IsZero() {
		resp.RateSetAt = setAt.Unix()
	}
	if market != nil {
		resp.RateCrossCheck = market.crossCheck()
		if !setAt.IsZero() {
			resp.RateExpiresAt = setAt.Add(res.rate.MaxAge).Unix()
		}
	} else if exp := svc.rates.expiresAt(); !exp.IsZero() {
		resp.RateExpiresAt = exp.Unix()
	}
	if m, err := prices.quoteRate(); err != nil {
		resp.Refusals = append(resp.Refusals, rateRefusal(err))
	} else {
		resp.RateVolatility = m.volatility
	}
}

// rateRefusal says why there is no rate to trade at, for Status.
func rateRefusal(err error) string {
	switch {
	case errors.Is(err, errNoReading):
		return "no rate yet: reading the market"

	case errors.Is(err, rate.ErrDisagreement):
		return "quoting nothing while the market's two readings " +
			"disagree: " + err.Error()

	case errors.Is(err, rate.ErrBroken):
		return "quoting nothing while the market settles: " +
			err.Error()

	case errors.Is(err, rate.ErrStale), errors.Is(err, errFeedUnreadable):
		return "quoting nothing without a current market rate: " +
			err.Error()
	}

	return err.Error()
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

	// While draining too: a rate file that cannot be read keeps the
	// bridge from coming up to finish its swaps, and setting a rate is
	// how it is replaced.
	if !s.cfg.Enabled && !s.draining {
		return nil, errDisabled()
	}
	if s.cfg.resolve().rateSource != RateSourceFixed {
		return nil, coded(codes.FailedPrecondition, "rate_from_market",
			"this bridge trades at the market's rate, read live from "+
				"Neoxa (bridgerpc.ratesource=neoxa); to post your "+
				"own, set bridgerpc.ratesource=fixed")
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
		s.rebuild(svc, "now that a rate is set, to bring up toBLAKE2b")
	}

	log.Infof("Bridge rate set to %g SHA256 coin per BLAKE2b coin", r)

	return &SetRateResponse{Rate: r, RateSetAt: at.Unix()}, nil
}
