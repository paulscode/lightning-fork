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

// TestTargetVersions checks which versions are run as migrations: every one
// but the independent secrets mark, and without the splicing mark when
// splicing is off. The marks are picked by number, so a migration appended
// after them still runs.
func TestTargetVersions(t *testing.T) {
	t.Parallel()

	numbers := func(versions []mandatoryVersion) []uint32 {
		var n []uint32
		for _, v := range versions {
			n = append(n, v.number)
		}

		return n
	}

	all := numbers(dbVersions)
	require.Equal(t, uint32(independentSecretsDBVersion), all[len(all)-1])
	require.Equal(t, all[:len(all)-1], numbers(targetVersions(true)))
	require.Equal(t, all[:len(all)-2], numbers(targetVersions(false)))

	later := append(append([]mandatoryVersion{}, dbVersions...),
		mandatoryVersion{number: independentSecretsDBVersion + 1})
	next := uint32(independentSecretsDBVersion + 1)
	require.Equal(t, append(append([]uint32{}, all[:len(all)-1]...), next),
		numbers(filterTargetVersions(later, true)))
	require.Equal(t, append(append([]uint32{}, all[:len(all)-2]...), next),
		numbers(filterTargetVersions(later, false)))
}
