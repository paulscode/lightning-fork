package bridgerpc

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/snacl"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // The wallet's database.
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// hkdfByHand is RFC 5869 written out, so the derivation is checked against the
// specification rather than against the library it was written with.
func hkdfByHand(secret, info []byte, n int) []byte {
	// Extract, with no salt: a salt of HashLen zero bytes.
	ext := hmac.New(sha256.New, make([]byte, sha256.Size))
	ext.Write(secret)
	prk := ext.Sum(nil)

	// Expand.
	var out, prev []byte
	for i := byte(1); len(out) < n; i++ {
		exp := hmac.New(sha256.New, prk)
		exp.Write(prev)
		exp.Write(info)
		exp.Write([]byte{i})
		prev = exp.Sum(nil)
		out = append(out, prev...)
	}

	return out[:n]
}

// The spec's numbers are fixed: changing one gives every operator a different
// SHA256 node, so a change has to be a decision, not an edit.
func TestSha256SeedConstants(t *testing.T) {
	require.EqualValues(t, 1000, Sha256SeedFamily)
	require.EqualValues(t, 0, Sha256SeedIndex)
	require.Equal(t, "lightning-fork/bridge/sha256-node/seed/v1",
		Sha256SeedInfo)
	require.Equal(t, "2026-10-01T00:00:00Z",
		Sha256SeedBirthday.Format(time.RFC3339))
}

func TestSha256SeedEntropyIsHKDF(t *testing.T) {
	for _, hexKey := range []string{
		"0000000000000000000000000000000000000000000000000000000000000001",
		"e8f32e723decf4051aefac8e2c93c9c5b214313817cdb01a1494b917c8436b35",
		// A key whose first byte is zero: the secret is still all 32
		// bytes, not a shortened number.
		"00a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f",
	} {
		raw, err := hex.DecodeString(hexKey)
		require.NoError(t, err)
		priv, _ := btcec.PrivKeyFromBytes(raw)

		got, err := Sha256SeedEntropy(priv)
		require.NoError(t, err)

		want := hkdfByHand(raw, []byte(Sha256SeedInfo), aezeed.EntropySize)
		require.Equal(t, want, got[:], "key %s", hexKey)
	}

	_, err := Sha256SeedEntropy(nil)
	require.Error(t, err)
}

func TestSha256SeedEntropyDependsOnTheKey(t *testing.T) {
	a, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{1}, 32))
	b, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{2}, 32))

	ea, err := Sha256SeedEntropy(a)
	require.NoError(t, err)
	eb, err := Sha256SeedEntropy(b)
	require.NoError(t, err)
	require.NotEqual(t, ea, eb)

	again, err := Sha256SeedEntropy(a)
	require.NoError(t, err)
	require.Equal(t, ea, again, "the derivation must be deterministic")
}

// Every phrase restores the same seed and birthday, whatever its salt.
func TestSha256SeedMnemonicRoundTrips(t *testing.T) {
	var entropy [aezeed.EntropySize]byte
	copy(entropy[:], bytes.Repeat([]byte{0xab}, aezeed.EntropySize))

	first, err := Sha256SeedMnemonic(entropy)
	require.NoError(t, err)
	second, err := Sha256SeedMnemonic(entropy)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "each phrase is salted afresh")

	for _, m := range []aezeed.Mnemonic{first, second} {
		seed, err := m.ToCipherSeed(nil)
		require.NoError(t, err)
		require.Equal(t, entropy, seed.Entropy)

		// aezeed counts whole days from the genesis block's time of
		// day, so the stored birthday is the day boundary at or before
		// the one asked for. Earlier is the safe direction: a rescan
		// that starts sooner misses nothing.
		birthday := seed.BirthdayTime()
		require.False(t, birthday.After(Sha256SeedBirthday))
		require.Less(t, Sha256SeedBirthday.Sub(birthday), 24*time.Hour)
	}
}

// The root key is plain BIP32 from the entropy, which is what lets someone
// reproduce it without aezeed tooling.
func TestSha256SeedMasterKey(t *testing.T) {
	var entropy [aezeed.EntropySize]byte
	copy(entropy[:], bytes.Repeat([]byte{7}, aezeed.EntropySize))

	got, err := Sha256SeedMasterKey(entropy, &chaincfg.MainNetParams)
	require.NoError(t, err)
	require.Equal(t, "xprv", got[:4])

	master, err := hdkeychain.NewMaster(entropy[:], &chaincfg.MainNetParams)
	require.NoError(t, err)
	require.Equal(t, master.String(), got)

	reg, err := Sha256SeedMasterKey(entropy, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	require.Equal(t, "tprv", reg[:4])
}

// testWallet is a btcwallet created from seed, as lnd creates its own: the
// derivation lnd actually uses, rather than a reading of it.
func testWallet(t *testing.T, seed []byte) *wallet.Wallet {
	t.Helper()

	return testWalletIn(t, seed, t.TempDir())
}

// testWalletIn is testWallet in a directory of the caller's, so it can be
// reopened.
func testWalletIn(t *testing.T, seed []byte, dir string) *wallet.Wallet {
	t.Helper()

	fast := waddrmgr.FastScryptOptions
	waddrmgr.SetSecretKeyGen(func(pass *[]byte,
		_ *waddrmgr.ScryptOptions) (*snacl.SecretKey, error) {

		return snacl.NewSecretKey(pass, fast.N, fast.R, fast.P)
	})

	loader := wallet.NewLoader(
		&chaincfg.SimNetParams, dir, true, time.Minute, 0,
	)
	pass := []byte("test")
	w, err := loader.CreateNewWallet(pass, pass, seed, time.Time{})
	require.NoError(t, err)
	require.NoError(t, w.Unlock(pass, nil))

	// lnd creates its key scope when it creates its wallet; a bare
	// btcwallet does not have it.
	for _, coin := range []uint32{0, 1} {
		scope := waddrmgr.KeyScope{
			Purpose: keychain.BIP0043Purpose, Coin: coin,
		}
		err := walletdb.Update(w.Database(), func(
			tx walletdb.ReadWriteTx) error {

			ns := tx.ReadWriteBucket([]byte("waddrmgr"))
			_, err := w.Manager.NewScopedKeyManager(
				ns, scope, waddrmgr.ScopeAddrSchema{
					ExternalAddrType: waddrmgr.WitnessPubKey,
					InternalAddrType: waddrmgr.WitnessPubKey,
				},
			)

			return err
		})
		require.NoError(t, err)
	}

	return w
}

// separatingSeeds are seeds for which getting one hardened level wrong gives a
// different key at m/1017'/coin'/family'/0/0: one where using standard BIP32
// at the coin type level would differ, and one where using btcwallet's
// non-standard derivation at the family level would. A derivation that is
// wrong at either level fails on one of them.
func separatingSeeds(t *testing.T, coin uint32,
	family keychain.KeyFamily) [][]byte {

	t.Helper()

	// alt derives as LndKeyAt does except at one level.
	alt := func(seed []byte, stdCoin, stdFamily bool) []byte {
		master, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
		require.NoError(t, err)
		k, err := master.Derive(hdkeychain.HardenedKeyStart + 1017)
		require.NoError(t, err)
		if stdCoin {
			k, err = k.Derive(hdkeychain.HardenedKeyStart + coin)
		} else {
			k, err = k.DeriveNonStandard( //nolint:staticcheck
				hdkeychain.HardenedKeyStart + coin)
		}
		require.NoError(t, err)
		if stdFamily {
			k, err = k.Derive(
				hdkeychain.HardenedKeyStart + uint32(family))
		} else {
			k, err = k.DeriveNonStandard( //nolint:staticcheck
				hdkeychain.HardenedKeyStart + uint32(family))
		}
		require.NoError(t, err)
		k, err = k.Derive(0)
		require.NoError(t, err)
		k, err = k.Derive(0)
		require.NoError(t, err)
		pub, err := k.ECPubKey()
		require.NoError(t, err)

		return pub.SerializeCompressed()
	}

	var coinSeed, familySeed []byte
	for i := 0; i < 20000 && (coinSeed == nil || familySeed == nil); i++ {
		h := sha256.Sum256([]byte{byte(i), byte(i >> 8), 0x5e,
			byte(family)})
		seed := h[:16]
		ours := alt(seed, false, true)
		if coinSeed == nil && !bytes.Equal(ours, alt(seed, true, true)) {
			coinSeed = seed
		}
		if familySeed == nil &&
			!bytes.Equal(ours, alt(seed, false, false)) {

			familySeed = seed
		}
	}
	require.NotNil(t, coinSeed, "no seed separates the coin type level")
	require.NotNil(t, familySeed, "no seed separates the family level")

	return [][]byte{coinSeed, familySeed}
}

// lndKeyring is lnd's keyring over a wallet created from seed, freshly made
// or reopened from disk: lnd derives its node key on the run that creates the
// wallet and on every run after, and both must give the same answer.
func lndKeyring(t *testing.T, seed []byte, coin uint32,
	reopen bool) keychain.SecretKeyRing {

	t.Helper()

	dir := t.TempDir()
	w := testWalletIn(t, seed, dir)
	if reopen {
		w.Lock()
		require.NoError(t, w.Database().Close())

		loader := wallet.NewLoader(
			&chaincfg.SimNetParams, dir, true, time.Minute, 0,
		)
		var err error
		w, err = loader.OpenExistingWallet([]byte("test"), false)
		require.NoError(t, err)
		require.NoError(t, w.Unlock([]byte("test"), nil))
	}
	t.Cleanup(w.Lock)

	return keychain.NewBtcWalletKeyRing(w, coin)
}

// Sha256NodeKey must be the identity a stock lnd creates from the entropy: the
// supervisor refuses a node whose key differs, so getting this wrong refuses
// our own node.
func TestSha256NodeKeyIsWhatLndDerives(t *testing.T) {
	for _, coin := range []uint32{0, 1} {
		for _, seed := range separatingSeeds(
			t, coin, keychain.KeyFamilyNodeKey,
		) {
			var entropy [aezeed.EntropySize]byte
			copy(entropy[:], seed)
			got, err := Sha256NodeKey(entropy, coin)
			require.NoError(t, err)

			for _, reopen := range []bool{false, true} {
				ring := lndKeyring(t, seed, coin, reopen)
				desc, err := ring.DeriveKey(keychain.KeyLocator{
					Family: keychain.KeyFamilyNodeKey,
				})
				require.NoError(t, err)
				require.True(t, got.IsEqual(desc.PubKey),
					"coin %d seed %x reopen %v", coin, seed,
					reopen)
			}
		}
	}
}

// The key the seed comes from is the one the spec names: the keyring's private
// key at the bridge's family is LndKeyAt's, which is what lets someone
// reproduce the derivation by hand.
func TestSha256SeedKeyIsAtThePathTheSpecNames(t *testing.T) {
	for _, coin := range []uint32{0, 1} {
		for _, seed := range separatingSeeds(t, coin, Sha256SeedFamily) {
			key, err := LndKeyAt(
				seed, coin, uint32(Sha256SeedFamily),
				Sha256SeedIndex,
			)
			require.NoError(t, err)
			byHand, err := key.ECPrivKey()
			require.NoError(t, err)

			for _, reopen := range []bool{false, true} {
				ring := lndKeyring(t, seed, coin, reopen)
				priv, err := ring.DerivePrivKey(
					keychain.KeyDescriptor{
						KeyLocator: keychain.KeyLocator{
							Family: Sha256SeedFamily,
							Index:  Sha256SeedIndex,
						},
					},
				)
				require.NoError(t, err)
				require.Equal(t, byHand.Serialize(),
					priv.Serialize(), "coin %d seed %x "+
						"reopen %v", coin, seed, reopen)
			}
		}
	}
}
