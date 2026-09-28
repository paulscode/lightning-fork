package btcwallet

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/stretchr/testify/require"
)

// TestLoaderWalletsUseRelayMaturity checks the hook the design rests on: a
// wallet opened through NewWalletLoader runs with the relay depth, and
// ImmatureCoinbaseBalance counts a coinbase until it reaches it.
func TestLoaderWalletsUseRelayMaturity(t *testing.T) {
	t.Parallel()

	params := &chaincfg.MainNetParams
	relay := params.RelayCoinbaseMaturity()
	require.Greater(t, relay, int32(params.CoinbaseMaturity))

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

	require.Equal(t, relay, int32(w.ChainParams().CoinbaseMaturity),
		"the wallet was not opened with the relay depth")
	require.Equal(t, params.Name, w.ChainParams().Name)

	b := &BtcWallet{wallet: w}

	// A coinbase credit mined at 1000.
	const mined = 1000
	coinbase := wire.NewMsgTx(1)
	coinbase.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		SignatureScript:  []byte{0x01, 0x02},
	})
	coinbase.AddTxOut(wire.NewTxOut(5_000_000, []byte{0x00, 0x14,
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18,
		19, 20}))

	rec, err := wtxmgr.NewTxRecordFromMsgTx(coinbase, time.Now())
	require.NoError(t, err)
	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Height: mined, Hash: chainhash.Hash{1}},
		Time:  time.Now(),
	}

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

	err = walletdb.Update(w.Database(), func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(wtxmgrNamespace)
		if err := w.TxStore.InsertTx(ns, rec, block); err != nil {
			return err
		}

		return w.TxStore.AddCredit(ns, rec, block, 0, false)
	})
	require.NoError(t, err)

	// 151 confirmations: past consensus, short of relay.
	syncTo(mined + 150)
	immature, err := b.ImmatureCoinbaseBalance()
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(5_000_000), immature)

	// At the relay depth it is mature.
	syncTo(mined + relay - 1)
	immature, err = b.ImmatureCoinbaseBalance()
	require.NoError(t, err)
	require.Zero(t, immature)
}
