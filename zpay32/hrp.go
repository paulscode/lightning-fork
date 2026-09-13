package zpay32

import (
	"strings"
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
)

// BOLT 11 identifies the network an invoice belongs to solely by the currency
// prefix of the human-readable part: "ln" + "bc" for Bitcoin mainnet, "tb" for
// testnet, and so on. Those prefixes come from the BIP-173 address prefix of
// the network. The Bitcoin BLAKE2b chain kept Bitcoin's address prefixes, so
// an invoice derived from them would be indistinguishable from a Bitcoin one
// and a reader on either chain would try to pay the other chain's invoice.
//
// A daemon on the BLAKE2b chain therefore registers an explicit prefix for
// the network it runs on, and both encoding and decoding use it. Nothing is
// registered by default, so this package behaves exactly as upstream for
// anything that does not register (tests, tools decoding Bitcoin invoices).

var (
	invoiceHRPMu sync.RWMutex

	// invoiceHRPs maps a chaincfg.Params name to the registered currency
	// prefix for that network.
	invoiceHRPs = map[string]string{}
)

// RegisterInvoiceHRP sets the BOLT 11 currency prefix used for invoices on
// the network with the given chaincfg.Params name. An empty prefix removes
// the registration and restores the upstream behaviour for that network.
func RegisterInvoiceHRP(netName, hrp string) {
	invoiceHRPMu.Lock()
	defer invoiceHRPMu.Unlock()

	if hrp == "" {
		delete(invoiceHRPs, netName)
		return
	}
	invoiceHRPs[netName] = hrp
}

// InvoiceHRP returns the BOLT 11 currency prefix for the given network: the
// registered prefix if there is one, otherwise the upstream rule (the BIP-173
// address prefix, with signet spelled "tbs" to tell it from testnet3).
func InvoiceHRP(net *chaincfg.Params) string {
	invoiceHRPMu.RLock()
	hrp, ok := invoiceHRPs[net.Name]
	invoiceHRPMu.RUnlock()
	if ok {
		return hrp
	}

	if net.Name == chaincfg.SigNetParams.Name {
		return "tbs"
	}
	return net.Bech32HRPSegwit
}

// bitcoinInvoicePrefixes are the currency prefixes every SHA256d Bitcoin
// implementation uses, longest first so that a prefix test is unambiguous.
var bitcoinInvoicePrefixes = []string{"bcrt", "tbs", "bc", "tb", "sb"}

// isBitcoinInvoiceHRP reports whether the part of an HRP after "ln" begins
// with one of the stock Bitcoin currency prefixes followed by either nothing
// or an amount (a digit).
func isBitcoinInvoiceHRP(rest string) bool {
	for _, prefix := range bitcoinInvoicePrefixes {
		if !strings.HasPrefix(rest, prefix) {
			continue
		}
		tail := rest[len(prefix):]
		if tail == "" || (tail[0] >= '0' && tail[0] <= '9') {
			return true
		}
	}
	return false
}
