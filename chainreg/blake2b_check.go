package chainreg

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
)

// The Bitcoin BLAKE2b chain shares its genesis block, network name and
// address format with Bitcoin, so every check a wallet backend already makes
// passes against a node on the other chain. A node on the SHA256d chain would
// be accepted, sync perfectly well, and report balances and confirmations for
// a chain the operator did not choose. That failure is silent, and on mainnet
// it is silent about money.
//
// The one piece of ordinary chain data that tells the two apart is the block
// header at the activation height: on the BLAKE2b chain it is 164 bytes and
// carries the v2 version bit; on the other chain it is 80 bytes. On mainnet
// its hash is also pinned. No node-specific RPC field is required, so any
// node on either chain can answer, and "cannot tell" is a refusal rather than
// a pass.
//
// The check runs before the chain backend is used and again periodically,
// because both platforms this daemon is packaged for let the operator swap
// the node under a running daemon.

// ChainIdentityState is the outcome of the activation-header check.
type ChainIdentityState string

const (
	// ChainIdentityWaiting means the node has not yet reached the
	// activation height, so the question cannot be answered yet.
	ChainIdentityWaiting ChainIdentityState = "waiting"

	// ChainIdentityConfirmed means the node serves the BLAKE2b chain.
	ChainIdentityConfirmed ChainIdentityState = "confirmed"

	// ChainIdentityRefused means the node is on another chain, or could
	// not be told apart from one.
	ChainIdentityRefused ChainIdentityState = "refused"

	// ChainIdentitySkipped means the check was deliberately not run: a
	// development build following a SHA256d regtest for upstream tests.
	ChainIdentitySkipped ChainIdentityState = "skipped"

	// ChainIdentityFileName is the status file written next to the
	// channel backup in the network's data directory, so that a wrapper
	// can show the outcome before the RPC server is reachable.
	ChainIdentityFileName = "chain-identity.json"

	// DefaultChainIdentityRecheckInterval is how often the check is
	// repeated while the daemon runs.
	DefaultChainIdentityRecheckInterval = 5 * time.Minute

	// chainIdentityPollInterval is how often the node is asked for its
	// header count while waiting for it to reach the activation height.
	chainIdentityPollInterval = 5 * time.Second

	// chainIdentityWaitLogInterval throttles the waiting log line.
	chainIdentityWaitLogInterval = time.Minute
)

// ChainIdentityStatus is what the status file carries.
type ChainIdentityStatus struct {
	State            ChainIdentityState `json:"state"`
	Reason           string             `json:"reason,omitempty"`
	Network          string             `json:"network"`
	ChainHash        string             `json:"chain_hash"`
	ActivationHeight uint32             `json:"activation_height"`
	ActivationHash   string             `json:"activation_hash,omitempty"`
	NodeHeaders      int32              `json:"node_headers,omitempty"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

// ErrWrongChain is returned when the connected node is not on the Bitcoin
// BLAKE2b chain, or cannot be told apart from a node that is not.
type ErrWrongChain struct {
	Reason string
}

// Error implements the error interface.
func (e *ErrWrongChain) Error() string {
	return "the connected Bitcoin node is not on the Bitcoin BLAKE2b " +
		"chain: " + e.Reason
}

// blake2bChainRPC is the slice of the RPC client the check needs. It is an
// interface so the check can be tested against a scripted backend.
type blake2bChainRPC interface {
	GetBlockChainInfo() (*btcjson.GetBlockChainInfoResult, error)
	GetBlockHash(int64) (*chainhash.Hash, error)
	RawRequest(string, []json.RawMessage) (json.RawMessage, error)
}

// chainIdentityChecker runs the activation-header check against one node.
type chainIdentityChecker struct {
	rpc        blake2bChainRPC
	params     BitcoinNetParams
	statusPath string
	log        btclog.Logger

	// pollInterval and logInterval are fields so tests can shorten them.
	pollInterval time.Duration
	logInterval  time.Duration

	// now is a field so tests can pin timestamps.
	now func() time.Time
}

func newChainIdentityChecker(rpc blake2bChainRPC, params BitcoinNetParams,
	statusPath string, logger btclog.Logger) *chainIdentityChecker {

	return &chainIdentityChecker{
		rpc:          rpc,
		params:       params,
		statusPath:   statusPath,
		log:          logger,
		pollInterval: chainIdentityPollInterval,
		logInterval:  chainIdentityWaitLogInterval,
		now:          time.Now,
	}
}

// ChainIdentityStatusPath returns where the status file lives for the given
// chain directory and network name.
func ChainIdentityStatusPath(chainDir, network string) string {
	return filepath.Join(chainDir, network, ChainIdentityFileName)
}

// activationHeight resolves the height of the first BLAKE2b block: from the
// network parameters or configuration when fixed there, otherwise from the
// node's own deployment info.
func (c *chainIdentityChecker) activationHeight() (uint32, error) {
	if c.params.Blake2bActivationHeight != 0 {
		return c.params.Blake2bActivationHeight, nil
	}

	resp, err := c.rpc.RawRequest("getdeploymentinfo", nil)
	if err != nil {
		return 0, &ErrWrongChain{Reason: fmt.Sprintf("unable to ask "+
			"the node for the BLAKE2b activation height on %s (%v) "+
			"and none is configured; set "+
			"--bitcoin.blake2b-activation-height to the height the "+
			"node activates BLAKE2b at", c.params.Name, err)}
	}

	info := struct {
		Deployments map[string]struct {
			Height int64 `json:"height"`
		} `json:"deployments"`
	}{}
	if err := json.Unmarshal(resp, &info); err != nil {
		return 0, fmt.Errorf("unable to decode getdeploymentinfo: %w", err)
	}

	dep, ok := info.Deployments["blake2b"]
	if !ok || dep.Height <= 0 {
		return 0, &ErrWrongChain{Reason: fmt.Sprintf("the node reports "+
			"no blake2b deployment on %s and no activation height is "+
			"configured; set --bitcoin.blake2b-activation-height to "+
			"the height the node activates BLAKE2b at",
			c.params.Name)}
	}

	return uint32(dep.Height), nil
}

// verifyOnce reads the activation header and decides. It returns the block
// id the node reports for the activation block on success.
func (c *chainIdentityChecker) verifyOnce(height uint32) (chainhash.Hash,
	error) {

	nodeHash, err := c.rpc.GetBlockHash(int64(height))
	if err != nil {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"the node cannot serve block %d: %v", height, err)}
	}

	hashParam, err := json.Marshal(nodeHash.String())
	if err != nil {
		return chainhash.Hash{}, err
	}
	verboseParam, err := json.Marshal(false)
	if err != nil {
		return chainhash.Hash{}, err
	}
	resp, err := c.rpc.RawRequest(
		"getblockheader", []json.RawMessage{hashParam, verboseParam},
	)
	if err != nil {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"the node cannot serve the header of block %d: %v",
			height, err)}
	}

	var headerHex string
	if err := json.Unmarshal(resp, &headerHex); err != nil {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"unexpected getblockheader reply for block %d: %v",
			height, err)}
	}
	raw, err := hex.DecodeString(headerHex)
	if err != nil {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"unexpected getblockheader reply for block %d: %v",
			height, err)}
	}

	if len(raw) != wire.BlockHeaderLenV2 {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"the header at the BLAKE2b activation height %d is %d "+
				"bytes, not %d: this node follows the SHA256d "+
				"chain (or has not activated BLAKE2b at that "+
				"height)", height, len(raw), wire.BlockHeaderLenV2)}
	}

	var header wire.BlockHeader
	if err := header.Deserialize(bytes.NewReader(raw)); err != nil {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"the header at height %d does not parse: %v", height, err)}
	}
	if !header.IsV2() {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"the header at the BLAKE2b activation height %d does not "+
				"carry the header-v2 version bit", height)}
	}

	// The node and this daemon must agree on the block id, or chain
	// tracking will read every block as a reorg from here on.
	ourHash := header.BlockHash()
	if ourHash != *nodeHash {
		return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
			"this daemon computes block id %v for the activation "+
				"header but the node reports %v; refusing to follow "+
				"a chain whose block ids it cannot reproduce",
			ourHash, nodeHash)}
	}

	if pinned := c.params.Blake2bActivationHash; pinned != nil {
		if *nodeHash != *pinned {
			return chainhash.Hash{}, &ErrWrongChain{Reason: fmt.Sprintf(
				"the activation block at height %d is %v, but the "+
					"Bitcoin BLAKE2b chain's is %v: this node is on "+
					"a different chain", height, nodeHash, pinned)}
		}
	}

	return *nodeHash, nil
}

// waitForHeight blocks until the node's header chain reaches the activation
// height, writing the waiting status and logging while it does. It returns
// early when quit is closed.
func (c *chainIdentityChecker) waitForHeight(height uint32,
	quit <-chan struct{}) error {

	var lastLog time.Time
	for {
		info, err := c.rpc.GetBlockChainInfo()
		if err != nil {
			return fmt.Errorf("unable to query the node while waiting "+
				"for the BLAKE2b activation height: %w", err)
		}
		if info.Headers >= int32(height) {
			return nil
		}

		c.writeStatus(ChainIdentityStatus{
			State: ChainIdentityWaiting,
			Reason: fmt.Sprintf("waiting for the node to reach block "+
				"%d (it has %d headers)", height, info.Headers),
			ActivationHeight: height,
			NodeHeaders:      info.Headers,
		})
		if c.now().Sub(lastLog) >= c.logInterval {
			c.log.Infof("Waiting for the Bitcoin node to reach the "+
				"BLAKE2b activation height %d before the chain can "+
				"be identified (node has %d headers)", height,
				info.Headers)
			lastLog = c.now()
		}

		select {
		case <-time.After(c.pollInterval):
		case <-quit:
			return errors.New("shutting down while waiting for the " +
				"BLAKE2b activation height")
		}
	}
}

// run performs the full check once: resolve the height, wait for the node to
// have it, verify, and record the outcome. The returned error is fatal.
func (c *chainIdentityChecker) run(quit <-chan struct{}) (uint32, error) {
	height, err := c.activationHeight()
	if err != nil {
		c.writeStatus(ChainIdentityStatus{
			State:  ChainIdentityRefused,
			Reason: err.Error(),
		})
		return 0, err
	}

	if err := c.waitForHeight(height, quit); err != nil {
		return 0, err
	}

	hash, err := c.verifyOnce(height)
	if err != nil {
		c.writeStatus(ChainIdentityStatus{
			State:            ChainIdentityRefused,
			Reason:           err.Error(),
			ActivationHeight: height,
		})
		return 0, err
	}

	c.writeStatus(ChainIdentityStatus{
		State:            ChainIdentityConfirmed,
		ActivationHeight: height,
		ActivationHash:   hash.String(),
	})
	c.log.Infof("Bitcoin BLAKE2b chain confirmed: block %d is %v (%d-byte "+
		"header v2); Lightning chain_hash %v, invoice prefix ln%s",
		height, hash, wire.BlockHeaderLenV2, c.params.ChainHash,
		c.params.InvoiceHRP)

	return height, nil
}

// watch repeats the verification every interval until quit is closed. A
// failure is recorded, logged, and handed to onMismatch, which is expected to
// stop the daemon.
func (c *chainIdentityChecker) watch(height uint32, interval time.Duration,
	quit <-chan struct{}, onMismatch func(error)) {

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-quit:
			return
		}

		hash, err := c.verifyOnce(height)
		if err != nil {
			c.writeStatus(ChainIdentityStatus{
				State:            ChainIdentityRefused,
				Reason:           err.Error(),
				ActivationHeight: height,
			})
			c.log.Criticalf("The Bitcoin node changed chains under a "+
				"running daemon: %v", err)
			onMismatch(err)
			return
		}

		c.writeStatus(ChainIdentityStatus{
			State:            ChainIdentityConfirmed,
			ActivationHeight: height,
			ActivationHash:   hash.String(),
		})
	}
}

// writeStatus records the outcome for the wrappers. Failing to write is
// logged and otherwise ignored: the file is for showing the state, never for
// deciding it.
func (c *chainIdentityChecker) writeStatus(st ChainIdentityStatus) {
	if c.statusPath == "" {
		return
	}

	st.Network = c.params.Name
	st.ChainHash = c.params.ChainHash.String()
	st.UpdatedAt = c.now()

	if err := WriteChainIdentityStatus(c.statusPath, st); err != nil {
		c.log.Warnf("Unable to write %s: %v", c.statusPath, err)
	}
}

// WriteChainIdentityStatus writes the status file atomically.
func WriteChainIdentityStatus(path string, st ChainIdentityStatus) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadChainIdentityStatus reads the status file.
func ReadChainIdentityStatus(path string) (*ChainIdentityStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st ChainIdentityStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}
