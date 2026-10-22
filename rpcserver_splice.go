package lnd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcwallet/waddrmgr"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/splice"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Splice status codes of a channel, as Channel.splice_status reports them.
const (
	spliceStatusOK                = "ok"
	spliceStatusDisabled          = "disabled"
	spliceStatusPeerOffline       = "peer_offline"
	spliceStatusPeerUnsupported   = "peer_unsupported"
	spliceStatusNoUnifiedSigs     = "no_unified_sigs"
	spliceStatusTaproot           = "taproot"
	spliceStatusZeroConf          = "zero_conf"
	spliceStatusPending           = "pending"
	spliceStatusClosing           = "closing"
	spliceStatusPeerNotLocked     = "peer_not_locked"
	spliceStatusPeerNotAnnouncing = "peer_not_announcing"
	spliceStatusSpentUnknown      = "funding_spent_unknown"
)

// spliceChannel finds a channel by its current or original channel point.
func (r *rpcServer) spliceChannel(
	cp *lnrpc.ChannelPoint) (*channeldb.OpenChannel, error) {

	if cp == nil {
		return nil, status.Error(codes.InvalidArgument,
			"channel point required")
	}
	txid, err := lnrpc.GetChanPointFundingTxid(cp)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	op := wire.OutPoint{Hash: *txid, Index: cp.OutputIndex}

	ch, err := r.server.chanStateDB.FetchChannel(op)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "channel %v: %v",
			op, err)
	}

	return ch, nil
}

// spliceStatus returns whether this node can start a splice of the channel
// now, and its splice machine when it can.
func (r *rpcServer) spliceStatus(
	ch *channeldb.OpenChannel) (string, *splice.Machine) {

	pending := false
	if records, err := ch.SpliceRecords(); err == nil {
		for _, rec := range records {
			if !rec.Retired {
				pending = true
			}
		}
	}

	switch {
	case ch.HasChanStatus(channeldb.ChanStatusFundingSpentUnknown):
		return spliceStatusSpentUnknown, nil

	case ch.HasChanStatus(channeldb.ChanStatusCoopBroadcasted),
		ch.HasChanStatus(channeldb.ChanStatusLocalCloseInitiator),
		ch.HasChanStatus(channeldb.ChanStatusRemoteCloseInitiator):

		return spliceStatusClosing, nil

	// Shutdown sent: a cooperative close is under way.
	case hasShutdownInfo(ch):
		return spliceStatusClosing, nil

	case !ch.ChanType.HasUnifiedSigs():
		return spliceStatusNoUnifiedSigs, nil

	case ch.ChanType.IsTaproot():
		return spliceStatusTaproot, nil

	case ch.ChanType.HasZeroConf():
		return spliceStatusZeroConf, nil

	case pending:
		if r.peerNotLocked(ch) {
			return spliceStatusPeerNotLocked, nil
		}

		return spliceStatusPending, nil

	case !r.cfg.ProtocolOptions.Splice():
		return spliceStatusDisabled, nil
	}

	p, err := r.server.FindPeer(ch.IdentityPub)
	if err != nil {
		return spliceStatusPeerOffline, nil
	}
	chanID := lnwire.NewChanIDFromOutPoint(ch.FundingOutpoint)
	m, ok := p.SpliceMachine(chanID)
	if !ok {
		return spliceStatusPeerUnsupported, nil
	}
	if code := m.Refusal(); code != "" {
		return code, nil
	}

	// Said, but no reason not to splice: the peer's missing half of the
	// last announcement doesn't stop another splice.
	if r.peerNotAnnouncing(ch) {
		return spliceStatusPeerNotAnnouncing, m
	}

	return spliceStatusOK, m
}

// maxSpliceSatPerVByte is the highest fee rate a splice may ask for, as the
// dashboard and the mobile API cap it: a mistyped rate would otherwise pay
// the channel's balance in fees.
const maxSpliceSatPerVByte = 10_000

// spliceFeeRate returns the fee rate a request asks for: a rate, or a
// confirmation target (6 when neither is given).
func (r *rpcServer) spliceFeeRate(satPerVByte uint64,
	targetConf int32) (chainfee.SatPerKWeight, error) {

	est := r.server.cc.FeeEstimator
	if satPerVByte > 0 {
		return splice.RequestedFeeRate(satPerVByte, est), nil
	}
	if targetConf <= 0 {
		targetConf = 6
	}

	return est.EstimateFeePerKW(uint32(targetConf))
}

// spliceDestination returns where funds moved out go: the address asked
// for, which must be a segwit v0 or v1 address on this chain, or a new one
// of the wallet's. It reports whether the destination is the wallet's own.
func (r *rpcServer) spliceDestination(addr string) ([]byte, bool, error) {
	wallet := r.server.cc.Wallet
	if addr == "" {
		a, err := wallet.NewAddress(
			lnwallet.TaprootPubkey, false,
			lnwallet.DefaultAccountName,
		)
		if err != nil {
			return nil, false, err
		}
		script, err := txscript.PayToAddrScript(a)

		return script, true, err
	}

	a, err := btcutil.DecodeAddress(addr, r.cfg.ActiveNetParams.Params)
	if err != nil {
		return nil, false, status.Errorf(codes.InvalidArgument,
			"address: %v", err)
	}
	if !a.IsForNet(r.cfg.ActiveNetParams.Params) {
		return nil, false, status.Error(codes.InvalidArgument,
			"address of another network")
	}
	switch a.(type) {
	case *btcutil.AddressWitnessPubKeyHash,
		*btcutil.AddressWitnessScriptHash, *btcutil.AddressTaproot:

	default:
		return nil, false, status.Error(codes.InvalidArgument,
			"only segwit v0 and v1 addresses")
	}
	script, err := txscript.PayToAddrScript(a)

	return script, r.inDefaultAccount(a), err
}

// inDefaultAccount reports whether an address is one of the wallet's own in
// its default account: what "this node's wallet" means to the user. An
// imported or watch-only account's address is someone else's to spend.
func (r *rpcServer) inDefaultAccount(a btcutil.Address) bool {
	info, err := r.server.cc.Wallet.AddressInfo(a)
	if err != nil || info == nil {
		return false
	}

	return info.InternalAccount() == waddrmgr.DefaultAccountNum
}

// closeDestination returns the channel's close address, if it was opened
// with one: funds moved out may only go there. It reports whether that
// address is in this node's default account.
func (r *rpcServer) closeDestination(
	ch *channeldb.OpenChannel) ([]byte, bool, bool) {

	script := []byte(ch.LocalShutdownScript)
	if len(script) == 0 {
		return nil, false, false
	}
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(
		script, r.cfg.ActiveNetParams.Params,
	)
	ours := err == nil && len(addrs) == 1 && r.inDefaultAccount(addrs[0])

	return script, ours, true
}

// checkSpliceRanges refuses a request's amount and fee rate out of range: a
// mistyped rate would pay the channel's balance in fees.
func checkSpliceRanges(amount int64, satPerVByte uint64) error {
	switch {
	case amount < 0:
		return status.Error(codes.InvalidArgument, "negative amount")

	case amount > btcutil.MaxSatoshi:
		return status.Error(codes.InvalidArgument,
			"amount above the most that can exist")

	case satPerVByte > maxSpliceSatPerVByte:
		return status.Errorf(codes.InvalidArgument,
			"fee rate above %d sat/vB", maxSpliceSatPerVByte)
	}

	return nil
}

// spliceOutpoints converts the coins a splice may spend; only funds in
// spend wallet coins.
func spliceOutpoints(direction lnrpc.SpliceDirection,
	outpoints []*lnrpc.OutPoint) ([]wire.OutPoint, error) {

	if len(outpoints) == 0 {
		return nil, nil
	}
	if direction != lnrpc.SpliceDirection_SPLICE_IN {
		return nil, status.Error(codes.InvalidArgument,
			"outpoints are for adding funds only")
	}
	ops, err := toWireOutpoints(outpoints)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"outpoints: %v", err)
	}

	return ops, nil
}

// spliceRequest turns the RPC's request into the machine's.
func (r *rpcServer) spliceRequest(ch *channeldb.OpenChannel,
	direction lnrpc.SpliceDirection, amount int64, useMax bool,
	satPerVByte uint64, targetConf int32, addr string,
	force bool, outpoints []*lnrpc.OutPoint) (splice.Request, bool,
	error) {

	req := splice.Request{
		Amount: btcutil.Amount(amount), UseMax: useMax,
		ForceLowFee: force,
	}
	if err := checkSpliceRanges(amount, satPerVByte); err != nil {
		return req, false, err
	}

	var err error
	req.Outpoints, err = spliceOutpoints(direction, outpoints)
	if err != nil {
		return req, false, err
	}
	req.FeeRate, err = r.spliceFeeRate(satPerVByte, targetConf)
	if err != nil {
		return req, false, err
	}

	ours := false
	switch direction {
	case lnrpc.SpliceDirection_SPLICE_IN:
		req.Direction = splice.DirectionIn

	case lnrpc.SpliceDirection_SPLICE_OUT:
		req.Direction = splice.DirectionOut

		// A channel opened with a close address moves funds out
		// there when no other is asked for.
		if close, closeOurs, set := r.closeDestination(ch); set &&
			addr == "" {

			req.Destination, ours = close, closeOurs
			break
		}
		req.Destination, ours, err = r.spliceDestination(addr)
		if err != nil {
			return req, false, err
		}

	default:
		return req, false, status.Error(codes.InvalidArgument,
			"unknown direction")
	}

	return req, ours, nil
}

// EstimateSplice reports what a splice of the channel this node would start
// would cost and allow. Purely local: the peer isn't contacted and nothing
// is leased.
func (r *rpcServer) EstimateSplice(_ context.Context,
	in *lnrpc.EstimateSpliceRequest) (*lnrpc.EstimateSpliceResponse,
	error) {

	if err := checkSpliceRanges(in.AmountSat, in.SatPerVbyte); err != nil {
		return nil, err
	}
	ch, err := r.spliceChannel(in.ChannelPoint)
	if err != nil {
		return nil, err
	}
	code, machine := r.spliceStatus(ch)
	refusal := ""
	if machine == nil {
		refusal = code
	}

	// The same fee rate the splice would use, and the destination it
	// would pay; an estimate of funds out to our wallet doesn't make a
	// fresh address, a P2TR placeholder weighs the same.
	feeRate, err := r.spliceFeeRate(in.SatPerVbyte, in.TargetConf)
	if err != nil {
		return nil, err
	}
	req := splice.Request{
		Amount: btcutil.Amount(in.AmountSat), UseMax: in.UseMax,
		FeeRate: feeRate,
	}
	req.Outpoints, err = spliceOutpoints(in.Direction, in.Outpoints)
	if err != nil {
		return nil, err
	}
	ours := false
	switch in.Direction {
	case lnrpc.SpliceDirection_SPLICE_IN:
		req.Direction = splice.DirectionIn

	case lnrpc.SpliceDirection_SPLICE_OUT:
		req.Direction = splice.DirectionOut
		close, closeOurs, closeSet := r.closeDestination(ch)
		switch {
		// Only the close address, whatever is asked for.
		case closeSet && in.Addr == "":
			req.Destination, ours = close, closeOurs

		case in.Addr == "":
			req.Destination = make([]byte, 34)
			req.Destination[0], req.Destination[1] = 0x51, 0x20
			ours = true

		default:
			req.Destination, ours, err = r.spliceDestination(
				in.Addr,
			)
			if err != nil {
				return nil, err
			}
			if closeSet && !bytes.Equal(close, req.Destination) &&
				refusal == "" {

				refusal = "close_address_set"
			}
		}

	default:
		return nil, status.Error(codes.InvalidArgument,
			"unknown direction")
	}

	wallet := r.server.cc.Wallet
	var coins []splice.Coin
	err = wallet.WithCoinSelectLock(func() error {
		var err error
		coins, err = splice.ListCoins(wallet.WalletController)

		return err
	})
	if err != nil {
		return nil, err
	}

	var available btcutil.Amount
	chanID := lnwire.NewChanIDFromOutPoint(ch.FundingOutpoint)
	if link, err := r.server.htlcSwitch.GetLink(chanID); err == nil {
		available = link.Bandwidth().ToSatoshis()
	}

	// What the wallet keeps for fee-bumping its anchor channels' closes:
	// the most going in leaves it.
	numAnchors, err := wallet.CurrentNumAnchorChans()
	if err != nil {
		return nil, err
	}
	reserve := wallet.RequiredReserve(uint32(numAnchors))

	// With coins chosen, only they are candidates, and the coins left
	// out stay in the wallet towards its reserve.
	keep := reserve
	if len(req.Outpoints) > 0 {
		coins, keep, err = splice.OnlyCoins(
			coins, req.Outpoints, reserve,
		)
		if err != nil {
			if refusal == "" {
				refusal = "unknown_coin"
			}
			coins, keep = nil, reserve
		}
	}

	est, err := splice.EstimateSplice(splice.EstimateRequest{
		Request:      req,
		Capacity:     ch.Capacity,
		LocalBalance: ch.LocalCommitment.LocalBalance.ToSatoshis(),
		Available:    available,
		Coins:        coins,
		ChangeScript: make([]byte, 22),
		DustLimit: func(script []byte) btcutil.Amount {
			return lnwallet.DustLimitForSize(len(script))
		},
		Reserve:   keep,
		Estimator: r.server.cc.FeeEstimator,
		Refusal:   refusal,
	})
	if err != nil {
		return nil, err
	}

	return &lnrpc.EstimateSpliceResponse{
		FeeSat:            int64(est.Fee),
		Vbytes:            (est.Weight + 3) / 4,
		SatPerVbyte:       uint64(feeRate.FeePerVByte()),
		AmountSat:         int64(est.Amount),
		MinSat:            int64(est.Min),
		MaxInSat:          int64(est.MaxIn),
		MaxOutSat:         int64(est.MaxOut),
		MinSatPerVbyte:    uint64(est.Floor.Rate.FeePerVByte()),
		FeeEstimateSource: string(est.Floor.Source),
		AnchorReserveSat:  int64(reserve),
		NewCapacitySat:    int64(est.NewCapacity),
		NewLocalSat:       int64(est.NewLocal),
		LockDepth: spliceLockDepth(
			ch.NumConfsRequired, 0,
			r.cfg.ProtocolOptions.SplicePeerInputConfs,
		),
		DestinationIsOurs: ours,
		RefusalCode:       est.RefusalCode,
	}, nil
}

// SpliceChannel starts a splice of the channel and streams its progress.
// The splice doesn't depend on the stream: if the stream ends before the
// construction is done the splice is canceled cleanly; after, it carries on.
func (r *rpcServer) SpliceChannel(in *lnrpc.SpliceChannelRequest,
	stream lnrpc.Lightning_SpliceChannelServer) error {

	ch, err := r.spliceChannel(in.ChannelPoint)
	if err != nil {
		return err
	}
	code, machine := r.spliceStatus(ch)
	if machine == nil {
		return status.Errorf(codes.FailedPrecondition, "%s", code)
	}

	req, _, err := r.spliceRequest(
		ch, in.Direction, in.AmountSat, in.UseMax, in.SatPerVbyte,
		in.TargetConf, in.Addr, in.ForceLowFee, in.Outpoints,
	)
	if err != nil {
		return err
	}

	updates, err := machine.Splice(req)
	if err != nil {
		if errors.Is(err, splice.ErrRefused) {
			return status.Error(codes.FailedPrecondition,
				err.Error())
		}

		return err
	}

	for {
		select {
		case u, ok := <-updates:
			if !ok {
				return nil
			}
			msg := &lnrpc.SpliceUpdate{
				State:          string(u.State),
				Reason:         u.Reason,
				ReasonFromPeer: u.ReasonFromPeer,
			}
			if u.Txid != [32]byte{} {
				msg.Txid = u.Txid.String()
				msg.NewChannelPoint = fmt.Sprintf("%v:%d",
					u.Txid, u.FundingIndex)
			}
			if err := stream.Send(msg); err != nil {
				machine.Cancel()
				return err
			}

		case <-stream.Context().Done():
			machine.Cancel()
			return stream.Context().Err()

		case <-r.quit:
			return nil
		}
	}
}

// addSpliceFields fills a channel listing's splice fields: the current
// funding output as its channel point and the original beside it, whether a
// splice can start, the splice in progress, the last that ended, and the
// local balance as the least over every funding while one is pending.
func (r *rpcServer) addSpliceFields(ch *channeldb.OpenChannel,
	channel *lnrpc.Channel) {

	channel.ChannelPoint = ch.CurrentFundingOutpoint().String()
	channel.OriginalChannelPoint = ch.FundingOutpoint.String()

	// The scid the graph and new route hints know the channel by.
	// Forwarding records keep the one it was opened with.
	channel.OriginalChanId = ch.ShortChanID().ToUint64()
	if current := ch.CurrentShortChanID(); current != ch.ShortChanID() {
		channel.ChanId = current.ToUint64()
	}
	channel.SpliceStatus, _ = r.spliceStatus(ch)

	records, err := ch.SpliceRecords()
	if err != nil {
		rpcsLog.Errorf("ChannelPoint(%v): unable to read splices: %v",
			ch.FundingOutpoint, err)
	}
	// A splice the peer replaced has a record per transaction: one is
	// shown (shownSplice). What we can count on is the least on any of
	// them.
	latest := shownSplice(records)
	least := leastCandidateBalance(
		records, lnwire.NewMSatFromSatoshis(
			btcutil.Amount(channel.LocalBalance),
		),
	)
	channel.LocalBalance = int64(least.ToSatoshis())
	if latest != nil {
		channel.PendingSplice = rpcPendingSplice(latest)
		if spliceWentOut(latest) {
			relay := r.spliceRelay(ch, latest.Txid())
			channel.PendingSplice.RelayStatus = string(relay.State)
			channel.PendingSplice.RelayReason = relay.Reason
		}
		setSpliceDepth(
			channel.PendingSplice, r.spliceConfirmations(latest),
			spliceLockDepth(
				ch.NumConfsRequired,
				latest.PeerAtRisk(),
				r.cfg.ProtocolOptions.SplicePeerInputConfs,
			),
		)
	}

	last, err := ch.LastSplice()
	if err == nil && last != nil {
		initiator := lnrpc.Initiator_INITIATOR_REMOTE
		if last.WeInitiated {
			initiator = lnrpc.Initiator_INITIATOR_LOCAL
		}
		channel.LastSplice = &lnrpc.LastSplice{
			Initiator:      initiator,
			LocalDeltaSat:  int64(last.LocalDelta),
			RemoteDeltaSat: int64(last.RemoteDelta),
			Txid:           last.Txid.String(),
			Time:           last.Time,
			Outcome:        last.Outcome,
			Reason:         last.Reason,
			ReasonFromPeer: last.ReasonFromPeer,
		}
		if last.Txid == [32]byte{} {
			channel.LastSplice.Txid = ""
		}
	}
}

// spliceConfirmations returns the confirmations of a splice's new funding
// output, 0 while it is unconfirmed or can't be checked. One backend call,
// only for a splice that went out.
func (r *rpcServer) spliceConfirmations(rec *channeldb.SpliceRecord) uint32 {
	if !spliceWentOut(rec) {
		return 0
	}
	chain := &spliceChain{wallet: r.server.cc.Wallet.WalletController}
	_, confs, _, err := chain.ConfirmedOutput(rec.OutPoint)
	if err != nil {
		return 0
	}

	return confs
}

// How long a peer may take over the steps after a splice confirms before
// the channel's status says so: splice_locked within 144 blocks of our
// lock depth, and its half of the announcement within 72 blocks of the
// six-block announcement depth.
const (
	spliceLockedGrace   = 144
	spliceAnnounceGrace = 6 + 72
)

// peerNotLocked reports whether we sent splice_locked for the channel's
// splice and the peer, spliceLockedGrace blocks past our lock depth, still
// hasn't.
func (r *rpcServer) peerNotLocked(ch *channeldb.OpenChannel) bool {
	records, err := ch.SpliceRecords()
	if err != nil {
		return false
	}
	for _, rec := range records {
		n := rec.Negotiation
		if rec.Retired || n == nil || !n.LockedSent || n.LockedReceived {
			continue
		}
		depth := spliceLockDepth(
			ch.NumConfsRequired,
			rec.PeerAtRisk(),
			r.cfg.ProtocolOptions.SplicePeerInputConfs,
		)
		if spliceOverdue(r.spliceConfirmations(rec), depth,
			spliceLockedGrace) {

			return true
		}
	}

	return false
}

// peerNotAnnouncing reports whether a public channel moved by a splice is
// still unannounced on its new funding spliceAnnounceGrace blocks after it
// confirmed: the peer never sent its half.
func (r *rpcServer) peerNotAnnouncing(ch *channeldb.OpenChannel) bool {
	if !r.server.spliceAnnouncementPending(ch) {
		return false
	}
	_, best, err := r.server.cc.ChainIO.GetBestBlock()
	if err != nil || best <= 0 {
		return false
	}
	confirmed := ch.CurrentShortChanID().BlockHeight
	if confirmed == 0 || uint32(best) < confirmed {
		return false
	}

	return spliceOverdue(
		uint32(best)-confirmed+1, 0, spliceAnnounceGrace,
	)
}

// spliceOverdue reports whether confirmations are grace past a depth.
func spliceOverdue(confs, depth, grace uint32) bool {
	return confs >= depth+grace
}

// spliceRelay returns what the channel's splice machine knows of a splice
// transaction's relay; unknown while the peer is offline.
func (r *rpcServer) spliceRelay(ch *channeldb.OpenChannel,
	txid chainhash.Hash) splice.RelayStatus {

	unknown := splice.RelayStatus{State: splice.RelayUnknown}
	p, err := r.server.FindPeer(ch.IdentityPub)
	if err != nil {
		return unknown
	}
	m, ok := p.SpliceMachine(lnwire.NewChanIDFromOutPoint(
		ch.FundingOutpoint,
	))
	if !ok {
		return unknown
	}

	return m.RelayStatus(txid)
}

// spliceFeeRate returns a splice record's fee rate, 0 before it has one.
func spliceFeeRate(rec *channeldb.SpliceRecord) uint64 {
	if rec.Negotiation == nil {
		return 0
	}

	return rec.Negotiation.FeeRate
}

// spliceWentOut reports whether a splice's transaction is complete, both
// sides' signatures in: from then this node broadcasts it.
func spliceWentOut(rec *channeldb.SpliceRecord) bool {
	n := rec.Negotiation

	return n != nil && n.OurTxSignatures != nil &&
		n.TheirTxSignatures != nil
}

// spliceBalances returns what a splice adds to a channel balance's pending
// fields: our contribution, once the transaction went out (until then the
// wallet still counts the coins it spends), and what it pays this node's
// wallet while unconfirmed (the wallet counts that as unconfirmed; once
// confirmed, as confirmed).
func spliceBalances(rec *channeldb.SpliceRecord, confirmations func() uint32,
	ourOutputs func(*wire.MsgTx, uint32) btcutil.Amount) (btcutil.Amount,
	btcutil.Amount) {

	// A dead one's coins are the wallet's again.
	if rec.Retired || !spliceWentOut(rec) || rec.Negotiation.Dead {
		return 0, 0
	}

	var in, toWallet btcutil.Amount
	if rec.LocalDelta > 0 {
		in = rec.LocalDelta
	}
	if rec.LocalDelta < 0 && rec.Negotiation.Tx != nil &&
		confirmations() == 0 {

		toWallet = ourOutputs(rec.Negotiation.Tx, rec.OutPoint.Index)
	}

	return in, toWallet
}

// setSpliceDepth adds a pending splice's confirmations and lock depth; one
// with a confirmation is "confirmed" while it waits for the lock depth.
func setSpliceDepth(ps *lnrpc.PendingSplice, confs, lockDepth uint32) {
	ps.Confirmations = confs
	ps.LockDepth = lockDepth
	if confs > 0 && ps.State == "broadcast" {
		ps.State = "confirmed"
	}
}

// leastCandidateBalance returns the least of local and our balance on each
// pending splice's commitment: what we can count on whichever confirms. A
// dead one never will, and a close pays out the current funding's balance.
func leastCandidateBalance(records []*channeldb.SpliceRecord,
	local lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	least := local
	for _, rec := range records {
		dead := rec.Negotiation != nil &&
			(rec.Negotiation.Dead || rec.Negotiation.Disowned)
		if rec.Retired || dead || rec.LocalCommit == nil {
			continue
		}
		least = min(least, rec.LocalCommit.LocalBalance)
	}

	return least
}

// shownSplice returns the record of the splice transaction shown for a
// channel: of those not retired, one that can still confirm before a dead
// one, then the highest fee rate, the latest.
func shownSplice(records []*channeldb.SpliceRecord) *channeldb.SpliceRecord {
	dead := func(r *channeldb.SpliceRecord) bool {
		n := r.Negotiation
		return n != nil && (n.Dead || n.Disowned)
	}
	var shown *channeldb.SpliceRecord
	for _, rec := range records {
		switch {
		case rec.Retired:
		case shown == nil:
			shown = rec
		case dead(shown) != dead(rec):
			if dead(shown) {
				shown = rec
			}
		case spliceFeeRate(rec) >= spliceFeeRate(shown):
			shown = rec
		}
	}

	return shown
}

// rpcPendingSplice describes a pending splice from its record.
func rpcPendingSplice(rec *channeldb.SpliceRecord) *lnrpc.PendingSplice {
	ps := &lnrpc.PendingSplice{
		Initiator:       lnrpc.Initiator_INITIATOR_REMOTE,
		State:           "negotiating",
		LocalDeltaSat:   int64(rec.LocalDelta),
		RemoteDeltaSat:  int64(rec.RemoteDelta),
		Txid:            rec.Txid().String(),
		NewChannelPoint: rec.OutPoint.String(),
		NewCapacitySat:  int64(rec.Capacity),
	}

	n := rec.Negotiation
	if n == nil {
		return ps
	}
	// Ours too when the peer replaced a splice we started.
	if n.WeInitiated || n.OursReplaced {
		ps.Initiator = lnrpc.Initiator_INITIATOR_LOCAL
	}
	ps.SatPerVbyte = uint64(chainfee.SatPerKWeight(n.FeeRate).FeePerVByte())
	ps.BroadcastHeight = n.BroadcastHeight
	ps.WeLocked = n.LockedSent
	ps.PeerLocked = n.LockedReceived
	// Disowned by the peer: it won't go through unless the peer
	// publishes it after all, and a normal close ends it.
	ps.Dead = n.Dead || n.Disowned

	switch {
	case n.LockedSent:
		ps.State = "confirmed"
	case n.OurTxSignatures != nil && n.TheirTxSignatures != nil:
		ps.State = "broadcast"
	case n.OurTxSignatures != nil || rec.SpliceSig != nil:
		ps.State = "signed"
	}

	// The fee: what the inputs spend less what the outputs pay.
	if n.Tx != nil {
		var in, out int64
		for _, i := range n.Inputs {
			in += int64(i.Value)
		}
		for _, o := range n.Tx.TxOut {
			out += int64(o.Value)
		}
		if in > out {
			ps.FeeSat = in - out
		}
		if n.WeInitiated {
			ps.OurFeeSat = ps.FeeSat
		}
	}

	return ps
}

// addSpliceBalances adds the splice amounts to a channel balance: our
// contributions to splices that haven't locked (in a channel once they do),
// our wallet outputs in splices not yet confirmed, and the local balance as
// the least over every funding of a channel with a splice pending.
func (r *rpcServer) addSpliceBalances(channels []*channeldb.OpenChannel,
	resp *lnrpc.ChannelBalanceResponse) {

	var (
		in, toWallet btcutil.Amount
		lower        lnwire.MilliSatoshi
	)
	for _, ch := range channels {
		records, err := ch.SpliceRecords()
		if err != nil {
			rpcsLog.Errorf("ChannelPoint(%v): unable to read "+
				"splices: %v", ch.FundingOutpoint, err)
			continue
		}

		// The splice's coins once: every transaction of a replaced
		// splice spends them. None for one the backend refused, its
		// coins the wallet's again.
		if rec := shownSplice(records); rec != nil &&
			r.spliceRelay(ch, rec.Txid()).State !=
				splice.RelayRejected {

			recIn, recToWallet := spliceBalances(
				rec, func() uint32 {
					return r.spliceConfirmations(rec)
				}, r.ourOutputs,
			)
			in += recIn
			toWallet += recToWallet
		}

		local := ch.LocalCommitment.LocalBalance
		lower += local - leastCandidateBalance(records, local)
	}

	resp.PendingSpliceInSat = int64(in)
	resp.PendingSpliceToWalletSat = int64(toWallet)
	if lower > 0 && resp.LocalBalance != nil &&
		lnwire.MilliSatoshi(resp.LocalBalance.Msat) >= lower {

		local := lnwire.MilliSatoshi(resp.LocalBalance.Msat) - lower
		resp.LocalBalance = &lnrpc.Amount{
			Sat: uint64(local.ToSatoshis()), Msat: uint64(local),
		}
		resp.Balance = int64(local.ToSatoshis())
	}
}

// ourOutputs sums the outputs of a splice transaction paying this node's
// wallet, the funding output aside.
func (r *rpcServer) ourOutputs(tx *wire.MsgTx,
	fundingIndex uint32) btcutil.Amount {

	var sum btcutil.Amount
	for i, out := range tx.TxOut {
		if uint32(i) == fundingIndex {
			continue
		}
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			out.PkScript, r.cfg.ActiveNetParams.Params,
		)
		if err != nil || len(addrs) != 1 {
			continue
		}
		if r.server.cc.Wallet.IsOurAddress(addrs[0]) {
			sum += btcutil.Amount(out.Value)
		}
	}

	return sum
}

// rpcSpliceEvent describes a splice step for the channel event stream.
func rpcSpliceEvent(e channelnotifier.SpliceEvent) *lnrpc.ChannelEventUpdate {
	txid := ""
	if e.Txid != [32]byte{} {
		txid = e.Txid.String()
	}

	return &lnrpc.ChannelEventUpdate{
		Type: lnrpc.ChannelEventUpdate_SPLICE_EVENT,
		Channel: &lnrpc.ChannelEventUpdate_SpliceEvent{
			SpliceEvent: &lnrpc.SpliceEvent{
				ChannelPoint:         e.ChannelPoint.String(),
				OriginalChannelPoint: e.OriginalChannelPoint.String(),
				Kind:                 e.Kind,
				Txid:                 txid,
				Reason:               e.Reason,
				ReasonFromPeer:       e.ReasonFromPeer,
			},
		},
	}
}

// currentFunding returns the funding output a channel named by either of its
// outpoints is on now, which its graph edge names; op itself for anything
// else.
func (r *rpcServer) currentFunding(op wire.OutPoint) wire.OutPoint {
	ch, err := r.server.chanStateDB.FetchChannel(op)
	if err != nil {
		return op
	}

	return ch.CurrentFundingOutpoint()
}

// hasShutdownInfo reports whether we sent shutdown on the channel.
func hasShutdownInfo(ch *channeldb.OpenChannel) bool {
	info, err := ch.ShutdownInfo()

	return err == nil && info.IsSome()
}
