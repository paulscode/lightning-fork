package funding

import (
	"errors"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// errUnsupportedChannelType is an error returned when a specific
	// channel commitment type is being explicitly negotiated but either
	// peer of the channel does not support it.
	errUnsupportedChannelType = errors.New("requested channel type " +
		"not supported")

	// errUnifiedSigsRequired is returned when a new channel's type lacks
	// option_unified_sigs on a node that signs under the unified hash.
	errUnifiedSigsRequired = errors.New("a new channel needs " +
		"option_unified_sigs: the peer does not support it, or the " +
		"channel type asked for (such as taproot) cannot carry it")

	// ErrDeprecatedChanType is returned when settling on the legacy
	// commitment type is the only option left, either because the caller of
	// our own RPC asked for it or because automatic selection would have
	// fallen back to it. We keep operating the legacy channels we already
	// have, but no longer open new ones.
	//
	// Unlike lnwire.ErrChanTypeDeprecated, which we send to a peer whose
	// proposal we reject, this never goes on the wire. The audience is our
	// own operator, who can act on the answer, so it spells out what to use
	// instead.
	ErrDeprecatedChanType = errors.New("the legacy commitment type is " +
		"deprecated, new channels must use the static remote key " +
		"commitment type or later")
)

// requireUnifiedSigs refuses a new channel without option_unified_sigs, on a
// node that follows the BLAKE2b proof of work rules and signs under the
// unified hash, which is every node that advertises the bit (always on
// mainnet). BOLT 2 asks this of both ends: the funder MUST include the bit in
// channel_type, and the fundee MUST fail an open whose type lacks it. A
// channel without it signs its commitment, HTLC and closing transactions so
// that they also verify for a node without these rules, and the fundee cannot
// see where the funder's inputs came from, so the channel type is the one
// thing either side can check.
//
// A nil type, which no open carries any more now that BOLT 2 requires an
// explicit channel_type, is refused all the same. Taproot types never carry it (see withUnifiedSigs), so they are
// refused too until the bit is defined for MuSig2 signatures. Existing
// channels are not affected: this runs only when a channel is opened.
func requireUnifiedSigs(chanType *lnwire.ChannelType,
	local *lnwire.FeatureVector) error {

	if !local.HasFeature(lnwire.UnifiedSigsOptional) {
		return nil
	}

	if chanType == nil {
		return errUnifiedSigsRequired
	}

	features := lnwire.RawFeatureVector(*chanType)
	if !features.IsSet(lnwire.UnifiedSigsRequired) {
		return errUnifiedSigsRequired
	}

	return nil
}

// withUnifiedSigs returns chanType with option_unified_sigs set, when both
// peers can do it and the type is one it applies to.
//
// This exists because the bit is not something the operator chooses. It binds
// the channel's bilateral signatures to this chain, so it belongs on every
// channel that can carry it, and a caller naming a commitment type is naming
// the shape of the transactions rather than which chain they are for.
//
// Without this, asking for a type by name produced a channel whose signatures
// were plain SIGHASH_ALL while the default for that same commitment type
// produced 0x21. Confirmed on chain before it was fixed: `--channel_type
// anchors` between two of these nodes closed with `01 01` in the witness.
// Nothing reported the difference, and privkeyio's build refuses such a type
// outright, so the channel was both unbound and unable to interoperate.
//
// Taproot is excluded deliberately, for the reason explicitNegotiateCommitment-
// Type gives: there the commitment signature is a MuSig2 partial signature over
// a BIP341 digest, so opting in is a wire change rather than a hash type.
func withUnifiedSigs(chanType lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) lnwire.ChannelType {

	if !hasFeatures(local, remote, lnwire.UnifiedSigsOptional) {
		return chanType
	}

	features := lnwire.RawFeatureVector(chanType)
	if isTaprootType(features) {
		return chanType
	}

	withBit := features.Clone()
	withBit.Set(lnwire.UnifiedSigsRequired)

	return lnwire.ChannelType(*withBit)
}

// isTaprootType reports whether a channel type is one whose commitment is
// signed with MuSig2, which the unified bit cannot yet be combined with: the
// simple taproot types and the taproot overlay type that Taproot Assets
// channels use, which is taproot underneath.
func isTaprootType(features lnwire.RawFeatureVector) bool {
	return features.IsSet(lnwire.SimpleTaprootChannelsRequiredFinal) ||
		features.IsSet(lnwire.SimpleTaprootChannelsRequiredStaging) ||
		features.IsSet(lnwire.SimpleTaprootOverlayChansRequired)
}

// funderChannelType is the channel type a funder proposes when its caller
// named one: the named type, with the unified bit added when both sides
// support it.
//
// Only a funder does this. A fundee answers the type the funder proposed as
// proposed, or refuses it, since a funder that is sent back a type it did not
// propose aborts the open.
func funderChannelType(desired *lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) *lnwire.ChannelType {

	if desired == nil {
		return nil
	}

	augmented := withUnifiedSigs(*desired, local, remote)

	return &augmented
}

// negotiateCommitmentType determines the commitment type of a newly opened
// channel. If desiredChanType is provided, it is validated against the
// commitment features supported by both peers. Otherwise, a default type is
// selected from those features.
//
// The legacy commitment type is never selected, whether it was requested
// explicitly or would only have been reached by falling back.
//
// On success, the returned ChannelType is non-nil and is signaled on the wire.
// An error is returned if the requested type is unsupported or deprecated, or
// if no supported default type can be selected.
func negotiateCommitmentType(desiredChanType *lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType, error) {

	// If a specific channel type was provided, verify it's supported.
	if desiredChanType != nil {
		commitType, err := explicitNegotiateCommitmentType(
			*desiredChanType, local, remote,
		)

		return desiredChanType, commitType, err
	}

	// No specific channel type was requested. Select a default type based
	// on locally-known feature compatibility. This default is then sent
	// explicitly over the wire.
	defaultChanType, commitType, err := selectDefaultChannelType(
		local, remote,
	)
	if err != nil {
		return nil, 0, err
	}

	return defaultChanType, commitType, nil
}

// explicitNegotiateCommitmentType attempts to explicitly negotiate for a
// specific channel type. Since the channel type is comprised of a set of even
// feature bits, we also make sure each feature is supported by both peers. An
// error is returned if either peer does not support said channel type.
func explicitNegotiateCommitmentType(channelType lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) (lnwallet.CommitmentType, error) {

	channelFeatures := lnwire.RawFeatureVector(channelType)

	// The unified signature hash is orthogonal to the commitment type: it
	// changes the digest every bilateral signature commits to, and nothing
	// about the transactions themselves. Rather than double every case
	// below, take it off, decide the commitment type from what is left,
	// and let the caller keep the bit on the type it returns.
	if channelFeatures.IsSet(lnwire.UnifiedSigsRequired) {
		if !hasFeatures(local, remote, lnwire.UnifiedSigsOptional) {
			return 0, errUnsupportedChannelType
		}

		// Not on a taproot channel. There the commitment signature is
		// a MuSig2 partial signature over a BIP341 digest, and making
		// that opt in is a wire change (a 65-byte signature in
		// commitment_signed) rather than a different hash type. Until
		// that is specified, refusing is better than agreeing to a
		// channel whose two sides would sign different digests.
		if isTaprootType(channelFeatures) {
			return 0, errUnsupportedChannelType
		}

		stripped := channelFeatures.Clone()
		stripped.Unset(lnwire.UnifiedSigsRequired)
		channelFeatures = *stripped
	}

	switch {
	// Lease script enforcement + anchors zero fee + static remote key +
	// zero conf + scid alias features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + zero conf + scid alias
	// features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Lease script enforcement + anchors zero fee + static remote key +
	// zero conf features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + zero conf features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Lease script enforcement + anchors zero fee + static remote key +
	// option-scid-alias features only.
	case channelFeatures.OnlyContains(
		lnwire.ScidAliasRequired,
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ScidAliasOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + option-scid-alias features
	// only.
	case channelFeatures.OnlyContains(
		lnwire.ScidAliasRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ScidAliasOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Lease script enforcement + anchors zero fee + static remote key
	// features only.
	case channelFeatures.OnlyContains(
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key features only.
	case channelFeatures.OnlyContains(
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Static remote key feature only.
	case channelFeatures.OnlyContains(lnwire.StaticRemoteKeyRequired):
		if !hasFeatures(local, remote, lnwire.StaticRemoteKeyOptional) {
			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeTweakless, nil

	// Simple taproot channels only (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels only (staging feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with scid only (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels with scid only (staging feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with zero conf only (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
		lnwire.ZeroConfRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
			lnwire.ZeroConfOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels with zero conf only (staging feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ZeroConfRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ZeroConfOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with scid and zero conf (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels with scid and zero conf (staging feature
	// bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels overlay only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// Simple taproot overlay channels with scid only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// Simple taproot overlay channels with zero conf only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
		lnwire.ZeroConfRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
			lnwire.ZeroConfOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// Simple taproot overlay channels with scid and zero conf.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// An empty channel type asks for the legacy commitment type, which was
	// removed from the spec in 2024 and which we refuse outright, not by
	// configuration. Note that this branch performs no feature check of its
	// own, since the legacy type predates feature bits entirely: any peer
	// sending an empty channel_type TLV used to get a legacy channel out of
	// us no matter what either side signalled.
	case channelFeatures.IsEmpty():
		return 0, lnwire.ErrChanTypeDeprecated

	default:
		return 0, errUnsupportedChannelType
	}
}

// selectDefaultChannelType selects a default channel type by choosing the most
// preferred non-taproot type supported by the local and remote features.
// Taproot channels must be requested explicitly, so that defaults stay on
// channel types usable for both public and private channels.
//
// An error is returned if there is no mutually supported type above the legacy
// one, which we no longer open.
//
// TODO(yy): Revisit taproot channel selection once public taproot channel
// announcements are supported.
func selectDefaultChannelType(local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType, error) {

	// If both peers are signalling support for anchor commitments with
	// zero-fee HTLC transactions, we'll use this type.
	if hasFeatures(local, remote, lnwire.AnchorsZeroFeeHtlcTxOptional) {
		chanType := withUnifiedSigs(
			lnwire.ChannelType(*lnwire.NewRawFeatureVector(
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.StaticRemoteKeyRequired,
			)), local, remote,
		)

		return &chanType, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			nil
	}

	// Since we don't want to support the "legacy" anchor type, we will fall
	// back to static remote key if the nodes don't support the zero fee
	// HTLC tx anchor type.
	//
	// If both nodes are signaling the proper feature bit for tweakless
	// commitments, we'll use that.
	if hasFeatures(local, remote, lnwire.StaticRemoteKeyOptional) {
		chanType := withUnifiedSigs(
			lnwire.ChannelType(*lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
			)), local, remote,
		)

		return &chanType, lnwallet.CommitmentTypeTweakless, nil
	}

	// Without a mutually supported type above it, the only one left to fall
	// back on is the legacy type, which we never open. Either side failing
	// to signal static remote key is enough to end up here.
	return nil, 0, ErrDeprecatedChanType
}

// hasFeatures determines whether a set of features is supported by both the set
// of local and remote features.
func hasFeatures(local, remote *lnwire.FeatureVector,
	features ...lnwire.FeatureBit) bool {

	for _, feature := range features {
		if !local.HasFeature(feature) || !remote.HasFeature(feature) {
			return false
		}
	}
	return true
}
