package interactivetx

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

var (
	sharedOutPoint = wire.OutPoint{Hash: chainhash.Hash{0xf0}, Index: 1}
	fundingScript  = append([]byte{0x00, 0x20}, bytes.Repeat(
		[]byte{0xfa}, 32,
	)...)
	oldFundingScript = append([]byte{0x00, 0x20}, bytes.Repeat(
		[]byte{0xfb}, 32,
	)...)
)

func p2wpkh(tag byte) []byte {
	return append([]byte{0x00, 0x14}, bytes.Repeat([]byte{tag}, 20)...)
}

func p2tr(tag byte) []byte {
	return append([]byte{0x51, 0x20}, bytes.Repeat([]byte{tag}, 32)...)
}

func p2wsh(tag byte) []byte {
	return append([]byte{0x00, 0x20}, bytes.Repeat([]byte{tag}, 32)...)
}

// prevTx returns a transaction paying value to script at output 0, made
// unique by tag.
func prevTx(tag byte, script []byte, value btcutil.Amount) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{tag}},
	})
	tx.AddTxOut(&wire.TxOut{Value: int64(value), PkScript: script})

	return tx
}

// witnessWeight accepts P2WPKH and P2TR key path inputs only.
func witnessWeight(pkScript []byte) (int64, error) {
	switch {
	case len(pkScript) == 22 && pkScript[0] == 0x00:
		return input.P2WKHWitnessSize, nil
	case len(pkScript) == 34 && pkScript[0] == 0x51:
		return 1 + 1 + 65, nil
	default:
		return 0, errors.New("only P2WPKH and P2TR key path inputs")
	}
}

// dustLimit is 294 for P2WPKH and 330 for the other segwit outputs, as
// Bitcoin Core computes them at its default relay fee.
func dustLimit(pkScript []byte) btcutil.Amount {
	if len(pkScript) == 22 {
		return 294
	}

	return 330
}

type params struct {
	capacity      btcutil.Amount
	initBalance   btcutil.Amount
	accBalance    btcutil.Amount
	initContrib   btcutil.Amount
	accContrib    btcutil.Amount
	feeRate       chainfee.SatPerKWeight
	lockTime      uint32
	maxPeerAddMsg int
}

func defaultParams() params {
	return params{
		capacity:    1_000_000,
		initBalance: 600_000,
		accBalance:  400_000,
		feeRate:     1000,
		lockTime:    900_000,
	}
}

func config(role Role, p params) Config {
	return Config{
		Role: role,
		Shared: SharedInput{
			OutPoint:      sharedOutPoint,
			Value:         p.capacity,
			PkScript:      oldFundingScript,
			WitnessWeight: input.MultiSigWitnessSize,
		},
		FundingScript:         fundingScript,
		InitiatorContribution: p.initContrib,
		AcceptorContribution:  p.accContrib,
		FeeRate:               p.feeRate,
		LockTime:              p.lockTime,
		MaxPeerAddMessages:    p.maxPeerAddMsg,
		WitnessWeight:         witnessWeight,
		Reserve: func(c btcutil.Amount) btcutil.Amount {
			return c / 100
		},
		CheckPeerInput:   func(*Input) error { return nil },
		CheckPeerOutput:  func(*Output) error { return nil },
		DustLimit:        dustLimit,
		InitiatorBalance: p.initBalance,
		AcceptorBalance:  p.accBalance,
	}
}

// pair returns an initiator and an acceptor session.
func pair(t *testing.T, p params) (*Session, *Session) {
	t.Helper()

	init, err := NewSession(config(Initiator, p))
	require.NoError(t, err)
	acc, err := NewSession(config(Acceptor, p))
	require.NoError(t, err)
	require.NoError(t, init.AddSharedInputAndFunding())

	return init, acc
}

// run exchanges messages until both are done or one fails.
func run(a, b *Session) error {
	for i := 0; i < 10_000; i++ {
		if a.Done() && b.Done() {
			return nil
		}

		from, to := a, b
		if !a.OurTurn() {
			from, to = b, a
		}
		msg, err := from.Next()
		if err != nil {
			return err
		}
		if err := to.Receive(msg); err != nil {
			return err
		}
	}

	return errors.New("no end in sight")
}

func requireAbort(t *testing.T, err error, contains string) {
	t.Helper()

	var abort *AbortError
	require.ErrorAs(t, err, &abort)
	require.Contains(t, abort.Reason, contains)
}

func TestSpliceInByInitiator(t *testing.T) {
	t.Parallel()

	p := defaultParams()
	p.initContrib = 500_000
	init, acc := pair(t, p)

	wallet := prevTx(1, p2wpkh(1), 600_000)
	init.AddLocalInput(Input{
		OutPoint:         wire.OutPoint{Hash: wallet.TxHash()},
		Value:            600_000,
		PkScript:         p2wpkh(1),
		Sequence:         MaxSequence,
		MaxWitnessWeight: input.P2WKHWitnessSize,
		PrevTx:           wallet,
	})
	init.AddLocalOutput(Output{Value: 98_500, PkScript: p2wpkh(2)})

	require.NoError(t, run(init, acc))

	ri, err := init.Build()
	require.NoError(t, err)
	ra, err := acc.Build()
	require.NoError(t, err)

	// Both sides build the same transaction.
	require.Equal(t, ri.Tx.TxHash(), ra.Tx.TxHash())
	require.Equal(t, btcutil.Amount(1_500_000),
		btcutil.Amount(ra.Tx.TxOut[ra.FundingIndex].Value))
	require.Equal(t, btcutil.Amount(1_500), ra.Fee)
	require.GreaterOrEqual(t, int64(ra.Fee),
		int64(p.feeRate.FeeForWeight(0))+ra.Weight*int64(p.feeRate)/1000)
	require.Equal(t, uint32(p.lockTime), ra.Tx.LockTime)

	// Inputs and outputs are in serial id order: the shared input and the
	// funding output first, as the initiator queued them.
	require.True(t, ra.Inputs[0].Shared)
	require.Equal(t, sharedOutPoint, ra.Tx.TxIn[0].PreviousOutPoint)
	require.True(t, ra.Outputs[0].Funding)
}

func TestSpliceOutByInitiator(t *testing.T) {
	t.Parallel()

	// The amount entered arrives in full; the initiator's contribution
	// covers it and the fee.
	p := defaultParams()
	const fee = 1_500
	p.initContrib = -(300_000 + fee)
	init, acc := pair(t, p)
	init.AddLocalOutput(Output{Value: 300_000, PkScript: p2tr(9)})

	require.NoError(t, run(init, acc))
	res, err := acc.Build()
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(fee), res.Fee)
	require.Equal(t, btcutil.Amount(1_000_000-301_500),
		btcutil.Amount(res.Tx.TxOut[res.FundingIndex].Value))
}

func TestAcceptorAddsNothing(t *testing.T) {
	t.Parallel()

	// A peer's splice in which this node, the acceptor, contributes
	// nothing: it pays nothing and signs first.
	p := defaultParams()
	p.initContrib = 200_000
	init, acc := pair(t, p)
	wallet := prevTx(3, p2tr(3), 250_000)
	init.AddLocalInput(Input{
		OutPoint:         wire.OutPoint{Hash: wallet.TxHash()},
		Value:            250_000,
		PkScript:         p2tr(3),
		Sequence:         MaxSequence,
		MaxWitnessWeight: 67,
		PrevTx:           wallet,
	})
	init.AddLocalOutput(Output{Value: 48_500, PkScript: p2tr(4)})

	require.NoError(t, run(init, acc))
	res, err := acc.Build()
	require.NoError(t, err)

	require.True(t, SignsFirst(res, Acceptor, []byte{9}, []byte{1}))
	require.False(t, SignsFirst(res, Initiator, []byte{1}, []byte{9}))
}

// The refusals below run the peer's messages straight into a session.

// acceptorFor returns an acceptor session that has had its turn to start:
// the initiator sends first.
func acceptorFor(t *testing.T, p params) *Session {
	t.Helper()

	acc, err := NewSession(config(Acceptor, p))
	require.NoError(t, err)

	return acc
}

func sharedAdd(serial uint64) *lnwire.TxAddInput {
	return &lnwire.TxAddInput{
		SerialID:        serial,
		PrevTxVout:      sharedOutPoint.Index,
		Sequence:        MaxSequence,
		SharedInputTxid: fn.Some(sharedOutPoint.Hash),
	}
}

func walletAdd(t *testing.T, serial uint64, tx *wire.MsgTx,
	vout uint32) *lnwire.TxAddInput {

	t.Helper()

	var b bytes.Buffer
	require.NoError(t, tx.Serialize(&b))

	return &lnwire.TxAddInput{
		SerialID:   serial,
		PrevTx:     b.Bytes(),
		PrevTxVout: vout,
		Sequence:   MaxSequence,
	}
}

// exchange feeds msgs to s as the peer's, answering each with s's own next
// message, and returns the first error.
func exchange(s *Session, msgs ...lnwire.Message) error {
	for _, m := range msgs {
		if err := s.Receive(m); err != nil {
			return err
		}
		if s.OurTurn() {
			if _, err := s.Next(); err != nil {
				return err
			}
		}
	}

	return nil
}

func TestPeerInputRefusals(t *testing.T) {
	t.Parallel()

	wallet := prevTx(4, p2wpkh(4), 100_000)
	legacy := prevTx(5, []byte{0x76, 0xa9, 0x14}, 100_000)
	scriptHash := prevTx(6, p2wsh(6), 100_000)

	cases := []struct {
		name     string
		msgs     func(t *testing.T) []lnwire.Message
		contains string
	}{{
		name: "odd serial from the initiator",
		msgs: func(t *testing.T) []lnwire.Message {
			return []lnwire.Message{walletAdd(t, 1, wallet, 0)}
		},
		contains: "parity",
	}, {
		name: "serial id reused",
		msgs: func(t *testing.T) []lnwire.Message {
			return []lnwire.Message{
				walletAdd(t, 2, wallet, 0),
				&lnwire.TxAddOutput{SerialID: 2, Amount: 1,
					Script: p2wpkh(7)},
			}
		},
		contains: "already used",
	}, {
		name: "same outpoint twice",
		msgs: func(t *testing.T) []lnwire.Message {
			return []lnwire.Message{
				walletAdd(t, 2, wallet, 0),
				walletAdd(t, 4, wallet, 0),
			}
		},
		contains: "already added",
	}, {
		name: "the funding output as a plain input",
		msgs: func(t *testing.T) []lnwire.Message {
			// A prevtx whose hash is the funding txid can't be
			// forged, so check by outpoint: the shared input added
			// twice.
			return []lnwire.Message{sharedAdd(0), sharedAdd(2)}
		},
		contains: "already added",
	}, {
		name: "the shared input with a prevtx",
		msgs: func(t *testing.T) []lnwire.Message {
			m := sharedAdd(0)
			m.PrevTx = []byte{0x01}

			return []lnwire.Message{m}
		},
		contains: "carries a prevtx",
	}, {
		name: "a shared input that isn't the funding output",
		msgs: func(t *testing.T) []lnwire.Message {
			m := sharedAdd(0)
			m.PrevTxVout = 7

			return []lnwire.Message{m}
		},
		contains: "is not the funding output",
	}, {
		name: "a non-segwit input",
		msgs: func(t *testing.T) []lnwire.Message {
			return []lnwire.Message{walletAdd(t, 2, legacy, 0)}
		},
		contains: "non-segwit",
	}, {
		name: "a P2WSH input, whose witness size is unknowable",
		msgs: func(t *testing.T) []lnwire.Message {
			return []lnwire.Message{
				walletAdd(t, 2, scriptHash, 0),
			}
		},
		contains: "type refused",
	}, {
		name: "a final sequence",
		msgs: func(t *testing.T) []lnwire.Message {
			m := walletAdd(t, 2, wallet, 0)
			m.Sequence = 0xffffffff

			return []lnwire.Message{m}
		},
		contains: "final",
	}, {
		name: "a relative lock in blocks",
		msgs: func(t *testing.T) []lnwire.Message {
			m := walletAdd(t, 2, wallet, 0)
			m.Sequence = 0x0000ffff

			return []lnwire.Message{m}
		},
		contains: "relative lock",
	}, {
		name: "a relative lock of one block",
		msgs: func(t *testing.T) []lnwire.Message {
			m := walletAdd(t, 2, wallet, 0)
			m.Sequence = 1

			return []lnwire.Message{m}
		},
		contains: "relative lock",
	}, {
		name: "a relative lock in time on the shared input",
		msgs: func(t *testing.T) []lnwire.Message {
			m := sharedAdd(0)
			m.Sequence = 0x0040ffff

			return []lnwire.Message{m}
		},
		contains: "relative lock",
	}, {
		name: "a prevtx counting four million witness items",
		msgs: func(t *testing.T) []lnwire.Message {
			// Version, marker and flag; one input of an empty
			// script; one output of an empty script; a witness
			// count of 4,000,000 and nothing after it.
			b := []byte{2, 0, 0, 0, 0x00, 0x01, 0x01}
			b = append(b, make([]byte, 36)...)
			b = append(b, 0x00, 0xff, 0xff, 0xff, 0xff)
			b = append(b, 0x01)
			b = append(b, make([]byte, 8)...)
			b = append(b, 0x00)
			b = append(b, 0xfe, 0x00, 0x09, 0x3d, 0x00)

			return []lnwire.Message{&lnwire.TxAddInput{
				SerialID: 2, PrevTx: b,
				Sequence: MaxSequence,
			}}
		},
		contains: "count larger than the transaction",
	}, {
		name: "prevtx_vout out of range",
		msgs: func(t *testing.T) []lnwire.Message {
			return []lnwire.Message{walletAdd(t, 2, wallet, 3)}
		},
		contains: "out of range",
	}, {
		name: "a prevtx that doesn't parse",
		msgs: func(t *testing.T) []lnwire.Message {
			m := walletAdd(t, 2, wallet, 0)
			m.PrevTx = m.PrevTx[:10]

			return []lnwire.Message{m}
		},
		contains: "doesn't parse",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := acceptorFor(t, defaultParams())
			requireAbort(t, exchange(acc, tc.msgs(t)...),
				tc.contains)
		})
	}
}

// TestPeerInputSequencesAccepted checks the sequences that hold nothing
// back: a relative lock of zero (the Core Lightning port's 0), and any with
// BIP 68's disable bit.
func TestPeerInputSequencesAccepted(t *testing.T) {
	t.Parallel()

	wallet := prevTx(4, p2wpkh(4), 100_000)
	for _, seq := range []uint32{0, 0x00400000, 0x80000001, MaxSequence} {
		acc := acceptorFor(t, defaultParams())
		m := walletAdd(t, 2, wallet, 0)
		m.Sequence = seq
		require.NoError(t, acc.Receive(m), "sequence %#x", seq)
	}
}

func TestSharedInputFromAcceptor(t *testing.T) {
	t.Parallel()

	// This node is the initiator; the peer, the acceptor, tries to add
	// the shared input.
	p := defaultParams()
	init, err := NewSession(config(Initiator, p))
	require.NoError(t, err)
	_, err = init.Next() // our tx_complete, nothing queued
	require.NoError(t, err)

	requireAbort(t, init.Receive(sharedAdd(1)), "acceptor added the shared")
}

func TestPeerPolicyHooks(t *testing.T) {
	t.Parallel()

	wallet := prevTx(8, p2wpkh(8), 100_000)

	p := defaultParams()
	cfg := config(Acceptor, p)
	var seen *Input
	cfg.CheckPeerInput = func(in *Input) error {
		seen = in

		return errors.New("a coinbase")
	}
	acc, err := NewSession(cfg)
	require.NoError(t, err)
	requireAbort(t, acc.Receive(walletAdd(t, 2, wallet, 0)), "a coinbase")

	// The hook sees what was parsed from the prevtx.
	require.Equal(t, wire.OutPoint{Hash: wallet.TxHash()}, seen.OutPoint)
	require.Equal(t, btcutil.Amount(100_000), seen.Value)

	cfg = config(Acceptor, p)
	cfg.CheckPeerOutput = func(out *Output) error {
		return errors.New("an OP_RETURN")
	}
	acc, err = NewSession(cfg)
	require.NoError(t, err)
	requireAbort(t, acc.Receive(&lnwire.TxAddOutput{
		SerialID: 2, Amount: 1_000, Script: []byte{0x6a},
	}), "an OP_RETURN")
}

func TestPeerAddLimit(t *testing.T) {
	t.Parallel()

	p := defaultParams()
	p.maxPeerAddMsg = 2
	acc := acceptorFor(t, p)

	msgs := []lnwire.Message{
		&lnwire.TxAddOutput{SerialID: 2, Amount: 1000,
			Script: p2wpkh(1)},
		&lnwire.TxAddOutput{SerialID: 4, Amount: 1000,
			Script: p2wpkh(2)},
		&lnwire.TxAddOutput{SerialID: 6, Amount: 1000,
			Script: p2wpkh(3)},
	}
	requireAbort(t, exchange(acc, msgs...), "more than 2 tx_add_output")
}

func TestTurnTaking(t *testing.T) {
	t.Parallel()

	// The initiator sends first: the acceptor can't speak first, and a
	// peer can't send twice in a row.
	acc := acceptorFor(t, defaultParams())
	_, err := acc.Next()
	require.ErrorIs(t, err, ErrNotOurTurn)

	require.NoError(t, acc.Receive(sharedAdd(0)))
	requireAbort(t, acc.Receive(&lnwire.TxComplete{}), "out of turn")
}

func TestRemoveRules(t *testing.T) {
	t.Parallel()

	wallet := prevTx(9, p2wpkh(9), 100_000)

	acc := acceptorFor(t, defaultParams())
	requireAbort(t, exchange(acc,
		walletAdd(t, 2, wallet, 0),
		&lnwire.TxRemoveInput{SerialID: 4},
	), "no input")

	// The peer can't remove what this node added.
	acc = acceptorFor(t, defaultParams())
	requireAbort(t, exchange(acc,
		&lnwire.TxRemoveOutput{SerialID: 1},
	), "isn't the peer's")

	// A removed input frees its outpoint and doesn't count at the end.
	p := defaultParams()
	init, acc2 := pair(t, p)
	_, err := init.Next() // shared input
	require.NoError(t, err)
	require.NoError(t, exchange(acc2,
		sharedAdd(0),
		walletAdd(t, 2, wallet, 0),
		&lnwire.TxRemoveInput{SerialID: 2},
		walletAdd(t, 4, wallet, 0),
	))
}

func TestAcceptorAddsFundingOutput(t *testing.T) {
	t.Parallel()

	p := defaultParams()
	init, err := NewSession(config(Initiator, p))
	require.NoError(t, err)
	_, err = init.Next()
	require.NoError(t, err)

	requireAbort(t, init.Receive(&lnwire.TxAddOutput{
		SerialID: 1, Amount: 1_000, Script: fundingScript,
	}), "acceptor added the funding output")
}

// TestBuildRefusals covers what's only known once both sides are done.
func TestBuildRefusals(t *testing.T) {
	t.Parallel()

	t.Run("the peer pays itself from our balance", func(t *testing.T) {
		// The initiator adds an output to itself with no inputs and
		// no contribution: the shared input's value would pay for it
		// out of the channel.
		p := defaultParams()
		init, acc := pair(t, p)
		init.AddLocalOutput(Output{Value: 300_000, PkScript: p2wpkh(1)})
		require.NoError(t, run(init, acc))
		_, err := acc.Build()
		requireAbort(t, err, "don't cover its fee")
	})

	t.Run("the funding output's value is wrong", func(t *testing.T) {
		// The initiator claims a bigger contribution than it told
		// the acceptor.
		p := defaultParams()
		pi := p
		pi.initContrib = 100_000
		init, err := NewSession(config(Initiator, pi))
		require.NoError(t, err)
		require.NoError(t, init.AddSharedInputAndFunding())
		acc := acceptorFor(t, p)
		require.NoError(t, run(init, acc))
		_, err = acc.Build()
		requireAbort(t, err, "funding output of")
	})

	t.Run("an output below the reserve", func(t *testing.T) {
		// The initiator moves almost its whole balance out to an
		// output: what it keeps is under 1% of the new capacity.
		p := defaultParams()
		p.initContrib = -(598_000 + 2_000)
		init, acc := pair(t, p)
		init.AddLocalOutput(Output{Value: 598_000, PkScript: p2wpkh(1)})

		// Refused as the output comes, before tx_complete: the Core
		// Lightning port signs its part once tx_complete is
		// exchanged and fails the channel on a tx_abort after.
		requireAbort(t, run(init, acc), "below the reserve")
	})

	t.Run("a contribution below the balance", func(t *testing.T) {
		p := defaultParams()
		p.initContrib = -700_000
		init, acc := pair(t, p)
		init.AddLocalOutput(Output{Value: 698_500, PkScript: p2wpkh(1)})
		requireAbort(t, run(init, acc), "below zero")
	})

	t.Run("shaped like a commitment", func(t *testing.T) {
		p := defaultParams()
		p.lockTime = 0x20000001
		p.initContrib = -2_000
		init, err := NewSession(config(Initiator, p))
		require.NoError(t, err)
		acc := acceptorFor(t, p)

		// The initiator's shared input with a state hint sequence.
		m := sharedAdd(0)
		m.Sequence = 0x80000001
		require.NoError(t, acc.Receive(m))
		_, err = acc.Next()
		require.NoError(t, err)
		require.NoError(t, acc.Receive(&lnwire.TxAddOutput{
			SerialID: 2, Amount: 998_000, Script: fundingScript,
		}))
		_, err = acc.Next()
		require.NoError(t, err)
		require.NoError(t, acc.Receive(&lnwire.TxComplete{}))
		_ = init

		_, err = acc.Build()
		requireAbort(t, err, "shaped like a commitment")
	})

	t.Run("two funding outputs", func(t *testing.T) {
		p := defaultParams()
		init, acc := pair(t, p)
		init.AddLocalOutput(Output{Value: 1_000, PkScript: fundingScript})
		require.NoError(t, run(init, acc))
		_, err := acc.Build()
		requireAbort(t, err, "2 funding outputs")
	})

	t.Run("no shared input", func(t *testing.T) {
		p := defaultParams()
		acc := acceptorFor(t, p)
		require.NoError(t, acc.Receive(&lnwire.TxAddOutput{
			SerialID: 0, Amount: 1_000_000, Script: fundingScript,
		}))
		_, err := acc.Next()
		require.NoError(t, err)
		require.NoError(t, acc.Receive(&lnwire.TxComplete{}))
		_, err = acc.Build()
		requireAbort(t, err, "0 shared inputs")
	})

	t.Run("over the standard weight", func(t *testing.T) {
		p := defaultParams()
		p.initContrib = -50_000
		init, acc := pair(t, p)
		for i := 0; i < 8; i++ {
			init.AddLocalOutput(Output{
				Value:    400,
				PkScript: bytes.Repeat([]byte{0x6a}, 15_000),
			})
		}
		require.NoError(t, run(init, acc))
		_, err := acc.Build()
		requireAbort(t, err, "exceeds 400000")
	})
}

// TestHonestSplicesProperty runs random honest splices from both sides and
// checks that both agree, that the funding output is the old capacity plus
// both contributions, and that the fee covers the feerate.
func TestHonestSplicesProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		p := defaultParams()
		p.feeRate = chainfee.SatPerKWeight(
			rapid.IntRange(253, 20_000).Draw(rt, "feeRate"),
		)

		// The initiator either adds funds (an input and change) or
		// moves funds out (an output, paid for by its contribution).
		spliceIn := rapid.Bool().Draw(rt, "spliceIn")
		amt := btcutil.Amount(rapid.IntRange(10_000, 300_000).Draw(
			rt, "amount",
		))
		// The initiator pays its share at the feerate, for a weight a
		// little above what these transactions weigh, within the cap
		// of twice its share.
		feeBudget := p.feeRate.FeeForWeight(1_200)

		if spliceIn {
			p.initContrib = amt
		} else {
			p.initContrib = -(amt + feeBudget)
		}

		init, err := NewSession(config(Initiator, p))
		require.NoError(rt, err)
		acc, err := NewSession(config(Acceptor, p))
		require.NoError(rt, err)
		require.NoError(rt, init.AddSharedInputAndFunding())

		if spliceIn {
			wallet := prevTx(1, p2wpkh(1), amt+feeBudget+20_000)
			init.AddLocalInput(Input{
				OutPoint: wire.OutPoint{Hash: wallet.TxHash()},
				Value:    amt + feeBudget + 20_000,
				PkScript: p2wpkh(1), Sequence: MaxSequence,
				MaxWitnessWeight: input.P2WKHWitnessSize,
				PrevTx:           wallet,
			})
			init.AddLocalOutput(Output{
				Value: 20_000, PkScript: p2wpkh(2),
			})
		} else {
			init.AddLocalOutput(Output{
				Value: amt, PkScript: p2tr(3),
			})
		}

		require.NoError(rt, run(init, acc))
		ri, err := init.Build()
		require.NoError(rt, err)
		ra, err := acc.Build()
		require.NoError(rt, err)

		require.Equal(rt, ri.Tx.TxHash(), ra.Tx.TxHash())
		require.Equal(rt, p.capacity+p.initContrib,
			btcutil.Amount(ra.Tx.TxOut[ra.FundingIndex].Value))
		require.GreaterOrEqual(rt, int64(ra.Fee),
			int64(p.feeRate)*ra.Weight/1000)
	})
}

func TestAcceptorPaysForItsOwn(t *testing.T) {
	t.Parallel()

	// An acceptor that contributes pays for its own input and change at
	// the feerate; one that leaves too little for that is refused by the
	// initiator.
	for _, tc := range []struct {
		name   string
		change btcutil.Amount
		ok     bool
	}{
		{"enough left for its fee", 49_000, true},
		{"not enough left for its fee", 49_990, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The initiator adds no inputs: it pays its share
			// (common fields, shared input, funding output) out of
			// its channel balance.
			p := defaultParams()
			p.initContrib = -1_000
			p.accContrib = 50_000
			init, acc := pair(t, p)

			wallet := prevTx(7, p2wpkh(7), 100_000)
			acc.AddLocalInput(Input{
				OutPoint: wire.OutPoint{Hash: wallet.TxHash()},
				Value:    100_000, PkScript: p2wpkh(7),
				Sequence:         MaxSequence,
				MaxWitnessWeight: input.P2WKHWitnessSize,
				PrevTx:           wallet,
			})
			acc.AddLocalOutput(Output{
				Value: tc.change, PkScript: p2wpkh(8),
			})

			require.NoError(t, run(init, acc))
			_, err := init.Build()
			if tc.ok {
				require.NoError(t, err)
				return
			}
			requireAbort(t, err, "acceptor's inputs")
		})
	}
}

func TestSignsFirstTieBreak(t *testing.T) {
	t.Parallel()

	// Equal contributions: the lower node id signs first.
	res := &Result{Inputs: []*Input{
		{SerialID: 0, Value: 100, Shared: true},
		{SerialID: 1, Value: 100},
	}}
	require.True(t, SignsFirst(res, Initiator, []byte{2}, []byte{3}))
	require.False(t, SignsFirst(res, Acceptor, []byte{3}, []byte{2}))
}

// FuzzSessionReceive feeds arbitrary peer messages to an acceptor session:
// it must never panic, and anything it builds must still satisfy the rules.
func FuzzSessionReceive(f *testing.F) {
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		acc, err := NewSession(config(Acceptor, defaultParams()))
		require.NoError(t, err)

		r := bytes.NewReader(data)
		for r.Len() > 0 {
			msg, err := lnwire.ReadMessage(r, 0)
			if err != nil {
				return
			}
			if acc.Receive(msg) != nil {
				return
			}
			if acc.OurTurn() {
				if _, err := acc.Next(); err != nil {
					return
				}
			}
			if acc.Done() {
				res, err := acc.Build()
				if err == nil {
					require.Len(t, res.Tx.TxIn,
						len(res.Inputs))
					require.LessOrEqual(t, res.Weight,
						int64(MaxStandardWeight))
				}

				return
			}
		}
	})
}

func TestCompleteResetsOnNewMessages(t *testing.T) {
	t.Parallel()

	// The peer's tx_complete counts only if its next message is also
	// tx_complete: after the peer completes, this node adds, the peer
	// adds again, this node completes, and the construction is not done
	// until the peer completes once more.
	wallet := prevTx(30, p2wpkh(30), 100_000)

	p := defaultParams()
	init, err := NewSession(config(Initiator, p))
	require.NoError(t, err)
	init.AddLocalOutput(Output{Value: 1_000, PkScript: p2wpkh(31)})
	init.AddLocalOutput(Output{Value: 1_000, PkScript: p2wpkh(32)})

	_, err = init.Next() // our first output
	require.NoError(t, err)
	require.NoError(t, init.Receive(&lnwire.TxComplete{}))
	_, err = init.Next() // our second output
	require.NoError(t, err)
	require.NoError(t, init.Receive(walletAdd(t, 1, wallet, 0)))
	msg, err := init.Next()
	require.NoError(t, err)
	require.IsType(t, &lnwire.TxComplete{}, msg)
	require.False(t, init.Done())

	require.NoError(t, init.Receive(&lnwire.TxComplete{}))
	require.True(t, init.Done())
}

func TestSentCompleteResetsOnOurAdd(t *testing.T) {
	t.Parallel()

	// This node's tx_complete is cleared when it sends anything else.
	p := defaultParams()
	acc := acceptorFor(t, p)
	acc.AddLocalOutput(Output{Value: 1_000, PkScript: p2wpkh(33)})

	require.NoError(t, acc.Receive(sharedAdd(0)))
	msg, err := acc.Next()
	require.NoError(t, err)
	require.IsType(t, &lnwire.TxAddOutput{}, msg)
	require.NoError(t, acc.Receive(&lnwire.TxComplete{}))
	require.False(t, acc.Done())
}

func TestPeerAmountRanges(t *testing.T) {
	t.Parallel()

	acc := acceptorFor(t, defaultParams())
	requireAbort(t, acc.Receive(&lnwire.TxAddOutput{
		SerialID: 0, Amount: -1, Script: p2wpkh(1),
	}), "out of range")

	acc = acceptorFor(t, defaultParams())
	requireAbort(t, acc.Receive(&lnwire.TxAddOutput{
		SerialID: 0, Amount: maxMoney + 1, Script: p2wpkh(1),
	}), "out of range")

	acc = acceptorFor(t, defaultParams())
	requireAbort(t, acc.Receive(&lnwire.TxAddOutput{
		SerialID: 0, Amount: 293, Script: p2wpkh(1),
	}), "dust")

	// A prevtx output can claim any value; the engine refuses one out of
	// range before adding it up.
	for _, v := range []int64{-1, int64(maxMoney) + 1} {
		tx := prevTx(34, p2wpkh(34), 0)
		tx.TxOut[0].Value = v
		acc = acceptorFor(t, defaultParams())
		requireAbort(t, acc.Receive(walletAdd(t, 0, tx, 0)),
			"out of range")
	}
}

func TestPeerCoinbaseInput(t *testing.T) {
	t.Parallel()

	cb := wire.NewMsgTx(2)
	cb.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
		Index: wire.MaxPrevOutIndex,
	}})
	cb.AddTxOut(&wire.TxOut{Value: 300_000_000, PkScript: p2wpkh(35)})

	acc := acceptorFor(t, defaultParams())
	requireAbort(t, acc.Receive(walletAdd(t, 0, cb, 0)), "coinbase")
}

func TestPeerInputLimitIsLive(t *testing.T) {
	t.Parallel()

	p := defaultParams()
	p.maxPeerAddMsg = 1_000
	acc := acceptorFor(t, p)

	var msgs []lnwire.Message
	for i := 0; i < MaxInputsOrOutputs+1; i++ {
		tx := prevTx(byte(i), p2wpkh(byte(i)), 10_000)
		tx.LockTime = uint32(i)
		msgs = append(msgs, walletAdd(t, uint64(2*i), tx, 0))
	}
	requireAbort(t, exchange(acc, msgs...), "more than 252 inputs")
}

func TestFundingOutputAsPlainInput(t *testing.T) {
	t.Parallel()

	// The peer knows the real funding transaction and sends it as a
	// prevtx. With a witness weight hook that accepts P2WSH, the engine's
	// own check refuses it.
	funding := prevTx(36, oldFundingScript, 1_000_000)
	cfg := config(Acceptor, defaultParams())
	cfg.Shared.OutPoint = wire.OutPoint{Hash: funding.TxHash()}
	cfg.WitnessWeight = func([]byte) (int64, error) { return 222, nil }
	acc, err := NewSession(cfg)
	require.NoError(t, err)

	requireAbort(t, acc.Receive(walletAdd(t, 0, funding, 0)),
		"funding output added as a plain input")
}

func TestOurCoinAddedByThePeerFirst(t *testing.T) {
	t.Parallel()

	// This node, the acceptor, has one of its coins queued; the peer
	// adds that same coin first (it can know it, from an earlier splice's
	// change for instance). This node refuses to spend it twice.
	coin := prevTx(37, p2wpkh(37), 100_000)
	acc := acceptorFor(t, defaultParams())
	acc.AddLocalInput(Input{
		OutPoint: wire.OutPoint{Hash: coin.TxHash()}, Value: 100_000,
		PkScript: p2wpkh(37), Sequence: MaxSequence,
		MaxWitnessWeight: input.P2WKHWitnessSize, PrevTx: coin,
	})

	require.NoError(t, acc.Receive(walletAdd(t, 0, coin, 0)))
	_, err := acc.Next()
	requireAbort(t, err, "already added")
}

func TestFeeBoundaries(t *testing.T) {
	t.Parallel()

	// The initiator's share for a splice-out with one P2TR output.
	shareFor := func(t *testing.T) btcutil.Amount {
		p := defaultParams()
		p.initContrib = -(300_000 + 1_500)
		init, acc := pair(t, p)
		init.AddLocalOutput(Output{Value: 300_000, PkScript: p2tr(9)})
		require.NoError(t, run(init, acc))
		res, err := acc.Build()
		require.NoError(t, err)

		return p.feeRate.FeeForWeight(lntypes.WeightUnit(res.Weight))
	}
	share := shareFor(t)

	// The acceptor's check of the initiator, the peer, and the
	// initiator's of itself.
	try := func(paid btcutil.Amount) (error, error) {
		p := defaultParams()
		p.initContrib = -(300_000 + paid)
		init, acc := pair(t, p)
		init.AddLocalOutput(Output{Value: 300_000, PkScript: p2tr(9)})
		require.NoError(t, run(init, acc))
		_, peerErr := acc.Build()
		_, ownErr := init.Build()

		return peerErr, ownErr
	}

	peer, own := try(share)
	require.NoError(t, peer)
	require.NoError(t, own)
	peer, own = try(share - 1)
	requireAbort(t, peer, "don't cover")
	requireAbort(t, own, "don't cover")
	peer, own = try(2*share + maxExcessFee)
	require.NoError(t, peer)
	require.NoError(t, own)

	// Overpaying is a check of our own arithmetic only: the peer's is
	// its own money (the Core Lightning port pays four times the rate
	// it declares).
	peer, own = try(2*share + maxExcessFee + 1)
	require.NoError(t, peer)
	requireAbort(t, own, "far more than")
}

func TestReserveOnNegativeContribution(t *testing.T) {
	t.Parallel()

	// The initiator takes 2,000 out of a 2,500 balance as fees only, no
	// output of its own: still bound by the reserve.
	p := defaultParams()
	p.initBalance = 2_500
	p.accBalance = 997_500
	p.initContrib = -1_500
	init, acc := pair(t, p)
	require.NoError(t, run(init, acc))
	_, err := acc.Build()
	requireAbort(t, err, "below the reserve")
}

func TestSequenceBoundary(t *testing.T) {
	t.Parallel()

	wallet := prevTx(39, p2wpkh(39), 100_000)
	m := walletAdd(t, 0, wallet, 0)
	m.Sequence = 0xfffffffe
	acc := acceptorFor(t, defaultParams())
	requireAbort(t, acc.Receive(m), "final")

	m = walletAdd(t, 0, wallet, 0)
	m.Sequence = MaxSequence
	acc = acceptorFor(t, defaultParams())
	require.NoError(t, acc.Receive(m))
}

func TestSignsFirstCountsSharedInput(t *testing.T) {
	t.Parallel()

	// The shared input counts in full as the initiator's: an acceptor
	// adding a bigger wallet input than the initiator's own still signs
	// first while the capacity outweighs it.
	res := &Result{Inputs: []*Input{
		{SerialID: 0, Value: 1_000_000, Shared: true},
		{SerialID: 1, Value: 500_000},
	}}
	init, acc := Contributed(res)
	require.Equal(t, btcutil.Amount(1_000_000), init)
	require.Equal(t, btcutil.Amount(500_000), acc)
	require.True(t, SignsFirst(res, Acceptor, []byte{9}, []byte{1}))
}

func TestHooksRequired(t *testing.T) {
	t.Parallel()

	for _, clear := range []func(*Config){
		func(c *Config) { c.CheckPeerInput = nil },
		func(c *Config) { c.CheckPeerOutput = nil },
		func(c *Config) { c.DustLimit = nil },
	} {
		cfg := config(Acceptor, defaultParams())
		clear(&cfg)
		_, err := NewSession(cfg)
		require.Error(t, err)
	}
}

// TestAdversarialInitiatorProperty runs splices where the peer, as
// initiator, claims one contribution in its splice_init (what our session
// is configured with) and builds the transaction for another, with random
// inputs and outputs of its own. Whenever our side accepts, the funding
// output is the old capacity plus the claimed contribution, and the peer's
// own inputs, less its own outputs, pay for that contribution and a fee at
// the rate: nothing of our balance went anywhere.
func TestAdversarialInitiatorProperty(t *testing.T) {
	t.Parallel()

	var accepted, lied int
	rapid.Check(t, func(rt *rapid.T) {
		amount := func(label string, lo, hi int) btcutil.Amount {
			return btcutil.Amount(rapid.IntRange(lo, hi).Draw(
				rt, label,
			))
		}

		ours := defaultParams()
		ours.feeRate = chainfee.SatPerKWeight(
			rapid.IntRange(253, 5_000).Draw(rt, "feeRate"),
		)
		ours.initContrib = amount("claimed", -500_000, 500_000)

		// Mostly honest, so that splices get through; often not.
		theirs := ours
		if rapid.Bool().Draw(rt, "lies") {
			theirs.initContrib = amount("real", -500_000, 500_000)
		}
		theirs.initBalance = 10_000_000

		init, err := NewSession(config(Initiator, theirs))
		if err != nil {
			return
		}
		acc, err := NewSession(config(Acceptor, ours))
		if err != nil {
			return
		}
		require.NoError(rt, init.AddSharedInputAndFunding())

		var in, out btcutil.Amount
		nInputs := rapid.IntRange(0, 3).Draw(rt, "inputs")
		for i := 0; i < nInputs; i++ {
			v := amount(fmt.Sprintf("in%d", i), 1_000, 900_000)
			tag := byte(10 + i)
			prev := prevTx(tag, p2wpkh(tag), v)
			init.AddLocalInput(Input{
				OutPoint: wire.OutPoint{Hash: prev.TxHash()},
				Value:    v, PkScript: p2wpkh(tag),
				Sequence:         MaxSequence,
				MaxWitnessWeight: input.P2WKHWitnessSize,
				PrevTx:           prev,
			})
			in += v
		}
		for i := 0; i < rapid.IntRange(0, 3).Draw(rt, "outputs"); i++ {
			v := amount(fmt.Sprintf("out%d", i), 330, 900_000)
			init.AddLocalOutput(Output{
				Value: v, PkScript: p2tr(byte(20 + i)),
			})
			out += v
		}

		if run(init, acc) != nil {
			return
		}
		res, err := acc.Build()
		if err != nil {
			return
		}

		accepted++
		if theirs.initContrib != ours.initContrib {
			lied++
		}
		require.Equal(rt, ours.capacity+ours.initContrib,
			btcutil.Amount(res.Tx.TxOut[res.FundingIndex].Value),
			"the funding output isn't what the peer claimed")

		// We put nothing in, so what the peer's own coins pay beyond
		// its contribution is the whole fee, at least the rate for the
		// weight with the peer's witnesses as light as we allow.
		paid := in - out - ours.initContrib
		require.Equal(rt, res.Fee, paid)
		minWeight := lntypes.WeightUnit(
			res.Weight - int64(nInputs)*peerWitnessSlack,
		)
		require.GreaterOrEqual(rt, paid,
			ours.feeRate.FeeForWeight(minWeight),
			"the peer didn't pay for its contribution")
	})
	require.Positive(t, accepted, "no splice got through")
	t.Logf("%d accepted, %d of them with the claim the peer built for "+
		"a different one", accepted, lied)
}

// TestPeerWitnessSlack checks the peer's inputs may weigh a little less
// than this node's estimate: the Core Lightning port counts a P2WPKH
// witness as 107 weight units, this node as 109, and pays for 107.
func TestPeerWitnessSlack(t *testing.T) {
	t.Parallel()

	build := func(change btcutil.Amount) (*Result, *Session, error) {
		p := defaultParams()
		p.initContrib = 500_000
		init, acc := pair(t, p)
		for i := byte(1); i <= 2; i++ {
			wallet := prevTx(i, p2wpkh(i), 300_000)
			init.AddLocalInput(Input{
				OutPoint: wire.OutPoint{
					Hash: wallet.TxHash(),
				},
				Value:            300_000,
				PkScript:         p2wpkh(i),
				Sequence:         MaxSequence,
				MaxWitnessWeight: input.P2WKHWitnessSize,
				PrevTx:           wallet,
			})
		}
		init.AddLocalOutput(Output{Value: change, PkScript: p2wpkh(9)})
		require.NoError(t, run(init, acc))
		res, err := acc.Build()

		return res, acc, err
	}

	// The initiator's share by this node's weights.
	res, acc, err := build(90_000)
	require.NoError(t, err)
	p := defaultParams()
	share := p.feeRate.FeeForWeight(lntypes.WeightUnit(
		acc.weights(res).initiator,
	))
	slack := p.feeRate.FeeForWeight(2 * peerWitnessSlack)
	require.Positive(t, slack)

	// Paying for its witnesses as it weighs them: accepted. Less still:
	// refused.
	_, _, err = build(100_000 - share + slack)
	require.NoError(t, err)
	_, _, err = build(100_000 - share + slack + 1)
	require.Error(t, err)
}

// TestWaiveOwnReserve checks the waiver of the reserve rule for our own side:
// our Build passes with our side short, while the peer's side stays bound,
// as the peer's Build, holding us to it, shows.
func TestWaiveOwnReserve(t *testing.T) {
	t.Parallel()

	// The initiator takes 2,000 out of a 2,500 balance as fees only.
	p := defaultParams()
	p.initBalance = 2_500
	p.accBalance = 997_500
	p.initContrib = -1_500
	init, acc := pair(t, p)
	init.WaiveOwnReserve()
	require.NoError(t, run(init, acc))
	_, err := init.Build()
	require.NoError(t, err)
	_, err = acc.Build()
	requireAbort(t, err, "below the reserve")

	// Waived on the acceptor, the initiator short: still refused.
	init, acc = pair(t, p)
	acc.WaiveOwnReserve()
	require.NoError(t, run(init, acc))
	_, err = acc.Build()
	requireAbort(t, err, "below the reserve")
}
