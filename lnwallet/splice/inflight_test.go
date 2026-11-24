//go:build !dev && !integration

package splice

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// percentOf returns a max_htlc_value_in_flight policy of a share of the
// capacity, in percent, as a node's default at opening.
func percentOf(pct int64) func(btcutil.Amount) lnwire.MilliSatoshi {
	return func(capacity btcutil.Amount) lnwire.MilliSatoshi {
		share := capacity * btcutil.Amount(pct) / 100

		return lnwire.NewMSatFromSatoshis(share)
	}
}

// TestOurMaxInFlight checks the value this node sets for a splice: the
// policy's for the splice's capacity, also when it wouldn't raise the value
// in force or the channel was opened with a lower cap, and none when there is
// no policy.
func TestOurMaxInFlight(t *testing.T) {
	t.Parallel()

	// The test channels open with 100% of their capacity each way.
	t.Run("always the policy's", func(t *testing.T) {
		t.Parallel()

		n, _ := soloAlice(t)
		n.machine.cfg.Policy.MaxValueInFlight = percentOf(99)
		capacity := n.ch.State().Capacity

		require.Equal(t, fn.Some(percentOf(99)(2*capacity)),
			n.machine.ourMaxInFlight(2*capacity))

		// Below what is in force (100% of the capacity now): sent
		// all the same, with no effect.
		require.Equal(t, fn.Some(percentOf(99)(capacity)),
			n.machine.ourMaxInFlight(capacity))
		require.True(t, n.machine.ourMaxInFlight(0).IsNone())
	})

	t.Run("no policy", func(t *testing.T) {
		t.Parallel()

		n, _ := soloAlice(t)
		capacity := n.ch.State().Capacity
		require.True(t, n.machine.ourMaxInFlight(2*capacity).IsNone())
	})

	t.Run("a cap chosen at opening", func(t *testing.T) {
		t.Parallel()

		n, _ := soloAlice(t)
		n.machine.cfg.Policy.MaxValueInFlight = percentOf(99)
		state := n.ch.State()
		capacity := state.Capacity

		// Opened requiring half of the capacity of the peer: the
		// splice raises it like any other.
		state.RemoteChanCfg.MaxPendingAmount =
			lnwire.NewMSatFromSatoshis(capacity / 2)
		require.Equal(t, fn.Some(percentOf(99)(2*capacity)),
			n.machine.ourMaxInFlight(2*capacity))
	})
}

// TestTheirMaxInFlight checks the peer's value for an attempt: the one in
// its message, else the one it set for the attempt replaced.
func TestTheirMaxInFlight(t *testing.T) {
	t.Parallel()

	none := fn.None[lnwire.MilliSatoshi]()
	replaced := &active{theirMaxInFlight: 7}

	require.Zero(t, theirMaxInFlight(none, nil))
	require.Equal(t, lnwire.MilliSatoshi(5),
		theirMaxInFlight(fn.Some(lnwire.MilliSatoshi(5)), nil))
	require.Equal(t, lnwire.MilliSatoshi(7),
		theirMaxInFlight(none, replaced))
	require.Equal(t, lnwire.MilliSatoshi(3),
		theirMaxInFlight(fn.Some(lnwire.MilliSatoshi(3)), replaced))
}

// negotiationOf returns a splice's negotiation as recorded.
func negotiationOf(t *testing.T, state *channeldb.OpenChannel,
	txid [32]byte) *channeldb.SpliceNegotiation {

	t.Helper()

	records, err := state.SpliceRecords()
	require.NoError(t, err)
	for _, r := range records {
		if r.Txid() == txid && r.Negotiation != nil {
			return r.Negotiation
		}
	}
	t.Fatalf("no record of splice %x", txid)

	return nil
}

// TestSpliceInFlightBetweenMachines runs a splice-in between two machines
// with different policies: each sends its value for the new capacity, each
// records both, and after the lock each reads the raised values back as the
// ones in force with the new funding.
func TestSpliceInFlightBetweenMachines(t *testing.T) {
	t.Parallel()

	p := newSplicePair(t)
	p.alice.machine.cfg.Policy.MaxValueInFlight = percentOf(99)
	p.bob.machine.cfg.Policy.MaxValueInFlight = percentOf(98)
	p.alice.fund(t, 1, 40_000_000)
	capacity := p.alice.ch.State().Capacity
	const amount = btcutil.Amount(30_000_000)

	updates, err := p.alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: amount, FeeRate: 2_500,
	})
	require.NoError(t, err)
	u := requireUpdates(t, updates,
		StatePreparing, StateNegotiating, StateSigned, StateBroadcast,
	)
	txid := u.Txid

	newCapacity := capacity + amount
	aliceValue := percentOf(99)(newCapacity)
	bobValue := percentOf(98)(newCapacity)

	n := negotiationOf(t, p.alice.ch.State(), txid)
	require.Equal(t, aliceValue, n.OurMaxInFlight)
	require.Equal(t, bobValue, n.TheirMaxInFlight)
	require.Eventually(t, func() bool {
		records, err := p.bob.ch.State().SpliceRecords()
		return err == nil && len(records) == 1 &&
			records[0].Negotiation != nil &&
			records[0].Negotiation.TheirMaxInFlight == aliceValue
	}, 10*time.Second, 10*time.Millisecond)
	n = negotiationOf(t, p.bob.ch.State(), txid)
	require.Equal(t, bobValue, n.OurMaxInFlight)

	p.confirm(t, txid)
	requireUpdates(t, updates, StateLocked)
	p.requireMoved(t, txid, newCapacity)

	for _, c := range []struct {
		n            *node
		ours, theirs lnwire.MilliSatoshi
	}{
		{p.alice, aliceValue, bobValue},
		{p.bob, bobValue, aliceValue},
	} {
		state := c.n.ch.State()
		ours, theirs, known, err := state.SpliceMaxInFlight(
			state.CurrentFundingOutpoint(),
		)
		require.NoError(t, err)
		require.True(t, known)
		require.Equal(t, c.ours, ours)
		require.Equal(t, c.theirs, theirs)

		// The opening values stay those of the funding left.
		ours, theirs, _, err = state.SpliceMaxInFlight(
			state.FundingOutpoint,
		)
		require.NoError(t, err)
		require.Equal(t, lnwire.NewMSatFromSatoshis(capacity), ours)
		require.Equal(t, lnwire.NewMSatFromSatoshis(capacity), theirs)
	}
}

// TestAcceptSpliceInFlightReplaced checks the values of a replacement on the
// acceptor's side: ours sent again for the replacement's capacity, and the
// peer's, absent from its tx_init_rbf, the one it set for the attempt it
// replaces.
func TestAcceptSpliceInFlightReplaced(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	h.machine.cfg.Policy.MaxValueInFlight = percentOf(98)
	capacity := h.bob.State().Capacity
	const amount = btcutil.Amount(40_000_000)
	want := percentOf(98)(capacity + amount)

	first := startAliceSplice(t, h, amount, 2_500)
	first.construct(t, h)
	bobKey := signSplice(t, h, first, nil)

	n := negotiationOf(t, h.bob.State(), first.res.Tx.TxHash())
	require.Equal(t, want, n.OurMaxInFlight)
	require.Zero(t, n.TheirMaxInFlight)

	// As if Alice had set hers in her splice_init.
	const aliceValue = lnwire.MilliSatoshi(1_234_567_000)
	h.machine.enqueue(func() {
		h.machine.update(func(a *active) {
			a.theirMaxInFlight = aliceValue
		})
	})

	second := replaceAliceSplice(t, h, first, bobKey, 5_000)
	second.construct(t, h)
	signSplice(t, h, second, bobKey)

	n = negotiationOf(t, h.bob.State(), second.res.Tx.TxHash())
	require.Equal(t, want, n.OurMaxInFlight)
	require.Equal(t, aliceValue, n.TheirMaxInFlight)

	// Locked on the replacement, its values are the ones in force.
	lockOn(t, h, second)
	state := h.bob.State()
	ours, theirs, known, err := state.SpliceMaxInFlight(
		state.CurrentFundingOutpoint(),
	)
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, want, ours)
	require.Equal(t, max(aliceValue, lnwire.NewMSatFromSatoshis(capacity)),
		theirs)
}

// rewriteNegotiation changes the recorded negotiation of a splice attempt.
func rewriteNegotiation(t *testing.T, state *channeldb.OpenChannel,
	txid [32]byte, f func(*channeldb.SpliceNegotiation)) {

	t.Helper()

	records, err := state.SpliceRecords()
	require.NoError(t, err)
	for _, r := range records {
		if r.Txid() == txid && r.Negotiation != nil {
			f(r.Negotiation)
			require.NoError(t, state.PutSpliceRecord(r))

			return
		}
	}
	t.Fatalf("no record of splice %x", txid)
}

// activeOf returns the machine's splice and the values a replacement of it
// inherits, read on the machine's goroutine.
func activeOf(t *testing.T, m *Machine) ([32]byte, inFlightPair) {
	t.Helper()

	type result struct {
		txid  [32]byte
		inher inFlightPair
	}
	done := make(chan result, 1)
	m.enqueue(func() {
		a := m.active
		done <- result{txid: a.txid, inher: inherited(a)}
	})
	select {
	case r := <-done:
		return r.txid, r.inher
	case <-time.After(10 * time.Second):
		t.Fatal("machine didn't answer")
	}

	return [32]byte{}, inFlightPair{}
}

// TestSpliceInFlightLastAgreedRestart checks that after a restart a
// replacement inherits the peer's value from the splice's last attempt
// agreed, also when that one died and an earlier one is the splice again,
// and that attempts of equal fee rates are told apart by when they were
// agreed.
func TestSpliceInFlightLastAgreedRestart(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (*acceptorHarness, *aliceSplice,
		*aliceSplice) {

		h := newAcceptorHarness(t)
		h.machine.cfg.Policy.MaxValueInFlight = percentOf(98)
		const amount = btcutil.Amount(40_000_000)

		first := startAliceSplice(t, h, amount, 2_500)
		first.construct(t, h)
		bobKey := signSplice(t, h, first, nil)
		second := replaceAliceSplice(t, h, first, bobKey, 5_000)
		second.construct(t, h)
		signSplice(t, h, second, bobKey)

		return h, first, second
	}
	restart := func(h *acceptorHarness) {
		h.machine.Stop()
		h.machine = NewMachine(h.machine.cfg)
		startReestablished(t, h.machine)
	}

	t.Run("dead last attempt", func(t *testing.T) {
		t.Parallel()

		h, first, second := setup(t)
		state := h.bob.State()
		firstTxid := first.res.Tx.TxHash()
		secondTxid := second.res.Tx.TxHash()
		rewriteNegotiation(t, state, firstTxid,
			func(n *channeldb.SpliceNegotiation) {
				n.TheirMaxInFlight = 111_000
				n.AgreedAt = 1_000
			})
		rewriteNegotiation(t, state, secondTxid,
			func(n *channeldb.SpliceNegotiation) {
				n.TheirMaxInFlight = 222_000
				n.AgreedAt = 2_000
				n.Dead = true
			})
		secondOurs := negotiationOf(t, state, secondTxid).OurMaxInFlight

		restart(h)
		txid, inher := activeOf(t, h.machine)
		require.Equal(t, [32]byte(firstTxid), txid)
		require.Equal(t, inFlightPair{
			ours: secondOurs, theirs: 222_000,
		}, inher)
	})

	t.Run("equal fee rates", func(t *testing.T) {
		t.Parallel()

		h, first, second := setup(t)
		state := h.bob.State()
		firstTxid := first.res.Tx.TxHash()
		secondTxid := second.res.Tx.TxHash()
		rewriteNegotiation(t, state, secondTxid,
			func(n *channeldb.SpliceNegotiation) {
				n.TheirMaxInFlight = 222_000
				n.AgreedAt = 2_000
			})
		rate := negotiationOf(t, state, secondTxid).FeeRate
		rewriteNegotiation(t, state, firstTxid,
			func(n *channeldb.SpliceNegotiation) {
				n.FeeRate = rate
				n.TheirMaxInFlight = 333_000
				n.AgreedAt = 3_000
			})

		// The one agreed last is the splice, whichever the fee.
		restart(h)
		txid, inher := activeOf(t, h.machine)
		require.Equal(t, [32]byte(firstTxid), txid)
		require.Equal(t, lnwire.MilliSatoshi(333_000), inher.theirs)
	})
}

// TestKeepLastAgreed checks the values a replacement inherits once a later
// attempt is dropped or dies: those of the later one if it was agreed, the
// earlier one's own otherwise.
func TestKeepLastAgreed(t *testing.T) {
	t.Parallel()

	none := fn.None[lnwire.MilliSatoshi]()
	newPrev := func() *active {
		return &active{ourMaxInFlight: 1, theirMaxInFlight: 2}
	}

	// Not agreed: nothing to remember.
	prev := newPrev()
	keepLastAgreed(&active{ourMaxInFlight: 3, theirMaxInFlight: 4}, prev)
	require.Equal(t, inFlightPair{ours: 1, theirs: 2}, inherited(prev))

	// Negotiated but dropped before both signed: not agreed either.
	prev = newPrev()
	keepLastAgreed(&active{
		ourMaxInFlight: 3, theirMaxInFlight: 4,
		res:       &interactivetx.Result{},
		ourTxSigs: &lnwire.TxSignatures{},
	}, prev)
	require.Equal(t, inFlightPair{ours: 1, theirs: 2}, inherited(prev))

	// Signed by both: its values, also through a chain of them.
	prev = newPrev()
	later := &active{
		ourMaxInFlight: 3, theirMaxInFlight: 4,
		res:         &interactivetx.Result{},
		ourTxSigs:   &lnwire.TxSignatures{},
		theirTxSigs: &lnwire.TxSignatures{},
	}
	latest := &active{
		ourMaxInFlight: 5, theirMaxInFlight: 6,
		res:         &interactivetx.Result{},
		ourTxSigs:   &lnwire.TxSignatures{},
		theirTxSigs: &lnwire.TxSignatures{},
	}
	keepLastAgreed(latest, later)
	keepLastAgreed(later, prev)
	require.Equal(t, inFlightPair{ours: 5, theirs: 6}, inherited(prev))
	require.Equal(t, lnwire.MilliSatoshi(6), theirMaxInFlight(none, prev))

	// An unsigned attempt passes on what it kept from a later signed one.
	prev = newPrev()
	unsigned := &active{ourMaxInFlight: 7, theirMaxInFlight: 8}
	keepLastAgreed(latest, unsigned)
	keepLastAgreed(unsigned, prev)
	require.Equal(t, inFlightPair{ours: 5, theirs: 6}, inherited(prev))

	// Its own values still apply to it.
	require.Equal(t, lnwire.MilliSatoshi(1), prev.ourMaxInFlight)
}
