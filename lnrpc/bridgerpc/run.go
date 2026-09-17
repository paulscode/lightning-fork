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
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/runner"
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

	// Sample once before anything else so the observers are not empty for
	// a whole interval.
	s.sample(s.ctx)

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
}

// stop ends the pollers and waits for swaps in flight.
//
// The wait is not optional. Stopping the sub-server is followed by the node
// tearing down the invoice registry and the router underneath it, and a swap
// still driving would find them gone mid-HTLC.
func (s *service) stop() {
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

		s.sample(ctx)
	}
}

// sample adds each chain's tip to its observer.
//
// A failure is logged and dropped rather than retried here: the next tick is
// the retry, and an observer that goes stale refuses to estimate, which
// refuses the quotes that would have been sized against it. That is the right
// way round.
func (s *service) sample(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	for _, src := range []struct {
		name string
		read func(context.Context) (BlockInfo, error)
		obs  *chainrate.Observer
	}{
		{"blake2b", s.local.BestBlock, s.lfChain},
		{"bitcoin", s.remote.BestBlock, s.btcChain},
	} {
		info, err := src.read(ctx)
		if err != nil {
			log.Warnf("Bridge could not read the %s tip: %v",
				src.name, err)

			continue
		}
		if info.Height == 0 || info.Time.IsZero() {
			continue
		}

		s.chainMu.Lock()
		// Add rejects a height it already holds, which is the common
		// case between blocks and is not worth logging.
		_ = src.obs.Add(chainrate.Block{
			Height: info.Height, Time: info.Time,
		})
		s.chainMu.Unlock()
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

	var errs []error
	for _, sd := range s.sides {
		dec, err := sd.out.Decode(ctx, invoice)
		if err == nil {
			return sd, dec, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", sd.name, err))
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

// drive runs a swap to completion in the background.
//
// It uses the service's own context, not the caller's. The caller may hang up
// the moment after they pay, and the HTLC does not go away with their
// connection: abandoning the swap there would leave one leg paid and the other
// held.
func (s *service) drive(sd *side, hash node.Hash) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		err := sd.runner.Drive(s.ctx, hash)
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
		}
	}()
}
