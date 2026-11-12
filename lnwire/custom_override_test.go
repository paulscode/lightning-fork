package lnwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCustomOverrideOfKnownType checks overrides of types lnd decodes
// itself: accepted, as upstream does, since they let the custom message RPCs
// send such a type (a test peer sending its own shutdown, for one), reported
// by HandledOverrides, and still decoded by lnd when received; a type lnd
// doesn't know becomes a custom message.
func TestCustomOverrideOfKnownType(t *testing.T) {
	// Not parallel: the overrides are a package global.
	t.Cleanup(func() {
		require.NoError(t, SetCustomOverrides(nil))
	})

	known := []uint16{
		uint16(MsgTxAddInput), uint16(MsgSpliceInit),
		uint16(MsgStartBatch), uint16(MsgCommitSig),
		uint16(MsgShutdown), uint16(MsgUpdateFulfillHTLC),
	}
	require.NoError(t, SetCustomOverrides(known))
	require.Len(t, HandledOverrides(known), len(known))
	for _, k := range known {
		_, err := NewCustom(MessageType(k), nil)
		require.NoError(t, err, "type %d sendable", k)

		msg, err := MakeEmptyMessage(MessageType(k))
		require.NoError(t, err)
		_, custom := msg.(*Custom)
		require.False(t, custom, "type %d still decoded by lnd", k)
	}

	// 100 is unassigned.
	require.NoError(t, SetCustomOverrides([]uint16{100}))
	require.Empty(t, HandledOverrides([]uint16{100}))
	require.True(t, IsCustomOverride(100))
	msg, err := MakeEmptyMessage(100)
	require.NoError(t, err)
	require.IsType(t, &Custom{}, msg)
}
