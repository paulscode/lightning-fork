package channeldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpenableDBVersions checks the versions a copying tool must accept:
// the one before the splicing mark, the splicing mark, and the latest, which
// marks a database with a channel with option_independent_secrets.
func TestOpenableDBVersions(t *testing.T) {
	t.Parallel()

	versions := OpenableDBVersions()
	require.Equal(t, []uint32{
		splicingDBVersion - 1, splicingDBVersion,
		independentSecretsDBVersion,
	}, versions)
	require.Equal(t, LatestDBVersion(), versions[2])
}
