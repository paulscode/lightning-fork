package lnd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/invoices"
	"github.com/stretchr/testify/require"
)

// A node that has never made an invoice says so with its own error, and the
// bridge must read that as "no such invoice" too, or a new bridge refuses its
// first swap.
func TestBridgeInvoiceAbsent(t *testing.T) {
	require.True(t, bridgeInvoiceAbsent(invoices.ErrInvoiceNotFound))
	require.True(t, bridgeInvoiceAbsent(invoices.ErrNoInvoicesCreated))
	require.True(t, bridgeInvoiceAbsent(
		fmt.Errorf("wrapped: %w", invoices.ErrNoInvoicesCreated)))
	require.False(t, bridgeInvoiceAbsent(errors.New("database is closed")))
	require.False(t, bridgeInvoiceAbsent(nil))
}
