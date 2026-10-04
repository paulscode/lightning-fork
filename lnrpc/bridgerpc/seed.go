package bridgerpc

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/keychain"
)

// The SHA256 node a bridge runs is given a seed of its own, derived from this
// node's wallet rather than shared with it or generated and stored.
//
// Not shared: a seed that predates the fork controls coins that exist on both
// chains, and the SHA256 node signs with plain SIGHASH_ALL, so its spends of
// those coins replay onto the BLAKE2b chain. Sharing would also give both nodes
// one identity and one set of addresses, and let each one's channel backups
// decrypt in the other. Not stored: one phrase to keep is the whole point, and
// a second secret on disk is a second thing to lose or leak.
//
// So the SHA256 node's aezeed entropy is HKDF-SHA256 over one private key of
// this wallet. Anyone holding this node's recovery phrase can reproduce it, and
// docs/bridge-sha256-node.md says how without this code. Every constant below
// is part of that specification: changing one gives every operator a different
// SHA256 node, so they are versioned in Sha256SeedInfo instead.
const (
	// Sha256SeedFamily is the key family (BIP43 account) of the key the
	// seed is derived from: m/1017'/coin'/1000'/0/0.
	//
	// Not the next free family after lnd's own (0 to 9): upstream lnd
	// claims families as it needs them, and a merge that gave 10 a meaning
	// of its own would leave the two uses sharing a key without anything
	// failing. 1000 is far from anything lnd numbers.
	Sha256SeedFamily keychain.KeyFamily = 1000

	// Sha256SeedIndex is the key's index within that family.
	Sha256SeedIndex uint32 = 0

	// Sha256SeedInfo is HKDF's info string. The version is in it so that a
	// future derivation can coexist with this one.
	Sha256SeedInfo = "lightning-fork/bridge/sha256-node/seed/v1"
)

// Sha256SeedBirthday is the birthday every derived SHA256 node is given.
//
// A wallet's birthday is where its rescan starts, so it only has to be no later
// than the wallet's first transaction. No wallet derived this way can have one
// before this code existed, so a fixed date is always safe, and it keeps the
// derivation free of anything that would have to be stored.
var Sha256SeedBirthday = time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)

// Sha256SeedEntropy is the SHA256 node's aezeed entropy, from the private key
// at Sha256SeedFamily/Sha256SeedIndex: HKDF-SHA256 with the key's 32 bytes,
// big-endian, as the secret, no salt, and Sha256SeedInfo as the info.
func Sha256SeedEntropy(priv *btcec.PrivateKey) ([aezeed.EntropySize]byte,
	error) {

	var entropy [aezeed.EntropySize]byte
	if priv == nil {
		return entropy, fmt.Errorf("no key to derive the SHA256 node's " +
			"seed from")
	}

	out, err := hkdf.Key(
		sha256.New, priv.Serialize(), nil, Sha256SeedInfo,
		aezeed.EntropySize,
	)
	if err != nil {
		return entropy, fmt.Errorf("deriving the SHA256 node's seed: %w",
			err)
	}
	copy(entropy[:], out)

	return entropy, nil
}

// Sha256SeedMnemonic is the SHA256 node's seed as an aezeed phrase, which lnd
// takes at wallet creation and which `lncli create` accepts to restore it.
//
// aezeed salts each phrase at random, so two calls give different words for the
// same entropy and birthday. Every one of them restores the same wallet.
func Sha256SeedMnemonic(entropy [aezeed.EntropySize]byte) (aezeed.Mnemonic,
	error) {

	seed, err := aezeed.New(
		aezeed.CipherSeedVersion, &entropy, Sha256SeedBirthday,
	)
	if err != nil {
		return aezeed.Mnemonic{}, err
	}

	return seed.ToMnemonic(nil)
}

// Sha256SeedMasterKey is the SHA256 node's BIP32 root key, serialised for the
// network it runs on. It is the second way to restore that node without
// Lightning Fork (`lncli create` accepts an extended master key), and the one
// that needs no aezeed tooling to reproduce by hand: lnd builds its root from
// the aezeed entropy exactly as BIP32 builds one from a seed.
func Sha256SeedMasterKey(entropy [aezeed.EntropySize]byte,
	net *chaincfg.Params) (string, error) {

	master, err := hdkeychain.NewMaster(entropy[:], net)
	if err != nil {
		return "", err
	}

	return master.String(), nil
}

// Sha256NodeKey is the identity key a stock lnd created from this entropy has:
// its key at m/1017'/coin'/6'/0/0, where coin is 0 on mainnet and 1 on the
// test networks, as lnd numbers them.
//
// The bridge compares it with what the supervised node reports, so that a node
// it did not create, or one restored from the wrong phrase, is refused rather
// than trusted with swaps.
func Sha256NodeKey(entropy [aezeed.EntropySize]byte,
	coinType uint32) (*btcec.PublicKey, error) {

	key, err := LndKeyAt(
		entropy[:], coinType, uint32(keychain.KeyFamilyNodeKey), 0,
	)
	if err != nil {
		return nil, err
	}

	return key.ECPubKey()
}

// LndKeyAt is the extended key lnd's wallet holds at m/1017'/coin'/family'/0/
// index for a wallet created from seed: lnd's derivation, as btcwallet
// actually performs it, which is not quite BIP32.
//
// BIP32 serialises a private key as 32 bytes. An old btcutil derived hardened
// children from the key's minimal big-endian bytes instead, dropping a leading
// zero, and btcwallet keeps that (DeriveNonStandard) for compatibility. It
// only matters for a hardened child whose parent's private key starts with a
// zero byte, and where it applies depends on which parent keys btcwallet has
// stored and read back, since a key read back is 32 bytes again:
//
//   - 1017' from the master key: the master is always 32 bytes, so the two
//     agree;
//   - coin' from the purpose key: non-standard (the purpose key is derived
//     and used without being stored);
//   - family' from the coin type key: standard (that key is stored with the
//     scope and read back before use);
//   - 0 and index: not hardened, so the private key is not hashed and the two
//     agree.
//
// Measured against btcwallet in seed_test.go, on seeds that tell each level
// apart, for a wallet just created and the same wallet reopened. The two
// cases where it applies are each about one seed in 256.
func LndKeyAt(seed []byte, coinType, family,
	index uint32) (*hdkeychain.ExtendedKey, error) {

	// Family 0 is the exception: btcwallet derives that account when it
	// creates the scope, from the coin type key it has just derived and
	// not yet stored, so non-standard at that level too. Nothing here
	// needs it, so it is refused rather than given a second rule.
	if family == 0 {
		return nil, fmt.Errorf("key family 0 is derived differently by " +
			"btcwallet; LndKeyAt does not cover it")
	}

	// The network only decides how the key would be serialised.
	master, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}

	purpose, err := master.Derive(
		hdkeychain.HardenedKeyStart + keychain.BIP0043Purpose,
	)
	if err != nil {
		return nil, err
	}
	coin, err := purpose.DeriveNonStandard( //nolint:staticcheck
		hdkeychain.HardenedKeyStart + coinType,
	)
	if err != nil {
		return nil, err
	}
	account, err := coin.Derive(hdkeychain.HardenedKeyStart + family)
	if err != nil {
		return nil, err
	}
	branch, err := account.Derive(0)
	if err != nil {
		return nil, err
	}

	return branch.Derive(index)
}
