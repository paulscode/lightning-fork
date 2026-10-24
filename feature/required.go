package feature

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrUnknownRequired signals that a feature vector requires certain features
// that our node is unaware of or does not implement.
type ErrUnknownRequired struct {
	unknown []lnwire.FeatureBit
}

// NewErrUnknownRequired initializes an ErrUnknownRequired with the unknown
// feature bits.
func NewErrUnknownRequired(unknown []lnwire.FeatureBit) ErrUnknownRequired {
	return ErrUnknownRequired{
		unknown: unknown,
	}
}

// Error returns a human-readable description of the error.
func (e ErrUnknownRequired) Error() string {
	return fmt.Sprintf("feature vector contains unknown required "+
		"features: %v", e.unknown)
}

// ValidateRequired returns an error if the feature vector contains a non-zero
// number of unknown, required feature bits.
func ValidateRequired(fv *lnwire.FeatureVector) error {
	unknown := fv.UnknownRequiredFeatures()
	if len(unknown) > 0 {
		return NewErrUnknownRequired(unknown)
	}
	return nil
}

// ValidateRoutable returns an error if a node with this feature vector, taken
// from its node_announcement or from an invoice, can't be paid or routed
// through: it requires a feature bit we don't know, or it sets the even bit of
// option_independent_secrets. That bit is only valid in channel_type, and BOLT
// 3 has a node that receives it in node_announcement treat it as an unknown
// even bit, as a node that doesn't know the bit already does.
func ValidateRoutable(fv *lnwire.FeatureVector) error {
	if err := ValidateRequired(fv); err != nil {
		return err
	}

	if fv.IsSet(lnwire.IndependentSecretsRequired) {
		return NewErrUnknownRequired([]lnwire.FeatureBit{
			lnwire.IndependentSecretsRequired,
		})
	}

	return nil
}
