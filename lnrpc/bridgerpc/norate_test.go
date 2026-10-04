//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/rate"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Starting without a rate: the bridge comes up and quotes nothing until the
// operator sets one, rather than keeping the node from starting.
// ---------------------------------------------------------------------------

// There is still no default rate: a configuration with none is valid, and
// what it means is "not yet", which the rate book answers by refusing quotes.
func TestNoConfiguredRateIsValid(t *testing.T) {
	t.Parallel()

	c := usable()
	c.FixedRate = 0
	require.NoError(t, c.Validate())
}

func TestARatebookWithNoRateRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0, time.Hour, nil)
	require.NoError(t, err)

	r, at := b.current()
	require.Zero(t, r)
	require.True(t, at.IsZero(), "no rate has no time it was set")
	require.True(t, b.expiresAt().IsZero())

	_, _, err = b.usable()
	require.ErrorIs(t, err, ErrNoRate)
	require.ErrorIs(t, err, rate.ErrStale,
		"a payer is told what an expired rate tells them")
	require.Equal(t, quote.CodePriceUnavailable, quote.CodeOf(err))
	require.Contains(t, err.Error(), "setrate")

	// A file holding no rate would be refused at the next start, so none
	// is written.
	_, err = os.Stat(filepath.Join(dir, rateFileName))
	require.True(t, errors.Is(err, os.ErrNotExist))

	// Opening again is the same state, not an error.
	_, err = openRatebook(dir, 0, time.Hour, nil)
	require.NoError(t, err)
}

func TestARateSetWithNoneConfiguredIsKept(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0, time.Hour, nil)
	require.NoError(t, err)
	_, _, err = b.set(0.005, 0)
	require.NoError(t, err)

	got, _, err := b.usable()
	require.NoError(t, err)
	require.Equal(t, 0.005, got)

	again, err := openRatebook(dir, 0, time.Hour, nil)
	require.NoError(t, err)
	got, _, err = again.usable()
	require.NoError(t, err)
	require.Equal(t, 0.005, got)

	// A rate configured since is still the newer decision.
	edited, err := openRatebook(dir, 0.006, time.Hour, nil)
	require.NoError(t, err)
	got, _, err = edited.usable()
	require.NoError(t, err)
	require.Equal(t, 0.006, got)
}

// Removing the configured rate is not a decision to forget the one set at
// runtime since: the operator would find their bridge quoting nothing for an
// edit that never mentioned the rate they set.
func TestRemovingTheConfiguredRateKeepsTheRuntimeOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0.004, time.Hour, nil)
	require.NoError(t, err)
	_, _, err = b.set(0.0048, 0.004)
	require.NoError(t, err)

	again, err := openRatebook(dir, 0, time.Hour, nil)
	require.NoError(t, err)
	got, _, err := again.usable()
	require.NoError(t, err)
	require.Equal(t, 0.0048, got)
}

// ---------------------------------------------------------------------------
// Exporting the supervised node's seed.
// ---------------------------------------------------------------------------

// The SHA256 node's seed controls its funds, so only a macaroon that could
// already move this node's may see it: a payment or read-only macaroon must
// not.
func TestExportSha256SeedNeedsAnAdminMacaroon(t *testing.T) {
	t.Parallel()

	ops := macPermissions["/bridgerpc.Bridge/ExportSha256Seed"]
	want := map[string]bool{
		"onchain:write": true, "offchain:write": true,
		"macaroon:generate": true, "signer:generate": true,
	}
	require.Len(t, ops, len(want))
	for _, op := range ops {
		require.True(t, want[op.Entity+":"+op.Action], "%+v", op)
	}
}

func TestExportSha256Seed(t *testing.T) {
	t.Parallel()

	f := &fakeNode{synced: true}
	deps := f.deps()
	deps.Network = "regtest"
	deps.DeriveSha256Seed = func() ([aezeed.EntropySize]byte, error) {
		return testEntropy, nil
	}
	// The bridge need not be on: an operator who turned it off still has
	// what that node holds.
	srv, _, err := New(&Config{Deps: deps})
	require.NoError(t, err)

	resp, err := srv.ExportSha256Seed(context.Background(),
		&ExportSha256SeedRequest{})
	require.NoError(t, err)

	var words aezeed.Mnemonic
	require.Len(t, resp.Mnemonic, len(words))
	copy(words[:], resp.Mnemonic)
	seed, err := words.ToCipherSeed(nil)
	require.NoError(t, err)
	require.Equal(t, testEntropy, seed.Entropy)

	require.True(t, strings.HasPrefix(resp.ExtendedMasterKey, "tprv"),
		"a regtest node's key is serialised for regtest")
	require.Equal(t, testIdentity(t), resp.IdentityPubkey)
	require.Equal(t, Sha256SeedBirthday.Unix(), resp.Birthday)
	require.Contains(t, resp.Derivation, "m/1017'/1'/1000'/0/0")
	require.Contains(t, resp.Derivation, Sha256SeedInfo)
}

func TestExportSha256SeedWithoutKeys(t *testing.T) {
	t.Parallel()

	srv, _, err := New(&Config{Deps: (&fakeNode{}).deps()})
	require.NoError(t, err)
	_, err = srv.ExportSha256Seed(context.Background(),
		&ExportSha256SeedRequest{})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// Status reports the supervised node while the bridge is waiting for it, which
// is when an operator most needs to know where it is.
func TestStatusReportsASupervisedNodeOnItsWay(t *testing.T) {
	t.Parallel()

	fake := &fakeSha256Node{}
	sup, cfg := newTestSupervisor(t, fake)
	cfg.ToSHA256 = true
	srv, _, err := New(cfg)
	require.NoError(t, err)
	srv.sup = sup

	// What the first attempt finds: no node started yet.
	require.ErrorIs(t, sup.prepare(context.Background()), errSha256Pending)

	resp, err := srv.Status(context.Background(), &StatusRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp.Sha256Node)
	require.Equal(t, "supervised", resp.Sha256Node.Mode)
	require.Equal(t, sha256Starting, resp.Sha256Node.State)
	require.Contains(t, resp.Sha256Node.Detail, "platform starts it")
	require.Empty(t, resp.Sha256Node.IdentityPubkey,
		"nothing is read from a node whose identity is unchecked")
}
