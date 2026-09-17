//go:build !bridgerpc
// +build !bridgerpc

package lnd

// validateBridgeConfig does nothing in a build without the bridge sub-server:
// there is no configuration to check, and a node that cannot bridge should not
// be able to fail to start because of bridge settings.
func validateBridgeConfig(_ *Config, _ string) error {
	return nil
}
