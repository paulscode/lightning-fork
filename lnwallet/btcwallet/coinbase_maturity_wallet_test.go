package btcwallet

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/stretchr/testify/require"
)

// TestLoaderWalletsUseRelayMaturity checks the hook the design rests on: a
// wallet opened through NewWalletLoader holds each coinbase for the relay
// depth its own height calls for, in btcwallet's balances (the btcwallet fork
// in go.mod) and in ImmatureCoinbaseBalance. On mainnet a coinbase mined
// before 979919 is spendable 6480 deep, and one from 979919 only 25920 deep.
func TestLoaderWalletsUseRelayMaturity(t *testing.T) {
	t.Parallel()

	params := &chaincfg.MainNetParams

	loader, err := NewWalletLoader(
		params, 0, LoaderWithLocalWalletDB(t.TempDir(), false,
			time.Minute),
	)
	require.NoError(t, err)

	seed := make([]byte, 32)
	seed[0] = 1
	w, err := loader.CreateNewWallet(
		[]byte("public"), []byte("private"), seed, time.Now(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = loader.UnloadWallet() })

	require.Equal(t, params.CoinbaseMaturity, w.ChainParams().CoinbaseMaturity,
		"the wallet's params were changed")

	// A wallet address, derived without the chain backend NewAddress
	// needs to watch it.
	scoped, err := w.Manager.FetchScopedKeyManager(waddrmgr.KeyScopeBIP0084)
	require.NoError(t, err)
	var pkScript []byte
	err = walletdb.Update(w.Database(), func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket([]byte("waddrmgr"))
		addrs, err := scoped.NextExternalAddresses(ns, 0, 1)
		if err != nil {
			return err
		}
		pkScript, err = txscript.PayToAddrScript(addrs[0].Address())

		return err
	})
	require.NoError(t, err)

	b := &BtcWallet{wallet: w}

	syncTo := func(height int32) {
		t.Helper()
		err := walletdb.Update(w.Database(), func(
			tx walletdb.ReadWriteTx) error {

			ns := tx.ReadWriteBucket([]byte("waddrmgr"))

			return w.Manager.SetSyncedTo(ns, &waddrmgr.BlockStamp{
				Height:    height,
				Hash:      chainhash.Hash{byte(height), 2},
				Timestamp: time.Now(),
			})
		})
		require.NoError(t, err)
	}

	// addCoinbase records a coinbase paying amount to the wallet, mined at
	// height.
	addCoinbase := func(height int32, amount int64) {
		t.Helper()

		coinbase := wire.NewMsgTx(1)
		coinbase.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Index: wire.MaxPrevOutIndex,
			},
			SignatureScript: []byte{0x01, byte(height)},
		})
		coinbase.AddTxOut(wire.NewTxOut(amount, pkScript))

		rec, err := wtxmgr.NewTxRecordFromMsgTx(coinbase, time.Now())
		require.NoError(t, err)
		block := &wtxmgr.BlockMeta{
			Block: wtxmgr.Block{
				Height: height,
				Hash:   chainhash.Hash{byte(height), 1},
			},
			Time: time.Now(),
		}

		err = walletdb.Update(w.Database(), func(
			tx walletdb.ReadWriteTx) error {

			ns := tx.ReadWriteBucket(wtxmgrNamespace)
			if err := w.TxStore.InsertTx(ns, rec, block); err != nil {
				return err
			}

			return w.TxStore.AddCredit(ns, rec, block, 0, false)
		})
		require.NoError(t, err)
	}

	const (
		before  = 979000 // long rule: 6480
		rolling = 979919 // rolling rule: 25920
	)
	addCoinbase(before, 1_000_000)
	addCoinbase(rolling, 2_000_000)

	check := func(tip int32, wantImmature btcutil.Amount) {
		t.Helper()
		syncTo(tip)

		immature, err := b.ImmatureCoinbaseBalance()
		require.NoError(t, err)
		require.Equal(t, wantImmature, immature, "at %d", tip)

		bals, err := w.CalculateAccountBalances(0, 1)
		require.NoError(t, err)
		require.Equal(t, wantImmature, bals.ImmatureReward,
			"btcwallet at %d", tip)
		require.Equal(t, 3_000_000-wantImmature, bals.Spendable,
			"btcwallet at %d", tip)
	}

	// Past consensus for both, short of relay for both.
	check(rolling+150, 3_000_000)

	// The earlier coinbase reaches 6480; the later one is held.
	check(before+6480-1, 2_000_000)

	// 6480 deep is not enough for a coinbase from 979919.
	check(rolling+6480-1, 2_000_000)

	// At 25920 it is mature too.
	check(rolling+25920-1, 0)
}
