package chainreg

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// The raw header of mainnet block 961640, the first BLAKE2b block, as served
// by Bitcoin Knots v29.4.1 on 2026-09-12.
const activationHeaderHex = "000000a0657e02138733654183a2c7320d85ca9d743fe139c4bb01000000000000000000c137a8515a0f6b3aaf6049cc7611787c022ad523d51094be0a0363d0dc0bc7684dca936a4f8d001a5671798c84daeb494dca936a00000000b1ccf00d0300000000000000000000001e0300000000000000000000000000000000000068ac0e000000000000000000000000000000000000000000000000000000000000000000"

// The raw header of mainnet block 961639, the last SHA256d block.
const lastSHA256HeaderHex = "10000a205fca17a6566978303e989d163e1aa9dc6715eef5542e0000000000000000000080fe52c98f1c1f8484213dff5a88315f7c334d0705f7d79579b289781868c0dff5c1916a3d350217510c87ed"

// scriptedRPC answers the three RPCs the check uses from fixed values.
type scriptedRPC struct {
	headers    int32
	infoErr    error
	blockHash  map[int64]*chainhash.Hash
	hashErr    error
	headerHex  map[string]string
	rawErr     error
	deployment json.RawMessage
	infoCalls  int
}

func (s *scriptedRPC) GetBlockChainInfo() (*btcjson.GetBlockChainInfoResult, error) {
	s.infoCalls++
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	return &btcjson.GetBlockChainInfoResult{
		Chain: "main", Headers: s.headers, Blocks: s.headers,
	}, nil
}

func (s *scriptedRPC) GetBlockHash(h int64) (*chainhash.Hash, error) {
	if s.hashErr != nil {
		return nil, s.hashErr
	}
	hash, ok := s.blockHash[h]
	if !ok {
		return nil, errors.New("Block height out of range")
	}
	return hash, nil
}

func (s *scriptedRPC) RawRequest(method string, params []json.RawMessage) (json.RawMessage, error) {
	if s.rawErr != nil {
		return nil, s.rawErr
	}
	switch method {
	case "getblockheader":
		var hash string
		if err := json.Unmarshal(params[0], &hash); err != nil {
			return nil, err
		}
		hexStr, ok := s.headerHex[hash]
		if !ok {
			return nil, errors.New("Block not found")
		}
		return json.Marshal(hexStr)
	case "getdeploymentinfo":
		if s.deployment == nil {
			return nil, errors.New("Method not found")
		}
		return s.deployment, nil
	}
	return nil, errors.New("unexpected method " + method)
}

func headerHash(t *testing.T, hexStr string) *chainhash.Hash {
	t.Helper()
	raw, err := hex.DecodeString(hexStr)
	require.NoError(t, err)
	var h wire.BlockHeader
	require.NoError(t, h.Deserialize(bytes.NewReader(raw)))
	hash := h.BlockHash()
	return &hash
}

func mainnetRPC(t *testing.T) *scriptedRPC {
	t.Helper()
	act := headerHash(t, activationHeaderHex)
	require.Equal(t, Blake2bMainnetActivationHash.String(), act.String(),
		"fixture header must hash to the pinned activation hash")
	return &scriptedRPC{
		headers:   971774,
		blockHash: map[int64]*chainhash.Hash{961640: act},
		headerHex: map[string]string{act.String(): activationHeaderHex},
	}
}

func newTestChecker(t *testing.T, rpc blake2bChainRPC, params BitcoinNetParams,
	statusPath string) *chainIdentityChecker {

	t.Helper()
	c := newChainIdentityChecker(rpc, params, statusPath, btclog.Disabled)
	c.pollInterval = time.Millisecond
	c.logInterval = 0
	return c
}

func readStatus(t *testing.T, path string) ChainIdentityStatus {
	t.Helper()
	st, err := ReadChainIdentityStatus(path)
	require.NoError(t, err)
	return *st
}

// TestChainIdentityMainnetConfirmed is the happy path against a scripted
// node that serves the real activation header.
func TestChainIdentityMainnetConfirmed(t *testing.T) {
	dir := t.TempDir()
	path := ChainIdentityStatusPath(dir, "mainnet")
	c := newTestChecker(t, mainnetRPC(t), BitcoinMainNetParams, path)

	height, err := c.run(make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, Blake2bMainnetActivationHeight, height)

	st := readStatus(t, path)
	require.Equal(t, ChainIdentityConfirmed, st.State)
	require.Equal(t, Blake2bMainnetActivationHeight, st.ActivationHeight)
	require.Equal(t, Blake2bMainnetActivationHash.String(), st.ActivationHash)
	require.Equal(t, BitcoinMainNetParams.ChainHash.String(), st.ChainHash)
	require.Equal(t, "mainnet", st.Network)
	require.Empty(t, st.Reason)
}

// TestChainIdentityRefusesSHA256Chain: a node whose block 961640 is an
// 80-byte header is on the SHA256d chain and must be refused.
func TestChainIdentityRefusesSHA256Chain(t *testing.T) {
	dir := t.TempDir()
	path := ChainIdentityStatusPath(dir, "mainnet")

	sha := headerHash(t, lastSHA256HeaderHex)
	rpc := &scriptedRPC{
		headers:   971774,
		blockHash: map[int64]*chainhash.Hash{961640: sha},
		headerHex: map[string]string{sha.String(): lastSHA256HeaderHex},
	}
	c := newTestChecker(t, rpc, BitcoinMainNetParams, path)

	_, err := c.run(make(chan struct{}))
	require.Error(t, err)
	var wrong *ErrWrongChain
	require.True(t, errors.As(err, &wrong), err)
	require.Contains(t, err.Error(), "SHA256d")
	require.Contains(t, err.Error(), "80 bytes")

	st := readStatus(t, path)
	require.Equal(t, ChainIdentityRefused, st.State)
	require.Contains(t, st.Reason, "SHA256d")
}

// TestChainIdentityRefusesWrongPinnedHash: a 164-byte header whose hash is
// not the pinned mainnet activation hash is some other BLAKE2b chain.
func TestChainIdentityRefusesWrongPinnedHash(t *testing.T) {
	rpc := mainnetRPC(t)
	params := BitcoinMainNetParams
	other := SyntheticChainHash(chaincfg.MainNetParams.GenesisHash)
	params.Blake2bActivationHash = &other

	c := newTestChecker(t, rpc, params, "")
	_, err := c.run(make(chan struct{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "different chain")
}

// TestChainIdentityRefusesParserMismatch: if the node reports a hash for the
// activation block that this daemon does not reproduce from the header, the
// daemon must refuse rather than track a chain it cannot hash.
func TestChainIdentityRefusesParserMismatch(t *testing.T) {
	rpc := mainnetRPC(t)
	var bogus chainhash.Hash
	bogus[3] = 0x42
	// The node claims a different id for the same header bytes.
	rpc.headerHex[bogus.String()] = activationHeaderHex
	rpc.blockHash[961640] = &bogus

	params := BitcoinMainNetParams
	params.Blake2bActivationHash = nil // isolate the parser check
	c := newTestChecker(t, rpc, params, "")
	_, err := c.run(make(chan struct{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot reproduce")
}

// TestChainIdentityWaitsForHeight: the node is still below the activation
// height at first; the check waits, records the waiting state, and passes
// once the node catches up.
func TestChainIdentityWaitsForHeight(t *testing.T) {
	dir := t.TempDir()
	path := ChainIdentityStatusPath(dir, "mainnet")
	rpc := mainnetRPC(t)
	rpc.headers = 900000
	c := newTestChecker(t, rpc, BitcoinMainNetParams, path)

	done := make(chan error, 1)
	go func() {
		_, err := c.run(make(chan struct{}))
		done <- err
	}()

	// Give it a few polls in the waiting state, then let the node catch
	// up.
	require.Eventually(t, func() bool {
		st, err := ReadChainIdentityStatus(path)
		return err == nil && st.State == ChainIdentityWaiting &&
			st.NodeHeaders == 900000
	}, time.Second, time.Millisecond)
	rpc.headers = 971774

	require.NoError(t, <-done)
	require.Equal(t, ChainIdentityConfirmed, readStatus(t, path).State)
	require.Greater(t, rpc.infoCalls, 1)
}

// TestChainIdentityWaitQuits: shutting down while waiting returns promptly.
func TestChainIdentityWaitQuits(t *testing.T) {
	rpc := mainnetRPC(t)
	rpc.headers = 1
	c := newTestChecker(t, rpc, BitcoinMainNetParams, "")
	quit := make(chan struct{})
	close(quit)
	_, err := c.run(quit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "shutting down")
}

// TestChainIdentityRPCErrors: every RPC failure is a refusal, never a pass.
func TestChainIdentityRPCErrors(t *testing.T) {
	boom := errors.New("connection refused")

	t.Run("blockchaininfo", func(t *testing.T) {
		rpc := mainnetRPC(t)
		rpc.infoErr = boom
		c := newTestChecker(t, rpc, BitcoinMainNetParams, "")
		_, err := c.run(make(chan struct{}))
		require.ErrorIs(t, err, boom)
	})
	t.Run("blockhash", func(t *testing.T) {
		rpc := mainnetRPC(t)
		rpc.hashErr = boom
		c := newTestChecker(t, rpc, BitcoinMainNetParams, "")
		_, err := c.run(make(chan struct{}))
		var wrong *ErrWrongChain
		require.True(t, errors.As(err, &wrong), err)
	})
	t.Run("blockheader", func(t *testing.T) {
		rpc := mainnetRPC(t)
		rpc.rawErr = boom
		c := newTestChecker(t, rpc, BitcoinMainNetParams, "")
		_, err := c.run(make(chan struct{}))
		var wrong *ErrWrongChain
		require.True(t, errors.As(err, &wrong), err)
	})
	t.Run("truncated header", func(t *testing.T) {
		rpc := mainnetRPC(t)
		act := rpc.blockHash[961640]
		rpc.headerHex[act.String()] = activationHeaderHex[:300]
		c := newTestChecker(t, rpc, BitcoinMainNetParams, "")
		_, err := c.run(make(chan struct{}))
		var wrong *ErrWrongChain
		require.True(t, errors.As(err, &wrong), err)
	})
}

// TestChainIdentityRegtestFormatOnly: on regtest there is no pinned hash;
// the configured height and the header format decide.
func TestChainIdentityRegtestFormatOnly(t *testing.T) {
	act := headerHash(t, activationHeaderHex)
	rpc := &scriptedRPC{
		headers:   30,
		blockHash: map[int64]*chainhash.Hash{20: act},
		headerHex: map[string]string{act.String(): activationHeaderHex},
	}
	params := BitcoinRegTestNetParams
	params.Blake2bActivationHeight = 20
	c := newTestChecker(t, rpc, params, "")

	height, err := c.run(make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, uint32(20), height)

	// Without a configured height and without a node-reported deployment,
	// the check refuses rather than guessing.
	params.Blake2bActivationHeight = 0
	c = newTestChecker(t, rpc, params, "")
	_, err = c.run(make(chan struct{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "blake2b-activation-height")
}

// TestChainIdentityHeightFromNode: testnet-style networks read the height
// from getdeploymentinfo when it is not configured.
func TestChainIdentityHeightFromNode(t *testing.T) {
	act := headerHash(t, activationHeaderHex)
	rpc := &scriptedRPC{
		headers:   200000,
		blockHash: map[int64]*chainhash.Hash{150308: act},
		headerHex: map[string]string{act.String(): activationHeaderHex},
		deployment: json.RawMessage(`{"deployments":{"blake2b":{"type":
			"buried","active":true,"height":150308}}}`),
	}
	c := newTestChecker(t, rpc, BitcoinTestNet4Params, "")
	height, err := c.run(make(chan struct{}))
	require.NoError(t, err)
	require.Equal(t, uint32(150308), height)
}

// TestChainIdentityWatchStopsOnMismatch: the periodic re-check hands a
// failure to the mismatch callback and records it.
func TestChainIdentityWatchStopsOnMismatch(t *testing.T) {
	dir := t.TempDir()
	path := ChainIdentityStatusPath(dir, "mainnet")
	rpc := mainnetRPC(t)
	c := newTestChecker(t, rpc, BitcoinMainNetParams, path)
	_, err := c.run(make(chan struct{}))
	require.NoError(t, err)

	// Swap the node's activation block for a SHA256d header, as a flavor
	// switch under a running daemon would.
	sha := headerHash(t, lastSHA256HeaderHex)
	rpc.blockHash[961640] = sha
	rpc.headerHex[sha.String()] = lastSHA256HeaderHex

	got := make(chan error, 1)
	quit := make(chan struct{})
	go c.watch(Blake2bMainnetActivationHeight, time.Millisecond, quit,
		func(err error) { got <- err })

	select {
	case err := <-got:
		require.Contains(t, err.Error(), "SHA256d")
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not report the chain change")
	}
	close(quit)
	require.Equal(t, ChainIdentityRefused, readStatus(t, path).State)
}

// TestChainIdentityStatusFileAtomic: the file is written whole and the
// temporary file does not linger.
func TestChainIdentityStatusFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "chain-identity.json")
	require.NoError(t, WriteChainIdentityStatus(path, ChainIdentityStatus{
		State: ChainIdentityConfirmed,
	}))
	_, err := os.Stat(path + ".tmp")
	require.True(t, os.IsNotExist(err))
	st, err := ReadChainIdentityStatus(path)
	require.NoError(t, err)
	require.Equal(t, ChainIdentityConfirmed, st.State)
}
