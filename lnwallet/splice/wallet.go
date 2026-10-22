package splice

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// LockID marks the wallet leases of splices, so that a lease left by a
	// splice this node forgot (a crash before it was persisted) can be
	// found and released at start.
	LockID = wtxmgr.LockID(sha256.Sum256(
		[]byte("lightning-fork-splice-lock-id"),
	))

	// LeaseDuration is how long a splice's coins are leased. A splice
	// releases them when it aborts or dies, and once it is broadcast the
	// wallet counts them as spent; the expiry only frees coins of a splice
	// this node forgot.
	LeaseDuration = 14 * 24 * time.Hour
)

// Wallet is what a splice needs from the node's wallet.
type Wallet interface {
	// LeaseOutput locks an output to id.
	LeaseOutput(id wtxmgr.LockID, op wire.OutPoint,
		duration time.Duration) (time.Time, error)

	// ReleaseOutput unlocks an output.
	ReleaseOutput(id wtxmgr.LockID, op wire.OutPoint) error

	// PublishTransaction broadcasts a transaction and stores it in the
	// wallet with a label.
	PublishTransaction(tx *wire.MsgTx, label string) error

	// RemoveDescendants removes an unconfirmed transaction, and every
	// one spending it, from the wallet.
	RemoveDescendants(tx *wire.MsgTx) error
}

// Direction is which way a splice moves funds.
type Direction string

const (
	// DirectionIn adds funds to the channel.
	DirectionIn Direction = "in"

	// DirectionOut moves funds out of it.
	DirectionOut Direction = "out"
)

// Label returns the wallet label of a splice transaction:
// "splice:<scid>:<in|out>", with the channel's original short channel id,
// which a splice doesn't change.
func Label(scid lnwire.ShortChannelID, dir Direction) string {
	return fmt.Sprintf("splice:%d:%s", scid.ToUint64(), dir)
}

// IsLabel reports whether a wallet label is a splice's.
func IsLabel(label string) bool {
	return strings.HasPrefix(label, "splice:")
}

// LeaseCoins leases the coins a splice spends. If one can't be leased, the
// ones already leased are released and the error returned.
func LeaseCoins(w Wallet, coins []Coin) error {
	for i, c := range coins {
		_, err := w.LeaseOutput(LockID, c.OutPoint, LeaseDuration)
		if err == nil {
			continue
		}

		releaseErr := ReleaseCoins(w, coins[:i])

		return errors.Join(
			fmt.Errorf("lease %v: %w", c.OutPoint, err), releaseErr,
		)
	}

	return nil
}

// ReleaseCoins releases a splice's leases.
func ReleaseCoins(w Wallet, coins []Coin) error {
	var errs []error
	for _, c := range coins {
		err := w.ReleaseOutput(LockID, c.OutPoint)
		if err != nil && !errors.Is(err, wtxmgr.ErrUnknownOutput) {
			errs = append(errs, fmt.Errorf("release %v: %w",
				c.OutPoint, err))
		}
	}

	return errors.Join(errs...)
}

// Forget undoes a splice that will never confirm, once the spend that
// conflicts with it is buried: the transaction leaves the wallet, so the
// coins it spent count again, and their leases are released.
func Forget(w Wallet, tx *wire.MsgTx, ours []Coin) error {
	return errors.Join(w.RemoveDescendants(tx), ReleaseCoins(w, ours))
}

// RelayState is what is known of a splice transaction's relay.
type RelayState string

const (
	// RelayAccepted is a transaction the backend accepted or already
	// had.
	RelayAccepted RelayState = "in_mempool"

	// RelayRejected is a transaction the backend refused.
	RelayRejected RelayState = "rejected"

	// RelayUnknown is a transaction not yet published.
	RelayUnknown RelayState = "unknown"
)

// RelayStatus is a splice transaction's relay state and, when rejected,
// the backend's reason.
type RelayStatus struct {
	State  RelayState
	Reason string
}

// Publish broadcasts a splice transaction and returns its relay status.
// Publishing a transaction the backend already has succeeds, so it is called
// again at every block while the splice is unconfirmed, which puts back a
// transaction the mempool evicted.
func Publish(w Wallet, tx *wire.MsgTx, label string) RelayStatus {
	err := w.PublishTransaction(tx, label)
	switch {
	case err == nil:
		return RelayStatus{State: RelayAccepted}

	case errors.Is(err, lnwallet.ErrDoubleSpend):
		return RelayStatus{
			State:  RelayRejected,
			Reason: "an input is already spent: " + err.Error(),
		}

	default:
		return RelayStatus{State: RelayRejected, Reason: err.Error()}
	}
}

// publish broadcasts the active splice's transaction and keeps its relay
// status for RelayStatus.
func (m *Machine) publish(tx *wire.MsgTx) RelayStatus {
	status := Publish(m.cfg.Wallet, tx, m.label())

	m.mu.Lock()
	m.relay[tx.TxHash()] = status
	m.mu.Unlock()

	return status
}

// RelayStatus returns what is known of a splice transaction's relay: from
// its last broadcast on this connection, unknown before one. Safe from any
// goroutine.
func (m *Machine) RelayStatus(txid chainhash.Hash) RelayStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	status, ok := m.relay[txid]
	if !ok {
		return RelayStatus{State: RelayUnknown}
	}

	return status
}

// CoinSource lists the wallet's coins.
type CoinSource interface {
	// ListUnspentWitness returns the wallet's unspent witness outputs.
	ListUnspentWitness(minConfs, maxConfs int32,
		accountFilter string) ([]*lnwallet.Utxo, error)

	// FetchTx returns a wallet transaction.
	FetchTx(chainhash.Hash) (*wire.MsgTx, error)
}

// ErrUnknownCoin is returned when a coin asked for isn't one a splice can
// spend: not in the wallet's default account, unconfirmed, spent or leased,
// not P2WKH or P2TR (the only kinds a splice spends), or a coinbase output
// (never spent by a splice; miners paying to their LND wallet meet this one).
// Also when more coins are asked for than a splice takes.
var ErrUnknownCoin = errors.New("coin not spendable by a splice (a splice " +
	"spends only confirmed P2WKH and P2TR coins of the default account, " +
	"never coinbase outputs)")

// OnlyCoins narrows coins to the ones at outpoints, for a splice that may
// spend only those (coin control), and lowers the reserve kept for
// fee-bumping anchor closes by what the coins left out hold: they stay in
// the wallet, so the chosen ones needn't keep it themselves. With no
// outpoints it returns coins and reserve as they are.
func OnlyCoins(coins []Coin, outpoints []wire.OutPoint,
	reserve btcutil.Amount) ([]Coin, btcutil.Amount, error) {

	if len(outpoints) == 0 {
		return coins, reserve, nil
	}

	// A splice takes at most this many of our inputs (SelectAddFunds caps
	// the candidates there): more would quietly leave the rest out.
	if len(outpoints) > interactivetx.MaxInputsOrOutputs-1 {
		return nil, 0, fmt.Errorf("%w: %d coins chosen, a splice takes "+
			"at most %d", ErrUnknownCoin, len(outpoints),
			interactivetx.MaxInputsOrOutputs-1)
	}

	want := make(map[wire.OutPoint]bool, len(outpoints))
	for _, op := range outpoints {
		if want[op] {
			return nil, 0, fmt.Errorf("%w: %v given twice",
				ErrUnknownCoin, op)
		}
		want[op] = true
	}

	var (
		chosen []Coin
		left   btcutil.Amount
	)
	for _, c := range coins {
		if want[c.OutPoint] {
			chosen = append(chosen, c)
			delete(want, c.OutPoint)
		} else {
			left += c.Value
		}
	}
	for op := range want {
		return nil, 0, fmt.Errorf("%w: %v", ErrUnknownCoin, op)
	}

	return chosen, max(0, reserve-left), nil
}

// ListCoins returns the confirmed coins of the default account a splice can
// spend, with their transactions. The caller holds the wallet's coin
// selection lock through selection and leasing.
func ListCoins(w CoinSource) ([]Coin, error) {
	utxos, err := w.ListUnspentWitness(
		1, math.MaxInt32, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	var coins []Coin
	for _, u := range utxos {
		if _, ok := CoinWitnessWeight(u.PkScript); !ok {
			continue
		}
		prev, err := w.FetchTx(u.OutPoint.Hash)
		if err != nil {
			return nil, fmt.Errorf("transaction of %v: %w",
				u.OutPoint, err)
		}
		coin := Coin{
			OutPoint: u.OutPoint, Value: u.Value,
			PkScript: u.PkScript, PrevTx: prev,
		}
		if coin.Usable() {
			coins = append(coins, coin)
		}
	}

	return coins, nil
}
