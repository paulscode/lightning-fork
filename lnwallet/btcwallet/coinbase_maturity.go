package btcwallet

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

// coinbaseMaturity returns the depth a coinbase output mined at height must
// reach before a spend of it will relay, and so before the wallet offers it.
//
// On the Bitcoin BLAKE2b chain that depends on where the coinbase was mined:
// under Bitcoin Knots 29.4.3, 6480 blocks for one mined before 979919 and
// 25920 for one from it, while consensus still allows many of them sooner.
// btcwallet asks the same question per coinbase (the fork in go.mod, wallet/
// coinbase_maturity.go there); upstream reads one CoinbaseMaturity for all of
// them, which cannot express this. Never less than CoinbaseMaturity.
func coinbaseMaturity(params *chaincfg.Params, height int32) int32 {
	maturity := params.RelayCoinbaseMaturityAt(height)
	if base := int32(params.CoinbaseMaturity); base > maturity {
		maturity = base
	}

	return maturity
}

// wtxmgrNamespace is the bucket btcwallet keeps its transaction store under.
// btcwallet does not export it; it has not changed since the store existed.
var wtxmgrNamespace = []byte("wtxmgr")

// ImmatureCoinbaseBalance returns the value of the coinbase outputs this
// wallet holds that are not yet deep enough for a spend of them to relay.
//
// btcwallet counts them in no spendable balance until they reach that depth
// (coinbaseMaturity), so without this a miner paid directly in coinbase
// outputs would see nothing of them for the length of the long maturity. It is one pass over the unspent outputs, which
// are all the wallet's own; leased outputs are left out, as they are counted
// as locked.
func (b *BtcWallet) ImmatureCoinbaseBalance() (btcutil.Amount, error) {
	w, ok := b.wallet.(*base.Wallet)
	if !ok {
		return 0, nil
	}

	params := w.ChainParams()
	tip := w.SyncedTo().Height

	var immature btcutil.Amount
	err := walletdb.View(w.Database(), func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(wtxmgrNamespace)
		if ns == nil {
			return nil
		}

		unspent, err := w.TxStore.UnspentOutputs(ns)
		if err != nil {
			return err
		}

		for _, output := range unspent {
			if !output.FromCoinBase || output.Height <= 0 {
				continue
			}
			maturity := coinbaseMaturity(params, output.Height)
			if tip-output.Height+1 < maturity {
				immature += output.Amount
			}
		}

		return nil
	})

	return immature, err
}
