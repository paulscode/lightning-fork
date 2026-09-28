package btcwallet

import (
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/waddrmgr"
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
// RelayCoinbaseMaturity directly.
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

// ImmatureCoinbaseBalance returns the value of the coinbase outputs this
// wallet holds that are not yet deep enough for a spend of them to relay.
//
// btcwallet counts them in no spendable balance, since the params it runs
// with carry the relay depth (see walletChainParams), so without this a
// miner paid directly in coinbase outputs would see nothing of them for the
// length of the long maturity. Accounts are shared across key scopes by
// number, so each number is counted once.
func (b *BtcWallet) ImmatureCoinbaseBalance() (btcutil.Amount, error) {
	accounts := make(map[uint32]struct{})
	for _, scopedMgr := range b.wallet.AddrManager().ActiveScopedKeyManagers() {
		results, err := b.wallet.Accounts(scopedMgr.Scope())
		if err != nil {
			return 0, err
		}
		for _, acct := range results.Accounts {
			accounts[acct.AccountNumber] = struct{}{}
		}
	}
	accounts[waddrmgr.ImportedAddrAccount] = struct{}{}

	var immature btcutil.Amount
	for acct := range accounts {
		bals, err := b.wallet.CalculateAccountBalances(acct, 1)
		if err != nil {
			return 0, err
		}
		immature += bals.ImmatureReward
	}

	return immature, nil
}
