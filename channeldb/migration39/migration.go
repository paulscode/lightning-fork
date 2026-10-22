// Package migration39 marks a database that holds a channel with
// option_independent_secrets.
package migration39

import (
	"github.com/lightningnetwork/lnd/kvdb"
)

// MarkIndependentSecrets changes nothing in the data, and is never run: the
// database moves to this version when it first stores a channel with
// option_independent_secrets, in the same transaction. Its number is the
// point: an older binary refuses a database at a version it doesn't know, with
// a clear error. Without the bump, an older binary would fail to decode such a
// channel's revocation state while the channel is open or being resolved, and
// once it had closed, start and fail its closedchannels and pendingchannels
// calls.
//
// A node that never has such a channel stays at the version before, and can
// still go back to an older release.
func MarkIndependentSecrets(_ kvdb.RwTx) error {
	return nil
}
