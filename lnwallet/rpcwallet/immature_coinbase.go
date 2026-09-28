package rpcwallet

import (
	"github.com/btcsuite/btcd/btcutil"
)

// ImmatureCoinbaseBalance forwards to the watch-only wallet, which holds the
// outputs; see btcwallet.BtcWallet.ImmatureCoinbaseBalance. The wallet
// controller is embedded as an interface, so the method would not otherwise
// be part of this type, and a node with a remote signer would report none.
func (r *RPCKeyRing) ImmatureCoinbaseBalance() (btcutil.Amount, error) {
	type reporter interface {
		ImmatureCoinbaseBalance() (btcutil.Amount, error)
	}

	w, ok := r.WalletController.(reporter)
	if !ok {
		return 0, nil
	}

	return w.ImmatureCoinbaseBalance()
}
