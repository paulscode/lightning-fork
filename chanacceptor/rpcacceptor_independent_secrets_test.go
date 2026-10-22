package chanacceptor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAcceptorCommitmentTypeIgnoresIndependentSecrets checks that a channel
// carrying option_independent_secrets, alone or with option_unified_sigs, is
// reported by its commitment type, as the same channel without it is.
func TestAcceptorCommitmentTypeIgnoresIndependentSecrets(t *testing.T) {
	t.Parallel()

	base := []lnwire.FeatureBit{
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
	}

	for _, extra := range [][]lnwire.FeatureBit{
		{lnwire.IndependentSecretsRequired},
		{lnwire.IndependentSecretsRequired, lnwire.UnifiedSigsRequired},
	} {
		bits := append(append([]lnwire.FeatureBit{}, base...), extra...)
		vec := lnwire.NewRawFeatureVector(bits...)
		require.Equal(t, lnrpc.CommitmentType_ANCHORS,
			acceptorCommitmentType(vec), "%v", bits)

		// The caller's vector is not changed.
		require.True(t, vec.IsSet(lnwire.IndependentSecretsRequired))
	}
}

// TestAcceptorReportsIndependentSecrets checks that the request sent to the
// channel acceptor says whether the proposed channel type carries
// option_independent_secrets, which commitment_type leaves out.
func TestAcceptorReportsIndependentSecrets(t *testing.T) {
	t.Parallel()

	node := btcec.NewPublicKey(
		new(btcec.FieldVal).SetInt(1), new(btcec.FieldVal).SetInt(1),
	)

	for _, withBit := range []bool{false, true} {
		bits := []lnwire.FeatureBit{
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
		}
		if withBit {
			bits = append(bits, lnwire.IndependentSecretsRequired)
		}
		chanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			bits...,
		))

		// The acceptor sends each request once; the mock remote answers
		// the pending channel it names, and the test reads what was sent.
		sent := make(chan *lnrpc.ChannelAcceptRequest, 1)
		pending := make(chan []byte, 1)
		quit := make(chan struct{})
		acceptor := NewRPCAcceptor(
			func() (*lnrpc.ChannelAcceptResponse, error) {
				select {
				case id := <-pending:
					return &lnrpc.ChannelAcceptResponse{
						PendingChanId: id,
					}, nil
				case <-quit:
					return nil, errShuttingDown
				}
			},
			func(req *lnrpc.ChannelAcceptRequest) error {
				sent <- req
				pending <- req.PendingChanId
				return nil
			},
			testTimeout*5, &chaincfg.RegressionNetParams, quit,
		)
		errChan := make(chan error, 1)
		go func() { errChan <- acceptor.Run() }()

		acceptor.Accept(&ChannelAcceptRequest{
			Node: node,
			OpenChanMsg: &lnwire.OpenChannel{
				PendingChannelID: [32]byte{1},
				ChannelType:      &chanType,
			},
		})

		req := <-sent
		require.Equal(t, withBit, req.WantsIndependentSecrets)
		require.Equal(t, lnrpc.CommitmentType_ANCHORS,
			req.CommitmentType)

		close(quit)
		require.ErrorIs(t, <-errChan, errShuttingDown)
	}
}
