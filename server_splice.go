package lnd

import (
	"context"
	"errors"
	"fmt"
	basewallet "github.com/btcsuite/btcwallet/wallet"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/splice"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing/route"
)

// spliceChain is the chain backend as the splice machines use it.
type spliceChain struct {
	notifier chainntnfs.ChainNotifier
	chainIO  lnwallet.BlockChainIO
	wallet   lnwallet.WalletController

	// processed is the tip this node has processed.
	processed interface{ BestHeight() (uint32, error) }
}

// staleTip is how old the backend's tip may be before the backend itself
// counts as catching up with the network. Far longer than this chain's
// gaps between blocks, which are often over the two hours the wallet's own
// sync check allows.
const staleTip = 24 * time.Hour

// RegisterConfirmationsNtfn implements splice.Chain.
func (c *spliceChain) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	return c.notifier.RegisterConfirmationsNtfn(
		txid, pkScript, numConfs, heightHint, opts...,
	)
}

// RegisterSpendNtfn implements splice.Chain.
func (c *spliceChain) RegisterSpendNtfn(op *wire.OutPoint, pkScript []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return c.notifier.RegisterSpendNtfn(op, pkScript, heightHint)
}

// RegisterBlockEpochNtfn implements splice.Chain.
func (c *spliceChain) RegisterBlockEpochNtfn(
	best *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	return c.notifier.RegisterBlockEpochNtfn(best)
}

// BestHeight implements splice.Chain.
func (c *spliceChain) BestHeight() (uint32, error) {
	_, height, err := c.chainIO.GetBestBlock()
	if err != nil {
		return 0, err
	}

	return uint32(height), nil
}

// utxoDetailer is a wallet that can ask the backend about an unspent output
// in detail.
type utxoDetailer interface {
	GetUtxoDetails(op *wire.OutPoint) (*wire.TxOut, uint32, bool, error)
}

// baseWallet returns the wallet a remote signer's wraps (rpcwallet's
// RPCKeyRing), whose chain methods the splice checks use; any other as it
// is. Without it, a node with a remote signer refused every peer coin and
// read every splice as unconfirmed.
func baseWallet(w lnwallet.WalletController) lnwallet.WalletController {
	type wrapper interface {
		UnwrapWalletController() lnwallet.WalletController
	}
	if u, ok := w.(wrapper); ok {
		return u.UnwrapWalletController()
	}

	return w
}

// confirmedOutputTimeout bounds the backend's answer about a peer's coin:
// the channel is quiescent while the peer's splice is negotiated, and a
// stalled backend would hold it so.
var confirmedOutputTimeout = 10 * time.Second

// ConfirmedOutput implements splice.Chain. A backend that can't tell, or
// doesn't within confirmedOutputTimeout, refuses the peer's coin.
func (c *spliceChain) ConfirmedOutput(op wire.OutPoint) (*wire.TxOut, uint32,
	bool, error) {

	d, ok := baseWallet(c.wallet).(utxoDetailer)
	if !ok {
		return nil, 0, false, errors.New("backend can't check " +
			"unspent outputs")
	}

	type answer struct {
		out      *wire.TxOut
		confs    uint32
		coinbase bool
		err      error
	}
	done := make(chan answer, 1)
	go func() {
		out, confs, coinbase, err := d.GetUtxoDetails(&op)
		done <- answer{out, confs, coinbase, err}
	}()

	select {
	case a := <-done:
		return a.out, a.confs, a.coinbase, a.err

	case <-time.After(confirmedOutputTimeout):
		return nil, 0, false, fmt.Errorf("backend didn't answer about "+
			"%v within %v", op, confirmedOutputTimeout)
	}
}

// Synced implements splice.Chain: whether this node has processed the
// backend's tip, and the backend isn't itself still catching up.
func (c *spliceChain) Synced() bool {
	_, backend, err := c.chainIO.GetBestBlock()
	if err != nil {
		return false
	}
	if c.processed != nil {
		ours, err := c.processed.BestHeight()
		if err != nil || int64(ours) < int64(backend) {
			return false
		}
	}

	// The wallet has done its initial sync and reached the backend's
	// tip, and that tip isn't so old that the backend is the one behind.
	// Its own IsSynced also wants a tip under two hours old, which a slow
	// day on this chain fails.
	if w, ok := baseWallet(c.wallet).(syncedWallet); ok {
		base := w.InternalWallet()
		synced := base.SyncedTo()

		return base.ChainSynced() && int64(synced.Height) >= int64(backend) &&
			time.Since(synced.Timestamp) < staleTip
	}

	synced, _, err := c.wallet.IsSynced()

	return err == nil && synced
}

// syncedWallet is a wallet that shows its own sync state.
type syncedWallet interface {
	InternalWallet() basewallet.Interface
}

// spliceLockDepth is the confirmations at which a splice of a channel locks:
// at least 6, at least the channel's own, and for coins the peer put in,
// which it could take back by reorganising the chain, up to holdConfs (the
// depth at which the hold on its coins ends): six plus a share of the rest
// for each 1/30 of a non-wumbo channel's worth the peer put in.
func spliceLockDepth(numConfsRequired uint16, peer btcutil.Amount,
	holdConfs uint32) uint32 {

	depth := max(uint32(6), uint32(numConfsRequired))
	if peer <= 0 || holdConfs <= 6 {
		return depth
	}

	const scale = btcutil.Amount(16_777_215)
	maxExtra := int64(holdConfs - 6)
	extra := maxExtra
	if peer < scale {
		extra = (int64(peer)*maxExtra + int64(scale) - 1) /
			int64(scale)
	}

	return max(depth, 6+uint32(extra))
}

// newSpliceMachine fills the node's part of a channel's splice machine's
// configuration.
func (s *server) newSpliceMachine(base splice.Config,
	peer *btcec.PublicKey) *splice.Machine {

	cfg := base
	copy(cfg.OurNodeID[:], s.identityECDH.PubKey().SerializeCompressed())
	copy(cfg.TheirNodeID[:], peer.SerializeCompressed())

	wallet := s.cc.Wallet
	cfg.Chain = &spliceChain{
		notifier:  s.cc.ChainNotifier,
		chainIO:   s.cc.ChainIO,
		wallet:    wallet.WalletController,
		processed: s.cc.BestBlockTracker,
	}
	cfg.Wallet = wallet.WalletController
	cfg.CoinSource = wallet.WalletController
	cfg.WithCoinSelectLock = wallet.WithCoinSelectLock
	cfg.ChangeScript = func() ([]byte, error) {
		addr, err := wallet.NewAddress(
			lnwallet.WitnessPubKey, true,
			lnwallet.DefaultAccountName,
		)
		if err != nil {
			return nil, err
		}

		return txscript.PayToAddrScript(addr)
	}
	cfg.CheckMempool = wallet.WalletController.CheckMempoolAcceptance
	cfg.Reserve = func() (btcutil.Amount, error) {
		n, err := wallet.CurrentNumAnchorChans()
		if err != nil {
			return 0, err
		}

		return wallet.RequiredReserve(uint32(n)), nil
	}
	cfg.IsOurOutpoint = func(op wire.OutPoint) bool {
		if _, err := wallet.FetchOutpointInfo(&op); err == nil {
			return true
		}
		_, err := s.chanStateDB.FetchChannel(op)

		return err == nil
	}
	cfg.Disabled = base.Disabled || !s.cfg.ProtocolOptions.Splice()
	cfg.Limits = s.spliceLimits
	cfg.KeyRing = s.cc.KeyRing
	cfg.Signer = s.cc.Signer
	cfg.Estimator = s.cc.FeeEstimator
	cfg.DustLimit = func(pkScript []byte) btcutil.Amount {
		return lnwallet.DustLimitForSize(len(pkScript))
	}

	// The lock depth: at least 6, at least the channel's own, and more for
	// a large amount the peer put in (it could double-spend it in a
	// reorg).
	numConfs := base.Channel.State().NumConfsRequired
	cfg.Policy = splice.Policy{
		MaxChannelSize: btcutil.Amount(s.cfg.MaxChanSize),
		LockDepth: func(peer btcutil.Amount) uint32 {
			return spliceLockDepth(
				numConfs, peer,
				s.cfg.ProtocolOptions.SplicePeerInputConfs,
			)
		},
		PeerInputConfs:   s.cfg.ProtocolOptions.SplicePeerInputConfs,
		MaxValueInFlight: requiredRemoteMaxValue,
	}

	ch := base.Channel
	cfg.NotifyEvent = func(kind string, txid chainhash.Hash,
		reason string, fromPeer bool) {

		state := ch.State()
		s.spliceStep(kind, txid, state.FundingOutpoint)
		s.channelNotifier.NotifySpliceEvent(channelnotifier.SpliceEvent{
			ChannelPoint:         state.CurrentFundingOutpoint(),
			OriginalChannelPoint: state.FundingOutpoint,
			Kind:                 kind,
			Txid:                 txid,
			Reason:               reason,
			ReasonFromPeer:       fromPeer,
		})
	}

	cfg.AnnouncementPending = func() bool {
		return s.spliceAnnouncementPending(ch.State())
	}
	cfg.ResendAnnouncement = func() {
		state := ch.State()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			err := s.resendSpliceAnnouncement(state)
			if err != nil {
				srvrLog.Errorf("ChannelPoint(%v): unable to "+
					"resend its announcement signatures: %v",
					state.FundingOutpoint, err)
			}
		}()
	}

	return splice.NewMachine(cfg)
}

// spliceAnnouncementPending reports whether a public channel a splice moved
// lacks the full proof of its announcement on its current funding output.
func (s *server) spliceAnnouncementPending(ch *channeldb.OpenChannel) bool {
	current := ch.CurrentFundingOutpoint()
	if current == ch.FundingOutpoint ||
		ch.ChannelFlags&lnwire.FFAnnounceChannel == 0 {

		return false
	}

	info, _, _, err := s.graphDB.FetchChannelEdgesByOutpoint(
		context.TODO(), &current,
	)
	if errors.Is(err, graphdb.ErrEdgeNotFound) {
		return true
	}

	return err == nil && info.AuthProof == nil
}

// resendSpliceAnnouncement sends our half of a spliced public channel's
// announcement on its current funding output again, the edge moved there
// first if it isn't.
func (s *server) resendSpliceAnnouncement(ch *channeldb.OpenChannel) error {
	if ch.CurrentFundingOutpoint() == ch.FundingOutpoint ||
		ch.ChannelFlags&lnwire.FFAnnounceChannel == 0 {

		return nil
	}

	// Moving announces while the proof is missing.
	pending := s.spliceAnnouncementPending(ch)
	if err := s.moveSpliceEdge(ch); err != nil {
		return err
	}
	if pending {
		return nil
	}

	return s.fundingMgr.AnnounceSplice(ch, nil, false)
}

// releaseForgottenSpliceLeases releases the wallet leases of splices no
// channel remembers: a crash between leasing coins and recording the splice
// leaves them locked until they expire otherwise.
func (s *server) releaseForgottenSpliceLeases() error {
	leases, err := s.cc.Wallet.ListLeasedOutputs()
	if err != nil {
		return err
	}

	channels, err := s.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		return err
	}
	kept := make(map[wire.OutPoint]bool)
	for _, ch := range channels {
		records, err := ch.SpliceRecords()
		if err != nil {
			return err
		}
		for _, r := range records {
			if r.Negotiation == nil {
				continue
			}
			for _, op := range r.Negotiation.OurInputs {
				kept[op] = true
			}
		}
	}

	for _, l := range leases {
		if l.LockedOutput == nil || l.LockID != splice.LockID ||
			kept[l.Outpoint] {

			continue
		}
		err := s.cc.Wallet.ReleaseOutput(splice.LockID, l.Outpoint)
		if err != nil {
			srvrLog.Errorf("Unable to release splice lease of %v: %v",
				l.Outpoint, err)
		}
	}

	return nil
}

// pendingSplices reports whether any open channel has a splice to finish:
// splicing stays advertised then, even turned off, or the peer would fail
// the channel at the next batch or reestablish.
func pendingSplices(db *channeldb.ChannelStateDB) bool {
	channels, err := db.FetchAllOpenChannels()
	if err != nil {
		srvrLog.Errorf("Unable to read channels for pending splices: "+
			"%v", err)

		return false
	}
	for _, ch := range channels {
		records, err := ch.SpliceRecords()
		if err != nil {
			continue
		}
		for _, r := range records {
			if !r.Retired {
				return true
			}
		}
	}

	return false
}

// spliceGraph is what the node keeps in memory of its channels' splices for
// the graph: the splice transactions of ours a block may hold, whose spend of
// a funding output moves a channel rather than closing it, and the short
// channel id each id a channel has had now maps to.
type spliceGraph struct {
	db *channeldb.ChannelStateDB

	mu     sync.Mutex
	loaded bool
	txids  map[chainhash.Hash]wire.OutPoint
	latest map[lnwire.ShortChannelID]lnwire.ShortChannelID
}

// newSpliceGraph returns a spliceGraph read from db when first used.
func newSpliceGraph(db *channeldb.ChannelStateDB) *spliceGraph {
	return &spliceGraph{
		db:     db,
		txids:  make(map[chainhash.Hash]wire.OutPoint),
		latest: make(map[lnwire.ShortChannelID]lnwire.ShortChannelID),
	}
}

// load reads the open channels' splices, once.
//
// NOTE: must be called with mu held.
func (g *spliceGraph) load() {
	if g.loaded {
		return
	}
	channels, err := g.db.FetchAllOpenChannels()
	if err != nil {
		srvrLog.Errorf("Unable to read channels for their splices: %v",
			err)
		return
	}
	g.loaded = true

	for _, ch := range channels {
		records, err := ch.SpliceRecords()
		if err != nil {
			srvrLog.Errorf("ChannelPoint(%v): unable to read "+
				"splices: %v", ch.FundingOutpoint, err)
		}
		for _, r := range records {
			if !r.Retired {
				g.txids[r.Txid()] = ch.CurrentFundingOutpoint()
			}
		}
		g.lockedLocked(ch)
	}
}

// ownSplice returns the funding output a pending splice of one of our
// channels spends.
func (g *spliceGraph) ownSplice(txid chainhash.Hash) (wire.OutPoint, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.load()
	funding, ok := g.txids[txid]

	return funding, ok
}

// pending records a splice of ours that may confirm, spending funding.
func (g *spliceGraph) pending(txid chainhash.Hash, funding wire.OutPoint) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.load()
	g.txids[txid] = funding
}

// gone forgets a splice of ours that won't confirm, or has locked.
func (g *spliceGraph) gone(txid chainhash.Hash) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.load()
	delete(g.txids, txid)
}

// locked records a channel's short channel ids after a splice locked.
func (g *spliceGraph) locked(ch *channeldb.OpenChannel) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.load()
	g.lockedLocked(ch)
}

// lockedLocked is locked with mu held.
func (g *spliceGraph) lockedLocked(ch *channeldb.OpenChannel) {
	current := ch.CurrentShortChanID()
	if current == ch.ShortChanID() {
		return
	}
	scids, err := ch.SpliceScids()
	if err != nil {
		srvrLog.Errorf("ChannelPoint(%v): unable to read its splices' "+
			"short channel ids: %v", ch.FundingOutpoint, err)
	}
	g.latest[ch.ShortChanID()] = current
	for _, scid := range scids {
		g.latest[scid] = current
	}
}

// latestScid returns the short channel id a channel that went by scid goes
// by now, if a splice moved it.
func (g *spliceGraph) latestScid(
	scid lnwire.ShortChannelID) (lnwire.ShortChannelID, bool) {

	g.mu.Lock()
	defer g.mu.Unlock()

	g.load()
	latest, ok := g.latest[scid]

	return latest, ok && latest != scid
}

// spliceStep handles a splice step for the graph and the switch: a splice
// signed may confirm, one locked moves the channel, one gone won't.
func (s *server) spliceStep(kind string, txid chainhash.Hash,
	chanPoint wire.OutPoint) {

	switch kind {
	case "signed":
		ch, err := s.chanStateDB.FetchChannel(chanPoint)
		if err != nil {
			srvrLog.Errorf("ChannelPoint(%v): unable to read the "+
				"channel for its splice: %v", chanPoint, err)
			return
		}
		s.spliceGraph.pending(txid, ch.CurrentFundingOutpoint())

	case "aborted", "dead":
		s.spliceGraph.gone(txid)

	case "locked":
		s.spliceGraph.gone(txid)

		ch, err := s.chanStateDB.FetchChannel(chanPoint)
		if err != nil {
			srvrLog.Errorf("ChannelPoint(%v): unable to read the "+
				"channel after its splice locked: %v", chanPoint,
				err)
			return
		}
		s.spliceGraph.locked(ch)

		chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
		err = s.htlcSwitch.AddSpliceScids(
			chanID, ch.CurrentShortChanID(),
		)
		if err != nil {
			srvrLog.Debugf("ChannelPoint(%v): no link for the "+
				"splice's short channel id: %v", chanPoint, err)
		}

		// The edge waits on the gossiper, which mustn't hold up the
		// splice machine.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			if err := s.moveSpliceEdge(ch); err != nil {
				srvrLog.Errorf("ChannelPoint(%v): unable to "+
					"move its graph edge to the splice's "+
					"funding: %v", chanPoint, err)
			}
		}()
	}
}

// moveSpliceEdge puts a channel a splice moved in the graph on its current
// funding output.
func (s *server) moveSpliceEdge(ch *channeldb.OpenChannel) error {
	// One at a time: the lock, the peer asking for the announcement and
	// the reconciliation at start can overlap.
	s.spliceEdgeMu.Lock()
	defer s.spliceEdgeMu.Unlock()

	var ourKey route.Vertex
	copy(ourKey[:], s.identityECDH.PubKey().SerializeCompressed())

	return moveSpliceEdge(
		context.TODO(), s.v1Graph, ourKey, s.fundingMgr.AnnounceSplice,
		ch,
	)
}

// spliceEdgeGraph is the graph as moving a spliced channel's edge uses it.
type spliceEdgeGraph interface {
	FetchChannelEdgesByOutpoint(ctx context.Context, op *wire.OutPoint) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	DeleteChannelEdges(ctx context.Context, strictZombiePruning,
		markZombie bool, chanIDs ...uint64) error
}

// spliceAnnouncer adds a spliced channel's edge on its current funding
// output, and announces a public one there.
type spliceAnnouncer func(ch *channeldb.OpenChannel,
	ourPolicy *models.ChannelEdgePolicy, addEdge bool) error

// moveSpliceEdge puts a channel a splice moved in the graph on its current
// funding output: added there with our policy carried over, before the edges
// of the outputs it left go, and announced there if public. Repeatable, and
// safe to repeat after a crash at any point: a new edge without our policy
// gets it, edges left on old outputs go, and a public channel's proof is
// asked for again while missing.
func moveSpliceEdge(ctx context.Context, graph spliceEdgeGraph,
	ourKey route.Vertex, announce spliceAnnouncer,
	ch *channeldb.OpenChannel) error {

	current := ch.CurrentFundingOutpoint()
	if current == ch.FundingOutpoint {
		return nil
	}

	// ourPolicy returns our direction of an edge.
	ourPolicy := func(info *models.ChannelEdgeInfo, e1,
		e2 *models.ChannelEdgePolicy) *models.ChannelEdgePolicy {

		if info.NodeKey1Bytes == ourKey {
			return e1
		}

		return e2
	}

	info, e1, e2, err := graph.FetchChannelEdgesByOutpoint(ctx, &current)
	moved := err == nil
	switch {
	case moved:
	case !errors.Is(err, graphdb.ErrEdgeNotFound):
		return err
	}
	var policy *models.ChannelEdgePolicy
	if moved {
		policy = ourPolicy(info, e1, e2)
	}
	needsPolicy := policy == nil

	// The edges of the outputs the channel left, and our policy on them.
	previous, err := ch.PreviousFundings()
	if err != nil {
		return err
	}
	var stale []uint64
	for _, pf := range previous {
		info, e1, e2, err := graph.FetchChannelEdgesByOutpoint(
			ctx, &pf.OutPoint,
		)
		switch {
		case errors.Is(err, graphdb.ErrEdgeNotFound):
			continue

		case err != nil:
			return err
		}
		if old := ourPolicy(info, e1, e2); old != nil && policy == nil {
			policy = old
		}
		stale = append(stale, info.ChannelID)
	}

	public := ch.ChannelFlags&lnwire.FFAnnounceChannel != 0
	switch {
	// Not there, or there without our policy (a crash between the
	// announcement and the update): added, with the policy.
	case needsPolicy:
		if policy != nil {
			// A splice out can leave the old maximum above the
			// new capacity.
			carried := *policy
			capacity := lnwire.NewMSatFromSatoshis(ch.Capacity)
			if carried.MaxHTLC > capacity {
				carried.MaxHTLC = capacity
			}
			policy = &carried
		} else {
			srvrLog.Warnf("ChannelPoint(%v): no policy of ours on "+
				"any of its edges; announcing the splice's "+
				"with the default policy", ch.FundingOutpoint)
		}

		srvrLog.Infof("ChannelPoint(%v): moving its graph edge to %v, "+
			"short_chan_id=%v", ch.FundingOutpoint, current,
			ch.CurrentShortChanID())

		if err := announce(ch, policy, true); err != nil {
			return err
		}

	case public && info.AuthProof == nil:
		if err := announce(ch, nil, false); err != nil {
			return err
		}
	}

	if len(stale) == 0 {
		return nil
	}

	return graph.DeleteChannelEdges(ctx, false, false, stale...)
}

// reconcileSpliceEdges moves the graph edges of channels whose splice locked
// while the node stopped before it could.
func (s *server) reconcileSpliceEdges() {
	channels, err := s.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		srvrLog.Errorf("Unable to read channels for their splices: %v",
			err)
		return
	}
	for _, ch := range channels {
		if err := s.moveSpliceEdge(ch); err != nil {
			srvrLog.Errorf("ChannelPoint(%v): unable to move its "+
				"graph edge to the splice's funding: %v",
				ch.FundingOutpoint, err)
		}
	}
}

// spliceAwareGraph finds a channel's edge by the outpoint it was opened with
// once a splice has moved the edge to the channel's current funding output.
type spliceAwareGraph struct {
	netann.ChannelGraph

	db *channeldb.ChannelStateDB
}

// FetchChannelEdgesByOutpoint implements netann.ChannelGraph.
func (g *spliceAwareGraph) FetchChannelEdgesByOutpoint(ctx context.Context,
	op *wire.OutPoint) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	info, e1, e2, err := g.ChannelGraph.FetchChannelEdgesByOutpoint(ctx, op)
	if !errors.Is(err, graphdb.ErrEdgeNotFound) {
		return info, e1, e2, err
	}

	ch, chErr := g.db.FetchChannel(*op)
	if chErr != nil {
		return info, e1, e2, err
	}
	current := ch.CurrentFundingOutpoint()
	if current == *op {
		return info, e1, e2, err
	}

	return g.ChannelGraph.FetchChannelEdgesByOutpoint(ctx, &current)
}

// updateForwardingPolicies hands policies keyed by the funding output a
// channel's graph edge names to the switch, which knows a channel by the
// outpoint it was opened with.
func (s *server) updateForwardingPolicies(
	policies map[wire.OutPoint]models.ForwardingPolicy) {

	// One read of the channels, not one per policy.
	names := make(map[wire.OutPoint]wire.OutPoint)
	channels, err := s.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		srvrLog.Errorf("Unable to read channels for policy updates: %v",
			err)
	}
	for _, ch := range channels {
		names[ch.CurrentFundingOutpoint()] = ch.FundingOutpoint
	}

	named := make(map[wire.OutPoint]models.ForwardingPolicy, len(policies))
	for op, policy := range policies {
		if name, ok := names[op]; ok {
			op = name
		}
		named[op] = policy
	}

	s.htlcSwitch.UpdateForwardingPolicies(named)
}

// findBaseByAlias finds the scid a channel's graph edge goes by from an alias
// of it: the alias manager's base, unless a splice moved the edge to its own.
func (s *server) findBaseByAlias(
	alias lnwire.ShortChannelID) (lnwire.ShortChannelID, error) {

	base, err := s.aliasMgr.FindBaseSCID(alias)
	if err != nil {
		return base, err
	}
	if latest, ok := s.spliceGraph.latestScid(base); ok {
		return latest, nil
	}

	return base, nil
}

// channelName returns the outpoint a channel named by either of its outpoints
// was opened with, which the status manager and the switch know it by; op
// itself for anything else.
func (s *server) channelName(op wire.OutPoint) wire.OutPoint {
	ch, err := s.chanStateDB.FetchChannel(op)
	if err != nil {
		return op
	}

	return ch.FundingOutpoint
}

// notifyFundingSpentUnknown tells the channel event subscribers a channel's
// funding output was spent by a transaction this version doesn't recognise.
func (s *server) notifyFundingSpentUnknown(chanPoint wire.OutPoint,
	txid chainhash.Hash) {

	current := chanPoint
	if ch, err := s.chanStateDB.FetchChannel(chanPoint); err == nil {
		current = ch.CurrentFundingOutpoint()
	}

	s.channelNotifier.NotifySpliceEvent(channelnotifier.SpliceEvent{
		ChannelPoint:         current,
		OriginalChannelPoint: chanPoint,
		Kind:                 "funding_spent_unknown",
		Txid:                 txid,
	})
}

// recordOpeningInFlight records, for each open channel that has none yet, the
// max_htlc_value_in_flight_msat of its opening as its stored config holds
// them (bolts#20): before any subsystem loads the channels and applies the
// values a splice raised to the config in memory. A channel opened since
// this was added has them from its first save; a failure here leaves them to
// be recorded on first use (see channeldb.OpenChannel.SpliceInFlight).
func (s *server) recordOpeningInFlight() {
	if err := s.chanStateDB.RecordOpeningInFlight(); err != nil {
		srvrLog.Errorf("Recording the channels' opening "+
			"max_htlc_value_in_flight: %v", err)
	}
}
