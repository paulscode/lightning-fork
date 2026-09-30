package discovery

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

// legacyProofs is the set of channels whose stored proof holds only under the
// withdrawn chain_hash.
type legacyProofs struct {
	mu    sync.RWMutex
	scids map[uint64]struct{}

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
// withdrawn chain_hash, records it, and hands each one of this node's own to
// ownChannel. It reads through series, which must be the unfiltered one.
func (l *legacyProofs) scan(ctx context.Context,
	series ChannelGraphTimeSeries, chain chainhash.Hash, floor,
	bestHeight uint32, self [33]byte,
	ownChannel func(lnwire.ShortChannelID)) error {

	defer l.scanned.Store(true)

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

			l.add(ann.ShortChannelID)
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

	return nil
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
		return !belowFloor(m.ShortChannelID, s.floor)
	}

	return true
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
func (d *AuthenticatedGossiper) scanLegacyProofs(ctx context.Context,
	bestHeight uint32) {

	defer d.wg.Done()

	var self [33]byte
	copy(self[:], d.selfKey.SerializeCompressed())

	err := d.legacyProofs.scan(
		ctx, d.cfg.ChanSeries, d.cfg.ChainHash,
		d.cfg.MinAnnouncementHeight, bestHeight, self,
		d.resignChannelProof,
	)
	if err != nil && ctx.Err() == nil {
		log.Errorf("Unable to scan the graph for proofs signed under "+
			"the withdrawn chain hash: %v", err)
	}
}

// resignChannelProof asks, in the background, for a fresh
// announcement_signatures for one of this node's channels. The answer comes
// back through the gossiper's own queue, so it must never be waited for from
// inside the gossiper.
func (d *AuthenticatedGossiper) resignChannelProof(
	scid lnwire.ShortChannelID) {

	if d.cfg.ResignChannelProof == nil {
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

// withoutLegacyProofs drops, from a batch about to be broadcast, the channel
// announcements whose proof holds only under the withdrawn chain_hash.
func (d *AuthenticatedGossiper) withoutLegacyProofs(
	msgs []msgWithSenders) []msgWithSenders {

	kept := msgs[:0:0]
	for _, m := range msgs {
		ann, ok := m.msg.(*lnwire.ChannelAnnouncement1)
		if ok && d.legacyProofs.isLegacyAnn(ann) {
			log.Debugf("Not broadcasting the announcement of %v: "+
				"its proof was signed under the withdrawn chain "+
				"hash", ann.ShortChannelID)

			continue
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
