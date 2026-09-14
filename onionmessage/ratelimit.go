package onionmessage

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnutils"
	"golang.org/x/time/rate"
)

var (
	// ErrPeerRateLimit is the sentinel error returned by
	// IngressLimiter.AllowN when the per-peer token bucket rejects an
	// incoming onion message. Callers match on it with errors.Is to
	// distinguish per-peer drops from global drops.
	ErrPeerRateLimit = errors.New("per-peer rate limit exceeded")

	// ErrGlobalRateLimit is the sentinel error returned by
	// IngressLimiter.AllowN when the global token bucket rejects an
	// incoming onion message. Callers match on it with errors.Is to
	// distinguish global drops from per-peer drops.
	ErrGlobalRateLimit = errors.New("global rate limit exceeded")
)

// kbpsToBytesPerSecond converts a configured kilobits-per-second value into
// bytes-per-second, suitable for passing to rate.NewLimiter. A Kbps value is
// decimal (1 Kbps = 1000 bits/second) so the conversion factor is 125.
func kbpsToBytesPerSecond(kbps uint64) float64 {
	return float64(kbps) * 125.0
}

// RateLimiter is the minimal token-bucket interface used at the onion message
// ingress path. Tokens are bytes: each call reports whether a message of size
// n bytes is permitted to proceed, and on success consumes n bytes from the
// underlying bucket. The interface is satisfied by *rate.Limiter (via a small
// counting wrapper) and a noop implementation used when a limit is configured
// as zero (disabled). It exists so that callers and tests can substitute
// alternate implementations without taking a hard dependency on the
// x/time/rate package.
//
// Implementations of AllowN must be safe for concurrent use by multiple
// goroutines; the ingress call site invokes it from per-peer readHandler
// goroutines without additional synchronization.
type RateLimiter interface {
	// AllowN reports whether an onion message of n bytes is permitted
	// to proceed at the current instant. It must be non-blocking and
	// safe for concurrent use.
	AllowN(n int) bool
}

// noopLimiter is a RateLimiter that always allows traffic. It is returned by
// NewGlobalLimiter when the configured rate or burst is zero, meaning rate
// limiting is disabled and all messages are permitted without restriction.
// Using a noopLimiter avoids branching at the call site. PeerRateLimiter
// does not use this type directly; it short-circuits via its own disabled()
// helper.
type noopLimiter struct{}

// AllowN always returns true regardless of the requested byte count.
func (noopLimiter) AllowN(int) bool { return true }

// countingLimiter wraps a *rate.Limiter and tracks how many calls to AllowN
// have been rejected. The counter is exposed via Dropped for observability,
// and a one-shot flag records whether a log line has been emitted for the
// first rejection so operators can see that the limiter actually fired.
type countingLimiter struct {
	limiter  *rate.Limiter
	dropped  atomic.Uint64
	firstLog atomic.Bool
}

// AllowN consults the underlying token bucket for n bytes and increments
// the dropped counter on rejection.
func (c *countingLimiter) AllowN(n int) bool {
	if c.limiter.AllowN(time.Now(), n) {
		return true
	}
	c.dropped.Add(1)

	return false
}

// FirstDropClaim atomically returns true exactly once, on the first call.
// The caller is responsible for only invoking it after a rejection has
// actually occurred; the method itself does not inspect the dropped
// counter. It exists so that the ingress call site can emit a single
// info-level log line when a limiter first trips, without spamming the
// log on every subsequent drop.
func (c *countingLimiter) FirstDropClaim() bool {
	return c.firstLog.CompareAndSwap(false, true)
}

// Dropped returns the total number of onion messages this limiter has
// rejected since process start.
func (c *countingLimiter) Dropped() uint64 {
	return c.dropped.Load()
}

// NewGlobalLimiter constructs a process-wide onion message rate limiter.
// kbps is the sustained rate in kilobits per second (1 Kbps = 1000 bits/s);
// burstBytes is the token bucket depth in bytes. A zero rate or a zero
// burst disables limiting and returns a noopLimiter. Otherwise the returned
// RateLimiter is a token bucket whose tokens are bytes.
func NewGlobalLimiter(kbps uint64, burstBytes uint64) RateLimiter {
	if kbps == 0 || burstBytes == 0 {
		return noopLimiter{}
	}

	bps := kbpsToBytesPerSecond(kbps)

	return &countingLimiter{
		limiter: rate.NewLimiter(rate.Limit(bps), int(burstBytes)),
	}
}

// PeerRateLimiter is a registry of per-peer onion message token buckets,
// keyed by the peer's compressed public key. Tokens are bytes: callers pass
// the on-the-wire size of each message to AllowN and the per-peer bucket is
// debited accordingly. Buckets are created lazily on the first call to
// AllowN for a given peer and retained for the lifetime of the process.
// When the configured rate or burst is zero the registry operates in
// disabled mode and AllowN is a no-op.
//
// Retention across disconnect is load-bearing. Without it, a peer could
// drain its burst, disconnect, reconnect, and get a fresh full-burst
// bucket on every cycle, effectively promoting the global limiter into
// its per-peer rate and using the shared budget as a personal allowance
// until the global bucket trips. By keeping the bucket, a drained peer
// stays drained until its bucket naturally refills regardless of how
// often it cycles the connection, and the per-peer rate becomes a real
// ceiling rather than a per-connection ceiling.
//
// The memory cost of retention is bounded by the registry itself, since
// on this chain the ingress call site admits peers with no channel by
// default (see lncfg.ProtocolOptions.OnionMsgChannelGate) and so any
// node key that sends one message would otherwise pin an entry for the
// life of the process. Past maxPeerBuckets entries, a new peer first
// evicts a bucket that has refilled completely, which loses nothing:
// a full bucket and a fresh one are the same bucket. When none has, the
// new peer shares one overflow bucket with every other newcomer until
// one frees up, so that a flood of identities throttles itself rather
// than the registry growing. At ~200 bytes per entry (rate.Limiter plus
// SyncMap overhead) the registry stays under a megabyte.
//
// The underlying bucket registry is an lnutils.SyncMap rather than a plain
// map guarded by a mutex. Per-peer keys are stable for the lifetime of the
// connection and the common path is a Load hit, which sync.Map serves
// without any write contention across peers. A plain map would serialize
// every hot-path AllowN call behind a single mutex even though rate.Limiter
// is already safe for concurrent use.
type PeerRateLimiter struct {
	rate     rate.Limit
	burst    int
	peers    lnutils.SyncMap[[33]byte, *rate.Limiter]
	dropped  atomic.Uint64
	firstLog atomic.Bool

	// maxPeers bounds the registry; count tracks its size. The slow
	// path that adds an entry past the bound holds evictMu.
	maxPeers int
	count    atomic.Int64
	evictMu  sync.Mutex
	overflow *rate.Limiter
}

// DefaultMaxPeerBuckets is the number of per-peer buckets kept before a
// newcomer has to take a refilled one or share the overflow bucket.
const DefaultMaxPeerBuckets = 4096

// FirstDropClaim atomically returns true exactly once, on the first call.
// The caller is responsible for only invoking it after a rejection has
// actually occurred. The ingress site uses this to emit a single
// info-level log line when per-peer rate limiting first trips, rather
// than spamming the log on every drop.
func (p *PeerRateLimiter) FirstDropClaim() bool {
	return p.firstLog.CompareAndSwap(false, true)
}

// NewPeerRateLimiter constructs a per-peer onion message rate limiter.
// kbps is the per-peer sustained rate in kilobits per second and burstBytes
// is the per-peer token bucket depth in bytes. A zero rate or a zero burst
// disables limiting; in that case AllowN always returns true and no
// per-peer state is retained.
func NewPeerRateLimiter(kbps uint64, burstBytes uint64) *PeerRateLimiter {
	return NewPeerRateLimiterBounded(kbps, burstBytes, DefaultMaxPeerBuckets)
}

// NewPeerRateLimiterBounded is NewPeerRateLimiter with the registry bound
// given; a bound of zero or less keeps every bucket.
func NewPeerRateLimiterBounded(kbps uint64, burstBytes uint64,
	maxPeers int) *PeerRateLimiter {

	p := &PeerRateLimiter{maxPeers: maxPeers}
	if kbps > 0 && burstBytes > 0 {
		p.rate = rate.Limit(kbpsToBytesPerSecond(kbps))
		p.burst = int(burstBytes)
		p.overflow = rate.NewLimiter(p.rate, p.burst)
	}

	return p
}

// disabled reports whether the limiter has been configured to permit all
// traffic.
func (p *PeerRateLimiter) disabled() bool {
	return p.rate == 0 || p.burst <= 0
}

// AllowN reports whether an onion message of n bytes from the given peer
// is permitted at the current instant. The peer's bucket is created on
// first use. Rejected calls are counted and visible via Dropped.
func (p *PeerRateLimiter) AllowN(peer [33]byte, n int) bool {
	if p.disabled() {
		return true
	}

	lim, ok := p.peers.Load(peer)
	if !ok {
		lim = p.bucketFor(peer)
	}

	if lim.AllowN(time.Now(), n) {
		return true
	}
	p.dropped.Add(1)

	return false
}

// bucketFor returns the bucket a peer not yet in the registry gets: its
// own while the registry has room or a refilled bucket can make room,
// the shared overflow bucket otherwise. The slow path is serialised so
// the bound holds under concurrent newcomers.
func (p *PeerRateLimiter) bucketFor(peer [33]byte) *rate.Limiter {
	p.evictMu.Lock()
	defer p.evictMu.Unlock()

	// A concurrent caller may have added it while we waited.
	if lim, ok := p.peers.Load(peer); ok {
		return lim
	}
	if p.maxPeers > 0 && p.count.Load() >= int64(p.maxPeers) &&
		!p.evictRefilled() {

		return p.overflow
	}
	lim := rate.NewLimiter(p.rate, p.burst)
	p.peers.Store(peer, lim)
	p.count.Add(1)

	return lim
}

// evictRefilled drops one bucket that has refilled completely, reporting
// whether it found one. Only ever called with evictMu held.
func (p *PeerRateLimiter) evictRefilled() bool {
	var victim *[33]byte
	p.peers.Range(func(key [33]byte, lim *rate.Limiter) bool {
		if lim.Tokens() >= float64(p.burst) {
			k := key
			victim = &k

			return false
		}

		return true
	})
	if victim == nil {
		return false
	}
	p.peers.Delete(*victim)
	p.count.Add(-1)

	return true
}

// Buckets returns how many per-peer buckets the registry holds.
func (p *PeerRateLimiter) Buckets() int {
	return int(p.count.Load())
}

// Dropped returns the total number of onion messages this registry has
// rejected since process start, summed across all peers.
func (p *PeerRateLimiter) Dropped() uint64 {
	return p.dropped.Load()
}

// IngressLimiter is the combined per-peer + global rate limiter surface
// consumed by the onion message ingress path. It hides the split between
// the two underlying buckets so callers in peer/brontide.go only need to
// thread a single object through Config and call a single method on every
// incoming onion message. The per-peer bucket is always checked first so
// that a hostile peer whose own budget is already empty cannot burn
// global tokens on every rejected attempt and starve legitimate peers.
//
// Implementations must be safe for concurrent use from per-peer
// readHandler goroutines. A nil IngressLimiter is a valid "disabled"
// sentinel at call sites and means "accept everything".
type IngressLimiter interface {
	// AllowN reports whether an onion message of n bytes from the
	// given peer is permitted. A successful result wraps fn.Unit; a
	// rejection wraps either ErrPeerRateLimit or ErrGlobalRateLimit
	// depending on which bucket fired. Callers use errors.Is against
	// those sentinels to pick their log / metric / drop path.
	//
	// Per-peer state is retained for the lifetime of the process so
	// that a peer cannot reset its bucket by cycling the connection;
	// see the PeerRateLimiter doc for the memory-bound argument.
	AllowN(peer [33]byte, n int) fn.Result[fn.Unit]

	// FirstPeerDropClaim atomically returns true exactly once, on
	// the first call, and is intended to gate a one-shot info log
	// when the per-peer limiter first trips.
	FirstPeerDropClaim() bool

	// FirstGlobalDropClaim atomically returns true exactly once, on
	// the first call, and is intended to gate a one-shot info log
	// when the global limiter first trips.
	FirstGlobalDropClaim() bool
}

// ingressLimiter is the stock IngressLimiter implementation that
// composes a PeerRateLimiter with a global RateLimiter. Either side may
// be nil / disabled independently.
type ingressLimiter struct {
	peer   *PeerRateLimiter
	global RateLimiter
}

// NewIngressLimiter constructs an IngressLimiter that first consults the
// given per-peer limiter and then the given global limiter for each
// incoming onion message. Either argument may be nil (or the zero-value
// disabled limiter returned by the constructors in this package) in
// which case that side of the check is skipped.
func NewIngressLimiter(peer *PeerRateLimiter,
	global RateLimiter) IngressLimiter {

	return &ingressLimiter{
		peer:   peer,
		global: global,
	}
}

// AllowN checks per-peer then global, returning the drop reason as a
// sentinel error wrapped in a fn.Result on rejection. The ordering is
// load-bearing: consulting the per-peer bucket first means over-limit
// traffic from one peer is rejected before it can touch the global
// bucket, so the global bucket only accounts for traffic that was
// within its source peer's allowance and a single hostile peer cannot
// drain the shared budget via rejected attempts.
func (l *ingressLimiter) AllowN(peer [33]byte,
	n int) fn.Result[fn.Unit] {

	if l.peer != nil && !l.peer.AllowN(peer, n) {
		return fn.Err[fn.Unit](ErrPeerRateLimit)
	}
	if l.global != nil && !l.global.AllowN(n) {
		return fn.Err[fn.Unit](ErrGlobalRateLimit)
	}

	return fn.Ok(fn.Unit{})
}

// FirstPeerDropClaim delegates to the per-peer limiter's one-shot
// claim. Returns false if the per-peer limiter is nil (disabled).
func (l *ingressLimiter) FirstPeerDropClaim() bool {
	if l.peer == nil {
		return false
	}

	return l.peer.FirstDropClaim()
}

// FirstGlobalDropClaim atomically returns true exactly once, on the
// first call, when the global limiter is an enabled countingLimiter
// that has just recorded its first rejection. A noop (disabled) global
// limiter, a nil global limiter, and a countingLimiter whose flag has
// already been claimed all return false. The type assertion is inlined
// here because the global limiter is consulted through the RateLimiter
// interface and only the countingLimiter implementation tracks drops.
func (l *ingressLimiter) FirstGlobalDropClaim() bool {
	cl, ok := l.global.(*countingLimiter)
	if !ok {
		return false
	}

	return cl.FirstDropClaim()
}
