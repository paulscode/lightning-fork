//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

var (
	// errNotStopped is returned when the operator asks to resolve a swap
	// the bridge has not stopped for them in paying or paid: one it is still
	// driving, or one it stopped elsewhere on its record.
	errNotStopped = errors.New("resolve decides only a swap the bridge " +
		"stopped in paying or paid")

	// errStillInFlight is returned when the paying node says the payment
	// of a stopped swap is still going.
	errStillInFlight = errors.New("the payment is still in flight")

	// errWrongNode is returned when the operator asks to resolve a paid
	// swap while the bridge reads an incoming node other than the one that
	// issued its hold invoice, which has no record of it either way.
	errWrongNode = errors.New("the bridge is not connected to the node " +
		"that issued the hold invoice")

	// errIssuerHere is returned when the operator says a swap's issuing
	// node is gone while the bridge reads that very node, or the node it
	// reads still has the invoice.
	errIssuerHere = errors.New("the issuing node is not gone")
)

// resolve decides a swap the runner stopped in paying, for the operator, by
// asking the paying node again what became of its payment.
//
// The runner stops there when the node has no record of a payment the journal
// says was started, because "never sent" and "sent and lost track of" call for
// opposite actions and only a person can rule out the second (a node database
// restored from a backup, payments deleted by hand). Asking for this is the
// operator saying they have. What the node says then decides it:
//
//   - No record, or failed: the swap fails, and the payer's held payment is
//     cancelled, which refunds them.
//   - Succeeded: the preimage is recorded and the swap is paid, which settles
//     the payer's held payment with it.
//   - In flight: refused. The payment may yet go either way, and the runner
//     would not have stopped on it.
//
// Only a swap the runner stopped is decided here. One it is still driving
// learns a failure or a success by itself, and recording one for it from here
// as well would have two writers on one record.
func (s *service) resolve(ctx context.Context, hash node.Hash,
	issuerGone bool) (store.Record, error) {

	s.resolveMu.Lock()
	defer s.resolveMu.Unlock()

	rec, err := s.journal.Get(ctx, hash)
	if err != nil {
		return store.Record{}, err
	}
	s.stoppedMu.Lock()
	_, stopped := s.stopped[hash]
	s.stoppedMu.Unlock()

	if issuerGone {
		return s.resolveIssuerGone(ctx, rec, stopped)
	}

	if rec.State != swap.Paying && rec.State != swap.Paid {
		if stopped {
			// Stopped on its record rather than on a decision: there
			// is nothing here for the operator to decide, unless the
			// node that issued its invoice is gone for good.
			hint := ""
			if rec.IncomingNode != "" {
				hint = ". If it stopped because the bridge reads " +
					"a node other than the one that issued its " +
					"hold invoice, and that node is gone for " +
					"good, resolve it with issuer_gone"
			}

			return rec, fmt.Errorf("%w: it stopped in %v, on "+
				"something resolve cannot decide; fix what "+
				"needs_operator gives as the cause, and lnd "+
				"drives it again when it restarts%s",
				errNotStopped, rec.State, hint)
		}

		return rec, fmt.Errorf("%w: it is %v", errNotStopped, rec.State)
	}

	if !stopped {
		return rec, fmt.Errorf("%w: the bridge is still driving it, "+
			"and stops a swap for the operator only when it must "+
			"not decide it alone", errNotStopped)
	}

	sd, _, err := s.route(ctx, rec.Invoice)
	if err != nil {
		return rec, fmt.Errorf("finding the direction that pays it: %w",
			err)
	}

	if rec.State == swap.Paid {
		return s.resolvePaid(ctx, sd, rec)
	}

	pmt, err := sd.out.LookupPayment(ctx, rec.Outgoing())
	if err != nil {
		return rec, fmt.Errorf("asking the paying node: %w", err)
	}

	var event swap.Event
	switch pmt.State {
	case node.PaymentInFlight:
		return rec, errStillInFlight

	case node.PaymentSucceeded:
		if pmt.Preimage == (node.Preimage{}) {
			return rec, fmt.Errorf("%w: the paying node reports the "+
				"payment succeeded with no preimage",
				ErrNodeContradiction)
		}

		// As the driver files it: the preimage is what claims the
		// payer's payment when the two legs share a hash.
		rec.OutgoingPreimage = pmt.Preimage
		if rec.Binding == swap.HashLinked {
			rec.Preimage = pmt.Preimage
		}
		event = swap.OutgoingSucceeded

	case node.PaymentFailed, node.PaymentNone:
		event = swap.OutgoingFailed

	default:
		return rec, fmt.Errorf("the paying node reports payment state "+
			"%v, which this build does not know", pmt.State)
	}

	// Recorded before anything is done about it, as the runner does, and
	// then carried out by the runner: it reissues what a paid or failing
	// swap needs until the payer's payment is settled or cancelled.
	if _, err := sd.runner.Driver.Apply(ctx, &rec, event); err != nil {
		return rec, fmt.Errorf("recording the outcome: %w", err)
	}

	s.stoppedMu.Lock()
	delete(s.stopped, hash)
	s.stoppedMu.Unlock()

	log.Infof("Bridge swap %x resolved by the operator: the paying node "+
		"reports %v, so the swap is %v", hash, pmt.State, rec.State)

	s.drive(sd, hash)

	return rec, nil
}

// resolvePaid decides a swap the runner stopped in paid: paid out, and the
// incoming node had no record of the hold invoice whose HTLC settling would
// claim the payer's payment. lnd deletes only a cancelled invoice, so the
// claim is gone, unless the bridge was reading a node other than the one that
// issued it or one restored from an older backup. Asking for this is the
// operator saying they have ruled those out. What the node says now decides
// it:
//
//   - Still no record: the swap is lost, as it is when the invoice is there
//     but can no longer be claimed, and stays listed as lost. Refused if the
//     swap recorded which node issued the invoice and the bridge is reading
//     another: that one has no record of it whatever became of it.
//   - The invoice is there after all: the runner takes the swap back, and
//     settles it or files it lost by what the invoice is doing.
func (s *service) resolvePaid(ctx context.Context, sd *side,
	rec store.Record) (store.Record, error) {

	said := "has the hold invoice"
	_, err := sd.in.LookupInvoice(ctx, rec.Hash)
	switch {
	case errors.Is(err, node.ErrUnknownHash):
		if rec.IncomingNode != "" {
			key, err := sd.in.IdentityKey(ctx)
			if err != nil {
				return rec, fmt.Errorf("asking the incoming node "+
					"who it is: %w", err)
			}
			if key != rec.IncomingNode {
				return rec, fmt.Errorf("%w: it was issued by %s, "+
					"and the bridge reads %s", errWrongNode,
					rec.IncomingNode, key)
			}
		}

		said = "has no record of the hold invoice"
		_, err = sd.runner.Driver.Apply(ctx, &rec, swap.IncomingUnsettleable)
		if err != nil {
			return rec, fmt.Errorf("recording the outcome: %w", err)
		}

	case err != nil:
		return rec, fmt.Errorf("asking the incoming node: %w", err)
	}

	s.stoppedMu.Lock()
	delete(s.stopped, rec.Hash)
	s.stoppedMu.Unlock()

	log.Infof("Bridge swap %x resolved by the operator: paid out, and the "+
		"incoming node %s, so the swap is %v", rec.Hash, said, rec.State)

	s.drive(sd, rec.Hash)

	return rec, nil
}

// resolveIssuerGone decides a swap stopped because the bridge reads a node
// other than the one that issued its hold invoice, once the operator says
// that node is gone for good: its data lost, or replaced by a node with
// another seed. The swap's claim, or the payer's HTLC, went with it.
//
// Checked here rather than taken on trust: the bridge must still read a node
// other than the recorded issuer, and that node must have no record of the
// invoice. The swap then forgets which node issued it, and is decided as a
// swap from before issuers were recorded: one that has not paid out ends as
// given back (its payment checked first where one may have left), and a paid
// one is lost.
func (s *service) resolveIssuerGone(ctx context.Context, rec store.Record,
	stopped bool) (store.Record, error) {

	switch rec.State {
	case swap.Offered, swap.Funded, swap.Failing, swap.Aborting,
		swap.Expiring, swap.Paid:

	default:
		return rec, fmt.Errorf("%w: it is %v, and issuer_gone is for a "+
			"swap whose hold invoice is held", errNotStopped,
			rec.State)
	}
	if !stopped {
		return rec, fmt.Errorf("%w: the bridge is still driving it",
			errNotStopped)
	}
	if rec.IncomingNode == "" {
		return rec, fmt.Errorf("%w: the swap does not record which node "+
			"issued its hold invoice, so there is nothing to forget",
			errNotStopped)
	}

	sd, _, err := s.route(ctx, rec.Invoice)
	if err != nil {
		return rec, fmt.Errorf("finding the direction that pays it: %w",
			err)
	}
	key, err := sd.in.IdentityKey(ctx)
	if err != nil {
		return rec, fmt.Errorf("asking the incoming node who it is: %w",
			err)
	}
	if key == rec.IncomingNode {
		return rec, fmt.Errorf("%w: the bridge reads %s, the node that "+
			"issued the hold invoice", errIssuerHere, key)
	}
	_, err = sd.in.LookupInvoice(ctx, rec.Hash)
	switch {
	case errors.Is(err, node.ErrUnknownHash):

	case err != nil:
		return rec, fmt.Errorf("asking the incoming node: %w", err)

	default:
		return rec, fmt.Errorf("%w: %s, which the bridge reads, has the "+
			"hold invoice", errIssuerHere, key)
	}

	issuer := rec.IncomingNode
	rec.IncomingNode = ""
	rec.Note = fmt.Sprintf("the operator said %s, the node that issued "+
		"the hold invoice, is gone", issuer)
	rec.Updated = time.Now()
	if err := s.journal.Put(ctx, rec); err != nil {
		return rec, fmt.Errorf("recording the outcome: %w", err)
	}
	if rec.State == swap.Paid {
		_, err := sd.runner.Driver.Apply(ctx, &rec,
			swap.IncomingUnsettleable)
		if err != nil {
			return rec, fmt.Errorf("recording the outcome: %w", err)
		}
	}

	s.stoppedMu.Lock()
	delete(s.stopped, rec.Hash)
	s.stoppedMu.Unlock()

	log.Infof("Bridge swap %x resolved by the operator: %s, the node that "+
		"issued its hold invoice, is gone; the swap in %v is decided "+
		"without it", rec.Hash, issuer, rec.State)

	s.drive(sd, rec.Hash)

	return rec, nil
}
