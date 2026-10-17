package wtclient

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/lookout"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/stretchr/testify/require"
)

// randomSecretProducer behaves like a peer that generates its per-commitment
// secrets independently: the base producer's secrets for the two indexes
// exchanged at channel creation, random ones after that.
type randomSecretProducer struct {
	base    shachain.Producer
	secrets map[uint64]chainhash.Hash
}

func (p *randomSecretProducer) AtIndex(i uint64) (*chainhash.Hash, error) {
	if i < 2 {
		return p.base.AtIndex(i)
	}
	if h, ok := p.secrets[i]; ok {
		return &h, nil
	}

	var h chainhash.Hash
	if _, err := rand.Read(h[:]); err != nil {
		return nil, err
	}
	p.secrets[i] = h

	return &h, nil
}

func (p *randomSecretProducer) Encode(w io.Writer) error {
	return p.base.Encode(w)
}

// TestTowerJusticeIndependentSecrets runs the watchtower path end to end on a
// channel with option_independent_secrets whose peer reveals random secrets:
// the client builds the backup for a revoked state from the database, as
// lnd's server does, the tower decrypts it and builds the justice
// transaction, and every input of that transaction must validate against the
// breached commitment.
func TestTowerJusticeIndependentSecrets(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit | channeldb.ZeroHtlcTxFeeBit |
		channeldb.IndependentSecretsBit

	alice, bob, err := lnwallet.CreateTestChannels(t, chanType)
	require.NoError(t, err)

	bob.State().RevocationProducer = &randomSecretProducer{
		base:    bob.State().RevocationProducer,
		secrets: make(map[uint64]chainhash.Hash),
	}

	// Move some money to Bob, so both sides have an output, and keep each
	// of Bob's commitments before it's revoked.
	var revoked []*wire.MsgTx
	for i := 0; i < 4; i++ {
		revoked = append(revoked, bob.State().LocalCommitment.CommitTx)

		preimage := bytes.Repeat([]byte{byte(i + 1)}, 32)
		htlc := &lnwire.UpdateAddHTLC{
			ID:          uint64(i),
			PaymentHash: sha256.Sum256(preimage),
			Amount:      lnwire.NewMSatFromSatoshis(100_000),
			Expiry:      500,
		}
		_, err := alice.AddHTLC(htlc, nil)
		require.NoError(t, err)
		_, err = bob.ReceiveHTLC(htlc)
		require.NoError(t, err)
		require.NoError(t, lnwallet.ForceStateTransition(alice, bob))

		err = bob.SettleHTLC([32]byte(preimage), uint64(i), nil, nil,
			nil)
		require.NoError(t, err)
		require.NoError(t, alice.ReceiveHTLCSettle(
			[32]byte(preimage), uint64(i),
		))
		require.NoError(t, lnwallet.ForceStateTransition(bob, alice))
	}

	// Each round revokes two of Bob's commitments, so revoked[i] is his
	// state 2*i. State 4 had a random secret and is long revoked.
	const breachedState = 4
	breachTx := revoked[breachedState/2]

	// Like lnd's server, build the retribution from the channel as stored.
	stored, err := alice.State().Db.FetchChannel(
		alice.State().FundingOutpoint,
	)
	require.NoError(t, err)

	getBreachInfo := func(lnwire.ChannelID, uint64) (
		*lnwallet.BreachRetribution, channeldb.ChannelType, error) {

		br, err := lnwallet.NewBreachRetribution(
			stored, breachedState, 0, nil,
			fn.Some[lnwallet.AuxLeafStore](
				&lnwallet.MockAuxLeafStore{},
			),
			fn.Some[lnwallet.AuxContractResolver](
				&lnwallet.MockAuxContractResolver{},
			),
		)

		return br, stored.ChanType, err
	}
	br, _, err := getBreachInfo(lnwire.ChannelID{}, breachedState)
	require.NoError(t, err)
	require.Equal(t, breachTx.TxHash(), br.BreachTxHash)

	// The client binds the backup to a session and encrypts it.
	sweepScript := append([]byte{0x00, 0x14}, bytes.Repeat(
		[]byte{0x42}, 20,
	)...)
	session := &wtdb.ClientSessionBody{
		Policy: wtpolicy.Policy{
			TxPolicy: wtpolicy.TxPolicy{
				BlobType:     blob.TypeAltruistAnchorCommit,
				SweepFeeRate: chainfee.SatPerKWeight(2500),
			},
		},
	}

	chanID := lnwire.NewChanIDFromOutPoint(stored.FundingOutpoint)
	task := newBackupTask(wtdb.BackupID{
		ChanID:       chanID,
		CommitHeight: breachedState,
	}, sweepScript)
	require.NoError(t, task.bindSession(session, getBreachInfo))

	hint, encBlob, err := task.craftSessionPayload(alice.Signer)
	require.NoError(t, err)

	// The tower recognises the breach, decrypts the backup and builds the
	// justice transaction.
	breachTxID := breachTx.TxHash()
	require.Equal(t, blob.NewBreachHintFromHash(&breachTxID), hint)

	kit, err := blob.Decrypt(
		blob.NewBreachKeyFromHash(&breachTxID), encBlob,
		session.Policy.BlobType,
	)
	require.NoError(t, err)

	justice := &lookout.JusticeDescriptor{
		BreachedCommitTx: breachTx,
		SessionInfo: &wtdb.SessionInfo{
			Policy: session.Policy,
		},
		JusticeKit: kit,
	}
	justiceTx, err := justice.CreateJusticeTxn()
	require.NoError(t, err)

	// Every input spends an output of the breached commitment, and does so
	// validly: the revocation key behind the to_local signature came from
	// the random secret stored in the revocation log.
	prevOuts := txscript.NewMultiPrevOutFetcher(nil)
	for i, txOut := range breachTx.TxOut {
		prevOuts.AddPrevOut(wire.OutPoint{
			Hash: breachTxID, Index: uint32(i),
		}, txOut)
	}
	sigHashes := txscript.NewTxSigHashes(justiceTx, prevOuts)

	require.NotEmpty(t, justiceTx.TxIn)
	var swept btcutil.Amount
	for i, txIn := range justiceTx.TxIn {
		require.Equal(t, breachTxID, txIn.PreviousOutPoint.Hash)
		prevOut := prevOuts.FetchPrevOutput(txIn.PreviousOutPoint)
		swept += btcutil.Amount(prevOut.Value)

		vm, err := txscript.NewEngine(
			prevOut.PkScript, justiceTx, i,
			txscript.StandardVerifyFlags, nil, sigHashes,
			prevOut.Value, prevOuts,
		)
		require.NoError(t, err)
		require.NoError(t, vm.Execute(), "input %d", i)
	}
	require.Positive(t, swept)
}
