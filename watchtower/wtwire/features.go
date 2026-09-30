package wtwire

import "github.com/lightningnetwork/lnd/lnwire"

// FeatureNames holds a mapping from each feature bit understood by this
// implementation to its common name.
var FeatureNames = map[lnwire.FeatureBit]string{
	AltruistSessionsRequired: "altruist-sessions",
	AltruistSessionsOptional: "altruist-sessions",
	AnchorCommitRequired:     "anchor-commit",
	AnchorCommitOptional:     "anchor-commit",
	TaprootCommitRequired:    "taproot-commit",
	TaprootCommitOptional:    "taproot-commit",
	Blake2bRequired:          "blake2b",
	Blake2bOptional:          "blake2b",
}

const (
	// AltruistSessionsRequired specifies that the advertising node requires
	// the remote party to understand the protocol for creating and updating
	// watchtower sessions.
	AltruistSessionsRequired lnwire.FeatureBit = 0

	// AltruistSessionsOptional specifies that the advertising node can
	// support a remote party who understand the protocol for creating and
	// updating watchtower sessions.
	AltruistSessionsOptional lnwire.FeatureBit = 1

	// AnchorCommitRequired specifies that the advertising tower requires
	// the remote party to negotiate sessions for protecting anchor
	// channels.
	AnchorCommitRequired lnwire.FeatureBit = 2

	// AnchorCommitOptional specifies that the advertising tower allows the
	// remote party to negotiate sessions for protecting anchor channels.
	AnchorCommitOptional lnwire.FeatureBit = 3

	// TaprootCommitRequired specifies that the advertising tower requires
	// the remote party to negotiate sessions for protecting taproot
	// channels.
	TaprootCommitRequired lnwire.FeatureBit = 4

	// TaprootCommitOptional specifies that the advertising tower allows the
	// remote party to negotiate sessions for protecting taproot channels.
	TaprootCommitOptional lnwire.FeatureBit = 5

	// Blake2bRequired says the advertiser follows the BLAKE2b proof of work
	// rules, which took effect on Bitcoin at block 961,640. It is the
	// watchtower counterpart of option_blake2b, with the same numbers.
	//
	// A tower is told the chain only by the genesis hash in Init, which a
	// tower that has not upgraded carries too. Such a tower would accept
	// sessions and then never see the breaches it guards against, which
	// happen in blocks it does not follow. The even
	// bit makes an implementation without these rules refuse at Init, and
	// CheckRemoteInit makes this one refuse a peer that does not set it.
	Blake2bRequired lnwire.FeatureBit = 512

	// Blake2bOptional is the optional form of Blake2bRequired. Either form
	// satisfies CheckRemoteInit.
	Blake2bOptional lnwire.FeatureBit = 513
)
