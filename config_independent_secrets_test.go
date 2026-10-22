//go:build integration

package lnd

import (
	"testing"

	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/stretchr/testify/require"
)

// TestDevIndependentSecretsSenderNeedsProtocolOption checks that the dev
// sender is refused unless protocol.independent-secrets is set: its secrets
// are only accepted on channels with the add-on, which the node only
// negotiates when it signals the feature.
func TestDevIndependentSecretsSenderNeedsProtocolOption(t *testing.T) {
	const msg = "needs protocol.independent-secrets"

	validate := func(t *testing.T, on bool) error {
		t.Helper()
		cfg := DefaultConfig()
		cfg.LndDir = t.TempDir()
		cfg.Bitcoin.RegTest = true
		cfg.Bitcoin.Node = "nochainbackend"
		cfg.Dev = &lncfg.DevConfig{IndependentSecretsSender: true}
		cfg.ProtocolOptions.IndependentSecrets = on
		fileParser := flags.NewParser(&cfg, flags.Default)
		flagParser := flags.NewParser(&cfg, flags.Default)
		_, err := ValidateConfig(
			cfg, signal.Interceptor{}, fileParser, flagParser,
		)
		return err
	}

	err := validate(t, false)
	require.ErrorContains(t, err, msg)

	if err := validate(t, true); err != nil {
		require.NotContains(t, err.Error(), msg)
	}
}
