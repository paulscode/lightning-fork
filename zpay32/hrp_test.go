package zpay32

import (
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// withInvoiceHRP registers a prefix for the test and restores the upstream
// behaviour afterwards, since the registry is process-wide.
func withInvoiceHRP(t *testing.T, net *chaincfg.Params, hrp string) {
	t.Helper()
	RegisterInvoiceHRP(net.Name, hrp)
	t.Cleanup(func() { RegisterInvoiceHRP(net.Name, "") })
}

func signHRPTest(t *testing.T, key *btcec.PrivateKey) MessageSigner {
	t.Helper()
	return MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			hash := chainhash.HashB(msg)
			return ecdsa.SignCompact(key, hash, true), nil
		},
	}
}

func newHRPTestInvoice(t *testing.T, net *chaincfg.Params,
	amt *lnwire.MilliSatoshi) *Invoice {

	t.Helper()
	var hash [32]byte
	copy(hash[:], []byte("lightning fork invoice prefix test hash"))
	inv, err := NewInvoice(net, hash, time.Unix(1788070477, 0),
		Description("prefix test"))
	require.NoError(t, err)
	inv.MilliSat = amt
	return inv
}

// TestInvoiceHRPDefaultsUnchanged pins the upstream rule when nothing is
// registered, so tools decoding Bitcoin invoices keep working.
func TestInvoiceHRPDefaultsUnchanged(t *testing.T) {
	require.Equal(t, "bc", InvoiceHRP(&chaincfg.MainNetParams))
	require.Equal(t, "tb", InvoiceHRP(&chaincfg.TestNet3Params))
	require.Equal(t, "tb", InvoiceHRP(&chaincfg.TestNet4Params))
	require.Equal(t, "tbs", InvoiceHRP(&chaincfg.SigNetParams))
	require.Equal(t, "bcrt", InvoiceHRP(&chaincfg.RegressionNetParams))
	require.Equal(t, "sb", InvoiceHRP(&chaincfg.SimNetParams))
}

// TestInvoiceHRPRegisteredRoundTrip encodes with a registered prefix, checks
// the human-readable part, and decodes it back on the same network with and
// without an amount.
func TestInvoiceHRPRegisteredRoundTrip(t *testing.T) {
	withInvoiceHRP(t, &chaincfg.MainNetParams, "blake")

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	signer := signHRPTest(t, key)

	amt := lnwire.MilliSatoshi(2_000_000_000) // 0.02 BTC = 20m
	for _, a := range []*lnwire.MilliSatoshi{nil, &amt} {
		inv := newHRPTestInvoice(t, &chaincfg.MainNetParams, a)
		encoded, err := inv.Encode(signer)
		require.NoError(t, err)

		if a == nil {
			require.True(t, strings.HasPrefix(encoded, "lnblake1"), encoded)
		} else {
			require.True(t, strings.HasPrefix(encoded, "lnblake20m1"), encoded)
		}
		require.False(t, strings.HasPrefix(encoded, "lnbc"))

		decoded, err := Decode(encoded, &chaincfg.MainNetParams)
		require.NoError(t, err)
		require.Equal(t, inv.PaymentHash, decoded.PaymentHash)
		if a == nil {
			require.Nil(t, decoded.MilliSat)
		} else {
			require.NotNil(t, decoded.MilliSat)
			require.Equal(t, amt, *decoded.MilliSat)
		}
	}
}

// TestInvoiceHRPRejectsBitcoinInvoice checks that a node with a registered
// prefix refuses a SHA256d Bitcoin invoice with a message that names the
// other network, and that a stock node refuses ours.
func TestInvoiceHRPRejectsBitcoinInvoice(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	signer := signHRPTest(t, key)

	// A stock mainnet invoice, produced before anything is registered.
	amt := lnwire.MilliSatoshi(1_000_000)
	stock := newHRPTestInvoice(t, &chaincfg.MainNetParams, &amt)
	stockEncoded, err := stock.Encode(signer)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(stockEncoded, "lnbc"))

	withInvoiceHRP(t, &chaincfg.MainNetParams, "blake")

	_, err = Decode(stockEncoded, &chaincfg.MainNetParams)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHA256")
	require.Contains(t, err.Error(), "blake")

	// Ours, decoded by a reader that has not registered anything (a stock
	// node), must fail the prefix check rather than be misread.
	ours := newHRPTestInvoice(t, &chaincfg.MainNetParams, &amt)
	oursEncoded, err := ours.Encode(signer)
	require.NoError(t, err)
	RegisterInvoiceHRP(chaincfg.MainNetParams.Name, "")
	_, err = Decode(oursEncoded, &chaincfg.MainNetParams)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not for current active network")
}

// TestInvoiceHRPTestNetworks checks the registered test-network prefixes
// decode on their own network only.
func TestInvoiceHRPTestNetworks(t *testing.T) {
	withInvoiceHRP(t, &chaincfg.TestNet4Params, "tblake")
	withInvoiceHRP(t, &chaincfg.RegressionNetParams, "blakert")

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	signer := signHRPTest(t, key)

	t4 := newHRPTestInvoice(t, &chaincfg.TestNet4Params, nil)
	t4Encoded, err := t4.Encode(signer)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(t4Encoded, "lntblake1"), t4Encoded)
	_, err = Decode(t4Encoded, &chaincfg.TestNet4Params)
	require.NoError(t, err)
	_, err = Decode(t4Encoded, &chaincfg.RegressionNetParams)
	require.Error(t, err)

	rt := newHRPTestInvoice(t, &chaincfg.RegressionNetParams, nil)
	rtEncoded, err := rt.Encode(signer)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(rtEncoded, "lnblakert1"), rtEncoded)
	_, err = Decode(rtEncoded, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
}

func TestIsBitcoinInvoiceHRP(t *testing.T) {
	for _, s := range []string{"bc", "bc20m", "tb", "tbs1", "bcrt", "bcrt10n", "sb"} {
		require.True(t, isBitcoinInvoiceHRP(s), s)
	}
	for _, s := range []string{"blake", "blake20m", "tblake", "blakert", "bl", "x"} {
		require.False(t, isBitcoinInvoiceHRP(s), s)
	}
}
