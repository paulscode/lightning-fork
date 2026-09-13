package lnd

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
)

// preForkChannels returns a description of every channel whose funding
// transaction confirmed below the BLAKE2b activation height. Such a channel's
// funding output exists on both chains: its commitment transactions are
// signed with the protocol's hash types, which any chain accepts, so a force
// close on one chain can be replayed on the other. Channels not yet confirmed
// (zero-conf before confirmation) cannot be judged and are skipped.
func preForkChannels(channels []*channeldb.OpenChannel,
	activationHeight uint32) []string {

	if activationHeight == 0 {
		return nil
	}

	var found []string
	for _, c := range channels {
		scid := c.ShortChanID()
		if c.IsZeroConf() {
			if !c.ZeroConfConfirmed() {
				continue
			}
			scid = c.ZeroConfRealScid()
		}
		if scid.BlockHeight == 0 || scid.BlockHeight >= activationHeight {
			continue
		}
		found = append(found, fmt.Sprintf("%v (confirmed at %d, with %x)",
			c.FundingOutpoint, scid.BlockHeight,
			c.IdentityPub.SerializeCompressed()))
	}

	return found
}

// warnPreForkChannels logs each pre-fork channel at startup with the advice
// that applies: close it, and prefer coins received after the fork.
func (s *server) warnPreForkChannels() {
	channels, err := s.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		srvrLog.Warnf("Unable to scan for pre-fork channels: %v", err)
		return
	}

	for _, desc := range preForkChannels(
		channels, s.cfg.ActiveNetParams.Blake2bActivationHeight,
	) {
		srvrLog.Warnf("Channel %s was funded before the BLAKE2b fork: "+
			"its funding output exists on both chains and its "+
			"commitment transactions are valid on both, so a close on "+
			"one chain can be replayed on the other. Close it and "+
			"reopen with coins received after block %d", desc,
			s.cfg.ActiveNetParams.Blake2bActivationHeight)
	}
}
