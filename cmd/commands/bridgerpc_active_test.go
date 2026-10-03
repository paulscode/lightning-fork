//go:build bridgerpc
// +build bridgerpc

package commands

import (
	"math"
	"testing"
)

// A participant's root key id is how the bridge knows them, so an id that was
// revoked must not be handed to the next participant, and one in use never.
func TestFreshRootKeyIDsAreNotReused(t *testing.T) {
	t.Parallel()

	seen := map[uint64]bool{}
	used := []uint64{0, firstParticipantRootKeyID}
	for range 1000 {
		id, err := freshRootKeyID(used)
		if err != nil {
			t.Fatal(err)
		}
		if id < firstParticipantRootKeyID || id > math.MaxInt64 {
			t.Fatalf("id %d out of range", id)
		}
		if seen[id] || id == firstParticipantRootKeyID {
			t.Fatalf("id %d given twice", id)
		}
		seen[id] = true
	}
}
