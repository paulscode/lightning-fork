package itest

import (
	"testing"

	"github.com/lightningnetwork/lnd/lntest/node"
)

// blake2bSkippedCases are the test cases that cannot pass against a Bitcoin
// Knots backend at its default policy, which is what a BLAKE2b regtest runs
// on, with the reason each is skipped there. Every other case runs.
var blake2bSkippedCases = map[string]string{
	// The to_local revocation leaf of a simple taproot channel,
	// <local_delayedpubkey> OP_DROP <revocationkey> OP_CHECKSIG, is refused
	// by Knots' dropped-push data carrier rule, so the justice transaction
	// is rejected with txn-datacarrier-nonstandard by the node's own
	// backend. Needs Knots with bitcoinknots/bitcoin#487
	// (https://github.com/bitcoinknots/bitcoin/pull/487); run these again
	// once a Knots release contains it.
	"revoked uncooperative close retribution": "taproot justice " +
		"transaction needs bitcoinknots/bitcoin#487",
	"revoked uncooperative close retribution remote hodl": "taproot " +
		"justice transaction needs bitcoinknots/bitcoin#487",
	"watchtower-revoked close retribution altruist": "taproot justice " +
		"transaction needs bitcoinknots/bitcoin#487",

	// Knots' default -mempooltruc=accept treats a v3 transaction like any
	// other, so the test's zero-fee v3 parent is refused ("min relay fee
	// not met"). That is Knots policy, not something lnd relies on: lnd
	// only passes SubmitPackage through for its callers.
	"submit package": "Knots' default -mempooltruc=accept refuses a " +
		"zero-fee v3 parent",
}

// checkBlake2bSkippedCases fails the run on a BLAKE2b regtest if a name in
// blake2bSkippedCases is not a test case, so a renamed case cannot silently
// start running there.
func checkBlake2bSkippedCases(t *testing.T) {
	if node.Blake2bHeight() == 0 {
		return
	}

	names := make(map[string]bool, len(allTestCases))
	for _, tc := range allTestCases {
		names[tc.Name] = true
	}
	for name := range blake2bSkippedCases {
		if !names[name] {
			t.Fatalf("blake2bSkippedCases names %q, which is "+
				"not a test case", name)
		}
	}
}

// skipOnBlake2b skips the test case on a BLAKE2b regtest if it is one of
// blake2bSkippedCases.
func skipOnBlake2b(t *testing.T, name string) {
	if node.Blake2bHeight() == 0 {
		return
	}

	if reason, ok := blake2bSkippedCases[name]; ok {
		t.Skipf("skipped on a BLAKE2b regtest: %s", reason)
	}
}
