//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The part a split payout is searched for: lnd's router halves a part that
// finds no path while the half is at least 10,000 sat, and sends at most
// sixteen parts, so one carries at least a sixteenth.
func TestPartToProbe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		amt, part uint64
		ok        bool
	}{
		{amt: 19_999_999},
		{amt: 20_000_000, part: 10_000_000, ok: true},
		{amt: 39_999_999, part: 19_999_999, ok: true},
		{amt: 40_000_000, part: 10_000_000, ok: true},
		{amt: 150_000_000, part: 18_750_000, ok: true},
		{amt: 1_000_000_000, part: 62_500_000, ok: true},
		{amt: 1_000_000_001, part: 62_500_001, ok: true},
	} {
		part, ok := partToProbe(tc.amt)
		require.Equal(t, tc.ok, ok, "amount %d", tc.amt)
		require.Equal(t, tc.part, part, "amount %d", tc.amt)
	}
}
