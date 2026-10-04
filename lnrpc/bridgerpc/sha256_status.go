//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc/codes"
)

// sha256SummaryTimeout bounds reading the SHA256 node for Status, which an
// operator's screen is waiting on.
const sha256SummaryTimeout = 10 * time.Second

// sha256Summary is the SHA256 node as Status reports it: where it is, and what
// it holds.
//
// It is answered whether or not the bridge is up, because that node is the
// usual reason it is not, and an operator with a new node needs its balance and
// address to get any further. A node the bridge has not connected to yet is
// dialled for the purpose, with the bridge's own macaroon.
func (s *Server) sha256Summary(ctx context.Context) *Sha256Node {
	out := &Sha256Node{Mode: "external"}
	if s.sup != nil {
		out.Mode = "supervised"
		out.State, out.Detail = s.sup.report()

		// Before its identity is confirmed there is nothing this node
		// will read from it: it may not be ours.
		if s.remoteNode() == nil && out.State != sha256Ready {
			return out
		}
	}

	ctx, cancel := context.WithTimeout(ctx, sha256SummaryTimeout)
	defer cancel()

	remote := s.remoteNode()
	if remote == nil {
		conn, err := dialSHA256Node(s.cfg)
		if err != nil {
			out.State = sha256Unreachable
			out.Detail = err.Error()

			return out
		}
		defer conn.Close()
		remote = NewRemote(conn)
	}

	info, err := remote.main.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		out.State = sha256Unreachable
		out.Detail = fmt.Sprintf("the SHA256 Lightning node does not "+
			"answer: %v", err)

		return out
	}
	out.IdentityPubkey = info.GetIdentityPubkey()
	out.Uris = info.GetUris()
	out.SyncedToChain = info.GetSyncedToChain()
	out.BlockHeight = info.GetBlockHeight()
	out.ActiveChannels = info.GetNumActiveChannels()
	out.InactiveChannels = info.GetNumInactiveChannels()
	out.PendingChannels = info.GetNumPendingChannels()
	out.Peers = info.GetNumPeers()
	out.Version = info.GetVersion()

	// A balance that cannot be read is said to be unknown, never shown as
	// zero: an external node's macaroon may lack onchain:read, and "empty,
	// send coins" for a funded node sends an operator the wrong way.
	var unread []string
	if wb, err := remote.main.WalletBalance(
		ctx, &lnrpc.WalletBalanceRequest{},
	); err == nil {
		out.OnchainConfirmedSat = wb.GetConfirmedBalance()
		out.OnchainUnconfirmedSat = wb.GetUnconfirmedBalance()
	} else {
		unread = append(unread, "its on-chain balance")
	}

	// What the bridge can pay with is the same number it sizes swaps
	// against, so the screen and the quotes agree.
	if outbound, err := remote.Balance(ctx); err == nil {
		out.OutboundMsat = outbound
	} else {
		unread = append(unread, "what its channels can send")
	}
	if cb, err := remote.main.ChannelBalance(
		ctx, &lnrpc.ChannelBalanceRequest{},
	); err == nil {
		out.InboundMsat = cb.GetRemoteBalance().GetMsat()
	} else {
		unread = append(unread, "what its channels can receive")
	}

	switch {
	case !out.SyncedToChain:
		out.State = sha256Syncing
		out.Detail = fmt.Sprintf("catching up with the SHA256 chain "+
			"(at block %d)", out.BlockHeight)

	case len(unread) > 0:
		out.State = sha256Ready
		out.Detail = "ready, but " + strings.Join(unread, ", ") +
			" cannot be read with the macaroon the bridge has, so " +
			"those numbers show as zero and are not"

	case out.ActiveChannels == 0 && out.PendingChannels == 0 &&
		out.OnchainConfirmedSat+out.OnchainUnconfirmedSat == 0:

		out.State = sha256Ready
		out.Detail = "ready, and empty: send coins on the SHA256 chain " +
			"to its deposit address, then open a channel from it"

	case out.ActiveChannels == 0 && out.PendingChannels == 0:
		out.State = sha256Ready
		out.Detail = "ready, with coins and no channel: open one from " +
			"it to a well-connected node on the SHA256 chain"

	case out.ActiveChannels == 0:
		out.State = sha256Ready
		out.Detail = "ready; its channel is still confirming"

	case out.OutboundMsat == 0:
		out.State = sha256Ready
		out.Detail = "ready, with nothing it can send yet: its channels " +
			"hold no outbound balance"

	default:
		out.State = sha256Ready
		out.Detail = "ready"
	}
	if s.sup != nil {
		if note := s.sup.restoreStatus(); note != "" {
			out.Detail += "; " + note
		}
	}

	return out
}

// ExportSha256Seed shows how to restore the supervised SHA256 node without
// Lightning Fork.
//
// It is answered whether or not the bridge is enabled now: an operator who
// turned the bridge off still has whatever that node holds, and this is how
// they reach it with other tools.
func (s *Server) ExportSha256Seed(_ context.Context,
	_ *ExportSha256SeedRequest) (*ExportSha256SeedResponse, error) {

	if s.cfg.Deps == nil || s.cfg.Deps.DeriveSha256Seed == nil {
		return nil, coded(codes.Unavailable, "unavailable", "this "+
			"node cannot derive the SHA256 node's seed")
	}
	network := s.cfg.Deps.Network
	params, ok := sha256NetParams(network)
	if !ok {
		return nil, coded(codes.Unavailable, "unavailable",
			fmt.Sprintf("no SHA256 network matches %q", network))
	}

	entropy, err := s.cfg.Deps.DeriveSha256Seed()
	if err != nil {
		return nil, coded(codes.Internal, "internal", err.Error())
	}
	mnemonic, err := Sha256SeedMnemonic(entropy)
	if err != nil {
		return nil, coded(codes.Internal, "internal", err.Error())
	}
	xprv, err := Sha256SeedMasterKey(entropy, params)
	if err != nil {
		return nil, coded(codes.Internal, "internal", err.Error())
	}
	coin := coinTypeFor(network)
	key, err := Sha256NodeKey(entropy, coin)
	if err != nil {
		return nil, coded(codes.Internal, "internal", err.Error())
	}

	log.Infof("Bridge: the SHA256 node's recovery phrase was exported")

	return &ExportSha256SeedResponse{
		Mnemonic:          mnemonic[:],
		ExtendedMasterKey: xprv,
		Birthday:          Sha256SeedBirthday.Unix(),
		IdentityPubkey:    hex.EncodeToString(key.SerializeCompressed()),
		Derivation:        sha256DerivationText(coin),
	}, nil
}

// sha256DerivationText is the derivation in one sentence, for a person
// checking it with other tools.
func sha256DerivationText(coin uint32) string {
	return strings.Join([]string{
		fmt.Sprintf("Take the private key at m/1017'/%d'/%d'/0/%d of "+
			"this Lightning Fork wallet (lnd's own derivation, as "+
			"btcwallet does it).", coin, Sha256SeedFamily,
			Sha256SeedIndex),
		fmt.Sprintf("HKDF-SHA256 over its 32 bytes, with no salt and "+
			"info %q, gives 16 bytes of aezeed entropy.",
			Sha256SeedInfo),
		fmt.Sprintf("The wallet's birthday is %s.",
			Sha256SeedBirthday.Format("2006-01-02")),
		"See docs/bridge-sha256-node.md.",
	}, " ")
}

// sha256NetParams is the SHA256 chain's parameters for a network as GetInfo
// names it, for serialising its keys.
func sha256NetParams(network string) (*chaincfg.Params, bool) {
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, true
	case "testnet":
		return &chaincfg.TestNet3Params, true
	case "testnet4":
		return &chaincfg.TestNet4Params, true
	case "signet":
		return &chaincfg.SigNetParams, true
	case "regtest":
		return &chaincfg.RegressionNetParams, true
	case "simnet":
		return &chaincfg.SimNetParams, true
	}

	return nil, false
}
