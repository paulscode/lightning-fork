package zpay32

import (
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
)

// BOLT 11 identifies the network an invoice belongs to by the currency prefix
// of the human-readable part: "ln" + "bc" for mainnet, "tb" for testnet, and
// so on, derived from the BIP-173 address prefix of the network.
//
// The Bitcoin BLAKE2b chain kept those prefixes. An earlier version of this
// fork gave the chain prefixes of its own ("blake", "tblake", "tbsblake",
// "blakert"); that was withdrawn, because the prefix is BOLT 11's currency
// field and giving the chain one of its own states that it is a different
// currency, which a change of proof of work is not. What separates an invoice
// on this chain from one on the earlier rules is option_blake2b, an even
// feature bit in the `9` field.
//
// The withdrawn prefixes are still accepted when decoding, and only when
// decoding. A node that ran the earlier build issued invoices carrying them
// and stored the strings; lnd re-decodes a stored payment request whenever it
// is listed, and a decode failure there fails the whole ListInvoices call
// rather than skipping the one record. Emitting is unaffected: invoices minted
// from here carry the ordinary prefix.

var (
	legacyHRPMu sync.RWMutex

	// legacyInvoiceHRPs maps a chaincfg.Params name to the prefix that
	// network's invoices used to carry, for decoding only.
	legacyInvoiceHRPs = map[string]string{}
)

// RegisterLegacyInvoiceHRP records the BOLT 11 currency prefix that invoices
// on the given network used to carry, so that strings already issued under it
// still decode. An empty prefix removes the registration. It has no effect on
// what this node emits.
func RegisterLegacyInvoiceHRP(netName, hrp string) {
	legacyHRPMu.Lock()
	defer legacyHRPMu.Unlock()

	if hrp == "" {
		delete(legacyInvoiceHRPs, netName)
		return
	}
	legacyInvoiceHRPs[netName] = hrp
}

// legacyInvoiceHRP returns the withdrawn prefix for a network, or "" if none
// is registered.
func legacyInvoiceHRP(net *chaincfg.Params) string {
	legacyHRPMu.RLock()
	defer legacyHRPMu.RUnlock()

	return legacyInvoiceHRPs[net.Name]
}

// InvoiceHRP returns the BOLT 11 currency prefix for the given network: the
// BIP-173 address prefix, with signet spelled "tbs" to tell it from testnet3.
// This is the upstream rule and the chain does not change it.
func InvoiceHRP(net *chaincfg.Params) string {
	if net.Name == chaincfg.SigNetParams.Name {
		return "tbs"
	}

	return net.Bech32HRPSegwit
}
