package peer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/splice"
	"github.com/lightningnetwork/lnd/lnwire"
)

// errAlreadyClosing refuses a request to start a close already under way.
var errAlreadyClosing = errors.New("the channel is already closing")

// errSpliceUnlocked is the reason a cooperative close is refused while a
// splice of the channel hasn't locked: the spec forbids shutdown then.
var errSpliceUnlocked = errors.New("a splice of this channel hasn't " +
	"locked yet")

// spliceUnlocked reports whether the channel has a splice that hasn't
// locked, or one being built. A dead one doesn't count: it can never confirm,
// and nothing else ever takes its funding out of the channel's set, so the
// channel could never close cooperatively again.
func (p *Brontide) spliceUnlocked(chanID lnwire.ChannelID) bool {
	channel, ok := p.activeChannels.Load(chanID)
	if !ok || channel == nil {
		return false
	}
	if pending := channel.PendingFundings(); len(pending) > 0 {
		dead := make(map[chainhash.Hash]bool)
		records, err := channel.State().SpliceRecords()
		if err != nil {
			// Unreadable: as unlocked.
			return true
		}
		// Dead, or disowned by the peer (a close spends the output
		// it spends, ending it).
		for _, r := range records {
			n := r.Negotiation
			if n != nil && (n.Dead || n.Disowned) {
				dead[r.Txid()] = true
			}
		}
		for _, f := range pending {
			if !dead[f.OutPoint.Hash] {
				return true
			}
		}
	}

	// Being negotiated or signed, before its funding joins the channel:
	// closing would race the splice, both transactions signed.
	m, ok := p.SpliceMachine(chanID)

	return ok && m.Busy()
}

// closeDisowned closes a channel whose peer dropped a splice after our
// tx_signatures: the peer's commitments leave the splice out, and ours on it
// must stay current, so the channel can take no update. By force when it has
// HTLCs, never to be settled; otherwise cooperatively, unless a close is
// under way, which a second request would replace.
func (p *Brontide) closeDisowned(chanID lnwire.ChannelID, force bool) {
	channel, ok := p.activeChannels.Load(chanID)
	if !ok || channel == nil {
		return
	}
	op := channel.ChannelPoint()
	if force {
		if _, err := p.cfg.ChainArb.ForceCloseContract(op); err != nil {
			p.log.Errorf("ChannelID(%v): unable to force close: %v",
				chanID, err)
		}

		return
	}

	feePerKw, err := p.cfg.FeeEstimator.EstimateFeePerKW(
		p.cfg.CoopCloseTargetConfs,
	)
	if err != nil {
		p.log.Errorf("ChannelID(%v): unable to estimate the close fee: "+
			"%v", chanID, err)
		return
	}

	ctx, cancel := p.cg.Create(context.Background())
	defer cancel()

	req := &htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      &op,
		TargetFeePerKw: feePerKw,
		Updates:        make(chan interface{}, 8),
		Err:            make(chan error, 1),
		Ctx:            ctx,
		NotIfClosing:   true,
	}
	p.HandleLocalCloseChanReqs(req)

	for {
		select {
		case u := <-req.Updates:
			if _, done := u.(*ChannelCloseUpdate); done {
				return
			}

		case err := <-req.Err:
			if err != nil && !errors.Is(err, errAlreadyClosing) {
				p.log.Errorf("ChannelID(%v): unable to close: %v",
					chanID, err)
			}
			return

		case <-ctx.Done():
			return
		}
	}
}

// spliceShutdown tells the channel's splice machine and link that the peer
// sent shutdown, as the message is read: BOLT 2 allows no splice from then
// on. A splice of ours still waiting for quiescence, the peer knowing
// nothing of it, fails with "closing", its coins released, and an stfu of
// ours not yet sent is withdrawn; the peer's shutdown is no breach then.
func (p *Brontide) spliceShutdown(chanID lnwire.ChannelID) {
	if m, ok := p.SpliceMachine(chanID); ok {
		m.ShutdownReceived()
	}

	// After the machine, so the withdrawn quiescence reaches it as the
	// close it is.
	link := p.fetchLinkFromKeyAndCid(chanID)
	if sl, ok := link.(htlcswitch.SpliceLink); ok {
		sl.ShutdownReceived()
	}
}

// spliceNegotiated reports whether splicing applies to this connection: we
// advertise it (on, or kept on for a splice still to finish somewhere) and
// the peer supports it. A machine that can't start new splices refuses
// them.
func (p *Brontide) spliceNegotiated(lnChan *lnwallet.LightningChannel) bool {
	if !p.remoteFeatures.HasFeature(lnwire.SpliceOptional) {
		return false
	}
	if p.cfg.SpliceMachine == nil {
		return false
	}

	return p.cfg.SpliceEnabled || len(lnChan.PendingFundings()) > 0
}

// spliceMachineMode reports whether the channel gets a splice machine on
// this connection, and whether it starts disabled: one only enforcing the
// channel's holds, since splicing doesn't apply.
func (p *Brontide) spliceMachineMode(
	lnChan *lnwallet.LightningChannel) (bool, bool) {

	if p.spliceNegotiated(lnChan) {
		return true, false
	}

	return p.spliceHeld(lnChan), true
}

// spliceHeld reports whether the channel holds coins the peer put into a
// splice that locked and isn't deep enough yet. The machine enforces that
// hold, so it runs even when splicing doesn't apply to this connection: a
// peer that leaves the feature out on reconnecting, or splicing turned off
// since, must not free the coins early.
func (p *Brontide) spliceHeld(lnChan *lnwallet.LightningChannel) bool {
	if p.cfg.SpliceMachine == nil {
		return false
	}
	holds, err := lnChan.State().SpliceHolds()
	if err != nil {
		// Unreadable: start the machine, which reads them again and
		// fails the link if it can't.
		p.log.Errorf("unable to read splice holds: %v", err)
		return true
	}

	return len(holds) > 0
}

// spliceLinkProxy is the channel's link as the splice machine sees it,
// looked up in the switch on each call: the link is created after the
// machine and replaced on reconnection.
type spliceLinkProxy struct {
	p      *Brontide
	chanID lnwire.ChannelID
}

// link returns the channel's link with its splice methods.
func (s *spliceLinkProxy) link() (htlcswitch.SpliceLink, error) {
	// The link is found by the peer's key, so a machine of an ended
	// connection, still finishing an event, would find the next
	// connection's link and act on a channel its machine doesn't know.
	if atomic.LoadInt32(&s.p.disconnect) == 1 {
		return nil, splice.ErrConnectionGone
	}

	links, err := s.p.cfg.Switch.GetLinksByInterface(s.p.PubKey())
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if link.ChanID() != s.chanID {
			continue
		}
		sl, ok := link.(htlcswitch.SpliceLink)
		if !ok {
			return nil, fmt.Errorf("link of %v can't splice",
				s.chanID)
		}

		return sl, nil
	}

	return nil, fmt.Errorf("no link for %v", s.chanID)
}

func errChan(err error) <-chan error {
	c := make(chan error, 1)
	c <- err

	return c
}

// QuiescenceInitiator implements splice.Link.
func (s *spliceLinkProxy) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	l, err := s.link()
	if err != nil {
		return fn.Err[lntypes.ChannelParty](err)
	}

	return l.QuiescenceInitiator()
}

// InitStfu implements splice.Link.
func (s *spliceLinkProxy) InitStfu() <-chan fn.Result[lntypes.ChannelParty] {
	l, err := s.link()
	if err != nil {
		c := make(chan fn.Result[lntypes.ChannelParty], 1)
		c <- fn.Err[lntypes.ChannelParty](err)

		return c
	}

	return l.InitStfu()
}

// ResumeQuiescence implements splice.Link.
func (s *spliceLinkProxy) ResumeQuiescence() <-chan error {
	l, err := s.link()
	if err != nil {
		return errChan(err)
	}

	return l.ResumeQuiescence()
}

// AddSpliceFunding implements splice.Link.
func (s *spliceLinkProxy) AddSpliceFunding(
	f lnwallet.PendingFunding) <-chan error {

	l, err := s.link()
	if err != nil {
		return errChan(err)
	}

	return l.AddSpliceFunding(f)
}

// SendSpliceCommitment implements splice.Link.
func (s *spliceLinkProxy) SendSpliceCommitment(
	txid chainhash.Hash) <-chan error {

	l, err := s.link()
	if err != nil {
		return errChan(err)
	}

	return l.SendSpliceCommitment(txid)
}

// LockSplice implements splice.Link.
func (s *spliceLinkProxy) LockSplice(txid chainhash.Hash,
	scid lnwire.ShortChannelID) <-chan error {

	l, err := s.link()
	if err != nil {
		return errChan(err)
	}

	return l.LockSplice(txid, scid)
}

// SpliceMachine returns the splice machine of one of the peer's channels.
func (p *Brontide) SpliceMachine(chanID lnwire.ChannelID) (*splice.Machine,
	bool) {

	p.spliceMtx.Lock()
	defer p.spliceMtx.Unlock()

	m, ok := p.spliceMachines[chanID]

	return m, ok
}

// startSpliceMachine creates and starts a channel's splice machine, stopping
// one left from the previous connection. A disabled machine refuses every
// splice and only enforces the channel's holds.
func (p *Brontide) startSpliceMachine(chanID lnwire.ChannelID,
	lnChan *lnwallet.LightningChannel, disabled bool) (*splice.Machine,
	error) {

	m := p.cfg.SpliceMachine(splice.Config{
		ChanID:   chanID,
		Link:     &spliceLinkProxy{p: p, chanID: chanID},
		Channel:  lnChan,
		Peer:     p,
		Disabled: disabled,
		Close: func(force bool) {
			go p.closeDisowned(chanID, force)
		},
	}, p.IdentityKey())
	if err := m.Start(); err != nil {
		return nil, err
	}

	p.spliceMtx.Lock()
	old := p.spliceMachines[chanID]
	p.spliceMachines[chanID] = m
	p.spliceMtx.Unlock()

	if old != nil {
		old.Stop()
	}

	// The connection ending meanwhile: its machines may have been stopped
	// before this one joined them, and nothing would stop it.
	if atomic.LoadInt32(&p.disconnect) == 1 {
		p.spliceMtx.Lock()
		if p.spliceMachines[chanID] == m {
			delete(p.spliceMachines, chanID)
		}
		p.spliceMtx.Unlock()
		m.Stop()
	}

	return m, nil
}

// stopSpliceMachines stops every splice machine of the peer.
func (p *Brontide) stopSpliceMachines() {
	p.spliceMtx.Lock()
	machines := p.spliceMachines
	p.spliceMachines = make(map[lnwire.ChannelID]*splice.Machine)
	p.spliceMtx.Unlock()

	for _, m := range machines {
		m.Stop()
	}
}

// HoldUpdates implements splice.Link.
func (s *spliceLinkProxy) HoldUpdates() <-chan error {
	l, err := s.link()
	if err != nil {
		return errChan(err)
	}

	return l.HoldUpdates()
}

// RemoveSpliceFunding implements splice.Link.
func (s *spliceLinkProxy) RemoveSpliceFunding(
	txid chainhash.Hash) <-chan error {

	l, err := s.link()
	if err != nil {
		return errChan(err)
	}

	return l.RemoveSpliceFunding(txid)
}
