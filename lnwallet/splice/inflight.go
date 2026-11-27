package splice

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// A splice can raise the max_htlc_value_in_flight_msat each node requires of
// the other (bolts#20): each sets one in splice_init, splice_ack or the RBF
// message of an attempt, as it would at opening for the splice's capacity.
// The machine sends this node's, records both nodes' with the attempt, and
// leaves applying them to the channel, at the exchange where the reserves
// change, to the code that applies those.

// ourMaxInFlight returns the max_htlc_value_in_flight_msat this node sets for
// a splice transaction of the capacity given: the one it would require at
// opening for a channel of that capacity, as BOLT 2 has a node set it. It is
// set with every splice, also when it is not above the value in force: the
// value in force is the greatest set, so a lower one has no effect. A cap
// chosen lower at opening (remote_max_value_in_flight_msat, or a channel
// acceptor) is raised by the next splice to this default for the new
// capacity, by design: the splice's capacity, not the opening choice, sets
// it. None when the policy gives none.
func (m *Machine) ourMaxInFlight(
	capacity btcutil.Amount) fn.Option[lnwire.MilliSatoshi] {

	maxValue := m.cfg.Policy.MaxValueInFlight
	if maxValue == nil || capacity <= 0 {
		return fn.None[lnwire.MilliSatoshi]()
	}

	return fn.Some(maxValue(capacity))
}

// inFlightPair is the max_htlc_value_in_flight_msat each node set for an
// attempt of a splice.
type inFlightPair struct {
	ours, theirs lnwire.MilliSatoshi
}

// inherited returns the values a replacement of a's splice inherits for a
// node whose message omits its own: those set for the splice's last attempt
// agreed, a itself unless a later one was dropped or died after agreement.
func inherited(a *active) inFlightPair {
	if a.lastAgreed != nil {
		return *a.lastAgreed
	}

	return inFlightPair{ours: a.ourMaxInFlight, theirs: a.theirMaxInFlight}
}

// keepLastAgreed has prev, the splice again, remember the values of a, the
// attempt that replaced it and is dropped or dead, if both sides signed a:
// still the last attempt agreed, as checkReplacementFee reads it. An attempt
// dropped before both signed passes on only what it kept from a later one.
// A signed attempt's record stays, so a restart finds it again (see
// restore).
func keepLastAgreed(a, prev *active) {
	switch {
	case signedBoth(a):
		last := inherited(a)
		prev.lastAgreed = &last

	case a.lastAgreed != nil:
		last := *a.lastAgreed
		prev.lastAgreed = &last
	}
}

// theirMaxInFlight returns the max_htlc_value_in_flight_msat the peer set for
// an attempt in its message, or, absent from a replacement's, the one it set
// for the splice's last attempt agreed. Zero if it set none.
func theirMaxInFlight(sent fn.Option[lnwire.MilliSatoshi],
	replaced *active) lnwire.MilliSatoshi {

	inheritedValue := lnwire.MilliSatoshi(0)
	if replaced != nil {
		inheritedValue = inherited(replaced).theirs
	}

	return sent.UnwrapOr(inheritedValue)
}
