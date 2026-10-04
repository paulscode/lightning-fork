//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// headerChain is a ChainKit with one block: the one at the activation height.
type headerChain struct {
	chainrpc.ChainKitClient

	hash    []byte
	header  []byte
	hashErr error

	calls int
}

func (c *headerChain) GetBlockHash(context.Context,
	*chainrpc.GetBlockHashRequest, ...grpc.CallOption) (
	*chainrpc.GetBlockHashResponse, error) {

	c.calls++
	if c.hashErr != nil {
		return nil, c.hashErr
	}

	return &chainrpc.GetBlockHashResponse{BlockHash: c.hash}, nil
}

func (c *headerChain) GetBlockHeader(_ context.Context,
	in *chainrpc.GetBlockHeaderRequest, _ ...grpc.CallOption) (
	*chainrpc.GetBlockHeaderResponse, error) {

	c.calls++

	return &chainrpc.GetBlockHeaderResponse{RawBlockHeader: c.header}, nil
}

// sha256Block is an 80 byte header and the id it hashes to, as a node on the
// SHA256 chain reports them.
func sha256Block(seed byte) ([]byte, []byte) {
	header := make([]byte, 80)
	for i := range header {
		header[i] = seed + byte(i)
	}
	id := chainhash.DoubleHashH(header)

	return header, id[:]
}

func TestCheckNotBlake2b(t *testing.T) {
	t.Parallel()

	const height = 961640
	header, id := sha256Block(1)

	// This node's activation block: anything but the SHA256 node's.
	var ours chainhash.Hash
	copy(ours[:], []byte("blake2b activation block id 32b!"))

	tip := func(h int64) *fakeMain {
		return &fakeMain{info: &lnrpc.GetInfoResponse{BlockHeight: uint32(h)}}
	}
	unimplemented := status.Error(codes.Unimplemented,
		"unknown service chainrpc.ChainKit")
	outOfRange := status.Error(codes.Unknown,
		"-8: Block height out of range")

	// A longer header that does hash to the id given with it, so only
	// its length gives it away.
	blake2bHeader := make([]byte, 164)
	copy(blake2bHeader, header)
	blake2bID := chainhash.DoubleHashH(blake2bHeader)
	wrongID, _ := sha256Block(2)

	for _, tc := range []struct {
		name   string
		chain  *headerChain
		main   *fakeMain
		height int32
		strict bool
		want   string // "" passes; otherwise a piece of the refusal
		config bool   // the refusal is ErrConfig
		notYet bool   // the refusal is errChainNotYet
	}{
		{
			name:   "a SHA256 chain block passes",
			chain:  &headerChain{hash: id, header: header},
			height: height, strict: true,
		},
		{
			name:   "this node's own block is refused",
			chain:  &headerChain{hash: ours[:], header: header},
			height: height, strict: true,
			want: "follows the BLAKE2b chain", config: true,
		},
		{
			name:   "refused off mainnet too",
			chain:  &headerChain{hash: ours[:], header: header},
			height: 300, strict: false,
			want: "follows the BLAKE2b chain", config: true,
		},
		{
			name:   "a BLAKE2b-length header is refused",
			chain:  &headerChain{hash: blake2bID[:], header: blake2bHeader},
			height: height, strict: true,
			want: "not a SHA256 chain block", config: true,
		},
		{
			name:   "a header that does not hash to its id is refused",
			chain:  &headerChain{hash: id, header: wrongID},
			height: height, strict: true,
			want: "not a SHA256 chain block", config: true,
		},
		{
			name:   "no chainrpc on mainnet is refused",
			chain:  &headerChain{hashErr: unimplemented},
			height: height, strict: true,
			want: "without chainrpc", config: true,
		},
		{
			name:   "no chainrpc off mainnet is let through",
			chain:  &headerChain{hashErr: unimplemented},
			height: 300, strict: false,
		},
		{
			name:   "below the height on mainnet waits",
			chain:  &headerChain{hashErr: outOfRange},
			main:   tip(height - 1),
			height: height, strict: true,
			notYet: true,
		},
		{
			name:   "below the height off mainnet passes",
			chain:  &headerChain{hashErr: outOfRange},
			main:   tip(299),
			height: 300, strict: false,
		},
		{
			name:   "an error past the height is an error",
			chain:  &headerChain{hashErr: outOfRange},
			main:   tip(height + 10),
			height: height, strict: true,
			want: "block at height",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := remote(tc.main, nil, nil)
			r.chain = tc.chain

			err := r.CheckNotBlake2b(
				context.Background(), tc.height, ours, tc.strict,
			)

			switch {
			case tc.notYet:
				if !errors.Is(err, errChainNotYet) {
					t.Fatalf("got %v, want to wait", err)
				}

			case tc.want == "":
				if err != nil {
					t.Fatalf("refused: %v", err)
				}

			default:
				if err == nil || !strings.Contains(err.Error(),
					tc.want) {

					t.Fatalf("got %v, want %q", err, tc.want)
				}
				if errors.Is(err, ErrConfig) != tc.config {
					t.Fatalf("ErrConfig=%v, want %v: %v",
						errors.Is(err, ErrConfig),
						tc.config, err)
				}
			}
		})
	}
}

// A network without an activation block asks the node nothing.
func TestCheckNotBlake2bWithoutActivation(t *testing.T) {
	t.Parallel()

	chain := &headerChain{hashErr: errors.New("must not be asked")}
	r := remote(nil, nil, nil)
	r.chain = chain

	if err := r.CheckNotBlake2b(
		context.Background(), 0, chainhash.Hash{}, true,
	); err != nil {
		t.Fatal(err)
	}
	if chain.calls != 0 {
		t.Fatalf("asked the node %d times", chain.calls)
	}
}
