package peer

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

func TestCheckPeerNetworks(t *testing.T) {
	var ours chainhash.Hash
	for i := range ours {
		ours[i] = 0x50
	}
	genesis := *chaincfg.MainNetParams.GenesisHash

	tests := []struct {
		name     string
		theirs   []chainhash.Hash
		required bool
		wantErr  error
	}{
		{"lists ours only", []chainhash.Hash{ours}, true, nil},
		{"lists ours among others", []chainhash.Hash{genesis, ours}, true, nil},
		{"lists genesis only, strict", []chainhash.Hash{genesis}, true,
			&ErrPeerNoCommonChain{}},
		{"lists genesis only, lenient", []chainhash.Hash{genesis}, false,
			&ErrPeerNoCommonChain{}},
		{"silent, strict", nil, true, &ErrPeerNetworksMissing{}},
		{"silent, lenient", nil, false, nil},
		{"empty list, strict", []chainhash.Hash{}, true,
			&ErrPeerNetworksMissing{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPeerNetworks(tc.theirs, ours, tc.required)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			switch tc.wantErr.(type) {
			case *ErrPeerNoCommonChain:
				var e *ErrPeerNoCommonChain
				require.True(t, errors.As(err, &e), err)
				require.Equal(t, ours, e.Ours)
			case *ErrPeerNetworksMissing:
				var e *ErrPeerNetworksMissing
				require.True(t, errors.As(err, &e), err)
			}
		})
	}
}
