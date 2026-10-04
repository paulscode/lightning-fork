//go:build bridgerpc
// +build bridgerpc

package lnd

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc/bridgerpc"
	"github.com/stretchr/testify/require"
)

func bridgeTestConfig(t *testing.T, sub *bridgerpc.Config) (*Config, string) {
	t.Helper()

	lndDir := t.TempDir()
	cfg := &Config{
		LndDir:          lndDir,
		ActiveNetParams: chainreg.BitcoinRegTestNetParams,
		SubRPCServers:   &subRPCServerConfigs{BridgeRPC: sub},
	}

	return cfg, filepath.Join(lndDir, "data", "chain", "bitcoin", "regtest")
}

// Turning the supervised node on is the whole configuration: everything the
// bridge needs to reach it is derived, and the layout is the one the platform
// packages and docs/bridge-sha256-node.md rely on.
func TestSupervisedPathsAreDerived(t *testing.T) {
	sub := &bridgerpc.Config{
		Enabled: true, Supervised: true, ToSHA256: true,
	}
	cfg, netDir := bridgeTestConfig(t, sub)

	require.NoError(t, validateBridgeConfig(cfg, netDir))

	node := filepath.Join(cfg.LndDir, "sha256-node")
	require.Equal(t, node, sub.SHA256Dir)
	require.Equal(t, "127.0.0.1:10019", sub.SHA256RPCHost)
	require.Equal(t, filepath.Join(node, "tls.cert"), sub.SHA256TLSCertPath)
	require.Equal(t, filepath.Join(netDir, "bridge", "sha256",
		"wallet.password"), sub.SHA256PasswordFile)
	require.Equal(t, filepath.Join(netDir, "bridge", "sha256",
		"bridge.macaroon"), sub.SHA256MacaroonPath)
	require.Equal(t, filepath.Join(node, "data", "chain", "bitcoin",
		"regtest", "admin.macaroon"), sub.SHA256AdminMacaroonPath)
}

// The address can be overridden, for a platform that runs the node in a
// container of its own, and the directory with it.
func TestSupervisedAddressAndDirectoryCanBeNamed(t *testing.T) {
	dir := t.TempDir()
	sub := &bridgerpc.Config{
		Enabled: true, Supervised: true, ToSHA256: true,
		SHA256RPCHost: "sha256:10009",
		SHA256Dir:     dir,
	}
	cfg, netDir := bridgeTestConfig(t, sub)

	require.NoError(t, validateBridgeConfig(cfg, netDir))
	require.Equal(t, "sha256:10009", sub.SHA256RPCHost)
	require.Equal(t, filepath.Join(dir, "tls.cert"), sub.SHA256TLSCertPath)
}

// A macaroon named for a supervised node is a mistake: this node makes that
// node's macaroon, and using another would trust a credential it did not
// make for a node it did not create.
func TestSupervisedRefusesANamedMacaroon(t *testing.T) {
	sub := &bridgerpc.Config{
		Enabled: true, Supervised: true, ToSHA256: true,
		SHA256MacaroonPath: "/somewhere/admin.macaroon",
	}
	cfg, netDir := bridgeTestConfig(t, sub)

	err := validateBridgeConfig(cfg, netDir)
	require.True(t, errors.Is(err, bridgerpc.ErrConfig), "%v", err)
	require.Contains(t, err.Error(), "supervised")
}

// The existing-node path is untouched by any of this.
func TestExternalNodeStillNeedsItsMacaroon(t *testing.T) {
	sub := &bridgerpc.Config{
		Enabled: true, ToSHA256: true, SHA256RPCHost: "lnd:10009",
	}
	cfg, netDir := bridgeTestConfig(t, sub)

	err := validateBridgeConfig(cfg, netDir)
	require.True(t, errors.Is(err, bridgerpc.ErrConfig), "%v", err)
	require.Contains(t, err.Error(), "macaroon")
	require.Empty(t, sub.SHA256PasswordFile)
}
