package btcwallet

import (
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

// walletChainParams returns the chain parameters to hand btcwallet: a copy of
// params whose CoinbaseMaturity is the depth at which a spend of a coinbase
// output will relay, when that is deeper than the consensus depth.
//
// On the Bitcoin BLAKE2b chain the two differ by thousands of blocks while
// the long coinbase maturity rule is deployed: consensus still allows a
// coinbase to be spent after 100 confirmations, but an upgraded node will not
// relay the spend until 6480. btcwallet reads CoinbaseMaturity, and nothing
// else, to decide when a coinbase output is spendable: in ListUnspent, in the
// coin selection behind SendOutputs and FundPsbt, and in the balances it
// reports. Left at the consensus depth it offered freshly mined coins that no
// node would relay, so a send or channel open from them failed at broadcast.
//
// Only the copy changes. btcwallet compares chain parameters by name only,
// and nothing else in lnd reads CoinbaseMaturity; the funding manager asks for
// RelayCoinbaseMaturity directly. The copy is for deciding spendability only:
// its RequiredCoinbaseMaturity no longer answers the consensus question, so
// never validate anything against the wallet's params.
func walletChainParams(params *chaincfg.Params) *chaincfg.Params {
	if params == nil {
		return nil
	}

	relay := params.RelayCoinbaseMaturity()
	if relay <= int32(params.CoinbaseMaturity) {
		return params
	}
	if relay > math.MaxUint16 {
		relay = math.MaxUint16
	}

	adjusted := *params
	adjusted.CoinbaseMaturity = uint16(relay)

	return &adjusted
}

// wtxmgrNamespace is the bucket btcwallet keeps its transaction store under.
// btcwallet does not export it; it has not changed since the store existed.
var wtxmgrNamespace = []byte("wtxmgr")

// ImmatureCoinbaseBalance returns the value of the coinbase outputs this
// wallet holds that are not yet deep enough for a spend of them to relay.
//
// btcwallet counts them in no spendable balance, since the params it runs
// with carry the relay depth (see walletChainParams), so without this a
// miner paid directly in coinbase outputs would see nothing of them for the
// length of the long maturity. It is one pass over the unspent outputs, which
// are all the wallet's own; leased outputs are left out, as they are counted
// as locked.
func (b *BtcWallet) ImmatureCoinbaseBalance() (btcutil.Amount, error) {
	w, ok := b.wallet.(*base.Wallet)
	if !ok {
		return 0, nil
	}

	maturity := int32(w.ChainParams().CoinbaseMaturity)
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
			if tip-output.Height+1 < maturity {
				immature += output.Amount
			}
		}

		return nil
	})

	return immature, err
}
