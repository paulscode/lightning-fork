package lnwallet

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// A splice moves a channel to a new funding output. From the moment the
// splice's commitments are signed until it locks, every commitment exists
// once per funding output: the current one and each pending splice's. They
// share the HTLCs, the fee rate, the commitment number and point; they differ
// in the funding input, the capacity and the two balances, which move by the
// splice's contributions. A pending splice's commitments are rebuilt from the
// current funding's, so the two can't drift apart.

// MaxPendingFundings is how many splices of one channel may be pending at
// once: a batch carries at most lnwire.MaxBatchSize signatures, one of them
// for the current funding.
const MaxPendingFundings = lnwire.MaxBatchSize - 1

var (
	// ErrNotQuiescent is returned when the splice's own commitments are
	// exchanged while either commitment chain has an unacknowledged
	// state or updates are waiting to be signed.
	ErrNotQuiescent = errors.New("channel has updates in flight")

	// ErrUnknownFunding is returned for a funding txid that is neither
	// the channel's nor a pending splice's.
	ErrUnknownFunding = errors.New("unknown funding transaction")

	// ErrSpliceNotExchanged is returned for a commitment step while a
	// pending splice's own commitments haven't been exchanged both ways:
	// the step would move the heights they are signed at.
	ErrSpliceNotExchanged = errors.New("splice commitments not exchanged")
)

// commitFunding is the funding output a commitment spends.
type commitFunding struct {
	// txIn spends the funding output.
	txIn wire.TxIn

	// capacity is the funding output's value.
	capacity btcutil.Amount

	// localDelta and remoteDelta are the changes a pending splice makes
	// to each side's balance, in millisatoshis; zero for the current
	// funding.
	localDelta, remoteDelta int64

	// localKey and remoteKey are a pending splice's funding keys, nil for
	// the current funding's. Each side's anchor locks to its key in the
	// funding output the commitment spends (BOLT 3).
	localKey, remoteKey *btcec.PublicKey
}

// configs returns the channel configurations a commitment on this funding is
// built with: the channel's, with a pending splice's funding keys.
func (f commitFunding) configs(ch *channeldb.OpenChannel) (
	*channeldb.ChannelConfig, *channeldb.ChannelConfig) {

	local, remote := ch.LocalChanCfg, ch.RemoteChanCfg
	if f.localKey != nil {
		local.MultiSigKey.PubKey = f.localKey
	}
	if f.remoteKey != nil {
		remote.MultiSigKey.PubKey = f.remoteKey
	}

	return &local, &remote
}

// applyDeltas moves pre-fee balances by the funding's deltas. A balance that
// would go negative is an error: the splice took more than the side had.
func (f commitFunding) applyDeltas(ours,
	theirs lnwire.MilliSatoshi) (lnwire.MilliSatoshi, lnwire.MilliSatoshi,
	error) {

	apply := func(b lnwire.MilliSatoshi, delta int64,
		who string) (lnwire.MilliSatoshi, error) {

		if b > math.MaxInt64 {
			return 0, fmt.Errorf("%s balance out of range", who)
		}
		v := int64(b) + delta
		if (delta > 0 && v < int64(b)) || v < 0 {
			return 0, fmt.Errorf("%w: %s balance %v moved by %d msat",
				ErrBelowChanReserve, who, b, delta)
		}

		return lnwire.MilliSatoshi(v), nil
	}

	ours, err := apply(ours, f.localDelta, "our")
	if err != nil {
		return 0, 0, err
	}
	theirs, err = apply(theirs, f.remoteDelta, "their")
	if err != nil {
		return 0, 0, err
	}

	return ours, theirs, nil
}

// currentCommitFunding returns the channel's current funding output.
func (lc *LightningChannel) currentCommitFunding() commitFunding {
	return commitFunding{
		txIn:     fundingTxIn(lc.channelState),
		capacity: lc.channelState.Capacity,
	}
}

// PendingFunding is a splice's funding output, which the channel moves to
// once the splice locks.
type PendingFunding struct {
	// OutPoint is the new funding output.
	OutPoint wire.OutPoint

	// Capacity is its value: the current capacity plus both deltas.
	Capacity btcutil.Amount

	// LocalKey is our key in its 2-of-2 script.
	LocalKey keychain.KeyDescriptor

	// RemoteKey is the peer's key in its 2-of-2 script.
	RemoteKey *btcec.PublicKey

	// LocalDelta and RemoteDelta are the changes to each side's balance:
	// the splice's funding contributions.
	LocalDelta, RemoteDelta btcutil.Amount
}

// pendingFunding is a pending splice with what this channel needs to sign
// and check its commitments.
type pendingFunding struct {
	PendingFunding

	// fundingOutput is the new funding output.
	fundingOutput wire.TxOut

	// signDesc signs for our key in it.
	signDesc *input.SignDescriptor

	// local is our commitment at each height on this funding, with the
	// peer's signatures once received.
	local map[uint64]*commitment

	// remote is the peer's commitment at each height on this funding, as
	// we signed it.
	remote map[uint64]*commitment
}

// retiredFunding is the funding a locked splice replaced, with the peer's
// commitments on it that were unrevoked at the lock: each is logged when the
// peer revokes it, in case a reorg brings the funding back.
type retiredFunding struct {
	txid   chainhash.Hash
	remote map[uint64]*channeldb.ChannelCommitment
}

// txid returns the splice transaction's id.
func (p *pendingFunding) txid() chainhash.Hash {
	return p.OutPoint.Hash
}

// commitFunding returns the funding a commitment on this splice spends.
func (p *pendingFunding) commitFunding() commitFunding {
	return commitFunding{
		txIn:        *wire.NewTxIn(&p.OutPoint, nil, nil),
		capacity:    p.Capacity,
		localDelta:  int64(p.LocalDelta) * 1000,
		remoteDelta: int64(p.RemoteDelta) * 1000,
		localKey:    p.LocalKey.PubKey,
		remoteKey:   p.RemoteKey,
	}
}

// AddPendingFunding adds a splice to the channel's funding set, once its
// transaction is agreed and before its commitments are exchanged. Its
// capacity must be the current one plus both deltas.
func (lc *LightningChannel) AddPendingFunding(f PendingFunding) error {
	lc.Lock()
	defer lc.Unlock()

	switch {
	case lc.spliceRestoreErr != nil:
		return lc.spliceRestoreErr

	// The splice's own commitments are exchanged at the current
	// heights, which must not move until they are.
	case !lc.quiescent():
		return ErrNotQuiescent

	case lc.channelState.ChanType.IsTaproot():
		return errors.New("taproot channels can't be spliced")

	case len(lc.pendingFundings) >= MaxPendingFundings:
		return fmt.Errorf("already %d pending splices",
			len(lc.pendingFundings))

	case f.RemoteKey == nil || f.LocalKey.PubKey == nil:
		return errors.New("pending funding without both keys")

	case f.OutPoint == lc.channelState.CurrentFundingOutpoint():
		return errors.New("pending funding is the current one")

	case f.Capacity != lc.channelState.Capacity+f.LocalDelta+
		f.RemoteDelta:

		return fmt.Errorf("pending funding of %v doesn't match "+
			"capacity %v moved by %v and %v", f.Capacity,
			lc.channelState.Capacity, f.LocalDelta, f.RemoteDelta)
	}
	for _, p := range lc.pendingFundings {
		if p.OutPoint == f.OutPoint {
			return fmt.Errorf("funding %v already pending",
				f.OutPoint)
		}
	}

	p, err := lc.newPendingFunding(f)
	if err != nil {
		return err
	}
	if err := lc.channelState.EnsureSpliceRecord(p.spliceRecord()); err != nil {
		return err
	}
	lc.pendingFundings = append(lc.pendingFundings, p)

	return nil
}

// newPendingFunding returns a pending splice with its funding script and
// sign descriptor.
func (lc *LightningChannel) newPendingFunding(
	f PendingFunding) (*pendingFunding, error) {

	witnessScript, err := input.GenMultiSigScript(
		f.LocalKey.PubKey.SerializeCompressed(),
		f.RemoteKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, err
	}
	pkScript, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return nil, err
	}

	p := &pendingFunding{
		PendingFunding: f,
		fundingOutput: wire.TxOut{
			PkScript: pkScript, Value: int64(f.Capacity),
		},
		local:  make(map[uint64]*commitment),
		remote: make(map[uint64]*commitment),
	}
	p.signDesc = &input.SignDescriptor{
		KeyDesc:       f.LocalKey,
		WitnessScript: witnessScript,
		Output:        &p.fundingOutput,
		HashType:      CommitSigHashType(lc.channelState.ChanType),
		InputIndex:    0,
	}

	return p, nil
}

// PendingFundings returns the channel's pending splices.
func (lc *LightningChannel) PendingFundings() []PendingFunding {
	lc.RLock()
	defer lc.RUnlock()

	fs := make([]PendingFunding, len(lc.pendingFundings))
	for i, p := range lc.pendingFundings {
		fs[i] = p.PendingFunding
	}

	return fs
}

// pendingFunding returns the pending splice with the given txid.
func (lc *LightningChannel) pendingFunding(
	txid chainhash.Hash) (*pendingFunding, error) {

	for _, p := range lc.pendingFundings {
		if p.txid() == txid {
			return p, nil
		}
	}

	return nil, fmt.Errorf("%w: %v", ErrUnknownFunding, txid)
}

// rebuildCommitment builds c, a commitment on the current funding, again on
// another funding: same HTLCs, fee rate, height and keys, the funding's input
// and capacity, the balances moved by its deltas.
func (lc *LightningChannel) rebuildCommitment(c *commitment,
	keyRing *CommitmentKeyRing, funding commitFunding) (*commitment,
	error) {

	// Back to the balances before the commitment fee, the way computeView
	// starts from a previous commitment.
	ours, theirs := c.ourBalance, c.theirBalance
	fee := lnwire.NewMSatFromSatoshis(c.fee)
	if lc.channelState.IsInitiator {
		ours += fee
	} else {
		theirs += fee
	}

	view := &HtlcView{
		NextHeight: c.height,
		FeePerKw:   c.feePerKw,
		Updates: lntypes.Dual[[]*paymentDescriptor]{
			Local:  make([]*paymentDescriptor, len(c.outgoingHTLCs)),
			Remote: make([]*paymentDescriptor, len(c.incomingHTLCs)),
		},
	}
	for i := range c.outgoingHTLCs {
		htlc := c.outgoingHTLCs[i]
		view.Updates.Local[i] = &htlc
	}
	for i := range c.incomingHTLCs {
		htlc := c.incomingHTLCs[i]
		view.Updates.Remote[i] = &htlc
	}

	tx, err := lc.commitBuilder.createUnsignedCommitmentTx(
		ours, theirs, c.whoseCommit, c.feePerKw, c.height, view, view,
		keyRing, c, funding,
	)
	if err != nil {
		return nil, err
	}

	rebuilt := &commitment{
		height:         c.height,
		whoseCommit:    c.whoseCommit,
		messageIndices: c.messageIndices,
		ourHtlcIndex:   c.ourHtlcIndex,
		theirHtlcIndex: c.theirHtlcIndex,
		txn:            tx.txn,
		ourBalance:     tx.ourBalance,
		theirBalance:   tx.theirBalance,
		fee:            tx.fee,
		feePerKw:       c.feePerKw,
		dustLimit:      c.dustLimit,
		customBlob:     c.customBlob,
		outgoingHTLCs: make(
			[]paymentDescriptor, len(c.outgoingHTLCs),
		),
		incomingHTLCs: make(
			[]paymentDescriptor, len(c.incomingHTLCs),
		),
	}
	copy(rebuilt.outgoingHTLCs, c.outgoingHTLCs)
	copy(rebuilt.incomingHTLCs, c.incomingHTLCs)

	err = rebuilt.populateHtlcIndexes(lc.channelState.ChanType, tx.cltvs)
	if err != nil {
		return nil, err
	}

	return rebuilt, nil
}

// localKeyRing returns the key ring of our commitment at height.
func (lc *LightningChannel) localKeyRing(
	height uint64) (*CommitmentKeyRing, error) {

	secret, err := lc.channelState.RevocationProducer.AtIndex(height)
	if err != nil {
		return nil, err
	}
	point := input.ComputeCommitmentPoint(secret[:])

	return DeriveCommitmentKeys(
		point, lntypes.Local, lc.channelState.ChanType,
		&lc.channelState.LocalChanCfg, &lc.channelState.RemoteChanCfg,
	), nil
}

// remoteKeyRing returns the key ring of the peer's current commitment, the
// tail of its chain.
func (lc *LightningChannel) remoteKeyRing() (*CommitmentKeyRing, error) {
	point := lc.channelState.RemoteCurrentRevocation
	if point == nil {
		return nil, errors.New("no remote commitment point")
	}

	return DeriveCommitmentKeys(
		point, lntypes.Remote, lc.channelState.ChanType,
		&lc.channelState.LocalChanCfg, &lc.channelState.RemoteChanCfg,
	), nil
}

// quiescent reports whether the splice's own commitments can be exchanged:
// each chain holds one commitment and nothing waits to be signed.
func (lc *LightningChannel) quiescent() bool {
	return !lc.commitChains.Local.hasUnackedCommitment() &&
		!lc.commitChains.Remote.hasUnackedCommitment() &&
		!lc.oweCommitment(lntypes.Local) &&
		!lc.oweCommitment(lntypes.Remote)
}

// signCommitmentOn signs the peer's commitment remote, built on a pending
// splice's funding: the commitment signature and one per HTLC, in output
// order.
func (lc *LightningChannel) signCommitmentOn(ctx context.Context,
	p *pendingFunding, remote *commitment,
	keyRing *CommitmentKeyRing) (*CommitSigs, error) {

	var leaseExpiry uint32
	if lc.channelState.ChanType.HasLeaseExpiration() {
		leaseExpiry = lc.channelState.ThawHeight
	}
	sigBatch, _, cancelChan, err := genRemoteHtlcSigJobs(
		keyRing, lc.channelState, leaseExpiry, remote, lc.leafStore,
	)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(sigBatch, func(i, j SignJob) int {
		return cmp.Compare(i.OutputIndex, j.OutputIndex)
	})
	lc.sigPool.SubmitSignBatch(sigBatch)

	signDesc := *p.signDesc
	signDesc.SigHashes = input.NewTxSigHashesV0Only(remote.txn)
	rawSig, err := lc.Signer.SignOutputRaw(remote.txn, &signDesc)
	if err != nil {
		close(cancelChan)
		return nil, err
	}
	sig, err := lnwire.NewSigFromSignature(rawSig)
	if err != nil {
		close(cancelChan)
		return nil, err
	}

	htlcSigs := make([]lnwire.Sig, 0, len(sigBatch))
	for i := range sigBatch {
		var resp SignJobResp
		select {
		case resp = <-sigBatch[i].Resp:
		case <-ctx.Done():
			return nil, errQuit
		}
		if resp.Err != nil {
			close(cancelChan)
			return nil, resp.Err
		}
		htlcSigs = append(htlcSigs, resp.Sig)
	}

	return &CommitSigs{CommitSig: sig, HtlcSigs: htlcSigs}, nil
}

// verifyCommitmentOn checks the peer's signatures for our commitment local,
// built on a pending splice's funding, and records them on it.
func (lc *LightningChannel) verifyCommitmentOn(p *pendingFunding,
	local *commitment, keyRing *CommitmentKeyRing,
	sigs *CommitSigs) error {

	var leaseExpiry uint32
	if lc.channelState.ChanType.HasLeaseExpiration() {
		leaseExpiry = lc.channelState.ThawHeight
	}
	verifyJobs, _, err := genHtlcSigValidationJobs(
		lc.channelState, local, keyRing, sigs.HtlcSigs, leaseExpiry,
		lc.leafStore, fn.None[AuxSigner](), fn.None[[]byte](),
	)
	if err != nil {
		return err
	}
	cancelChan := make(chan struct{})
	verifyResps := lc.sigPool.SubmitVerifyBatch(verifyJobs, cancelChan)

	// The unified digest commits to the spent output's script, so the
	// fetcher carries the pending funding output.
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		p.fundingOutput.PkScript, p.fundingOutput.Value,
	)
	sigHash, err := txscript.CalcWitnessSigHash(
		p.signDesc.WitnessScript,
		txscript.NewTxSigHashes(local.txn, prevFetcher),
		CommitSigHashType(lc.channelState.ChanType), local.txn, 0,
		p.fundingOutput.Value,
	)
	if err != nil {
		close(cancelChan)
		return err
	}
	cSig, err := sigs.CommitSig.ToSignature()
	if err != nil {
		close(cancelChan)
		return err
	}
	if !cSig.Verify(sigHash, p.RemoteKey) {
		close(cancelChan)

		var txBytes bytes.Buffer
		_ = local.txn.Serialize(&txBytes)

		return &InvalidCommitSigError{
			commitHeight: local.height,
			commitSig:    sigs.CommitSig.ToSignatureBytes(),
			sigHash:      sigHash,
			commitTx:     txBytes.Bytes(),
		}
	}

	for range verifyJobs {
		if htlcErr := <-verifyResps; htlcErr != nil {
			close(cancelChan)

			return fmt.Errorf("htlc %d signature on splice %v: %w",
				htlcErr.HtlcIndex, p.txid(), htlcErr)
		}
	}

	local.sig = sigs.CommitSig.ToSignatureBytes()

	return nil
}

// SignSpliceCommitment signs the peer's current commitment on a pending
// splice's funding: the splice's own commitment_signed, sent once its
// transaction is agreed, at the same commitment number and without a
// revocation.
func (lc *LightningChannel) SignSpliceCommitment(ctx context.Context,
	txid chainhash.Hash) (*CommitSigs, error) {

	lc.Lock()
	defer lc.Unlock()

	if !lc.quiescent() {
		return nil, ErrNotQuiescent
	}
	p, err := lc.pendingFunding(txid)
	if err != nil {
		return nil, err
	}

	keyRing, err := lc.remoteKeyRing()
	if err != nil {
		return nil, err
	}
	tail := lc.commitChains.Remote.tail()
	remote, err := lc.rebuildCommitment(tail, keyRing, p.commitFunding())
	if err != nil {
		return nil, err
	}

	sigs, err := lc.signCommitmentOn(ctx, p, remote, keyRing)
	if err != nil {
		return nil, err
	}

	err = lc.channelState.UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.RemoteTail = remote.toDiskCommit(lntypes.Remote)
			r.SpliceSig = lc.commitSigMsg(txid, sigs)

			return nil
		})
	if err != nil {
		return nil, err
	}
	p.remote[remote.height] = remote

	return sigs, nil
}

// ReceiveSpliceCommitment checks the peer's signatures for our current
// commitment on a pending splice's funding, the splice's own
// commitment_signed. Once it passes, the commitment on the new funding can be
// broadcast if the splice confirms.
func (lc *LightningChannel) ReceiveSpliceCommitment(txid chainhash.Hash,
	sigs *CommitSigs) error {

	lc.Lock()
	defer lc.Unlock()

	if !lc.quiescent() {
		return ErrNotQuiescent
	}
	p, err := lc.pendingFunding(txid)
	if err != nil {
		return err
	}

	tail := lc.commitChains.Local.tail()
	keyRing, err := lc.localKeyRing(tail.height)
	if err != nil {
		return err
	}
	local, err := lc.rebuildCommitment(tail, keyRing, p.commitFunding())
	if err != nil {
		return err
	}

	if err := lc.verifyCommitmentOn(p, local, keyRing, sigs); err != nil {
		return err
	}

	err = lc.channelState.UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.LocalCommit = local.toDiskCommit(lntypes.Local)
			return nil
		})
	if err != nil {
		return err
	}
	p.local[local.height] = local

	return nil
}

// SignedSpliceCommitment returns our latest commitment on a pending splice's
// funding with the peer's signature: the transaction to publish if the
// splice confirms and the channel must be closed from it.
func (lc *LightningChannel) SignedSpliceCommitment(
	txid chainhash.Hash) (*wire.MsgTx, error) {

	lc.RLock()
	defer lc.RUnlock()

	p, err := lc.pendingFunding(txid)
	if err != nil {
		return nil, err
	}
	local, ok := p.local[lc.commitChains.Local.tail().height]
	if !ok || local.sig == nil {
		return nil, fmt.Errorf("no signed commitment on splice %v", txid)
	}

	signDesc := *p.signDesc

	return GetSignedCommitTx(SignedCommitTxInputs{
		CommitTx:  local.txn,
		CommitSig: local.sig,
		OurKey:    p.LocalKey,
		TheirKey:  keychain.KeyDescriptor{PubKey: p.RemoteKey},
		SignDesc:  &signDesc,
	}, lc.Signer)
}

// SpliceCommitSigs is a pending splice's member of a commitment batch.
type SpliceCommitSigs struct {
	// FundingTxid names the splice.
	FundingTxid chainhash.Hash

	*CommitSigs
}

// spliceRecord returns the database record of a pending splice's funding.
func (p *pendingFunding) spliceRecord() *channeldb.SpliceRecord {
	return &channeldb.SpliceRecord{
		OutPoint:    p.OutPoint,
		Capacity:    p.Capacity,
		LocalKey:    p.LocalKey,
		RemoteKey:   p.RemoteKey,
		LocalDelta:  p.LocalDelta,
		RemoteDelta: p.RemoteDelta,
	}
}

// commitSigMsg returns signatures as the commitment_signed that carries them
// for a splice's funding.
func (lc *LightningChannel) commitSigMsg(txid chainhash.Hash,
	sigs *CommitSigs) *lnwire.CommitSig {

	return &lnwire.CommitSig{
		ChanID: lnwire.NewChanIDFromOutPoint(
			lc.channelState.FundingOutpoint,
		),
		CommitSig:   sigs.CommitSig,
		HtlcSigs:    sigs.HtlcSigs,
		FundingTxid: fn.Some(txid),
	}
}

// signPendingFundings signs the peer's next commitment, remote, on every
// pending splice's funding, in the set's order. Nothing is recorded: the
// caller persists the results with the commitment step.
func (lc *LightningChannel) signPendingFundings(ctx context.Context,
	remote *commitment, keyRing *CommitmentKeyRing) ([]SpliceCommitSigs,
	[]*commitment, error) {

	var (
		sigs    []SpliceCommitSigs
		commits []*commitment
	)
	for _, p := range lc.pendingFundings {
		c, err := lc.rebuildCommitment(remote, keyRing, p.commitFunding())
		if err != nil {
			return nil, nil, fmt.Errorf("splice %v: %w", p.txid(),
				err)
		}
		s, err := lc.signCommitmentOn(ctx, p, c, keyRing)
		if err != nil {
			return nil, nil, err
		}
		sigs = append(sigs, SpliceCommitSigs{
			FundingTxid: p.txid(), CommitSigs: s,
		})
		commits = append(commits, c)
	}

	return sigs, commits, nil
}

// spliceRemoteWrites returns the writes recording the peer's next
// commitments on the pending splices' fundings, and our signatures for them.
func (lc *LightningChannel) spliceRemoteWrites(sigs []SpliceCommitSigs,
	commits []*commitment) []channeldb.SpliceCommitWrite {

	writes := make([]channeldb.SpliceCommitWrite, len(sigs))
	for i, s := range sigs {
		writes[i] = channeldb.SpliceCommitWrite{
			Txid:         s.FundingTxid,
			RemoteTip:    commits[i].toDiskCommit(lntypes.Remote),
			RemoteTipSig: lc.commitSigMsg(s.FundingTxid, s.CommitSigs),
		}
	}

	return writes
}

// checkBatch checks that a batch's splice members name exactly the pending
// splices, each once. Anything else is refused before any state changes.
// ErrSpliceBatchMismatch is a commitment batch with a signature for more or
// fewer pending splices than the channel has: the two sides disagree on the
// set, as when one dropped a splice before the other. Checked before
// anything is accepted, so the link drops the connection rather than force
// closing; the retransmission after the reconnection agrees once both sides
// have done the same.
var ErrSpliceBatchMismatch = errors.New("commitment batch doesn't match " +
	"the pending splices")

func (lc *LightningChannel) checkBatch(sigs []SpliceCommitSigs) error {
	if len(sigs) != len(lc.pendingFundings) {
		return fmt.Errorf("%w: %d splice signatures for %d pending "+
			"splices", ErrSpliceBatchMismatch, len(sigs),
			len(lc.pendingFundings))
	}

	seen := make(map[chainhash.Hash]bool, len(sigs))
	for _, s := range sigs {
		if s.CommitSigs == nil {
			return fmt.Errorf("no signatures for splice %v",
				s.FundingTxid)
		}
		if seen[s.FundingTxid] {
			return fmt.Errorf("splice %v signed twice in a batch",
				s.FundingTxid)
		}
		seen[s.FundingTxid] = true

		if _, err := lc.pendingFunding(s.FundingTxid); err != nil {
			return err
		}
	}

	return nil
}

// verifyPendingFundings checks the peer's signatures for our next
// commitment, local, on every pending splice's funding, and returns those
// commitments in the set's order with the signatures on them.
func (lc *LightningChannel) verifyPendingFundings(local *commitment,
	keyRing *CommitmentKeyRing, sigs []SpliceCommitSigs) ([]*commitment,
	error) {

	byTxid := make(map[chainhash.Hash]*CommitSigs, len(sigs))
	for _, s := range sigs {
		byTxid[s.FundingTxid] = s.CommitSigs
	}

	commits := make([]*commitment, len(lc.pendingFundings))
	for i, p := range lc.pendingFundings {
		c, err := lc.rebuildCommitment(local, keyRing, p.commitFunding())
		if err != nil {
			return nil, fmt.Errorf("splice %v: %w", p.txid(), err)
		}
		err = lc.verifyCommitmentOn(p, c, keyRing, byTxid[p.txid()])
		if err != nil {
			return nil, err
		}
		commits[i] = c
	}

	return commits, nil
}

// requireAllSigned refuses to go on unless we hold the peer's signatures for
// our commitment at height on every pending splice's funding: revoking the
// previous state without them would leave a splice that confirms without a
// commitment we can publish.
func (lc *LightningChannel) requireAllSigned(height uint64) error {
	for _, p := range lc.pendingFundings {
		c, ok := p.local[height]
		if !ok || c.sig == nil {
			return fmt.Errorf("no signed commitment at height %d "+
				"on splice %v", height, p.txid())
		}
	}

	return nil
}

// spliceLocalWrites returns the writes recording our commitments at height
// on the pending splices' fundings.
func (lc *LightningChannel) spliceLocalWrites(
	height uint64) []channeldb.SpliceCommitWrite {

	writes := make([]channeldb.SpliceCommitWrite, len(lc.pendingFundings))
	for i, p := range lc.pendingFundings {
		writes[i] = channeldb.SpliceCommitWrite{
			Txid:        p.txid(),
			LocalCommit: p.local[height].toDiskCommit(lntypes.Local),
		}
	}

	return writes
}

// spliceRevocationWrites returns the writes moving the peer's commitments at
// height on the pending splices' fundings into their revocation logs, with
// their own output indexes.
func (lc *LightningChannel) spliceRevocationWrites(preimage *chainhash.Hash,
	height uint64) ([]channeldb.SpliceCommitWrite, error) {

	writes := make([]channeldb.SpliceCommitWrite, len(lc.pendingFundings))
	for i, p := range lc.pendingFundings {
		c, ok := p.remote[height]
		if !ok {
			return nil, fmt.Errorf("no commitment at height %d on "+
				"splice %v to revoke", height, p.txid())
		}
		ours, theirs, err := findOutputIndexesIn(
			preimage, lc.channelState,
			*c.toDiskCommit(lntypes.Remote), lc.leafStore,
		)
		if err != nil {
			return nil, err
		}
		writes[i] = channeldb.SpliceCommitWrite{
			Txid:             p.txid(),
			OurOutputIndex:   ours,
			TheirOutputIndex: theirs,
		}
	}

	for _, r := range lc.retiredFundings {
		c, ok := r.remote[height]
		if !ok {
			continue
		}
		ours, theirs, err := findOutputIndexesIn(
			preimage, lc.channelState, *c, lc.leafStore,
		)
		if err != nil {
			return nil, err
		}
		writes = append(writes, channeldb.SpliceCommitWrite{
			Txid:             r.txid,
			OurOutputIndex:   ours,
			TheirOutputIndex: theirs,
		})
	}

	return writes, nil
}

// pruneFundingCommits forgets the pending splices' commitments below the
// tails of both chains.
func (lc *LightningChannel) pruneFundingCommits() {
	localTail := lc.commitChains.Local.tail().height
	remoteTail := lc.commitChains.Remote.tail().height
	for _, p := range lc.pendingFundings {
		for h := range p.local {
			if h < localTail {
				delete(p.local, h)
			}
		}
		for h := range p.remote {
			if h < remoteTail {
				delete(p.remote, h)
			}
		}
	}

	kept := lc.retiredFundings[:0]
	for _, r := range lc.retiredFundings {
		for h := range r.remote {
			if h < remoteTail {
				delete(r.remote, h)
			}
		}
		if len(r.remote) > 0 {
			kept = append(kept, r)
		}
	}
	lc.retiredFundings = kept
}

// restorePendingFundings loads the channel's pending splices after a
// restart. Their commitments are rebuilt from the restored chains, checked
// against the stored ones, and given back their signatures.
func (lc *LightningChannel) restorePendingFundings() error {
	if lc.channelState.Db == nil {
		return nil
	}
	records, err := lc.channelState.SpliceRecords()
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Retired {
			retired := &retiredFunding{
				txid: r.Txid(),
				remote: make(
					map[uint64]*channeldb.ChannelCommitment,
				),
			}
			for _, c := range []*channeldb.ChannelCommitment{
				r.RemoteTail, r.RemoteTip,
			} {
				if c != nil {
					retired.remote[c.CommitHeight] = c
				}
			}
			lc.retiredFundings = append(lc.retiredFundings, retired)

			continue
		}

		p, err := lc.newPendingFunding(PendingFunding{
			OutPoint: r.OutPoint, Capacity: r.Capacity,
			LocalKey: r.LocalKey, RemoteKey: r.RemoteKey,
			LocalDelta: r.LocalDelta, RemoteDelta: r.RemoteDelta,
		})
		if err != nil {
			return err
		}

		restore := func(stored *channeldb.ChannelCommitment,
			base *commitment, keyRing *CommitmentKeyRing,
			into map[uint64]*commitment) error {

			if stored == nil {
				return nil
			}
			if stored.CommitHeight != base.height {
				return fmt.Errorf("splice %v commitment at "+
					"height %d, chain at %d", p.txid(),
					stored.CommitHeight, base.height)
			}
			c, err := lc.rebuildCommitment(
				base, keyRing, p.commitFunding(),
			)
			if err != nil {
				return err
			}
			if c.txn.TxHash() != stored.CommitTx.TxHash() {
				return fmt.Errorf("splice %v commitment at "+
					"height %d doesn't rebuild", p.txid(),
					base.height)
			}
			c.sig = stored.CommitSig
			if err := attachHtlcSigs(c, stored); err != nil {
				return err
			}
			into[c.height] = c

			return nil
		}

		localTail := lc.commitChains.Local.tail()
		keyRing, err := lc.localKeyRing(localTail.height)
		if err != nil {
			return err
		}
		err = restore(r.LocalCommit, localTail, keyRing, p.local)
		if err != nil {
			return err
		}

		keyRing, err = lc.remoteKeyRing()
		if err != nil {
			return err
		}
		remoteTail := lc.commitChains.Remote.tail()
		err = restore(r.RemoteTail, remoteTail, keyRing, p.remote)
		if err != nil {
			return err
		}

		remoteTip := lc.commitChains.Remote.tip()
		switch {
		case remoteTip != remoteTail:
			if r.RemoteTip == nil {
				return fmt.Errorf("splice %v without the "+
					"peer's pending commitment", p.txid())
			}
			nextPoint, err := lc.channelState.
				RemoteNextRevocation.UnwrapOrErr(
				ErrNoNextRevocation,
			)
			if err != nil {
				return err
			}
			keyRing = DeriveCommitmentKeys(
				&nextPoint,
				lntypes.Remote, lc.channelState.ChanType,
				&lc.channelState.LocalChanCfg,
				&lc.channelState.RemoteChanCfg,
			)
			err = restore(r.RemoteTip, remoteTip, keyRing, p.remote)
			if err != nil {
				return err
			}

		case r.RemoteTip != nil:
			return fmt.Errorf("splice %v has a pending commitment "+
				"the channel doesn't", p.txid())
		}

		lc.pendingFundings = append(lc.pendingFundings, p)
	}

	return nil
}

// checkPendingFundingBalances applies validateCommitmentSanity's fee and
// reserve checks on each pending splice's funding, where both balances move
// by the splice's contributions. ours and theirs are the balances before the
// commitment fee; the initial balances are the commitment chain tip's.
// ErrSpliceReserve is ErrBelowChanReserve on a pending splice's funding
// only, the reserve or the commitment fee: a peer checking what it can
// afford against the current funding, as the Core Lightning port does for a
// fee update, can send a commitment that keeps the reserve there and not on
// the splice's, where its balance is lower. Refused all the same; the link disconnects rather than force
// closing, as nothing invalid was accepted.
var ErrSpliceReserve = fmt.Errorf("%w on a pending splice",
	ErrBelowChanReserve)

func (lc *LightningChannel) checkPendingFundingBalances(ours,
	theirs, ourInitial, theirInitial lnwire.MilliSatoshi,
	commitWeight lntypes.WeightUnit, feePerKw chainfee.SatPerKWeight,
	buffer BufferType) error {

	ourReserve := lnwire.NewMSatFromSatoshis(
		lc.channelState.LocalChanCfg.ChanReserve,
	)
	theirReserve := lnwire.NewMSatFromSatoshis(
		lc.channelState.RemoteChanCfg.ChanReserve,
	)

	for _, p := range lc.pendingFundings {
		f := p.commitFunding()
		ob, tb, err := f.applyDeltas(ours, theirs)
		if err != nil {
			return fmt.Errorf("%w: splice %v: %w", ErrSpliceReserve,
				p.txid(), err)
		}
		oi, ti, err := f.applyDeltas(ourInitial, theirInitial)
		if err != nil {
			return fmt.Errorf("%w: splice %v: %w", ErrSpliceReserve,
				p.txid(), err)
		}

		// The opener pays the fee, with the buffer only when that is
		// us, as on the current funding.
		if lc.channelState.IsInitiator {
			ob, _, _, err = lc.applyCommitFee(
				ob, commitWeight, feePerKw, buffer,
			)
		} else {
			tb, _, _, err = lc.applyCommitFee(
				tb, commitWeight, feePerKw, NoBuffer,
			)
		}
		if err != nil {
			return fmt.Errorf("%w: splice %v: %w", ErrSpliceReserve,
				p.txid(), err)
		}

		switch {
		case ob < oi && ob < ourReserve:
			return fmt.Errorf("%w: our balance below chan reserve "+
				"on splice %v", ErrSpliceReserve, p.txid())

		case tb < ti && tb < theirReserve:
			return fmt.Errorf("%w: their balance below chan "+
				"reserve on splice %v", ErrSpliceReserve,
				p.txid())
		}
	}

	return nil
}

// minAvailableOnPendingFundings lowers available, our balance available for
// a new HTLC, to the least of it on every pending splice's funding, on both
// commitments.
func (lc *LightningChannel) minAvailableOnPendingFundings(view *HtlcView,
	buffer BufferType, available lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	for _, p := range lc.pendingFundings {
		for _, whose := range []lntypes.ChannelParty{
			lntypes.Local, lntypes.Remote,
		} {
			b, _ := lc.availableCommitmentBalanceOn(
				view, whose, buffer, p.commitFunding(),
			)
			if b < available {
				available = b
			}
		}
	}

	return available
}

// LockPendingFunding completes a splice once both sides have sent and
// received splice_locked for it: its funding output becomes the channel's,
// the commitments on it replace the current ones, and every other pending
// splice is dropped (they spend the same output and can't confirm). The
// database moves in one transaction, before anything is signed for the new
// funding alone. scid is the splice's short channel id.
func (lc *LightningChannel) LockPendingFunding(txid chainhash.Hash,
	scid lnwire.ShortChannelID) error {

	lc.Lock()
	defer lc.Unlock()

	p, err := lc.pendingFunding(txid)
	if err != nil {
		return err
	}

	// Every commitment in both chains must exist on the new funding.
	replacement := func(chain *commitmentChain,
		on map[uint64]*commitment) (map[*fn.Node[*commitment]]*commitment,
		error) {

		swap := make(map[*fn.Node[*commitment]]*commitment)
		for e := chain.commitments.Front(); e != nil; e = e.Next() {
			c, ok := on[e.Value.height]
			if !ok {
				return nil, fmt.Errorf("no commitment at "+
					"height %d on splice %v",
					e.Value.height, txid)
			}
			swap[e] = c
		}

		return swap, nil
	}
	localSwap, err := replacement(lc.commitChains.Local, p.local)
	if err != nil {
		return err
	}
	remoteSwap, err := replacement(lc.commitChains.Remote, p.remote)
	if err != nil {
		return err
	}
	if c := p.local[lc.commitChains.Local.tail().height]; c.sig == nil {
		return fmt.Errorf("commitment on splice %v not signed", txid)
	}

	remoteTail := lc.commitChains.Remote.tail()
	lock := channeldb.SpliceLock{
		Funding: channeldb.SpliceFunding{
			OutPoint: p.OutPoint, Scid: scid,
		},
		Capacity:         p.Capacity,
		LocalMultiSigKey: p.LocalKey,
		RemoteMultiSigKey: keychain.KeyDescriptor{
			PubKey: p.RemoteKey,
		},
		LocalCommit: p.local[lc.commitChains.Local.tail().height].
			toDiskCommit(lntypes.Local),
		RemoteCommit: p.remote[remoteTail.height].toDiskCommit(
			lntypes.Remote,
		),
	}
	if lc.commitChains.Remote.hasUnackedCommitment() {
		lock.RemoteTip = p.remote[lc.commitChains.Remote.tip().height].
			toDiskCommit(lntypes.Remote)
	}
	previous := lc.channelState.CurrentFundingOutpoint().Hash
	retired := &retiredFunding{
		txid:   previous,
		remote: make(map[uint64]*channeldb.ChannelCommitment),
	}
	for e := lc.commitChains.Remote.commitments.Front(); e != nil; e = e.Next() {
		retired.remote[e.Value.height] = e.Value.toDiskCommit(
			lntypes.Remote,
		)
	}

	if err := lc.channelState.LockSplice(lock); err != nil {
		return err
	}
	lc.retiredFundings = append(lc.retiredFundings, retired)

	for e, c := range localSwap {
		e.Value = c
	}
	for e, c := range remoteSwap {
		e.Value = c
	}
	if lc.obsoleteFundings == nil {
		lc.obsoleteFundings = make(map[chainhash.Hash]bool)
	}
	lc.obsoleteFundings[previous] = true
	// The other transactions of the splice the lock retired, as their
	// records were: their commitments are logged as they are revoked.
	// The lock is written: unable to read them, the switch in memory is
	// finished all the same (left half done, the next commitments named
	// the new funding twice and were signed with the old one's key), and
	// they are logged from the next start, which reads them again.
	records, err := lc.channelState.SpliceRecords()
	if err != nil {
		lc.log.Errorf("Unable to read splices after locking %v, their "+
			"commitments logged from the next start: %v", txid,
			err)
	}
	retiredRecords := make(map[chainhash.Hash]bool)
	for _, r := range records {
		if r.Retired {
			retiredRecords[r.Txid()] = true
		}
	}
	for _, other := range lc.pendingFundings {
		if other != p {
			lc.obsoleteFundings[other.txid()] = true
			if retiredRecords[other.txid()] {
				lc.retireFunding(other)
			}
		}
	}
	lc.pendingFundings = nil
	lc.Capacity = p.Capacity

	return lc.createSignDesc()
}

// SpliceRestoreError returns why the channel's pending splices couldn't be
// restored, if they couldn't: the channel must not be updated, only closed.
func (lc *LightningChannel) SpliceRestoreError() error {
	lc.RLock()
	defer lc.RUnlock()

	return lc.spliceRestoreErr
}

// IsObsoleteFunding reports whether txid names a funding a locked splice
// replaced: the channel's previous funding, or another splice that was
// pending when one locked. A batch from the peer may still carry a
// commitment_signed for it, which is ignored.
func (lc *LightningChannel) IsObsoleteFunding(txid chainhash.Hash) bool {
	lc.RLock()
	defer lc.RUnlock()

	return lc.obsoleteFundings[txid]
}

// loadObsoleteFundings marks the fundings the channel moved away from as
// obsolete, after a restart.
func (lc *LightningChannel) loadObsoleteFundings() error {
	if lc.channelState.Db == nil {
		return nil
	}
	prev, err := lc.channelState.PreviousFundings()
	if err != nil {
		return err
	}
	if lc.obsoleteFundings == nil {
		lc.obsoleteFundings = make(map[chainhash.Hash]bool)
	}
	for _, f := range prev {
		lc.obsoleteFundings[f.OutPoint.Hash] = true
	}

	// The splice transactions dropped once their commitments were
	// exchanged: a batch the peer signed before learning it, or
	// retransmits after a restart, may still name one.
	dropped, err := lc.channelState.DroppedSplices()
	if err != nil {
		return err
	}
	for _, txid := range dropped {
		lc.obsoleteFundings[txid] = true
	}

	return nil
}

// retransmittedCommitSigs returns the messages that carried the pending
// commitment in diff: its commitment_signed, or the batch it went out in
// while splices were pending, rebuilt from the splices' records.
func (lc *LightningChannel) retransmittedCommitSigs(
	diff *channeldb.CommitDiff) ([]lnwire.Message, error) {

	records, err := lc.channelState.SpliceRecords()
	if err != nil {
		return nil, err
	}

	var members []lnwire.Message
	for _, r := range records {
		if r.RemoteTipSig == nil || r.RemoteTip == nil ||
			r.RemoteTip.CommitHeight != diff.Commitment.CommitHeight {

			continue
		}
		members = append(members, r.RemoteTipSig)
	}
	if len(members) == 0 {
		return []lnwire.Message{diff.CommitSig}, nil
	}

	msgs := []lnwire.Message{
		&lnwire.StartBatch{
			ChannelID: diff.CommitSig.ChanID,
			BatchSize: uint16(1 + len(members)),
			BatchMessageType: fn.Some(
				lnwire.MessageType(lnwire.MsgCommitSig),
			),
		},
		diff.CommitSig,
	}

	return append(msgs, members...), nil
}

// SpliceCommitReceived reports whether the peer's commitment_signed for a
// pending splice's own commitment has been received: our commitment on its
// funding at the current height is signed. Any later commitment_signed
// naming that funding belongs to a batch, or follows the splice's lock.
func (lc *LightningChannel) SpliceCommitReceived(txid chainhash.Hash) bool {
	lc.RLock()
	defer lc.RUnlock()

	p, err := lc.pendingFunding(txid)
	if err != nil {
		return false
	}
	local, ok := p.local[lc.commitChains.Local.tail().height]

	return ok && local.sig != nil
}

// requireExchanged refuses a commitment step until every pending splice's
// own commitments have been exchanged both ways at the current heights.
func (lc *LightningChannel) requireExchanged() error {
	localTail := lc.commitChains.Local.tail().height
	remoteTail := lc.commitChains.Remote.tail().height
	for _, p := range lc.pendingFundings {
		local, ok := p.local[localTail]
		if !ok || local.sig == nil {
			return fmt.Errorf("%w: splice %v", ErrSpliceNotExchanged,
				p.txid())
		}
		if _, ok := p.remote[remoteTail]; !ok {
			return fmt.Errorf("%w: splice %v", ErrSpliceNotExchanged,
				p.txid())
		}
	}

	return nil
}

// retireFunding keeps the peer's commitments on a pending splice that is
// dropped, from the tail of its chain on, as its record keeps them: each is
// logged when the peer revokes it, since a reorganisation could still
// confirm the splice.
func (lc *LightningChannel) retireFunding(p *pendingFunding) {
	retired := &retiredFunding{
		txid:   p.txid(),
		remote: make(map[uint64]*channeldb.ChannelCommitment),
	}
	tail := lc.commitChains.Remote.tail().height
	for h, c := range p.remote {
		if h >= tail {
			retired.remote[h] = c.toDiskCommit(lntypes.Remote)
		}
	}
	if len(retired.remote) > 0 {
		lc.retiredFundings = append(lc.retiredFundings, retired)
	}
}

// RemovePendingFunding drops a pending splice that will never lock: one
// aborted before our tx_signatures, or one that can no longer confirm. A
// commitment_signed the peer still sends for it in a batch is ignored.
func (lc *LightningChannel) RemovePendingFunding(txid chainhash.Hash) error {
	lc.Lock()
	defer lc.Unlock()

	p, err := lc.pendingFunding(txid)
	if err != nil {
		return err
	}
	retired, err := lc.channelState.RetireSpliceRecord(txid)
	if err != nil {
		return err
	}
	if retired {
		lc.retireFunding(p)
	}

	kept := lc.pendingFundings[:0]
	for _, p := range lc.pendingFundings {
		if p.txid() != txid {
			kept = append(kept, p)
		}
	}
	lc.pendingFundings = kept

	if lc.obsoleteFundings == nil {
		lc.obsoleteFundings = make(map[chainhash.Hash]bool)
	}
	lc.obsoleteFundings[txid] = true

	return nil
}

// attachHtlcSigs gives a rebuilt commitment of ours back the peer's HTLC
// signatures from its stored copy: without them its second-level
// transactions can't be signed, and a force close from it fails.
func attachHtlcSigs(c *commitment, stored *channeldb.ChannelCommitment) error {
	type key struct {
		incoming bool
		index    uint64
	}
	sigs := make(map[key][]byte)
	for _, h := range stored.Htlcs {
		if len(h.Signature) > 0 {
			sigs[key{h.Incoming, h.HtlcIndex}] = h.Signature
		}
	}

	attach := func(htlcs []paymentDescriptor, incoming bool) error {
		for i := range htlcs {
			b, ok := sigs[key{incoming, htlcs[i].HtlcIndex}]
			if !ok {
				continue
			}
			sig, err := ecdsa.ParseDERSignature(b)
			if err != nil {
				return fmt.Errorf("htlc %d signature: %w",
					htlcs[i].HtlcIndex, err)
			}
			htlcs[i].sig = sig
		}

		return nil
	}
	if err := attach(c.outgoingHTLCs, false); err != nil {
		return err
	}

	return attach(c.incomingHTLCs, true)
}

// SpliceCommitSig returns our commitment_signed for a pending splice's own
// transaction, as first sent, to send again when the peer asks for it on
// reconnecting.
func (lc *LightningChannel) SpliceCommitSig(
	txid chainhash.Hash) (*lnwire.CommitSig, error) {

	lc.RLock()
	defer lc.RUnlock()

	if _, err := lc.pendingFunding(txid); err != nil {
		return nil, err
	}
	records, err := lc.channelState.SpliceRecords()
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		if r.Txid() == txid && r.SpliceSig != nil {
			return r.SpliceSig, nil
		}
	}

	return nil, fmt.Errorf("no commitment_signed sent for splice %v",
		txid)
}

// spliceAnchorResolutions returns the anchor resolutions of the commitments
// on each pending splice's funding output: the same commitments as the
// channel's, so the same keys, but anchored with the splice's funding key.
// pendingRemote is nil without a remote pending commitment.
//
// NOTE: must be called with lc's lock held.
func (lc *LightningChannel) spliceAnchorResolutions(local, remote,
	pendingRemote *CommitmentKeyRing) ([]*AnchorResolutions, error) {

	if !lc.channelState.ChanType.HasAnchors() {
		return nil, nil
	}

	var all []*AnchorResolutions
	for _, p := range lc.pendingFundings {
		view := lc.channelState.Copy()
		view.LocalChanCfg.MultiSigKey = p.LocalKey
		view.RemoteChanCfg.MultiSigKey = keychain.KeyDescriptor{
			PubKey: p.RemoteKey,
		}

		var (
			res AnchorResolutions
			err error
		)
		localHeight := lc.commitChains.Local.tail().height
		if c, ok := p.local[localHeight]; ok {
			res.Local, err = NewAnchorResolution(
				view, c.txn, local, lntypes.Local,
			)
			if err != nil {
				return nil, err
			}
		}
		remoteHeight := lc.commitChains.Remote.tail().height
		if c, ok := p.remote[remoteHeight]; ok {
			res.Remote, err = NewAnchorResolution(
				view, c.txn, remote, lntypes.Remote,
			)
			if err != nil {
				return nil, err
			}
		}
		tipHeight := lc.commitChains.Remote.tip().height
		c, ok := p.remote[tipHeight]
		if pendingRemote != nil && tipHeight != remoteHeight && ok {
			res.RemotePending, err = NewAnchorResolution(
				view, c.txn, pendingRemote, lntypes.Remote,
			)
			if err != nil {
				return nil, err
			}
		}

		all = append(all, &res)
	}

	return all, nil
}

// ApplySpliceReserves sets a spliced channel's reserves, both sides', to
// those after its splices (channeldb.OpenChannel.SpliceReserve), as the Core
// Lightning port's channel process reads them when it starts: at each
// connection, and after each tx_abort exchange. Not at a lock: the process
// running then keeps what it started with, and so does this channel until
// one of those. A channel never spliced keeps those of its opening.
func (lc *LightningChannel) ApplySpliceReserves() {
	reserve, ok := lc.channelState.SpliceReserve()
	if !ok {
		return
	}

	lc.Lock()
	defer lc.Unlock()

	lc.channelState.SetReserves(reserve)
}
