//go:build !dev && !integration

// Release builds only: a spliceable channel has option_unified_sigs, which
// development builds never sign with.

package splice

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// spliceChanType is a channel a splice applies to.
const spliceChanType = channeldb.SingleFunderTweaklessBit |
	channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit |
	channeldb.UnifiedSigsBit

// testLink is the channel's link as the machine sees it, run inline on a
// LightningChannel. Our commitment_signed goes to the test's peer.
type testLink struct {
	t          *testing.T
	ch         *lnwallet.LightningChannel
	initiator  lntypes.ChannelParty
	stfuResult lntypes.ChannelParty

	// lockErr, set, is LockSplice's answer once it has locked: an answer
	// lost, the lock done all the same.
	lockErr error

	// commitAnswerLost, set, has SendSpliceCommitment send the
	// commitment and never answer.
	commitAnswerLost bool

	// commitErr, set, is SendSpliceCommitment's answer, nothing sent.
	commitErr error

	// lockFail, set, has LockSplice fail without locking.
	lockFail atomic.Bool
	stfuGate chan struct{}
	commits  chan *lnwire.CommitSig
	resumed  chan struct{}
	held     atomic.Bool

	// stfuErr, set before stfuGate closes, is InitStfu's answer: the
	// request withdrawn.
	stfuErr error

	// mu guards initiator, which a test may change.
	mu sync.Mutex
}

func done(err error) <-chan error {
	c := make(chan error, 1)
	c <- err

	return c
}

func (l *testLink) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	l.mu.Lock()
	defer l.mu.Unlock()

	return fn.Ok(l.initiator)
}

func (l *testLink) InitStfu() <-chan fn.Result[lntypes.ChannelParty] {
	c := make(chan fn.Result[lntypes.ChannelParty], 1)
	if l.stfuGate == nil {
		c <- fn.Ok(l.stfuResult)
		return c
	}

	// The test decides when the channel is quiescent.
	go func() {
		<-l.stfuGate
		if l.stfuErr != nil {
			c <- fn.Err[lntypes.ChannelParty](l.stfuErr)
			return
		}
		c <- fn.Ok(l.stfuResult)
	}()

	return c
}

func (l *testLink) HoldUpdates() <-chan error {
	l.held.Store(true)

	return done(nil)
}

func (l *testLink) ResumeQuiescence() <-chan error {
	l.resumed <- struct{}{}

	return done(nil)
}

func (l *testLink) AddSpliceFunding(f lnwallet.PendingFunding) <-chan error {
	return done(l.ch.AddPendingFunding(f))
}

func (l *testLink) SendSpliceCommitment(txid chainhash.Hash) <-chan error {
	if l.commitErr != nil {
		return done(l.commitErr)
	}
	sigs, err := l.ch.SignSpliceCommitment(l.t.Context(), txid)
	if err != nil {
		return done(err)
	}
	l.commits <- &lnwire.CommitSig{
		CommitSig: sigs.CommitSig, HtlcSigs: sigs.HtlcSigs,
		FundingTxid: fn.Some(txid),
	}
	if l.commitAnswerLost {
		return make(chan error)
	}

	return done(nil)
}

func (l *testLink) RemoveSpliceFunding(txid chainhash.Hash) <-chan error {
	return done(l.ch.RemovePendingFunding(txid))
}

func (l *testLink) LockSplice(txid chainhash.Hash,
	scid lnwire.ShortChannelID) <-chan error {

	if l.lockFail.Load() {
		return done(errors.New("unable to lock"))
	}
	err := l.ch.LockPendingFunding(txid, scid)
	if err == nil && l.lockErr != nil {
		err = l.lockErr
	}

	return done(err)
}

// testPeer collects what the machine sends.
type testPeer struct {
	msgs         chan lnwire.Message
	mu           sync.Mutex
	disconnected error

	// onSend, if set, sees each message as it is sent.
	onSend func(lnwire.Message)
}

func (p *testPeer) SendMessage(_ bool, msgs ...lnwire.Message) error {
	for _, m := range msgs {
		if p.onSend != nil {
			p.onSend(m)
		}
		p.msgs <- m
	}

	return nil
}

func (p *testPeer) Disconnect(reason error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.disconnected = reason
}

// testChain answers for the coins it knows and lets the test confirm
// transactions, spend outputs and mine blocks.
type testChain struct {
	height uint32
	outs   map[wire.OutPoint]*wire.TxOut

	// behind is set to report the node still catching up.
	behind atomic.Bool

	mu     sync.Mutex
	confs  map[confKey][]*chainntnfs.ConfirmationEvent
	spends map[wire.OutPoint][]chan *chainntnfs.SpendDetail
	epochs []chan *chainntnfs.BlockEpoch
}

// confKey names a confirmation registration.
type confKey struct {
	txid     chainhash.Hash
	numConfs uint32
}

func newTestChain() *testChain {
	return &testChain{
		height: 900_000,
		outs:   make(map[wire.OutPoint]*wire.TxOut),
		confs:  make(map[confKey][]*chainntnfs.ConfirmationEvent),
		spends: make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
	}
}

func (c *testChain) RegisterConfirmationsNtfn(txid *chainhash.Hash, _ []byte,
	numConfs, heightHint uint32, _ ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	// As the notifier: no search from height zero.
	if heightHint == 0 {
		return nil, chainntnfs.ErrNoHeightHint
	}

	ev := &chainntnfs.ConfirmationEvent{
		Confirmed:    make(chan *chainntnfs.TxConfirmation, 1),
		NegativeConf: make(chan int32, 1),
		Cancel:       func() {},
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	k := confKey{*txid, numConfs}
	c.confs[k] = append(c.confs[k], ev)

	return ev, nil
}

func (c *testChain) Synced() bool {
	return !c.behind.Load()
}

func (c *testChain) RegisterSpendNtfn(op *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	if heightHint == 0 {
		return nil, chainntnfs.ErrNoHeightHint
	}

	ch := make(chan *chainntnfs.SpendDetail, 1)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.spends[*op] = append(c.spends[*op], ch)

	return &chainntnfs.SpendEvent{Spend: ch, Cancel: func() {}}, nil
}

func (c *testChain) RegisterBlockEpochNtfn(
	*chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	ch := make(chan *chainntnfs.BlockEpoch, 4)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.epochs = append(c.epochs, ch)

	return &chainntnfs.BlockEpochEvent{Epochs: ch, Cancel: func() {}}, nil
}

// registered reports whether txid is watched for numConfs, n times.
func (c *testChain) registered(txid chainhash.Hash, numConfs uint32,
	n int) bool {

	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.confs[confKey{txid, numConfs}]) >= n
}

// confirm confirms txid to every registration for numConfs, waiting for n of
// them.
func (c *testChain) confirm(t *testing.T, txid chainhash.Hash,
	numConfs uint32, n int, conf *chainntnfs.TxConfirmation) {

	t.Helper()

	require.Eventually(t, func() bool {
		return c.registered(txid, numConfs, n)
	}, 5*time.Second, 10*time.Millisecond)

	c.mu.Lock()
	evs := c.confs[confKey{txid, numConfs}]
	c.mu.Unlock()
	for _, ev := range evs {
		ev.Confirmed <- conf
	}
}

// reorg sends a reorg to every registration of txid for numConfs.
func (c *testChain) reorg(t *testing.T, txid chainhash.Hash,
	numConfs uint32) {

	t.Helper()

	c.mu.Lock()
	evs := c.confs[confKey{txid, numConfs}]
	c.mu.Unlock()
	require.NotEmpty(t, evs)
	for _, ev := range evs {
		ev.NegativeConf <- 1
	}
}

// spend spends op by tx to every watcher of op, waiting for n of them.
func (c *testChain) spend(t *testing.T, op wire.OutPoint, tx *wire.MsgTx,
	n int) {

	t.Helper()

	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()

		return len(c.spends[op]) >= n
	}, 5*time.Second, 10*time.Millisecond)

	c.mu.Lock()
	chans := c.spends[op]
	c.spends[op] = nil
	height := c.height
	c.mu.Unlock()

	hash := tx.TxHash()
	for _, ch := range chans {
		ch <- &chainntnfs.SpendDetail{
			SpentOutPoint: &op, SpenderTxHash: &hash,
			SpendingTx: tx, SpendingHeight: int32(height),
		}
	}
}

// mine sends a block to every block watcher.
func (c *testChain) mine() {
	c.mu.Lock()
	chans := c.epochs
	c.height++
	height := c.height
	c.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- &chainntnfs.BlockEpoch{Height: int32(height)}:
		default:
		}
	}
}

func (c *testChain) BestHeight() (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.height, nil
}

func (c *testChain) ConfirmedOutput(op wire.OutPoint) (*wire.TxOut, uint32,
	bool, error) {

	out, ok := c.outs[op]
	if !ok {
		return nil, 0, false, errors.New("unknown output")
	}

	return out, 10, false, nil
}

// testKeyRing derives fresh keys the channel's signer can sign with.
type testKeyRing struct {
	signer *input.MockSigner
}

func (k *testKeyRing) DeriveNextKey(
	fam keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}
	k.signer.Privkeys = append(k.signer.Privkeys, priv)

	return keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{Family: fam, Index: 1},
		PubKey:     priv.PubKey(),
	}, nil
}

func (k *testKeyRing) DeriveKey(
	loc keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	return keychain.KeyDescriptor{}, errors.New("not used")
}

// acceptorHarness is our machine as acceptor on Bob's side of a channel,
// with Alice's side driven by the test as the peer.
type acceptorHarness struct {
	alice, bob *lnwallet.LightningChannel
	machine    *Machine
	link       *testLink
	peer       *testPeer
	chain      *testChain
	wallet     *fakeWallet
}

func newAcceptorHarness(t *testing.T) *acceptorHarness {
	t.Helper()

	alice, bob, err := lnwallet.CreateTestChannels(t, spliceChanType)
	require.NoError(t, err)

	h := &acceptorHarness{
		alice: alice,
		bob:   bob,
		link: &testLink{
			t: t, ch: bob, initiator: lntypes.Remote,
			commits: make(chan *lnwire.CommitSig, 4),
			resumed: make(chan struct{}, 4),
		},
		peer:   &testPeer{msgs: make(chan lnwire.Message, 64)},
		chain:  newTestChain(),
		wallet: newFakeWallet(),
	}

	var aliceID, bobID [33]byte
	copy(aliceID[:], alice.State().IdentityPub.SerializeCompressed())
	copy(bobID[:], bob.State().IdentityPub.SerializeCompressed())

	h.machine = NewMachine(Config{
		ChanID:      lnwire.NewChanIDFromOutPoint(bob.ChannelPoint()),
		OurNodeID:   bobID,
		TheirNodeID: aliceID,
		Link:        h.link,
		Channel:     bob,
		Peer:        h.peer,
		Chain:       h.chain,
		Wallet:      h.wallet,
		KeyRing: &testKeyRing{
			signer: bob.Signer.(*input.MockSigner),
		},
		Signer:    bob.Signer,
		Estimator: chainfee.NewStaticEstimator(2_500, 253),
		DustLimit: dustLimit,
		Policy: Policy{
			MaxChannelSize: 100 * btcutil.SatoshiPerBitcoin,
			LockDepth: func(btcutil.Amount) uint32 {
				return 6
			},
		},
	})
	startReestablished(t, h.machine)
	t.Cleanup(h.machine.Stop)

	return h
}

// next returns the machine's next message to the peer.
func next[T lnwire.Message](t *testing.T, h *acceptorHarness) T {
	t.Helper()

	select {
	case msg := <-h.peer.msgs:
		m, ok := msg.(T)
		require.True(t, ok, "got %T", msg)

		return m

	case <-time.After(5 * time.Second):
		var zero T
		t.Fatalf("no %T from the machine", zero)

		return zero
	}
}

// aliceSplice is the splice-in Alice drives: her coin, her new funding key,
// and her side of the construction.
type aliceSplice struct {
	coinKey    *btcec.PrivateKey
	coin       Coin
	fundingKey *btcec.PrivateKey
	sel        *AddFunds
	session    *interactivetx.Session
	res        *interactivetx.Result
	change     []byte
}

// startAliceSplice sends Alice's splice_init for adding funds from one P2WPKH
// coin and returns her side, with Bob's splice_ack checked.
func startAliceSplice(t *testing.T, h *acceptorHarness,
	amount btcutil.Amount, feeRate chainfee.SatPerKWeight) *aliceSplice {

	t.Helper()

	var err error
	s := &aliceSplice{}
	s.coinKey, err = btcec.NewPrivateKey()
	require.NoError(t, err)
	s.fundingKey, err = btcec.NewPrivateKey()
	require.NoError(t, err)
	aliceSigner := h.alice.Signer.(*input.MockSigner)
	aliceSigner.Privkeys = append(aliceSigner.Privkeys, s.fundingKey)

	pkScript, err := input.WitnessPubKeyHash(
		s.coinKey.PubKey().SerializeCompressed(),
	)
	require.NoError(t, err)
	s.coin = coin(77, pkScript, amount+500_000)
	h.chain.outs[s.coin.OutPoint] = s.coin.PrevTx.TxOut[0]
	s.change = p2wpkh(0x42)

	h.machine.HandleSpliceMessage(&lnwire.SpliceInit{
		ChannelID:              h.machine.cfg.ChanID,
		FundingContribution:    amount,
		FundingFeeRatePerKw:    uint32(feeRate),
		LockTime:               h.chain.height,
		FundingPubKey:          s.fundingKey.PubKey(),
		RequireConfirmedInputs: true,
	})
	ack := next[*lnwire.SpliceAck](t, h)
	require.Zero(t, ack.FundingContribution)
	require.True(t, ack.RequireConfirmedInputs)

	state := h.alice.State()
	_, out, err := input.GenFundingPkScript(
		state.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		state.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		int64(state.Capacity),
	)
	require.NoError(t, err)
	_, newOut, err := input.GenFundingPkScript(
		s.fundingKey.PubKey().SerializeCompressed(),
		ack.FundingPubKey.SerializeCompressed(),
		int64(state.Capacity+amount),
	)
	require.NoError(t, err)

	s.sel, err = SelectAddFunds(Shape{
		SharedWitnessWeight: input.MultiSigWitnessSize,
		FundingScript:       newOut.PkScript,
	}, []Coin{s.coin}, amount, false, feeRate, s.change, dustLimit, 0)
	require.NoError(t, err)

	s.session, err = interactivetx.NewSession(interactivetx.Config{
		Role:      interactivetx.Initiator,
		ChannelID: h.machine.cfg.ChanID,
		Shared: interactivetx.SharedInput{
			OutPoint:      state.CurrentFundingOutpoint(),
			Value:         state.Capacity,
			PkScript:      out.PkScript,
			WitnessWeight: input.MultiSigWitnessSize,
		},
		FundingScript:         newOut.PkScript,
		InitiatorContribution: amount,
		FeeRate:               feeRate,
		LockTime:              h.chain.height,
		CheckPeerInput:        func(*interactivetx.Input) error { return nil },
		CheckPeerOutput:       func(*interactivetx.Output) error { return nil },
		DustLimit:             dustLimit,
		WitnessWeight:         peerWitnessWeight,
		Reserve:               func(c btcutil.Amount) btcutil.Amount { return c / 100 },
		InitiatorBalance:      state.LocalCommitment.LocalBalance.ToSatoshis(),
		AcceptorBalance:       state.LocalCommitment.RemoteBalance.ToSatoshis(),
	})
	require.NoError(t, err)
	require.NoError(t, s.session.AddSharedInputAndFunding())
	s.session.AddLocalInput(interactivetx.Input{
		OutPoint: s.coin.OutPoint, Value: s.coin.Value,
		PkScript: s.coin.PkScript, Sequence: interactivetx.MaxSequence,
		MaxWitnessWeight: input.P2WKHWitnessSize, PrevTx: s.coin.PrevTx,
	})
	if s.sel.Change > 0 {
		s.session.AddLocalOutput(interactivetx.Output{
			Value: s.sel.Change, PkScript: s.change,
		})
	}

	return s
}

// construct runs the interactive construction between Alice and the
// machine.
func (s *aliceSplice) construct(t *testing.T, h *acceptorHarness) {
	t.Helper()

	for !s.session.Done() {
		if s.session.OurTurn() {
			msg, err := s.session.Next()
			require.NoError(t, err)
			h.machine.HandleSpliceMessage(msg)

			continue
		}

		select {
		case msg := <-h.peer.msgs:
			require.NoError(t, s.session.Receive(msg))
		case <-time.After(5 * time.Second):
			t.Fatal("machine stopped answering")
		}
	}

	var err error
	s.res, err = s.session.Build()
	require.NoError(t, err)
}

// pendingFunding is Alice's view of the splice's funding.
func (s *aliceSplice) pendingFunding(h *acceptorHarness,
	bobKey *btcec.PublicKey) lnwallet.PendingFunding {

	return lnwallet.PendingFunding{
		OutPoint: wire.OutPoint{
			Hash: s.res.Tx.TxHash(), Index: s.res.FundingIndex,
		},
		Capacity: h.alice.State().Capacity + s.sel.Contribution,
		LocalKey: keychain.KeyDescriptor{
			PubKey: s.fundingKey.PubKey(),
		},
		RemoteKey:  bobKey,
		LocalDelta: s.sel.Contribution,
	}
}

// aliceTxSignatures is Alice's tx_signatures: her coin's witness and her
// shared input signature over the unified digest.
func (s *aliceSplice) aliceTxSignatures(t *testing.T,
	h *acceptorHarness) *lnwire.TxSignatures {

	t.Helper()

	fetcher := interactivetx.PrevOutFetcher(s.res)
	sigHashes := txscript.NewTxSigHashes(s.res.Tx, fetcher)
	var witnesses []wire.TxWitness
	for i, in := range s.res.Inputs {
		if in.Shared {
			continue
		}
		w, err := txscript.WitnessSignature(
			s.res.Tx, sigHashes, i, int64(in.Value), in.PkScript,
			txscript.SigHashAll, s.coinKey, true,
		)
		require.NoError(t, err)
		witnesses = append(witnesses, w)
	}

	state := h.alice.State()
	witnessScript, _, err := input.GenFundingPkScript(
		state.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		state.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		int64(state.Capacity),
	)
	require.NoError(t, err)
	shared := s.res.Inputs[s.res.SharedIndex]
	raw, err := h.alice.Signer.SignOutputRaw(s.res.Tx, &input.SignDescriptor{
		KeyDesc:       state.LocalChanCfg.MultiSigKey,
		WitnessScript: witnessScript,
		Output: &wire.TxOut{
			Value: int64(shared.Value), PkScript: shared.PkScript,
		},
		HashType:          interactivetx.SharedInputHashType,
		SigHashes:         sigHashes,
		PrevOutputFetcher: fetcher,
		InputIndex:        int(s.res.SharedIndex),
	})
	require.NoError(t, err)
	sig, err := lnwire.NewSigFromSignature(raw)
	require.NoError(t, err)

	return &lnwire.TxSignatures{
		ChannelID: h.machine.cfg.ChanID, TxID: s.res.Tx.TxHash(),
		Witnesses: witnesses, SharedInputSignature: fn.Some(sig),
	}
}

// requireValid checks every input of tx with the script engine.
func requireValid(t *testing.T, tx *wire.MsgTx, res *interactivetx.Result) {
	t.Helper()

	fetcher := interactivetx.PrevOutFetcher(res)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, in := range res.Inputs {
		vm, err := txscript.NewEngine(
			in.PkScript, tx, i, txscript.StandardVerifyFlags, nil,
			sigHashes, int64(in.Value), fetcher,
		)
		require.NoError(t, err)
		require.NoError(t, vm.Execute(), "input %d", i)
	}
}

// TestAcceptSpliceIn runs a splice the peer starts, adding funds, through to
// completion: the machine answers splice_init, joins the construction adding
// nothing, exchanges commitments, signs first, checks the peer's witnesses,
// publishes a transaction every input of which verifies, sends
// splice_locked at its depth and moves the channel once the peer has too.
func TestAcceptSpliceIn(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	const amount = btcutil.Amount(50_000_000)
	capacity := h.bob.State().Capacity

	s := startAliceSplice(t, h, amount, 2_500)
	s.construct(t, h)
	txid := s.res.Tx.TxHash()

	// Bob added the splice and signed his commitment_signed.
	var bobCommit *lnwire.CommitSig
	select {
	case bobCommit = <-h.link.commits:
	case <-time.After(5 * time.Second):
		t.Fatal("no commitment_signed from the machine")
	}
	require.Len(t, h.bob.PendingFundings(), 1)
	bobKey := h.bob.PendingFundings()[0].LocalKey.PubKey

	// Alice's side of the commitment exchange.
	require.NoError(t, h.alice.AddPendingFunding(
		s.pendingFunding(h, bobKey),
	))
	require.NoError(t, h.alice.ReceiveSpliceCommitment(txid,
		&lnwallet.CommitSigs{
			CommitSig: bobCommit.CommitSig,
			HtlcSigs:  bobCommit.HtlcSigs,
		}))
	aliceSigs, err := h.alice.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	h.machine.SpliceCommitReceived(txid,
		h.bob.ReceiveSpliceCommitment(txid, aliceSigs))

	// Bob put nothing in: he signs first, with no witnesses and his
	// shared input signature over the unified digest.
	bobTxSigs := next[*lnwire.TxSignatures](t, h)
	require.Equal(t, txid, bobTxSigs.TxID)
	require.Empty(t, bobTxSigs.Witnesses)

	// Alice's, then the published transaction is complete and valid.
	h.machine.HandleSpliceMessage(s.aliceTxSignatures(t, h))
	select {
	case <-h.link.resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("quiescence not ended")
	}
	published := h.wallet.publishedTxs()
	require.Len(t, published, 1)
	label, ok := published[txid]
	require.True(t, ok)
	require.Equal(t, Label(h.bob.State().ShortChannelID, DirectionIn),
		label)
	require.Eventually(t, func() bool {
		return h.chain.registered(txid, 6, 1)
	}, 5*time.Second, 10*time.Millisecond)

	// The machine's own copy of the signed transaction.
	h.machine.mu.Lock()
	tx, err := h.machine.signedTx()
	h.machine.mu.Unlock()
	require.NoError(t, err)
	requireValid(t, tx, s.res)

	// Confirmed at depth: splice_locked; then Alice's; then moved.
	h.chain.confirm(t, txid, 6, 1, &chainntnfs.TxConfirmation{
		BlockHeight: 900_006, TxIndex: 3,
	})
	locked := next[*lnwire.SpliceLocked](t, h)
	require.Equal(t, txid, locked.SpliceTxid)
	h.machine.HandleSpliceMessage(&lnwire.SpliceLocked{
		ChannelID: h.machine.cfg.ChanID, SpliceTxid: txid,
	})

	require.Eventually(t, func() bool {
		return h.bob.State().CurrentFundingOutpoint().Hash == txid
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, capacity+amount, h.bob.State().Capacity)
	require.Empty(t, h.bob.PendingFundings())
}

// abortReason returns the reason of the machine's next message, a tx_abort.
func abortReason(t *testing.T, h *acceptorHarness) string {
	t.Helper()

	return string(next[*lnwire.TxAbort](t, h).Data)
}

// requireResumed waits for the machine to end quiescence.
func requireResumed(t *testing.T, h *acceptorHarness) {
	t.Helper()

	select {
	case <-h.link.resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("quiescence not ended")
	}
}

// requireDisconnected waits for the machine to drop the peer.
func requireDisconnected(t *testing.T, h *acceptorHarness) {
	t.Helper()

	require.Eventually(t, func() bool {
		h.peer.mu.Lock()
		defer h.peer.mu.Unlock()

		return h.peer.disconnected != nil
	}, 5*time.Second, 10*time.Millisecond)
}

// estimateOnly is a fee estimator with a real estimate and no fallback.
type estimateOnly struct {
	rate chainfee.SatPerKWeight
}

func (e estimateOnly) EstimateFeePerKW(uint32) (chainfee.SatPerKWeight,
	error) {

	return e.rate, nil
}
func (e estimateOnly) RelayFeePerKW() chainfee.SatPerKWeight { return 253 }
func (e estimateOnly) Start() error                          { return nil }
func (e estimateOnly) Stop() error                           { return nil }

// splicePastSigning runs Alice's splice-in until Bob has sent his
// tx_signatures, and returns Alice's side.
func splicePastSigning(t *testing.T, h *acceptorHarness) *aliceSplice {
	t.Helper()

	s := startAliceSplice(t, h, 30_000_000, 2_500)
	s.construct(t, h)
	txid := s.res.Tx.TxHash()

	bobCommit := <-h.link.commits
	bobKey := h.bob.PendingFundings()[0].LocalKey.PubKey
	require.NoError(t, h.alice.AddPendingFunding(
		s.pendingFunding(h, bobKey),
	))
	require.NoError(t, h.alice.ReceiveSpliceCommitment(txid,
		&lnwallet.CommitSigs{
			CommitSig: bobCommit.CommitSig,
			HtlcSigs:  bobCommit.HtlcSigs,
		}))
	aliceSigs, err := h.alice.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	h.machine.SpliceCommitReceived(txid,
		h.bob.ReceiveSpliceCommitment(txid, aliceSigs))
	next[*lnwire.TxSignatures](t, h)

	return s
}

// TestAcceptSpliceRefused checks splice_init refusals: each gets a tx_abort
// with its code and ends quiescence, leaving nothing behind; one from the
// side that didn't start quiescence breaks the protocol.
func TestAcceptSpliceRefused(t *testing.T) {
	t.Parallel()

	init := func(h *acceptorHarness, contribution btcutil.Amount,
		feeRate uint32) {

		key, _ := btcec.NewPrivateKey()
		h.machine.HandleSpliceMessage(&lnwire.SpliceInit{
			ChannelID:           h.machine.cfg.ChanID,
			FundingContribution: contribution,
			FundingFeeRatePerKw: feeRate,
			LockTime:            h.chain.height,
			FundingPubKey:       key.PubKey(),
		})
	}

	t.Run("fee below floor", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		h.machine.cfg.Estimator = estimateOnly{rate: 10_000}
		init(h, 1_000_000, 4_999)
		reason := abortReason(t, h)
		require.Contains(t, reason, codeFeeBelowFloor)
		require.Equal(t, 1, strings.Count(reason, codeFeeBelowFloor),
			reason)
		requireResumed(t, h)
		require.Empty(t, h.bob.PendingFundings())
	})

	t.Run("more than the peer's balance", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		balance := h.bob.State().LocalCommitment.RemoteBalance
		init(h, -balance.ToSatoshis()-1, 2_500)
		require.Contains(t, abortReason(t, h), codeAboveBalance)
		requireResumed(t, h)
	})

	// Shutdown sent, a cooperative close under way: BOLT 2 allows no
	// splice from then on.
	t.Run("closing", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		require.NoError(t, h.bob.State().MarkShutdownSent(
			channeldb.NewShutdownInfo(
				lnwire.DeliveryAddress(p2wpkh(3)), true,
			),
		))
		init(h, 1_000_000, 2_500)
		require.Contains(t, abortReason(t, h), codeClosing)
		requireResumed(t, h)
	})

	// The peer's shutdown read: BOLT 2 has its splice_init answered with
	// a warning and the connection closed.
	t.Run("after the peer's shutdown", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		require.False(t, h.machine.ShutdownReceived())
		init(h, 1_000_000, 2_500)
		w := next[*lnwire.Warning](t, h)
		require.Contains(t, string(w.Data), "after shutdown")
		requireDisconnected(t, h)
		require.Empty(t, h.bob.PendingFundings())
	})

	// Leaving less than the reserve the peer is held to: refused before
	// anything is signed, though 1% of the new capacity would allow it.
	t.Run("below the peer's reserve", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		state := h.bob.State()
		balance := state.LocalCommitment.RemoteBalance.ToSatoshis()
		reserve := state.RemoteChanCfg.ChanReserve
		init(h, -(balance - reserve + 1), 2_500)
		require.Contains(t, abortReason(t, h), "below the reserve")
		requireResumed(t, h)
	})

	// The construction's reserve, 1% of the new capacity, above the
	// channel's own: refused at splice_init, not at tx_complete, where
	// the Core Lightning port has signed its part.
	t.Run("below 1% of the new capacity", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		state := h.bob.State()
		state.RemoteChanCfg.ChanReserve = 1_000
		balance := state.LocalCommitment.RemoteBalance.ToSatoshis()
		init(h, -(balance - 4_000_000), 2_500)
		require.Contains(t, abortReason(t, h), "below the reserve")
		requireResumed(t, h)
	})

	// Contributions no amount can be, before any arithmetic on them:
	// the extremes would overflow the balance and capacity sums.
	for _, c := range []btcutil.Amount{
		math.MinInt64, math.MaxInt64, -maxMoney - 1, maxMoney + 1,
	} {
		t.Run(fmt.Sprintf("contribution %d", int64(c)),
			func(t *testing.T) {
				t.Parallel()

				h := newAcceptorHarness(t)
				init(h, c, 2_500)
				require.Contains(t, abortReason(t, h),
					codePeerMisbehaved)
				requireResumed(t, h)
				require.Empty(t, h.bob.PendingFundings())
			})
	}

	t.Run("still catching up with the chain", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		h.chain.behind.Store(true)
		init(h, 1_000_000, 2_500)
		require.Contains(t, abortReason(t, h), codeChainNotSynced)
		requireResumed(t, h)
	})

	t.Run("locktime as a time", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		key, _ := btcec.NewPrivateKey()
		h.machine.HandleSpliceMessage(&lnwire.SpliceInit{
			ChannelID:           h.machine.cfg.ChanID,
			FundingContribution: 1_000_000,
			FundingFeeRatePerKw: 2_500,
			LockTime:            1_600_000_000,
			FundingPubKey:       key.PubKey(),
		})
		require.Contains(t, abortReason(t, h), codeLocktime)
		requireResumed(t, h)
	})

	t.Run("above the largest channel", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		h.machine.cfg.Policy.MaxChannelSize =
			h.bob.State().Capacity + 1_000
		init(h, 1_001, 2_500)
		require.Contains(t, abortReason(t, h), codeAboveMaxSize)
		requireResumed(t, h)
	})

	t.Run("locktime in the future", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		key, _ := btcec.NewPrivateKey()
		h.machine.HandleSpliceMessage(&lnwire.SpliceInit{
			ChannelID:           h.machine.cfg.ChanID,
			FundingContribution: 1_000_000,
			FundingFeeRatePerKw: 2_500,
			LockTime:            h.chain.height + 2,
			FundingPubKey:       key.PubKey(),
		})
		require.Contains(t, abortReason(t, h), codeLocktime)
		requireResumed(t, h)
	})

	t.Run("peer didn't start quiescence", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		h.link.initiator = lntypes.Local
		init(h, 1_000_000, 2_500)
		next[*lnwire.Warning](t, h)
		requireDisconnected(t, h)
	})

	t.Run("splicing turned off", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		h.machine.cfg.Disabled = true
		init(h, 1_000_000, 2_500)
		require.Contains(t, abortReason(t, h), codeDisabled)
		requireResumed(t, h)

		_, err := h.machine.Splice(Request{
			Direction: DirectionOut, Amount: 1_000_000,
			FeeRate: 2_500, Destination: p2tr(1),
		})
		require.ErrorContains(t, err, codeDisabled)
	})

	t.Run("replacement", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		h.machine.HandleSpliceMessage(&lnwire.TxInitRbf{
			ChannelID: h.machine.cfg.ChanID,
		})
		require.Contains(t, abortReason(t, h), codeRbfUnsupported)
	})
}

// TestAcceptSpliceNotUnified checks a channel without option_unified_sigs
// can't be spliced.
func TestAcceptSpliceNotUnified(t *testing.T) {
	t.Parallel()

	alice, bob, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err)
	_ = alice

	peer := &testPeer{msgs: make(chan lnwire.Message, 8)}
	link := &testLink{
		t: t, ch: bob, initiator: lntypes.Remote,
		resumed: make(chan struct{}, 2),
	}
	m := NewMachine(Config{
		ChanID:    lnwire.NewChanIDFromOutPoint(bob.ChannelPoint()),
		Link:      link,
		Channel:   bob,
		Peer:      peer,
		Chain:     newTestChain(),
		Estimator: chainfee.NewStaticEstimator(2_500, 253),
	})
	startReestablished(t, m)
	t.Cleanup(m.Stop)

	key, _ := btcec.NewPrivateKey()
	m.HandleSpliceMessage(&lnwire.SpliceInit{
		ChannelID: m.cfg.ChanID, FundingContribution: 1_000_000,
		FundingFeeRatePerKw: 2_500, FundingPubKey: key.PubKey(),
	})
	select {
	case msg := <-peer.msgs:
		abort, ok := msg.(*lnwire.TxAbort)
		require.True(t, ok, "%T", msg)
		require.Contains(t, string(abort.Data), codeNoUnifiedSigs)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
}

// TestAcceptConstructionRefused checks inputs the machine refuses during the
// construction: one the backend doesn't see confirmed and unspent, and one
// whose witness size isn't known in advance. The construction ends with
// tx_abort and nothing is left behind.
func TestAcceptConstructionRefused(t *testing.T) {
	t.Parallel()

	t.Run("unknown to the backend", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		s := startAliceSplice(t, h, 10_000_000, 2_500)
		delete(h.chain.outs, s.coin.OutPoint)

		// Alice's messages until the machine aborts.
		for {
			msg, err := s.session.Next()
			require.NoError(t, err)
			h.machine.HandleSpliceMessage(msg)

			reply := <-h.peer.msgs
			if abort, ok := reply.(*lnwire.TxAbort); ok {
				require.Contains(t, string(abort.Data),
					"not found unspent")
				break
			}
			require.NoError(t, s.session.Receive(reply))
		}
		requireResumed(t, h)
		require.Empty(t, h.bob.PendingFundings())
	})

	// The peer adding one of our own coins (a wallet coin, or a
	// channel's funding output) is refused before the backend is asked.
	t.Run("one of our own coins", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		s := startAliceSplice(t, h, 10_000_000, 2_500)
		ours := s.coin.OutPoint
		h.machine.mu.Lock()
		h.machine.cfg.IsOurOutpoint = func(op wire.OutPoint) bool {
			return op == ours
		}
		h.machine.mu.Unlock()

		for {
			msg, err := s.session.Next()
			require.NoError(t, err)
			h.machine.HandleSpliceMessage(msg)

			reply := <-h.peer.msgs
			if abort, ok := reply.(*lnwire.TxAbort); ok {
				require.Contains(t, string(abort.Data),
					"is ours")
				break
			}
			require.NoError(t, s.session.Receive(reply))
		}
		requireResumed(t, h)
		require.Empty(t, h.bob.PendingFundings())
	})

	t.Run("OP_RETURN output", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		key, _ := btcec.NewPrivateKey()
		h.machine.HandleSpliceMessage(&lnwire.SpliceInit{
			ChannelID:           h.machine.cfg.ChanID,
			FundingContribution: -1_000_000,
			FundingFeeRatePerKw: 2_500,
			LockTime:            h.chain.height,
			FundingPubKey:       key.PubKey(),
		})
		next[*lnwire.SpliceAck](t, h)

		script, err := txscript.NullDataScript(
			bytes.Repeat([]byte{1}, 80),
		)
		require.NoError(t, err)
		h.machine.HandleSpliceMessage(&lnwire.TxAddOutput{
			ChannelID: h.machine.cfg.ChanID, SerialID: 2,
			Amount: 10_000, Script: script,
		})
		require.Contains(t, abortReason(t, h),
			"output script not accepted")
		requireResumed(t, h)
		require.Empty(t, h.bob.PendingFundings())
	})

	t.Run("P2WSH input", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		key, _ := btcec.NewPrivateKey()
		h.machine.HandleSpliceMessage(&lnwire.SpliceInit{
			ChannelID:           h.machine.cfg.ChanID,
			FundingContribution: 1_000_000,
			FundingFeeRatePerKw: 2_500,
			LockTime:            h.chain.height,
			FundingPubKey:       key.PubKey(),
		})
		next[*lnwire.SpliceAck](t, h)

		wsh := coin(5, append([]byte{0x00, 0x20}, make(
			[]byte, 32)...), 2_000_000)
		h.chain.outs[wsh.OutPoint] = wsh.PrevTx.TxOut[0]
		var buf bytes.Buffer
		require.NoError(t, wsh.PrevTx.Serialize(&buf))
		h.machine.HandleSpliceMessage(&lnwire.TxAddInput{
			ChannelID: h.machine.cfg.ChanID, SerialID: 2,
			PrevTx: buf.Bytes(), PrevTxVout: 0,
			Sequence: interactivetx.MaxSequence,
		})
		require.Contains(t, abortReason(t, h),
			"only P2WPKH and P2TR key path inputs")
		requireResumed(t, h)
	})
}

// TestAcceptSpliceAborts checks the peer's tx_abort: before our
// tx_signatures the splice is forgotten and the abort echoed; after, it is
// kept, since the peer could still publish it, and the peer is marked.
func TestAcceptSpliceAborts(t *testing.T) {
	t.Parallel()

	t.Run("during the construction", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		startAliceSplice(t, h, 10_000_000, 2_500)
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("bye"),
		})
		next[*lnwire.TxAbort](t, h)
		requireResumed(t, h)
	})

	t.Run("after the commitments, before our signatures",
		func(t *testing.T) {
			t.Parallel()

			h := newAcceptorHarness(t)
			s := startAliceSplice(t, h, 10_000_000, 2_500)
			s.construct(t, h)
			<-h.link.commits
			require.Len(t, h.bob.PendingFundings(), 1)

			h.machine.HandleSpliceMessage(&lnwire.TxAbort{
				ChannelID: h.machine.cfg.ChanID,
			})
			next[*lnwire.TxAbort](t, h)
			requireResumed(t, h)
			require.Empty(t, h.bob.PendingFundings())
			records, err := h.bob.State().SpliceRecords()
			require.NoError(t, err)
			require.Empty(t, records)
		})

	// After ours, the peer's not in: as the Core Lightning port does in
	// its normal course, waiting for the echo and taking nothing else.
	// Its user's abort: the splice is kept, as the peer holds ours and
	// could still publish it, the channel's state there kept current,
	// but recorded disowned, before the echo (the port forgets it on
	// the echo): no longer being signed, nor named on reconnecting. The
	// peer isn't marked. The channel, taking no update now, has our
	// updates held before quiescence ends, and is closed cooperatively,
	// which a disowned splice doesn't bar; asked again on the next
	// connection, should the close not have started, its link told to
	// hold our updates from its start.
	t.Run("after our signatures", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }
		recorded := make(chan bool, 1)
		h.peer.onSend = func(msg lnwire.Message) {
			if _, ok := msg.(*lnwire.TxAbort); !ok {
				return
			}
			recs, err := h.bob.State().SpliceRecords()
			require.NoError(t, err)
			recorded <- recs[0].Negotiation.Disowned &&
				h.link.held.Load()
		}
		require.True(t, h.machine.Busy())

		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		echo := next[*lnwire.TxAbort](t, h)
		require.Empty(t, echo.Data)
		require.True(t, <-recorded, "echoed before the record and hold")
		requireResumed(t, h)
		select {
		case force := <-closes:
			require.False(t, force)
		case <-time.After(5 * time.Second):
			t.Fatal("no close")
		}
		require.False(t, h.machine.Busy())
		done := make(chan bool)
		h.machine.enqueue(func() { done <- h.machine.misbehaved })
		require.False(t, <-done)
		require.Len(t, h.bob.PendingFundings(), 1)
		recs, err := h.bob.State().SpliceRecords()
		require.NoError(t, err)
		require.True(t, recs[0].Negotiation.Disowned)
		signing, _ := h.machine.Signing()
		require.False(t, signing)
		named, _ := h.machine.ReestablishTLVs()
		require.True(t, named.IsNone())

		h.peer.onSend = nil
		cfg := h.machine.cfg
		h.machine.Stop()
		h.machine = NewMachine(cfg)
		require.NoError(t, h.machine.Start())
		t.Cleanup(h.machine.Stop)
		require.False(t, h.machine.Busy())
		held, closing := h.machine.Disowned()
		require.True(t, held)
		require.True(t, closing)
		h.machine.HandleReestablish(&lnwire.ChannelReestablish{})
		select {
		case force := <-closes:
			require.False(t, force)
		case <-time.After(5 * time.Second):
			t.Fatal("no close after restoring")
		}
	})

	// With an HTLC on the channel, which could never be settled now: a
	// force close, our commitment current.
	t.Run("after our signatures, with an HTLC", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		var preimage [32]byte
		preimage[0] = 1
		htlc := &lnwire.UpdateAddHTLC{
			PaymentHash: sha256Hash(preimage[:]),
			Amount:      lnwire.NewMSatFromSatoshis(50_000),
			Expiry:      500,
		}
		_, err := h.alice.AddHTLC(htlc, nil)
		require.NoError(t, err)
		_, err = h.bob.ReceiveHTLC(htlc)
		require.NoError(t, err)
		require.NoError(t, lnwallet.ForceStateTransition(h.alice, h.bob))

		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		next[*lnwire.TxAbort](t, h)
		select {
		case force := <-closes:
			require.True(t, force)
		case <-time.After(5 * time.Second):
			t.Fatal("no close")
		}
	})

	// With an update of the peer's not yet committed, which arrived
	// after the echo: it could never be, so a force close as well.
	t.Run("after our signatures, an update pending", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }

		var preimage [32]byte
		preimage[0] = 2
		htlc := &lnwire.UpdateAddHTLC{
			PaymentHash: sha256Hash(preimage[:]),
			Amount:      lnwire.NewMSatFromSatoshis(50_000),
			Expiry:      500,
		}
		h.machine.enqueue(func() {
			_, err := h.alice.AddHTLC(htlc, nil)
			require.NoError(t, err)
			_, err = h.bob.ReceiveHTLC(htlc)
			require.NoError(t, err)
		})
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		next[*lnwire.TxAbort](t, h)
		select {
		case force := <-closes:
			require.True(t, force)
		case <-time.After(5 * time.Second):
			t.Fatal("no close")
		}
	})

	// The earlier transaction never confirming (too low a fee, which the
	// peer can't bump while the disowned one is ours): force closed
	// disownedHoldBlocks after its broadcast, not held for good.
	t.Run("a replacement, the earlier never confirming", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }
		h.chain.mu.Lock()
		start := h.chain.height
		h.chain.mu.Unlock()
		h.machine.enqueue(func() {
			h.machine.active.replaced = append(
				h.machine.active.replaced, &active{
					txid:            chainhash.Hash{9},
					broadcastHeight: start + 10,
				},
			)
		})
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		next[*lnwire.TxAbort](t, h)
		requireResumed(t, h)
		require.Eventually(t, func() bool {
			h.chain.mu.Lock()
			defer h.chain.mu.Unlock()

			return len(h.chain.epochs) > 0
		}, 5*time.Second, 10*time.Millisecond)

		for i := 0; i < disownedHoldBlocks+9; i++ {
			h.chain.mine()
			time.Sleep(2 * time.Millisecond)
		}
		select {
		case <-closes:
			t.Fatal("closed before the deadline")
		case <-time.After(200 * time.Millisecond):
		}
		h.chain.mine()
		select {
		case force := <-closes:
			require.True(t, force)
		case <-time.After(5 * time.Second):
			t.Fatal("no close at the deadline")
		}
	})

	// The earlier transaction confirmed by the deadline, on its way to a
	// lock depth past it: not closed for it.
	t.Run("a replacement, the earlier confirmed in time", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }
		h.chain.mu.Lock()
		start := h.chain.height
		h.chain.mu.Unlock()
		earlier := chainhash.Hash{9}
		h.machine.enqueue(func() {
			h.machine.active.replaced = append(
				h.machine.active.replaced, &active{
					txid: earlier, broadcastHeight: start,
				},
			)
		})
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		next[*lnwire.TxAbort](t, h)
		requireResumed(t, h)
		h.chain.confirm(t, earlier, 1, 1, &chainntnfs.TxConfirmation{})

		for i := 0; i < disownedHoldBlocks+1; i++ {
			h.chain.mine()
			time.Sleep(2 * time.Millisecond)
		}
		select {
		case <-closes:
			t.Fatal("closed, the earlier transaction confirmed")
		case <-time.After(300 * time.Millisecond):
		}
	})

	// Confirmed, then reorganised out: no longer spares the channel.
	t.Run("a replacement, the earlier confirmed then reorged", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }
		h.chain.mu.Lock()
		start := h.chain.height
		h.chain.mu.Unlock()
		earlier := chainhash.Hash{9}
		h.machine.enqueue(func() {
			h.machine.active.replaced = append(
				h.machine.active.replaced, &active{
					txid: earlier, broadcastHeight: start,
				},
			)
		})
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		next[*lnwire.TxAbort](t, h)
		requireResumed(t, h)
		h.chain.confirm(t, earlier, 1, 1, &chainntnfs.TxConfirmation{})
		time.Sleep(50 * time.Millisecond)
		h.chain.reorg(t, earlier, 1)

		for i := 0; i < disownedHoldBlocks+1; i++ {
			h.chain.mine()
			time.Sleep(2 * time.Millisecond)
		}
		select {
		case force := <-closes:
			require.True(t, force)
		case <-time.After(5 * time.Second):
			t.Fatal("no close, the earlier transaction reorged out")
		}
	})

	// The earlier transaction the splice again by the deadline (its
	// confirmation adopted it): nothing closed.
	t.Run("a replacement, the earlier adopted in time", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }
		earlier := &active{txid: chainhash.Hash{9}}
		h.machine.enqueue(func() {
			h.machine.active.replaced = append(
				h.machine.active.replaced, earlier,
			)
		})
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		next[*lnwire.TxAbort](t, h)
		requireResumed(t, h)
		require.Eventually(t, func() bool {
			h.chain.mu.Lock()
			defer h.chain.mu.Unlock()

			return len(h.chain.epochs) > 0
		}, 5*time.Second, 10*time.Millisecond)
		h.machine.enqueue(func() { h.machine.setActive(earlier) })

		for i := 0; i < disownedHoldBlocks+1; i++ {
			h.chain.mine()
			time.Sleep(2 * time.Millisecond)
		}
		select {
		case <-closes:
			t.Fatal("closed, the earlier transaction adopted")
		case <-time.After(300 * time.Millisecond):
		}
	})

	// The replacement of a transaction signed both ways, disowned: the
	// peer keeps the earlier one, which locks in time, the disowned one
	// with it. Updates held, nothing closed.
	t.Run("a replacement, after our signatures", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		splicePastSigning(t, h)
		closes := make(chan bool, 4)
		h.machine.cfg.Close = func(force bool) { closes <- force }
		earlier := &active{txid: chainhash.Hash{9}}
		other := &active{txid: chainhash.Hash{10}}
		h.machine.enqueue(func() {
			h.machine.active.replaced = append(
				h.machine.active.replaced, earlier, other,
			)
		})
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
		})
		next[*lnwire.TxAbort](t, h)
		requireResumed(t, h)
		require.Eventually(t, h.link.held.Load, 5*time.Second,
			10*time.Millisecond)
		select {
		case <-closes:
			t.Fatal("closed with an earlier transaction live")
		case <-time.After(200 * time.Millisecond):
		}
		held, closing := h.machine.Disowned()
		require.True(t, held)
		require.False(t, closing)

		// One dies, another still live: nothing closed. The last
		// dies: nothing will lock in time, and the channel is closed.
		h.machine.enqueue(func() { h.machine.dropCandidate(other) })
		select {
		case <-closes:
			t.Fatal("closed with an earlier transaction live")
		case <-time.After(200 * time.Millisecond):
		}
		h.machine.enqueue(func() { h.machine.dropCandidate(earlier) })
		select {
		case force := <-closes:
			require.False(t, force)
		case <-time.After(5 * time.Second):
			t.Fatal("no close once the earlier one died")
		}
		_, closing = h.machine.Disowned()
		require.True(t, closing)
	})

	// Answering the next_funding of our reestablish, the port keeping the
	// splice (its user's signatures still missing): kept, not disowned.
	// A later abort on the connection, its user's, disowns it.
	t.Run("after our signatures, answering our reestablish",
		func(t *testing.T) {
			t.Parallel()

			h := newAcceptorHarness(t)
			splicePastSigning(t, h)
			h.machine.Stop()
			h.machine = NewMachine(h.machine.cfg)
			require.NoError(t, h.machine.Start())
			t.Cleanup(h.machine.Stop)
			named, _ := h.machine.ReestablishTLVs()
			require.True(t, named.IsSome())
			h.machine.HandleReestablish(&lnwire.ChannelReestablish{})

			disowned := func() bool {
				recs, err := h.bob.State().SpliceRecords()
				require.NoError(t, err)

				return recs[0].Negotiation.Disowned
			}
			abort := func(why string) {
				h.machine.HandleSpliceMessage(&lnwire.TxAbort{
					ChannelID: h.machine.cfg.ChanID,
					Data:      []byte(why),
				})
				echo := next[*lnwire.TxAbort](t, h)
				require.Empty(t, echo.Data)
				requireResumed(t, h)
			}
			abort("next_funding_txid not recognized")
			require.False(t, disowned())
			signing, _ := h.machine.Signing()
			require.True(t, signing)

			abort("user")
			require.True(t, disowned())
			require.Len(t, h.bob.PendingFundings(), 1)
		})

	// After both: an abort the peer may not send. Marked, and the
	// connection dropped with a warning.
	t.Run("after both signatures", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		s := splicePastSigning(t, h)
		h.machine.HandleSpliceMessage(s.aliceTxSignatures(t, h))
		requireResumed(t, h)

		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("no"),
		})
		requireDisconnected(t, h)
		done := make(chan bool)
		h.machine.enqueue(func() { done <- h.machine.misbehaved })
		require.True(t, <-done)
		require.Len(t, h.bob.PendingFundings(), 1)
	})

	t.Run("invalid signatures after ours", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		s := splicePastSigning(t, h)
		sigs := s.aliceTxSignatures(t, h)
		sigs.Witnesses[0][0][10] ^= 0xff
		h.machine.HandleSpliceMessage(sigs)

		require.Eventually(t, func() bool {
			done := make(chan bool)
			h.machine.enqueue(func() { done <- h.machine.misbehaved })
			return <-done
		}, 5*time.Second, 10*time.Millisecond)
		require.Len(t, h.bob.PendingFundings(), 1)
		require.Empty(t, h.wallet.publishedTxs())
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		h.machine.cfg.Policy.NegotiationTimeout = 50 * time.Millisecond
		startAliceSplice(t, h, 10_000_000, 2_500)
		require.Contains(t, abortReason(t, h), codeTimeout)
		requireResumed(t, h)
	})
}

// TestAcceptSpliceRestart checks restarts: in the middle of signing (Bob's
// tx_signatures sent, Alice's not yet in) the machine comes back asking for
// nothing but her tx_signatures, and finishes when they arrive; once pending,
// it publishes again and waits for the lock.
func TestAcceptSpliceRestart(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	s := splicePastSigning(t, h)
	txid := s.res.Tx.TxHash()

	restart := func() {
		h.machine.Stop()
		h.machine = NewMachine(h.machine.cfg)
		startReestablished(t, h.machine)
	}
	restart()

	// next_funding without a request: both commitments are in.
	nextFunding, locked := h.machine.ReestablishTLVs()
	nf := nextFunding.UnsafeFromSome()
	require.Equal(t, txid, nf.Txid)
	require.Zero(t, nf.RetransmitFlags)
	require.Equal(t, h.bob.State().CurrentFundingOutpoint().Hash,
		locked.UnsafeFromSome().Txid)

	// Alice reconnects naming the splice: Bob sends his tx_signatures
	// again.
	h.machine.HandleReestablish(&lnwire.ChannelReestablish{
		NextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: txid}),
	})
	require.Equal(t, txid, next[*lnwire.TxSignatures](t, h).TxID)

	h.machine.HandleSpliceMessage(s.aliceTxSignatures(t, h))
	requireResumed(t, h)
	require.Len(t, h.wallet.publishedTxs(), 1)

	// Restart while pending: published again, watched again.
	h.wallet.mu.Lock()
	h.wallet.published = make(map[chainhash.Hash]string)
	h.wallet.mu.Unlock()
	restart()
	require.Len(t, h.wallet.publishedTxs(), 1)

	h.chain.confirm(t, txid, 6, 2, &chainntnfs.TxConfirmation{
		BlockHeight: 900_010,
	})
	next[*lnwire.SpliceLocked](t, h)
	h.machine.HandleSpliceMessage(&lnwire.SpliceLocked{
		ChannelID: h.machine.cfg.ChanID, SpliceTxid: txid,
	})
	require.Eventually(t, func() bool {
		return h.bob.State().CurrentFundingOutpoint().Hash == txid
	}, 5*time.Second, 20*time.Millisecond)

	// An unknown next_funding gets tx_abort.
	h.machine.HandleReestablish(&lnwire.ChannelReestablish{
		NextFunding: fn.Some(lnwire.FundingTxidFlags{
			Txid: chainhash.Hash{0xee},
		}),
	})
	next[*lnwire.TxAbort](t, h)
}

// startReestablished starts a machine as the link does: then the peer's
// channel_reestablish, after which splice_locked may be sent.
func startReestablished(t *testing.T, m *Machine) {
	t.Helper()

	require.NoError(t, m.Start())
	m.HandleReestablish(&lnwire.ChannelReestablish{})
}

// TestCheckPeerOutput checks the outputs a peer may add: witness v0 and v1
// programs only, which every relay policy accepts.
func TestCheckPeerOutput(t *testing.T) {
	t.Parallel()

	m := &Machine{}
	opReturn, err := txscript.NullDataScript(bytes.Repeat([]byte{1}, 80))
	require.NoError(t, err)
	p2pkh := append([]byte{
		txscript.OP_DUP, txscript.OP_HASH160, txscript.OP_DATA_20,
	}, append(make([]byte, 20), txscript.OP_EQUALVERIFY,
		txscript.OP_CHECKSIG)...)
	p2sh := append([]byte{txscript.OP_HASH160, txscript.OP_DATA_20},
		append(make([]byte, 20), txscript.OP_EQUAL)...)

	for _, tc := range []struct {
		name   string
		script []byte
		ok     bool
	}{
		{"P2WPKH", append([]byte{0x00, 0x14}, make([]byte, 20)...), true},
		{"P2WSH", append([]byte{0x00, 0x20}, make([]byte, 32)...), true},
		{"P2TR", append([]byte{0x51, 0x20}, make([]byte, 32)...), true},
		{"80-byte OP_RETURN", opReturn, false},
		{"P2PKH", p2pkh, false},
		{"P2SH", p2sh, false},
		{"witness v2", append([]byte{0x52, 0x20}, make([]byte, 32)...),
			false},
		{"empty", nil, false},
	} {
		err := m.checkPeerOutput(&interactivetx.Output{
			Value: 10_000, PkScript: tc.script,
		})
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.Error(t, err, tc.name)
		}
	}
}

// TestPrintable checks how the peer's tx_abort text is kept: printable
// ASCII only, at most 160 bytes of what was sent.
func TestPrintable(t *testing.T) {
	t.Parallel()

	require.Equal(t, "no thanks", printable([]byte("no thanks")))
	require.Equal(t, "a", printable([]byte("a\x00\n\x1b[31mb")[:3]))
	require.Equal(t, "a[31mb", printable([]byte("a\x1b[31mb")))
	require.Equal(t, "", printable([]byte{0xff, 0xfe, 0x7f}))
	require.Equal(t, "caf", printable([]byte("café")))

	long := bytes.Repeat([]byte("x"), 400)
	require.Len(t, printable(long), 160)

	// The limit counts what was sent, not what is kept.
	mixed := append(bytes.Repeat([]byte{0x01}, 150), []byte("tail of it")...)
	require.Equal(t, "tail of it", printable(mixed))
	mixed = append(bytes.Repeat([]byte{0x01}, 155), []byte("tail of it")...)
	require.Equal(t, "tail", printable(mixed)[:4])
	require.Len(t, printable(mixed), 5)
}
