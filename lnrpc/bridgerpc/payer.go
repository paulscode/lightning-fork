//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

// PayerAPIVersion is the version of the payer API this bridge speaks: the
// calls a wallet makes, the codes it branches on, and the bridge code it is
// given. It changes only when one of those does.
const PayerAPIVersion = 1

// operatorRootKeyID is the root key every macaroon the node creates for
// itself is baked under. A caller presenting one is the operator, who is not
// limited as a participant is.
const operatorRootKeyID = "0"

// unknownParticipant stands for a caller whose macaroon cannot be read. Such a
// caller is limited rather than taken for the operator: authentication has
// already passed, so this should not happen, and if it does the cautious
// reading is the one that bounds what they can reserve.
const unknownParticipant = "unknown"

// participantOf is who is asking, for the limits each participant has on their
// own: the root key id of the caller's macaroon, or "" for the operator.
//
// A participant's macaroon is baked with a root key of its own, which is what
// makes it revocable alone (lncli deletemacaroonid) and what identifies it
// here. Nothing secret is read: the id is the macaroon's public identifier,
// and the macaroon itself was verified before this call was reached.
func participantOf(ctx context.Context) string {
	raw, err := macaroons.RawMacaroonFromContext(ctx)
	if err != nil {
		// No macaroon at all is a node running without them, where
		// every caller is the operator.
		return ""
	}

	bytes, err := hex.DecodeString(raw)
	if err != nil {
		return unknownParticipant
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(bytes); err != nil {
		return unknownParticipant
	}
	id := mac.Id()
	if len(id) < 2 || id[0] != byte(bakery.LatestVersion) {
		return unknownParticipant
	}
	decoded := &lnrpc.MacaroonId{}
	if err := proto.Unmarshal(id[1:], decoded); err != nil {
		return unknownParticipant
	}

	storage := string(decoded.GetStorageId())
	if storage == operatorRootKeyID || storage == "" {
		return ""
	}

	return storage
}

// refusal turns an error from quoting into a status a payer can act on: a
// stable code at the front of the message, then the sentence, and an HTTP
// status through the REST gateway that says whether to try again.
//
// Two kinds of error get a fixed sentence instead of their own. A swap that
// already exists for the hash belongs to whoever quoted it, so another payer
// holding the same invoice learns only that it exists, not its state. And an
// internal error can carry paths and database detail that are the operator's
// business: it goes to the log, and the payer is told to try later.
func refusal(err error) error {
	code := quote.CodeOf(err)
	msg := string(code) + ": " + err.Error()

	switch code {
	case quote.CodeInProgress:
		msg = string(code) + ": " + quote.ErrInProgress.Error()
	case quote.CodeAlreadyPaid:
		msg = string(code) + ": " + quote.ErrAlreadyPaid.Error()
	case quote.CodeNeedsOperator:
		msg = string(code) + ": " + quote.ErrNeedsOperator.Error()
	case quote.CodeInternal:
		log.Errorf("Bridge could not quote: %v", err)
		msg = string(code) + ": the bridge could not quote this " +
			"invoice; try again later"
	}

	switch code {
	case quote.CodeInProgress, quote.CodeAlreadyPaid,
		quote.CodeNeedsOperator:

		return status.Error(codes.AlreadyExists, msg)

	case quote.CodeLimit:
		return status.Error(codes.ResourceExhausted, msg)

	case quote.CodeUnavailable:
		return status.Error(codes.Unavailable, msg)

	case quote.CodeInternal:
		return status.Error(codes.Internal, msg)
	}

	return status.Error(codes.FailedPrecondition, msg)
}

// coded is an error with a code the quote package does not produce, in the
// same form as refusal.
func coded(c codes.Code, code, sentence string) error {
	return status.Error(c, code+": "+sentence)
}

// isUntracked reports whether a quote failed after its swap was recorded, so
// the swap must be driven anyway.
func isUntracked(err error) bool {
	return errors.Is(err, quote.ErrUntracked)
}
