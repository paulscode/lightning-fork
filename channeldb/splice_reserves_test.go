package channeldb

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestSpliceReservesEncoding checks the record round-trips, keeps only the
// newest MaxLenientReserves sets, and refuses a count above it.
func TestSpliceReservesEncoding(t *testing.T) {
	t.Parallel()

	r := SpliceReserves{
		InForceLocal: 1_010_000, InForceRemote: 1_010_000,
		OpeningLocal: 10_000, OpeningRemote: 20_000,
		PeerCommitSeen: true,
		Lenient:        []ReservePair{{Local: 10_000, Remote: 20_000}},
	}
	b, err := encodeSpliceReserves(r)
	require.NoError(t, err)
	got, err := decodeSpliceReserves(b)
	require.NoError(t, err)
	require.Equal(t, r, got)

	none, err := encodeSpliceReserves(SpliceReserves{InForceLocal: 5})
	require.NoError(t, err)
	got, err = decodeSpliceReserves(none)
	require.NoError(t, err)
	require.Empty(t, got.Lenient)

	var many SpliceReserves
	for i := 0; i < MaxLenientReserves+3; i++ {
		many.Lenient = append(many.Lenient, ReservePair{
			Local: btcutil.Amount(i), Remote: btcutil.Amount(i),
		})
	}
	b, err = encodeSpliceReserves(many)
	require.NoError(t, err)
	got, err = decodeSpliceReserves(b)
	require.NoError(t, err)
	require.Len(t, got.Lenient, MaxLenientReserves)
	require.Equal(t, btcutil.Amount(3), got.Lenient[0].Local)

	b[33] = MaxLenientReserves + 1
	_, err = decodeSpliceReserves(b)
	require.Error(t, err)
}
