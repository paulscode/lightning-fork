package feature

import "github.com/lightningnetwork/lnd/lnwire"

// setDesc describes which feature bits should be advertised in which feature
// sets.
type setDesc map[lnwire.FeatureBit]map[Set]struct{}

// defaultSetDesc are the default set descriptors for generating feature
// vectors. Each set is annotated with the corresponding identifier from BOLT 9
// indicating where it should be advertised.
var defaultSetDesc = setDesc{
	// Even, so a peer that does not know it must close the connection.
	//
	// This was the odd bit until the chain_hash reversal, on the reasoning
	// that chain_hash was what kept this node off the other chain and the
	// bit was a courtesy. chain_hash is now the genesis hash both chains
	// share, so that reasoning is gone and nothing in init separates them
	// on its own: two nodes on different chains agree on chain_hash, and an
	// odd bit the other cannot read is ignored by definition.
	//
	// The even bit separates them symmetrically, using BOLT 1's existing
	// rule rather than a new one, and needs no cooperation from the other
	// side. Until this change the separation rested on
	// RequirePeerNetworks, which is a heuristic: it drops any peer that
	// sends no networks TLV, and sending it is optional. That is now off by
	// default precisely because this bit does the job, so there is no
	// longer a second thing quietly covering for it. Reverting this to the
	// odd bit would leave the two chains on one network.
	//
	// privkeyio's Core Lightning already sets 68 and not 69, and does not
	// require a peer to, so this is also what lets this node talk to
	// theirs: the last release, .9, does not know the bit at all and
	// refuses them over it.
	//
	// It is *not* safe for nodes in the field, and an earlier version of
	// this comment claimed otherwise. The check a peer applies is whether
	// the bit is known rather than whether it is also set, but lnwire only
	// learned the bit on 2026-09-15 and .9 was released on 2026-09-14, so
	// no released build knows it. Upgrading past this is a flag day either
	// way, because the chain_hash reversal on its own already is one:
	// measured, a build with this bit removed still cannot peer with .9,
	// which refuses it with "no common chain".
	//
	// Not set in invoices or offers: a payer that cannot read the bit must
	// still be refused, and the invoice prefix does that.
	lnwire.Blake2bRequired: {
		SetInit:    {}, // I+
		SetNodeAnn: {}, // N+
	},
	// Signalled so a peer knows a unified-signing channel can be
	// negotiated with this node. Whether one is depends on the channel
	// type both sides agree, not on this bit alone.
	lnwire.UnifiedSigsOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.DataLossProtectRequired: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.GossipQueriesOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.TLVOnionPayloadRequired: {
		SetInit:         {}, // I
		SetNodeAnn:      {}, // N
		SetInvoice:      {}, // 9
		SetInvoiceAmp:   {}, // 9A
		SetLegacyGlobal: {},
	},
	lnwire.StaticRemoteKeyRequired: {
		SetInit:         {}, // I
		SetNodeAnn:      {}, // N
		SetLegacyGlobal: {},
	},
	lnwire.UpfrontShutdownScriptOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.PaymentAddrRequired: {
		SetInit:       {}, // I
		SetNodeAnn:    {}, // N
		SetInvoice:    {}, // 9
		SetInvoiceAmp: {}, // 9A
	},
	lnwire.MPPOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
		SetInvoice: {}, // 9
	},
	lnwire.AnchorsZeroFeeHtlcTxOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.WumboChannelsOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.AMPOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.AMPRequired: {
		SetInvoiceAmp: {}, // 9A
	},
	lnwire.ExplicitChannelTypeRequired: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.KeysendOptional: {
		SetNodeAnn: {}, // N
	},
	lnwire.ScriptEnforcedLeaseOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ScidAliasOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ZeroConfOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.RouteBlindingOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
		SetInvoice: {}, // 9
	},
	lnwire.QuiescenceOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ShutdownAnySegwitOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.SimpleTaprootChannelsOptionalStaging: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.SimpleTaprootChannelsOptionalFinal: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.SimpleTaprootOverlayChansOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ExperimentalAccountabilityOptional: {
		SetNodeAnn: {}, // N
	},
	lnwire.RbfCoopCloseOptionalStaging: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.RbfCoopCloseOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.OnionMessagesOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
}
