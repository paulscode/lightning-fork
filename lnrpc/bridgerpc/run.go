//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/driver"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/runner"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pollInterval is how often each chain's tip is sampled.
//
// Fast enough that a new block is noticed within a fraction of the shorter
// chain's spacing, and slow enough to be nothing next to what the node already
// does per block.
const pollInterval = 30 * time.Second

// start brings the bridge up: resume anything unfinished, then poll.
//
// Resuming comes first deliberately. A swap interrupted by the last shutdown
// may be holding an HTLC with a deadline, and that is a clock already running;
// a new quote can wait for the observers to fill.
func (s *service) start() {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// History first, then the tip. The observers need a hundred blocks in
	// the current epoch before they will estimate, and reading the ones
	// that already exist is the difference between quoting in seconds and
	// quoting tomorrow.
	s.backfill(s.ctx)
	s.sample(s.ctx, sampleTimeout)

	// After the balances can be read, because that is what it sizes from.
	s.sizeInventory(s.ctx)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.poll(s.ctx)
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.resume(s.ctx)
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.compact(s.ctx)
	}()
}

// stop ends the pollers and waits for swaps in flight.
//
// The wait is not optional. Stopping the sub-server is followed by the node
// tearing down the invoice registry and the router underneath it, and a swap
// still driving would find them gone mid-HTLC.
func (s *service) stop() {
	s.driveMu.Lock()
	s.halted = true
	s.driveMu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	if n := s.active(); n > 0 {
		log.Infof("Bridge waiting for %d swap(s) in flight", n)
	}
	s.wg.Wait()
	s.close()
}

// active is how many swaps are being driven, across directions.
func (s *service) active() int {
	var n int
	for _, sd := range s.sides {
		if sd.runner != nil {
			n += sd.runner.Active()
		}
	}

	return n
}

// poll keeps both chain observers fed until ctx ends.
func (s *service) poll(ctx context.Context) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		s.sample(ctx, sha256InfoTimeout)

		// Retried until each direction has been sized from a real
		// balance. A node whose peer was still reconnecting at startup
		// reports nothing to pay with, and that must not be the answer
		// for the life of the process.
		s.sizeInventory(ctx)
	}
}

// sampleTimeout bounds reading this node's tip, and the SHA256 node's while
// the bridge starts: stopping this node waits for a start under way, and a
// longer wait there would hold that up.
const sampleTimeout = 20 * time.Second

// tipSource is one chain's tip, as sample reads it.
type tipSource struct {
	// name keys headerRead and tipWarns; label is the chain's name in
	// what is logged.
	name, label string

	// wait bounds reading it.
	wait time.Duration

	read func(context.Context) (BlockInfo, error)
	at   func(context.Context, int32) (BlockInfo, error)
	obs  *chainrate.Observer
}

// sample adds each chain's tip to its observer, waiting up to sha256Wait for
// the SHA256 node's: stock lnd asks its chain node before it answers, and a
// busy one can take most of a minute.
//
// A failure is logged and dropped rather than retried here: the next tick is
// the retry, and an observer that goes stale refuses to estimate, which
// refuses the quotes that would have been sized against it. That is the right
// way round.
func (s *service) sample(ctx context.Context, sha256Wait time.Duration) {
	for _, src := range []tipSource{
		{"blake2b", "BLAKE2b", sampleTimeout, s.local.BestBlock,
			s.local.BlockAt, s.b2bChain},
		{"bitcoin", "SHA256", sha256Wait, s.remote.BestBlock,
			s.remote.BlockAt, s.shaChain},
	} {
		s.sampleTip(ctx, src)
	}
}

// sampleTip adds one chain's tip to its observer; see sample.
func (s *service) sampleTip(parent context.Context, src tipSource) {
	ctx, cancel := context.WithTimeout(parent, src.wait)
	defer cancel()

	info, err := src.read(ctx)
	if err != nil {
		// The bridge stopping cuts a read off, which says nothing
		// about the node; warning that it is slow would.
		if parent.Err() != nil {
			return
		}
		s.tipUnread(src, err)

		return
	}
	s.tipRead(src)
	if info.Height == 0 || info.Time.IsZero() {
		return
	}
	// A node catching up (after every restart, for a while) reports its
	// backend's tip height beside its wallet's best header time, which
	// belong to different blocks: that pair would rewrite a block's time
	// and skew the spacing every margin is sized on. A synced node's pair
	// is one block's, but for a block arriving between the two reads,
	// which is rare and leaves one block's time at most one block early:
	// within what the bounds allow for.
	if !info.SyncedToChain {
		return
	}

	s.chainMu.Lock()
	read := s.headerRead[src.name] == info.Height
	s.chainMu.Unlock()
	if read {
		// This tip's header is already in the observer, with its
		// difficulty; between blocks there is nothing new.
		return
	}

	// The tip's own header, for its difficulty: the observer carries
	// spacing across a retarget by scaling the previous epoch by the
	// difficulty change, and without it the bridge refuses every swap for
	// the first hundred blocks of each new epoch, most of a day. A header
	// that cannot be read leaves the tip without one, and it is read again
	// next poll.
	block := chainrate.Block{Height: info.Height, Time: info.Time}
	hdr, err := src.at(ctx, info.Height)
	haveHeader := err == nil && hdr.Height == info.Height &&
		!hdr.Time.IsZero()
	if haveHeader {
		block.Time = hdr.Time
		block.Difficulty = hdr.difficulty()
	} else if err != nil {
		log.Debugf("Bridge could not read the %s tip's header at "+
			"%d: %v", src.label, info.Height, err)
	}

	s.chainMu.Lock()
	err = src.obs.Add(block)
	if err == nil && haveHeader && block.Difficulty > 0 {
		s.headerRead[src.name] = info.Height
	}
	s.chainMu.Unlock()
}

// tipWarnEvery is how often a tip that still cannot be read is warned about
// again. Every poll would be twice a minute for as long as a node is in
// trouble, which buries everything else in the log.
const tipWarnEvery = 10 * time.Minute

// tipWarn is how reading one chain's tip has been failing.
type tipWarn struct {
	// failed is how many reads in a row have failed.
	failed int

	// said is when that was last logged as a warning.
	said time.Time
}

// fail counts a failed read and reports whether to warn about it: the first
// of a run, then once every tipWarnEvery.
func (w *tipWarn) fail(now time.Time) bool {
	w.failed++
	if w.failed > 1 && now.Sub(w.said) < tipWarnEvery {
		return false
	}
	w.said = now

	return true
}

// tipUnread logs a tip that could not be read; see tipWarn.
func (s *service) tipUnread(src tipSource, err error) {
	why := err.Error()
	if src.name == "bitcoin" && timedOut(err) {
		why = slowGetInfo(false, src.wait)
	}

	s.tipMu.Lock()
	if s.tipWarns == nil {
		s.tipWarns = make(map[string]*tipWarn)
	}
	w := s.tipWarns[src.name]
	if w == nil {
		w = &tipWarn{}
		s.tipWarns[src.name] = w
	}
	warn, failed := w.fail(time.Now()), w.failed
	s.tipMu.Unlock()

	switch {
	case !warn:
		log.Debugf("Bridge could not read the %s tip: %v", src.label,
			err)

	case failed == 1:
		log.Warnf("Bridge could not read the %s tip: %s", src.label,
			why)

	default:
		log.Warnf("Bridge still cannot read the %s tip after %d "+
			"tries, at least %v apart: %s", src.label, failed,
			pollInterval, why)
	}
}

// tipRead notes a tip that was read, saying so if it had not been.
func (s *service) tipRead(src tipSource) {
	s.tipMu.Lock()
	w := s.tipWarns[src.name]
	delete(s.tipWarns, src.name)
	s.tipMu.Unlock()

	if w != nil {
		log.Infof("Bridge reads the %s tip again, after %d failed "+
			"tries", src.label, w.failed)
	}
}

// resume drives every swap the journal says is unfinished.
//
// Each goes to the direction whose paying node can read its outgoing invoice,
// because that is the node that will pay it. Running every direction's Resume
// over one shared journal would have both drive every swap, and the one whose
// nodes are the wrong way round would decide against a chain the swap is not
// on.
func (s *service) resume(ctx context.Context) {
	pending, err := s.journal.Pending(ctx)
	if err != nil {
		log.Errorf("Bridge could not read pending swaps: %v", err)

		return
	}
	if len(pending) == 0 {
		return
	}

	log.Infof("Bridge resuming %d swap(s)", len(pending))

	for _, rec := range pending {
		sd, _, err := s.route(ctx, rec.Invoice)
		if err != nil {
			// Nothing will drive this swap, and if its incoming
			// HTLC is locked in it has a deadline. The usual cause
			// is a direction that was enabled when the swap was
			// quoted and is not now.
			log.Errorf("Bridge cannot resume swap %x (state %v): "+
				"no enabled direction can pay %q, so nothing "+
				"will drive it: %v", rec.Hash, rec.State,
				rec.Invoice, err)

			continue
		}
		s.drive(sd, rec.Hash)
	}
}

// route finds the direction whose paying node can read this invoice.
//
// Asking the nodes rather than reading the invoice's prefix here: a second
// parser that disagreed by one field would pick the wrong direction, and the
// node that will actually pay is the authority on whether it can.
func (s *service) route(ctx context.Context, invoice string) (*side,
	node.Decoded, error) {

	var errs, unanswered []error
	readable := false
	for _, sd := range s.sides {
		dec, err := sd.out.Decode(ctx, invoice)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sd.name, err))
			if notAnswering(err) {
				unanswered = append(unanswered,
					fmt.Errorf("%s: %w", sd.name, err))
			}

			continue
		}
		readable = true

		// Both chains' invoices share a prefix and both nodes read
		// every one, so being able to decode it says nothing about the
		// chain. option_blake2b does.
		if dec.BLAKE2b != sd.paysBLAKE2b {
			continue
		}

		return sd, dec, nil
	}

	// A paying node that did not answer could be the one this invoice is
	// for, so nothing about the invoice is known yet: unavailable, which a
	// payer tries again, rather than a verdict on the invoice that it
	// would take as final.
	if len(unanswered) > 0 {
		return nil, node.Decoded{}, fmt.Errorf("%w: %w: a paying node "+
			"is not answering: %w", ErrNoDirection, quote.ErrRefused,
			errors.Join(unanswered...))
	}

	// Read, but for a chain no enabled direction pays on: that is the
	// reason, rather than whatever the nodes said about other invoices.
	if readable {
		errs = []error{quote.ErrWrongChain}
	} else {
		errs = append([]error{quote.ErrInvalidInvoice}, errs...)
	}

	// Naming the directions that are configured but off sends an operator
	// to the line they changed, rather than to their nodes.
	if len(s.disabled) > 0 {
		errs = append(errs, fmt.Errorf("configured but not enabled: %v",
			s.disabled))
	}

	return nil, node.Decoded{}, fmt.Errorf("%w: %w", ErrNoDirection,
		errors.Join(errs...))
}

// routeQuote is route for a new swap: a direction built only to finish its
// own unfinished swaps is refused as the direction that is off it is.
func (s *service) routeQuote(ctx context.Context, invoice string) (*side,
	node.Decoded, error) {

	sd, dec, err := s.route(ctx, invoice)
	if err != nil {
		return nil, dec, err
	}
	if !sd.quoting() {
		return nil, dec, fmt.Errorf("%w: %w: configured but not "+
			"enabled: [%s]", ErrNoDirection, quote.ErrWrongChain,
			sd.name)
	}

	// An invoice the paying node would refuse (no payment address, AMP,
	// a required feature it does not know) is refused now, while nothing
	// is held, rather than left paying a payment that never starts.
	if p, ok := sd.out.(payable); ok {
		if err := p.Payable(ctx, invoice); err != nil {
			if notAnswering(err) {
				return nil, dec, fmt.Errorf("%w: %w: the paying "+
					"node is not answering: %w",
					ErrNoDirection, quote.ErrRefused, err)
			}

			return nil, dec, fmt.Errorf("%w: the paying node would "+
				"not pay it: %w", quote.ErrInvalidInvoice, err)
		}
	}

	return sd, dec, nil
}

// payable is a paying node that can say, before quoting, whether it would pay
// an invoice.
type payable interface {
	Payable(ctx context.Context, invoice string) error
}

// notAnswering reports whether a node failed to answer at all, as opposed to
// answering that it cannot read the invoice.
func notAnswering(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {

		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}

	return false
}

// timedOut reports a call the node took too long over or cut off, as opposed
// to one it refused or that found nothing listening.
func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Canceled:
		return true
	}

	return false
}

// slowGetInfo says that the SHA256 node did not answer GetInfo within a wait,
// and why that usually is. running is whether it is known to be up, having
// answered something else.
//
// Stock lnd answers GetInfo only after asking its chain node for the best
// block and whether it is synced, through a client that queues and retries,
// so a chain node that is busy holds it up long after every other call would
// have answered.
func slowGetInfo(running bool, within time.Duration) string {
	who := "the SHA256 Lightning node did not answer"
	if running {
		who = "the SHA256 Lightning node is running but did not answer"
	}

	return fmt.Sprintf("%s GetInfo within %v; lnd answers it only after "+
		"asking its chain node (bitcoind), which may be busy, "+
		"restarting or overloaded", who, within)
}

// drive runs a swap to completion in the background.
//
// It uses the service's own context, not the caller's. The caller may hang up
// the moment after they pay, and the HTLC does not go away with their
// connection: abandoning the swap there would leave one leg paid and the other
// held.
func (s *service) drive(sd *side, hash node.Hash) {
	// Before the goroutine, so a quote returning immediately after cannot
	// read headroom without this swap counted against its own side.
	s.remember(hash, sd)

	// Not on a service that is stopping: stop's wait would not cover
	// this one. The record is as far along as the nodes are, so the next
	// service resumes the swap.
	s.driveMu.Lock()
	if s.halted || (s.ctx != nil && s.ctx.Err() != nil) {
		s.driveMu.Unlock()
		log.Infof("Bridge not driving %s swap %s: stopping, so the "+
			"next start resumes it", sd.name,
			hex.EncodeToString(hash[:]))

		return
	}
	s.wg.Add(1)
	s.driveMu.Unlock()

	go func() {
		defer s.wg.Done()

		err := sd.runner.Drive(s.ctx, hash)
		if !errors.Is(err, runner.ErrBusy) {
			s.driven(hash)
		}
		switch {
		case err == nil:

		case errors.Is(err, runner.ErrBusy):
			// Already being driven, which is what should happen
			// when someone asks twice for the same invoice. The
			// runner's rule is one goroutine per hash, and this is
			// that rule working.
			log.Debugf("Bridge already driving %s swap %s",
				sd.name, hex.EncodeToString(hash[:]))

		case errors.Is(err, context.Canceled):
			// Shutdown. The record is always at least as far along
			// as the nodes are, so the next process picks it up.
			log.Infof("Bridge stopped driving %s swap %s for "+
				"shutdown", sd.name,
				hex.EncodeToString(hash[:]))

		default:
			log.Errorf("Bridge failed driving %s swap %s: %v",
				sd.name, hex.EncodeToString(hash[:]), err)
			if errors.Is(err, driver.ErrNeedsOperator) {
				s.markStopped(hash, err.Error())
			}
		}
	}()
}

// backfillBlocks is how many recent blocks are read into each observer at
// startup.
//
// Enough to clear the hundred-sample floor the observer applies to the current
// difficulty epoch, with room for the window straddling an epoch boundary,
// where the blocks before it count towards the previous epoch instead.
const backfillBlocks = 150

// backfill seeds both chain observers from history.
//
// Without this an observer learns only from tips as they arrive, one per poll,
// and refuses to estimate until it has a hundred in the current epoch. On a
// chain with ten minute blocks that is most of a day after every restart, and
// the bridge refuses every swap throughout. The headers already exist; reading
// them turns that wait into a few seconds.
//
// A failure is logged and not fatal. The SHA256 node serves these through
// ChainKit, which a stock lnd only has when built with the chainrpc tag, so a
// node without it falls back to the slow path rather than stopping the bridge
// from running at all.
func (s *service) backfill(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for _, src := range []struct {
		name string
		tip  func(context.Context) (BlockInfo, error)
		at   func(context.Context, int32) (BlockInfo, error)
		obs  *chainrate.Observer
	}{
		{"blake2b", s.local.BestBlock, s.local.BlockAt, s.b2bChain},
		{"bitcoin", s.remote.BestBlock, s.remote.BlockAt, s.shaChain},
	} {
		tip, err := src.tip(ctx)
		if err != nil {
			log.Warnf("Bridge could not read the %s tip to "+
				"backfill from: %v", src.name, err)

			continue
		}

		from := tip.Height - backfillBlocks + 1
		if from < 0 {
			from = 0
		}

		var added int
		for h := from; h <= tip.Height; h++ {
			if ctx.Err() != nil {
				break
			}

			info, err := src.at(ctx, h)
			if err != nil {
				// The first failure is the informative one:
				// on a node without ChainKit every height
				// fails the same way, and a log line each
				// would bury everything else.
				log.Infof("Bridge cannot read %s history, so "+
					"it will learn block spacing from new "+
					"blocks instead, which takes a while: "+
					"%v", src.name, err)

				break
			}
			if info.Time.IsZero() {
				continue
			}

			s.chainMu.Lock()
			if err := src.obs.Add(chainrate.Block{
				Height: info.Height, Time: info.Time,
				Difficulty: info.difficulty(),
			}); err == nil {
				added++
			}
			s.chainMu.Unlock()
		}

		if added > 0 {
			log.Infof("Bridge read %d %s blocks of history", added,
				src.name)
		}
	}
}

// DefaultCompactInterval is how often the journal is rewritten with one line
// per swap.
const DefaultCompactInterval = time.Hour

// compact rewrites the journal with one line per swap, until ctx ends: the
// latest record of each, finished or not. A finished swap keeps its last
// state, which is how a later quote for the same hash is told whether it may
// go ahead, so it is the history before that state that is dropped.
//
// The journal is append-only, so every state a swap passes through is another
// line, and a node that runs for months replays all of them at startup.
// Nothing else calls this, which is why it belongs here rather than in an
// operator's memory. It matters more here than in the standalone daemon: this
// journal lives in the lnd data directory, so an unbounded one is also an
// unbounded backup.
func (s *service) compact(ctx context.Context) {
	if s.journal == nil {
		return
	}

	every := s.compactEvery
	if every <= 0 {
		every = DefaultCompactInterval
	}

	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		// Not while swaps are being driven. Compaction takes the
		// journal's lock, and a swap blocked on writing its own state
		// is a swap that is not watching an HTLC.
		if n := s.active(); n > 0 {
			log.Debugf("Bridge not compacting the journal yet, "+
				"%d swap(s) in flight", n)

			continue
		}

		if err := s.journal.Compact(ctx); err != nil {
			log.Warnf("Bridge could not compact the journal: %v",
				err)

			continue
		}
		log.Debugf("Bridge compacted the journal")
	}
}
