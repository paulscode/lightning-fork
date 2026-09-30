package discovery

import (
	"context"
	"errors"
	"iter"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
)

// Releases up to v0.21.3-beta-blake2b.9 signed channel announcements under
// the chain_hash this network advertised until 2026-09-17. Those proofs are
// still in the graph, the fork's gossip still accepts them (see
// netann.SignedUnderLegacyChainHash), and they are served with the current
// chain_hash, which is the only one stored. Any other implementation, checking
// the signatures over the message as sent, finds them bad: Core Lightning
// answers each one with a warning.
//
// So they are kept for this node's own routing and not passed on, and a
// channel of this node's with such a proof is signed again: each side sends a
// fresh announcement_signatures, and once both halves are in, the new proof
// replaces the old one and the channel is announced afresh, readable by
// everyone. A third party's channel is upgraded the same way when its new
// announcement arrives.

// legacyScanBatch is how many channels the startup scan fetches at a time.
const legacyScanBatch = 500

// legacyScanRetry is how long a failed startup scan waits before trying again.
const legacyScanRetry = time.Minute

// legacyProofs is the set of channels whose stored proof holds only under the
// withdrawn chain_hash.
type legacyProofs struct {
	mu    sync.RWMutex
	scids map[uint64]struct{}

	// resigned holds the channels this node has signed again this run.
	resigned map[uint64]struct{}

	// scanned is set once the startup scan has covered the whole graph.
	// Until then a channel not in scids may still be one, and is checked
	// by its signatures instead.
	scanned atomic.Bool
}

func newLegacyProofs() *legacyProofs {
	return &legacyProofs{scids: make(map[uint64]struct{})}
}

func (l *legacyProofs) add(scid lnwire.ShortChannelID) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.scids[scid.ToUint64()] = struct{}{}
}

func (l *legacyProofs) remove(scid lnwire.ShortChannelID) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.scids, scid.ToUint64())
}

func (l *legacyProofs) has(scid lnwire.ShortChannelID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	_, ok := l.scids[scid.ToUint64()]

	return ok
}

func (l *legacyProofs) len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return len(l.scids)
}

// isLegacyAnn reports whether an announcement about to be sent carries a
// legacy proof. After the scan it is a lookup; before it, the signatures are
// checked, and a legacy one is remembered.
func (l *legacyProofs) isLegacyAnn(ann *lnwire.ChannelAnnouncement1) bool {
	if l.has(ann.ShortChannelID) {
		return true
	}
	if l.scanned.Load() {
		return false
	}

	if netann.SignedUnderLegacyChainHash(ann) {
		l.add(ann.ShortChannelID)

		return true
	}

	return false
}

// scan finds every channel in the graph whose proof holds only under the
// withdrawn chain_hash and hands it to confirm, which checks the stored proof
// again under the channel's lock (a concurrent replacement may have landed
// since the batch was read) and records it if it still holds. Each confirmed
// one of this node's own goes to ownChannel. It reads through series, which
// must be the unfiltered one.
//
// Only a scan that covers the whole graph marks the set as scanned; until
// then announcements keep being checked by their signatures as they are sent.
func (l *legacyProofs) scan(ctx context.Context,
	series ChannelGraphTimeSeries, chain chainhash.Hash, floor,
	bestHeight uint32, self [33]byte,
	confirm func(*lnwire.ChannelAnnouncement1) bool,
	ownChannel func(lnwire.ShortChannelID)) error {

	ranges, err := series.FilterChannelRange(
		chain, floor, bestHeight, false,
	)
	if err != nil {
		return err
	}

	var scids []lnwire.ShortChannelID
	for _, r := range ranges {
		for _, c := range r.Channels {
			scids = append(scids, c.ShortChannelID)
		}
	}

	var found, own int
	for len(scids) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n := min(len(scids), legacyScanBatch)
		batch := scids[:n]
		scids = scids[n:]

		msgs, err := series.FetchChanAnns(chain, batch)
		if err != nil {
			return err
		}

		for _, msg := range msgs {
			ann, ok := msg.(*lnwire.ChannelAnnouncement1)
			if !ok || !netann.SignedUnderLegacyChainHash(ann) {
				continue
			}
			if !confirm(ann) {
				continue
			}
			found++

			if ann.NodeID1 == self || ann.NodeID2 == self {
				own++
				ownChannel(ann.ShortChannelID)
			}
		}
	}

	if found > 0 {
		log.Infof("%d channels in the graph carry a proof signed under "+
			"the withdrawn chain hash, %d of them this node's; "+
			"they are not passed on, and this node's are being "+
			"signed again with their peers", found, own)
	}
	l.scanned.Store(true)

	return nil
}

// markResigned records that this node's half of scid's proof has been asked
// for in this run, and reports whether it had not been already. A channel is
// signed again at most once per run: after that this node's half waits in
// the proof store for the peer's, so a peer sending halves that do not verify
// cannot have it sign on demand.
func (l *legacyProofs) markResigned(scid lnwire.ShortChannelID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.resigned == nil {
		l.resigned = make(map[uint64]struct{})
	}
	if _, ok := l.resigned[scid.ToUint64()]; ok {
		return false
	}
	l.resigned[scid.ToUint64()] = struct{}{}

	return true
}

// servedSeries is the channel series the syncers answer peers from. It
// leaves out what BOLT-blake2b #7 says not to pass on: channel announcements
// whose proof holds only under the withdrawn chain_hash, and anything about a
// channel funded below the gossip floor, which is treated as unannounced.
type servedSeries struct {
	ChannelGraphTimeSeries

	legacy *legacyProofs
	floor  uint32
}

// keep reports whether msg may be sent to a peer.
func (s *servedSeries) keep(msg lnwire.Message) bool {
	switch m := msg.(type) {
	case *lnwire.ChannelAnnouncement1:
		if belowFloor(m.ShortChannelID, s.floor) {
			return false
		}

		return !s.legacy.isLegacyAnn(m)

	case *lnwire.ChannelUpdate1:
		// A channel whose announcement is held back is one the peer
		// cannot place; its updates would only prompt a query that
		// comes back empty.
		return !belowFloor(m.ShortChannelID, s.floor) &&
			!s.legacy.has(m.ShortChannelID)
	}

	return true
}

// FilterKnownChanIDs returns what the inner series does, plus the channels
// whose stored proof holds only under the withdrawn chain_hash: those are
// asked for again, so that a peer holding the proof its nodes signed afresh
// hands it over, and the stored one is replaced (see upgradeLegacyProof).
func (s *servedSeries) FilterKnownChanIDs(chain chainhash.Hash,
	superSet []graphdb.ChannelUpdateInfo,
	isZombieChan func(graphdb.ChannelUpdateInfo) bool) (
	[]lnwire.ShortChannelID, error) {

	unknown, err := s.ChannelGraphTimeSeries.FilterKnownChanIDs(
		chain, superSet, isZombieChan,
	)
	if err != nil {
		return nil, err
	}

	listed := make(map[lnwire.ShortChannelID]struct{}, len(unknown))
	for _, scid := range unknown {
		listed[scid] = struct{}{}
	}
	for _, c := range superSet {
		if _, ok := listed[c.ShortChannelID]; ok {
			continue
		}
		if s.legacy.has(c.ShortChannelID) {
			unknown = append(unknown, c.ShortChannelID)
			listed[c.ShortChannelID] = struct{}{}
		}
	}

	return unknown, nil
}

// UpdatesInHorizon returns the inner series' messages less those not to be
// passed on.
func (s *servedSeries) UpdatesInHorizon(ctx context.Context,
	startTime, endTime time.Time) iter.Seq2[lnwire.Message, error] {

	inner := s.ChannelGraphTimeSeries.UpdatesInHorizon(
		ctx, startTime, endTime,
	)

	return func(yield func(lnwire.Message, error) bool) {
		for msg, err := range inner {
			if err == nil && !s.keep(msg) {
				continue
			}
			if !yield(msg, err) {
				return
			}
		}
	}
}

// FetchChanAnns returns the inner series' messages less those not to be
// passed on.
func (s *servedSeries) FetchChanAnns(chain chainhash.Hash,
	shortChanIDs []lnwire.ShortChannelID) ([]lnwire.Message, error) {

	msgs, err := s.ChannelGraphTimeSeries.FetchChanAnns(chain, shortChanIDs)
	if err != nil {
		return nil, err
	}

	kept := msgs[:0:0]
	for _, msg := range msgs {
		if s.keep(msg) {
			kept = append(kept, msg)
		}
	}

	return kept, nil
}

// FilterChannelRange returns the inner series' ranges less the channels this
// node will not serve, so that a reply_channel_range does not list channels
// a query for which would come back empty.
func (s *servedSeries) FilterChannelRange(chain chainhash.Hash, startHeight,
	endHeight uint32, withTimestamps bool) ([]graphdb.BlockChannelRange,
	error) {

	ranges, err := s.ChannelGraphTimeSeries.FilterChannelRange(
		chain, startHeight, endHeight, withTimestamps,
	)
	if err != nil {
		return nil, err
	}

	kept := ranges[:0:0]
	for _, r := range ranges {
		if s.floor != 0 && r.Height < s.floor {
			continue
		}

		chans := r.Channels[:0:0]
		for _, c := range r.Channels {
			if !s.legacy.has(c.ShortChannelID) {
				chans = append(chans, c)
			}
		}
		if len(chans) == 0 {
			continue
		}

		r.Channels = chans
		kept = append(kept, r)
	}

	return kept, nil
}

// belowFloor reports whether a channel was funded below the gossip floor. A
// zero floor has nothing below it.
func belowFloor(scid lnwire.ShortChannelID, floor uint32) bool {
	return floor != 0 && scid.BlockHeight < floor
}

// A compile-time check that the filter is still a full series.
var _ ChannelGraphTimeSeries = (*servedSeries)(nil)

// scanLegacyProofs runs the startup scan and asks for a fresh proof for each
// of this node's own channels it finds.
//
// NOTE: must be run as a goroutine.
func (d *AuthenticatedGossiper) scanLegacyProofs(ctx context.Context) {

	defer d.wg.Done()

	var self [33]byte
	copy(self[:], d.selfKey.SerializeCompressed())

	for {
		// Every channel above the floor, whatever the height: once
		// the scan finishes the set is taken as complete.
		err := d.legacyProofs.scan(
			ctx, d.cfg.ChanSeries, d.cfg.ChainHash,
			d.cfg.MinAnnouncementHeight, math.MaxUint32, self,
			d.confirmLegacyProof, d.resignChannelProof,
		)
		if err == nil || ctx.Err() != nil {
			return
		}

		log.Errorf("Unable to scan the graph for proofs signed under "+
			"the withdrawn chain hash, retrying in %v: %v",
			legacyScanRetry, err)

		select {
		case <-time.After(legacyScanRetry):
		case <-ctx.Done():
			return
		case <-d.quit:
			return
		}
	}
}

// confirmLegacyProof checks, under the channel's lock, that the stored proof
// of ann's channel still holds only under the withdrawn chain_hash, and
// records it if so.
func (d *AuthenticatedGossiper) confirmLegacyProof(
	ann *lnwire.ChannelAnnouncement1) bool {

	scid := ann.ShortChannelID
	d.channelMtx.Lock(scid.ToUint64())
	defer d.channelMtx.Unlock(scid.ToUint64())

	info, e1, e2, err := d.cfg.Graph.GetChannelByID(scid)
	if err != nil || info.AuthProof == nil {
		return false
	}
	stored, _, _, err := netann.CreateChanAnnouncement(info, e1, e2)
	if err != nil || !netann.SignedUnderLegacyChainHash(stored) {
		return false
	}
	d.legacyProofs.add(scid)

	return true
}

// resignChannelProof asks, in the background, for a fresh
// announcement_signatures for one of this node's channels. The answer comes
// back through the gossiper's own queue, so it must never be waited for from
// inside the gossiper.
func (d *AuthenticatedGossiper) resignChannelProof(
	scid lnwire.ShortChannelID) {

	if d.cfg.ResignChannelProof == nil ||
		!d.legacyProofs.markResigned(scid) {

		return
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()

		log.Infof("Signing the proof of channel %v again: the stored "+
			"one was made under the withdrawn chain hash", scid)

		if err := d.cfg.ResignChannelProof(scid); err != nil {
			log.Warnf("Unable to sign the proof of channel %v "+
				"again: %v", scid, err)
		}
	}()
}

// withoutLegacyProofs drops, from a batch about to be broadcast, anything
// about a channel funded below the gossip floor, the channel announcements
// whose proof holds only under the withdrawn chain_hash, and,
// when relayed is set, the updates of such channels as well. This node's own
// updates for them stay: its counterparty, on an earlier release, still
// routes by them.
func (d *AuthenticatedGossiper) withoutLegacyProofs(msgs []msgWithSenders,
	relayed bool) []msgWithSenders {

	kept := msgs[:0:0]
	for _, m := range msgs {
		floor := d.cfg.MinAnnouncementHeight
		switch msg := m.msg.(type) {
		case *lnwire.ChannelAnnouncement1:
			// Nothing below the gossip floor is announced, this
			// node's own channels included (BOLT-blake2b #7); an
			// exchange of proofs that completes late for one would
			// otherwise send it out.
			if belowFloor(msg.ShortChannelID, floor) {
				continue
			}
			if d.legacyProofs.isLegacyAnn(msg) {
				log.Debugf("Not broadcasting the announcement "+
					"of %v: its proof was signed under the "+
					"withdrawn chain hash", msg.ShortChannelID)

				continue
			}

		case *lnwire.ChannelUpdate1:
			if belowFloor(msg.ShortChannelID, floor) {
				continue
			}
			if relayed && d.legacyProofs.has(msg.ShortChannelID) {
				continue
			}
		}
		kept = append(kept, m)
	}

	return kept
}

// upgradeLegacyProof replaces the stored proof of a known channel with the one
// in ann, if the stored one holds only under the withdrawn chain_hash and
// ann's holds over the message as sent. It returns the announcements to
// broadcast in that case, and nothing otherwise.
//
// The new signatures are checked against an announcement rebuilt from the
// stored channel, not against ann alone, so a proof that does not match what
// this node already knows of the channel is never stored.
func (d *AuthenticatedGossiper) upgradeLegacyProof(
	ann *lnwire.ChannelAnnouncement1) []networkMsg {

	scid := ann.ShortChannelID

	// Cheap to reject first: another copy of the legacy announcement, or
	// a bad one.
	if netann.ValidateChannelAnnStrict(ann) != nil {
		return nil
	}

	d.channelMtx.Lock(scid.ToUint64())
	defer d.channelMtx.Unlock(scid.ToUint64())

	// Asked again under the lock: a concurrent upgrade may have won.
	if !d.legacyProofs.has(scid) {
		return nil
	}

	chanInfo, e1, e2, err := d.cfg.Graph.GetChannelByID(scid)
	if err != nil {
		log.Debugf("Unable to fetch channel %v to replace its proof: "+
			"%v", scid, err)

		return nil
	}

	proof := models.NewV1ChannelAuthProof(
		ann.NodeSig1.ToSignatureBytes(),
		ann.NodeSig2.ToSignatureBytes(),
		ann.BitcoinSig1.ToSignatureBytes(),
		ann.BitcoinSig2.ToSignatureBytes(),
	)

	info := *chanInfo
	info.AuthProof = proof

	rebuilt, e1Ann, e2Ann, err := netann.CreateChanAnnouncement(
		&info, e1, e2,
	)
	if err != nil {
		log.Debugf("Unable to rebuild the announcement of %v: %v",
			scid, err)

		return nil
	}
	if err := netann.ValidateChannelAnnStrict(rebuilt); err != nil {
		log.Debugf("New proof for %v does not match the stored "+
			"channel: %v", scid, err)

		return nil
	}

	if err := d.cfg.Graph.AddProof(scid, proof); err != nil {
		log.Errorf("Unable to replace the proof of %v: %v", scid, err)

		return nil
	}
	d.legacyProofs.remove(scid)

	log.Infof("Replaced the proof of channel %v, which was signed under "+
		"the withdrawn chain hash, with one its nodes signed again",
		scid)

	anns := []networkMsg{{source: d.selfKey, msg: rebuilt}}
	if e1Ann != nil {
		anns = append(anns, networkMsg{source: d.selfKey, msg: e1Ann})
	}
	if e2Ann != nil {
		anns = append(anns, networkMsg{source: d.selfKey, msg: e2Ann})
	}

	return anns
}

// isOwnPeerUpdate reports whether a remote channel update is one this node's
// own peer sent, directly, for its own side of a channel with this node.
func (d *AuthenticatedGossiper) isOwnPeerUpdate(nMsg *networkMsg,
	chanInfo *models.ChannelEdgeInfo, signer *btcec.PublicKey) bool {

	if nMsg.source == nil || signer == nil ||
		!nMsg.source.IsEqual(signer) {

		return false
	}

	var self [33]byte
	copy(self[:], d.selfKey.SerializeCompressed())

	return chanInfo.NodeKey1Bytes == self || chanInfo.NodeKey2Bytes == self
}

// keepOwnHalf replaces, in the waiting proof store, a peer's half that did
// not pair with this node's by this node's own.
func (d *AuthenticatedGossiper) keepOwnHalf(own *channeldb.WaitingProof) {
	if err := d.cfg.WaitingProofStore.Remove(own.OppositeKey()); err != nil &&
		!errors.Is(err, channeldb.ErrWaitingProofNotFound) {

		log.Debugf("Unable to drop the peer's half of %v: %v",
			own.OppositeKey(), err)
	}
	if err := d.cfg.WaitingProofStore.Add(own); err != nil {
		log.Debugf("Unable to keep this node's half: %v", err)
	}
}

// storedProofIsLegacy reports whether a channel's stored proof holds only
// under the withdrawn chain_hash. After the startup scan the set answers;
// before it, the stored announcement's signatures do, and a legacy one is
// remembered, so that the re-proof exchange works from the first message.
func (d *AuthenticatedGossiper) storedProofIsLegacy(
	info *models.ChannelEdgeInfo, e1, e2 *models.ChannelEdgePolicy) bool {

	scid := lnwire.NewShortChanIDFromInt(info.ChannelID)
	if d.legacyProofs.has(scid) {
		return true
	}
	if d.legacyProofs.scanned.Load() || info.AuthProof == nil {
		return false
	}

	stored, _, _, err := netann.CreateChanAnnouncement(info, e1, e2)
	if err != nil || !netann.SignedUnderLegacyChainHash(stored) {
		return false
	}
	d.legacyProofs.add(scid)

	return true
}
