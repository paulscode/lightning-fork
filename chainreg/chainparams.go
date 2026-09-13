package chainreg

import (
	"encoding/hex"
	"fmt"

	bitcoinCfg "github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	bitcoinWire "github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
)

// Lightning Fork follows the Bitcoin BLAKE2b chain (Bitcoin Knots v29.4.1),
// which hard-forked from Bitcoin at mainnet height 961640 on 2026-08-30
// without changing the genesis block, the address format or the key
// derivation. That leaves two chains sharing one genesis hash, so the genesis
// hash can no longer serve as the Lightning network identifier: a node on the
// other chain would pass every check that compares it.
//
// Two identities are therefore kept apart on purpose:
//
//   - Params.GenesisHash stays the true genesis of the shared history. The
//     wallet backend identifies the connected node's network by it, and the
//     node reports the same chain name either way.
//   - ChainHash is the BOLT chain_hash this daemon advertises in init,
//     open_channel, gossip, channel backups and offers. On mainnet it is the
//     hash of the first BLAKE2b block, which is checkpointed and already the
//     discriminator the wallet ecosystem for this chain uses. Test networks
//     restart and move their activation height, so they use a tagged hash of
//     their genesis instead.
//
// Whether the connected backend really is on the BLAKE2b chain is a separate
// question answered at startup by reading the block header at the activation
// height (see blake2b_check.go).

const (
	// Blake2bMainnetActivationHeight is the height of the first BLAKE2b
	// block on mainnet: 961639 is the last SHA256d block.
	Blake2bMainnetActivationHeight uint32 = 961640

	// blake2bMainnetActivationHashStr is the id of block 961640, the first
	// BLAKE2b block, in display order. Checkpointed in Bitcoin Knots since
	// v29.4.1rc5; read from the chain and confirmed against an independent
	// explorer on 2026-09-08 and again on 2026-09-12.
	blake2bMainnetActivationHashStr = "0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb"

	// chainHashTag is the BIP-340 tag used to derive a chain hash for the
	// test networks, where no stable activation block exists.
	chainHashTag = "Lightning Fork chain_hash"

	// Invoice prefixes. BOLT 11 identifies a network only by the currency
	// prefix of the human-readable part; the BLAKE2b chain kept "bc" for
	// addresses, so an explicit, visually distinct prefix is needed. It
	// contains no digits because the amount that follows begins at the
	// first digit.
	invoiceHRPMainnet = "blake"
	invoiceHRPTestnet = "tblake"
	invoiceHRPSignet  = "tbsblake"
	invoiceHRPSimnet  = "sblake"
	invoiceHRPRegtest = "blakert"
)

// Blake2bMainnetActivationHash is the hash of the first BLAKE2b block on
// mainnet, in internal byte order.
var Blake2bMainnetActivationHash = mustHashFromStr(blake2bMainnetActivationHashStr)

// BitcoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network, plus
// the Lightning-level identity of the BLAKE2b chain on that network.
type BitcoinNetParams struct {
	*bitcoinCfg.Params
	RPCPort  string
	CoinType uint32

	// ChainHash is the BOLT chain_hash advertised for this network. It is
	// never the genesis hash: see the package comment above.
	ChainHash chainhash.Hash

	// InvoiceHRP is the BOLT 11 currency prefix; the human-readable part
	// of an invoice is "ln" followed by it.
	InvoiceHRP string

	// Blake2bActivationHeight is the height of the first BLAKE2b block on
	// this network, or zero where it is not fixed (regtest chooses it per
	// run; testnet4 has moved it between release candidates) and must be
	// configured or read from the node.
	Blake2bActivationHeight uint32

	// Blake2bActivationHash is the pinned id of the activation block, set
	// on mainnet only. Where it is nil, the header's format is as far as
	// identification goes.
	Blake2bActivationHash *chainhash.Hash
}

// BitcoinTestNetParams contains parameters specific to the 3rd version of the
// test network.
var BitcoinTestNetParams = BitcoinNetParams{
	Params:     &bitcoinCfg.TestNet3Params,
	RPCPort:    "18334",
	CoinType:   keychain.CoinTypeTestnet,
	ChainHash:  SyntheticChainHash(bitcoinCfg.TestNet3Params.GenesisHash),
	InvoiceHRP: invoiceHRPTestnet,
}

// BitcoinTestNet4Params contains parameters specific to the 4th version of the
// test network.
var BitcoinTestNet4Params = BitcoinNetParams{
	Params:     &bitcoinCfg.TestNet4Params,
	RPCPort:    "48334",
	CoinType:   keychain.CoinTypeTestnet,
	ChainHash:  SyntheticChainHash(bitcoinCfg.TestNet4Params.GenesisHash),
	InvoiceHRP: invoiceHRPTestnet,
}

// BitcoinMainNetParams contains parameters specific to the current Bitcoin
// mainnet.
var BitcoinMainNetParams = BitcoinNetParams{
	Params:                  &bitcoinCfg.MainNetParams,
	RPCPort:                 "8334",
	CoinType:                keychain.CoinTypeBitcoin,
	ChainHash:               *Blake2bMainnetActivationHash,
	InvoiceHRP:              invoiceHRPMainnet,
	Blake2bActivationHeight: Blake2bMainnetActivationHeight,
	Blake2bActivationHash:   Blake2bMainnetActivationHash,
}

// BitcoinSimNetParams contains parameters specific to the simulation test
// network.
var BitcoinSimNetParams = BitcoinNetParams{
	Params:     &bitcoinCfg.SimNetParams,
	RPCPort:    "18556",
	CoinType:   keychain.CoinTypeTestnet,
	ChainHash:  SyntheticChainHash(bitcoinCfg.SimNetParams.GenesisHash),
	InvoiceHRP: invoiceHRPSimnet,
}

// BitcoinSigNetParams contains parameters specific to the signet test network.
var BitcoinSigNetParams = BitcoinNetParams{
	Params:     &bitcoinCfg.SigNetParams,
	RPCPort:    "38332",
	CoinType:   keychain.CoinTypeTestnet,
	ChainHash:  SyntheticChainHash(bitcoinCfg.SigNetParams.GenesisHash),
	InvoiceHRP: invoiceHRPSignet,
}

// BitcoinRegTestNetParams contains parameters specific to a local bitcoin
// regtest network.
var BitcoinRegTestNetParams = BitcoinNetParams{
	Params:     &bitcoinCfg.RegressionNetParams,
	RPCPort:    "18334",
	CoinType:   keychain.CoinTypeTestnet,
	ChainHash:  SyntheticChainHash(bitcoinCfg.RegressionNetParams.GenesisHash),
	InvoiceHRP: invoiceHRPRegtest,
}

// IsTestnet tests if the given params correspond to a testnet parameter
// configuration.
func IsTestnet(params *BitcoinNetParams) bool {
	return params.Params.Net == bitcoinWire.TestNet3 ||
		params.Params.Net == bitcoinWire.TestNet4
}

// SyntheticChainHash derives the BOLT chain_hash used on a network that has
// no stable activation block: a BIP-340 tagged hash of the network's genesis
// hash. It is stable across testnet restarts, distinct from the genesis hash
// every SHA256d implementation advertises, and distinct per network.
func SyntheticChainHash(genesis *chainhash.Hash) chainhash.Hash {
	return *chainhash.TaggedHash([]byte(chainHashTag), genesis[:])
}

// ParseChainHashOverride parses a display-order hex chain hash supplied by
// configuration.
func ParseChainHashOverride(s string) (chainhash.Hash, error) {
	if len(s) != chainhash.HashSize*2 {
		return chainhash.Hash{}, fmt.Errorf("chain hash must be %d hex "+
			"characters, got %d", chainhash.HashSize*2, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return chainhash.Hash{}, fmt.Errorf("chain hash is not hex: %w", err)
	}
	h, err := chainhash.NewHashFromStr(s)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return *h, nil
}

func mustHashFromStr(s string) *chainhash.Hash {
	h, err := chainhash.NewHashFromStr(s)
	if err != nil {
		panic(fmt.Sprintf("invalid built-in hash %q: %v", s, err))
	}
	return h
}
