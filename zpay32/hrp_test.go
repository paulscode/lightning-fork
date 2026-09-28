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

// withLegacyInvoiceHRP registers a withdrawn prefix for the test and clears it
// afterwards, since the registry is process-wide.
func withLegacyInvoiceHRP(t *testing.T, net *chaincfg.Params, hrp string) {
	t.Helper()
	RegisterLegacyInvoiceHRP(net.Name, hrp)
	t.Cleanup(func() { RegisterLegacyInvoiceHRP(net.Name, "") })
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

// TestInvoiceHRPIsUpstream pins that the chain mints ordinary BOLT 11
// prefixes. Giving it one of its own was withdrawn: the prefix is BOLT 11's
// currency field, and a change of proof of work is not a change of currency.
func TestInvoiceHRPIsUpstream(t *testing.T) {
	for _, tc := range []struct {
		net  *chaincfg.Params
		want string
	}{
		{&chaincfg.MainNetParams, "bc"},
		{&chaincfg.TestNet3Params, "tb"},
		{&chaincfg.SigNetParams, "tbs"},
		{&chaincfg.RegressionNetParams, "bcrt"},
	} {
		require.Equal(t, tc.want, InvoiceHRP(tc.net), tc.net.Name)
	}
}

// TestLegacyPrefixIsNotEmitted checks that registering a withdrawn prefix
// changes nothing about what this node mints. It is a decode-side allowance
// only.
func TestLegacyPrefixIsNotEmitted(t *testing.T) {
	withLegacyInvoiceHRP(t, &chaincfg.MainNetParams, "blake")

	require.Equal(t, "bc", InvoiceHRP(&chaincfg.MainNetParams))

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	inv := newHRPTestInvoice(t, &chaincfg.MainNetParams, nil)
	str, err := inv.Encode(signHRPTest(t, key))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(str, "lnbc"), str[:12])
	require.False(t, strings.HasPrefix(str, "lnblake"), str[:12])
}

// TestLegacyPrefixStillDecodes is the reason the registry survives at all. A
// node that ran the earlier build issued invoices under the withdrawn prefix
// and stored the strings, and lnd re-decodes a stored payment request every
// time it is listed. A failure there fails the whole ListInvoices call rather
// than skipping the record, so those strings have to keep parsing.
func TestLegacyPrefixStillDecodes(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Mint one the way the withdrawn build did, by encoding against a
	// network whose prefix is the old value.
	oldNet := chaincfg.MainNetParams
	oldNet.Bech32HRPSegwit = "blake"
	inv := newHRPTestInvoice(t, &oldNet, nil)
	str, err := inv.Encode(signHRPTest(t, key))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(str, "lnblake"), str[:12])

	// Without the registration it is refused, as any foreign prefix is.
	_, err = Decode(str, &chaincfg.MainNetParams)
	require.Error(t, err)

	// With it, the stored string still parses.
	withLegacyInvoiceHRP(t, &chaincfg.MainNetParams, "blake")
	decoded, err := Decode(str, &chaincfg.MainNetParams)
	require.NoError(t, err)
	require.Equal(t, &chaincfg.MainNetParams, decoded.Net)
}

// TestLegacyPrefixCarriesAmount checks the amount still parses after a legacy
// prefix, since the offset it is read from depends on which prefix matched.
func TestLegacyPrefixCarriesAmount(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	oldNet := chaincfg.MainNetParams
	oldNet.Bech32HRPSegwit = "blake"
	amt := lnwire.MilliSatoshi(1500000)
	inv := newHRPTestInvoice(t, &oldNet, &amt)
	str, err := inv.Encode(signHRPTest(t, key))
	require.NoError(t, err)

	withLegacyInvoiceHRP(t, &chaincfg.MainNetParams, "blake")
	decoded, err := Decode(str, &chaincfg.MainNetParams)
	require.NoError(t, err)
	require.NotNil(t, decoded.MilliSat)
	require.Equal(t, amt, *decoded.MilliSat)
}

// TestPrefixNoLongerSeparatesTheChains records the consequence of the
// withdrawal, so that nobody reintroduces the prefix check by accident. An
// invoice minted on the chain that did not upgrade carries the same prefix as
// one minted here and is accepted on that basis alone. What is meant to
// separate them is option_blake2b, an even bit in the `9` field, which is not
// implemented yet.
func TestPrefixNoLongerSeparatesTheChains(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inv := newHRPTestInvoice(t, &chaincfg.MainNetParams, nil)
	str, err := inv.Encode(signHRPTest(t, key))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(str, "lnbc"), str[:12])

	decoded, err := Decode(str, &chaincfg.MainNetParams)
	require.NoError(t, err, "the prefix does not separate the chains")
	require.Equal(t, &chaincfg.MainNetParams, decoded.Net)
}

// TestLegacyPrefixShadowing covers one withdrawn prefix being a prefix of
// another: regtest's "blakert" begins with mainnet's "blake". A regtest string
// offered to a mainnet node must be refused on the network rather than
// stumbling into amount parsing.
func TestLegacyPrefixShadowing(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	mint := func(hrp string, amt *lnwire.MilliSatoshi) string {
		n := chaincfg.MainNetParams
		n.Bech32HRPSegwit = hrp
		inv := newHRPTestInvoice(t, &n, amt)
		s, err := inv.Encode(signHRPTest(t, key))
		require.NoError(t, err)
		return s
	}

	withLegacyInvoiceHRP(t, &chaincfg.MainNetParams, "blake")
	amt := lnwire.MilliSatoshi(100000)

	for _, s := range []string{mint("blakert", nil), mint("blakert", &amt)} {
		_, err := Decode(s, &chaincfg.MainNetParams)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not for current active network")
	}

	// And the legitimate case still decodes, so the boundary check has not
	// made the allowance useless.
	for _, s := range []string{mint("blake", nil), mint("blake", &amt)} {
		_, err := Decode(s, &chaincfg.MainNetParams)
		require.NoError(t, err)
	}
}

// TestLegacyPrefixOnEveryNetwork mints an invoice under each network's
// withdrawn prefix, with and without an amount, and decodes it. On the test
// networks the standard prefix is a prefix of the withdrawn one ("tb" of
// "tblake", "tbs" of "tbsblake"), so matching the standard one first left the
// rest of the old prefix to be parsed as an amount and failed the decode, and
// with it every listing that re-decodes a stored invoice.
func TestLegacyPrefixOnEveryNetwork(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	nets := []struct {
		params *chaincfg.Params
		legacy string
	}{
		{&chaincfg.MainNetParams, "blake"},
		{&chaincfg.TestNet3Params, "tblake"},
		{&chaincfg.TestNet4Params, "tblake"},
		{&chaincfg.SigNetParams, "tbsblake"},
		{&chaincfg.SimNetParams, "sblake"},
		{&chaincfg.RegressionNetParams, "blakert"},
	}

	for _, n := range nets {
		withLegacyInvoiceHRP(t, n.params, n.legacy)

		amt := lnwire.MilliSatoshi(1500000)
		for _, a := range []*lnwire.MilliSatoshi{nil, &amt} {
			oldNet := *n.params
			oldNet.Bech32HRPSegwit = n.legacy
			if n.params.Name == chaincfg.SigNetParams.Name {
				// InvoiceHRP spells signet "tbs" by name, so
				// mint under another name to get the old one.
				oldNet.Name = "signet-legacy"
			}

			str, err := newHRPTestInvoice(t, &oldNet, a).Encode(
				signHRPTest(t, key),
			)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(str, "ln"+n.legacy),
				"%v: minted %v", n.params.Name, str[:14])

			decoded, err := Decode(str, n.params)
			require.NoError(t, err, "%v: %v", n.params.Name, str[:14])
			require.Equal(t, a, decoded.MilliSat, n.params.Name)
		}

		// An ordinary invoice for the network still decodes.
		str, err := newHRPTestInvoice(t, n.params, nil).Encode(
			signHRPTest(t, key),
		)
		require.NoError(t, err)
		_, err = Decode(str, n.params)
		require.NoError(t, err, n.params.Name)
	}
}
