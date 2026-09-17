//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/inventory"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

// Every test in this file pins something a lab run found and a unit test had
// missed. They are together rather than spread through the other files so that
// the set is visible: these are the failures that a passing suite did not
// prevent, and each one is cheap to reintroduce.

// ---------------------------------------------------------------------------
// 1. Reachable is not Check.
//
// The startup gate required both nodes to be caught up. Every node is behind
// its chain for a while after starting, and this one runs inside the node it
// checks, so that made the daemon unbootable on every restart.
// ---------------------------------------------------------------------------

func TestReachableAcceptsANodeThatIsStillCatchingUp(t *testing.T) {
	t.Parallel()

	behind := &fakeNode{height: 800_000, synced: false, blockTime: time.Now()}

	// Check refuses, because no swap may be sized against a stale height.
	if err := local(behind).Check(context.Background()); err == nil {
		t.Error("Check should refuse a node that has not caught up")
	}

	// Reachable accepts, because the node answered and being behind is
	// temporary. Refusing here is what made the daemon unbootable.
	info, err := local(behind).Reachable(context.Background())
	if err != nil {
		t.Fatalf("Reachable refused a node that answered: %v", err)
	}
	if info.SyncedToChain {
		t.Error("Reachable should report the node is not yet synced")
	}
	if info.Height != 800_000 {
		t.Errorf("height %d, wanted 800000", info.Height)
	}
}

func TestReachableStillRefusesANodeThatDoesNotAnswer(t *testing.T) {
	t.Parallel()

	dead := &fakeNode{htErr: errors.New("connection refused")}

	if _, err := local(dead).Reachable(context.Background()); err == nil {
		t.Fatal("Reachable must refuse a node that does not answer: " +
			"a wrong address has to be found at startup rather " +
			"than by a swap that already took someone's money")
	}
}

func TestRemoteReachableMakesTheSameDistinction(t *testing.T) {
	t.Parallel()

	behind := remote(&fakeMain{info: &lnrpc.GetInfoResponse{
		BlockHeight: 800_000, SyncedToChain: false,
		BestHeaderTimestamp: time.Now().Unix(),
	}}, nil, nil)

	if err := behind.Check(context.Background()); err == nil {
		t.Error("Check should refuse a node that has not caught up")
	}
	if _, err := behind.Reachable(context.Background()); err != nil {
		t.Errorf("Reachable refused a node that answered: %v", err)
	}

	dead := remote(&fakeMain{infoErr: errors.New("unavailable")}, nil, nil)
	if _, err := dead.Reachable(context.Background()); err == nil {
		t.Error("Reachable must refuse a node that does not answer")
	}
}

// ---------------------------------------------------------------------------
// 2. Balance excludes what cannot actually be sent.
//
// Channel balance counted funds below the channel reserve and channels whose
// peer is offline. The bridge quoted a swap it could not pay. It failed safely
// and refunded, but it should not have promised it.
// ---------------------------------------------------------------------------

func TestRemoteBalanceExcludesReserveAndInactiveChannels(t *testing.T) {
	t.Parallel()

	main := &fakeMain{channels: &lnrpc.ListChannelsResponse{
		Channels: []*lnrpc.Channel{
			// Spendable: 100,000 less a 10,000 reserve.
			{LocalBalance: 100_000, LocalChanReserveSat: 10_000},

			// Entirely reserve, so it can carry nothing.
			{LocalBalance: 10_000, LocalChanReserveSat: 10_000},

			// Below its reserve, which must not go negative and
			// wrap into an enormous balance.
			{LocalBalance: 5_000, LocalChanReserveSat: 10_000},

			// A nil entry must not panic: this runs while quoting.
			nil,
		},
	}}

	got, err := remote(main, nil, nil).Balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(90_000 * 1000); got != want {
		t.Errorf("balance %d msat, wanted %d: the reserve is the "+
			"operator's own funds but the commitment has to leave "+
			"it behind, so counting it quotes swaps that cannot "+
			"be paid", got, want)
	}
}

// The request must ask for active channels only. A peer that is offline holds
// balance no payment can route through.
func TestRemoteBalanceAsksForActiveChannelsOnly(t *testing.T) {
	t.Parallel()

	main := &fakeMain{channels: &lnrpc.ListChannelsResponse{}}

	if _, err := remote(main, nil, nil).Balance(
		context.Background(),
	); err != nil {

		t.Fatal(err)
	}
	if main.lastChannelsReq == nil {
		t.Fatal("no request was made")
	}
	if !main.lastChannelsReq.GetActiveOnly() {
		t.Error("balance counted channels whose peer may be offline, " +
			"which cannot carry a payment")
	}
}

func TestRemoteBalanceFailsRatherThanReportingZero(t *testing.T) {
	t.Parallel()

	main := &fakeMain{channelsErr: errors.New("unavailable")}

	if _, err := remote(main, nil, nil).Balance(
		context.Background(),
	); err == nil {

		t.Fatal("an unreadable balance must refuse rather than read " +
			"as nothing to pay with")
	}
}

// ---------------------------------------------------------------------------
// 3. Inventory is sized from a real balance, and retried until there is one.
//
// The shipped default is a mainnet half a bitcoin. A node holding less than
// its floor refused every swap while reporting only that it was under a number
// nobody had chosen. And sizing once at startup read a reconnecting peer as
// empty, for the life of the process.
// ---------------------------------------------------------------------------

func TestSizeInventoryDerivesFromWhatTheSideHolds(t *testing.T) {
	t.Parallel()

	const held = 900_000_000

	f := &fakeNode{synced: true, balance: held}
	svc := serviceFor(t, usable(), f, remote(nil, nil, nil))

	sd := svc.sides[0]
	sd.balance = f.deps().ChannelBalance

	// The shipped default would refuse this node outright.
	if sd.inventory.FloorOutgoingMsat <= held {
		t.Fatalf("the default floor %d is not above the balance %d, "+
			"so this test no longer covers what it was written "+
			"for", sd.inventory.FloorOutgoingMsat, held)
	}

	svc.sizeInventory(context.Background())

	if sd.inventory.TargetOutgoingMsat != held {
		t.Errorf("target %d, wanted the balance %d",
			sd.inventory.TargetOutgoingMsat, held)
	}
	if want := uint64(held / DefaultFloorFraction); sd.inventory.
		FloorOutgoingMsat != want {

		t.Errorf("floor %d, wanted %d",
			sd.inventory.FloorOutgoingMsat, want)
	}
	if err := sd.inventory.Valid(); err != nil {
		t.Errorf("the derived policy is unusable: %v", err)
	}
	if !sd.sized {
		t.Error("the side was not marked sized, so it would be " +
			"re-derived on every poll and track the balance " +
			"instead of the position")
	}
}

// A peer still reconnecting reports nothing. That must be retried, not taken
// as the answer for the life of the process.
func TestSizeInventoryRetriesUntilThereIsABalance(t *testing.T) {
	t.Parallel()

	f := &fakeNode{synced: true, balance: 0}
	svc := serviceFor(t, usable(), f, remote(nil, nil, nil))

	sd := svc.sides[0]
	sd.balance = f.deps().ChannelBalance

	svc.sizeInventory(context.Background())
	if sd.sized {
		t.Fatal("a side with nothing to pay with was marked sized, " +
			"so a peer that was merely reconnecting would be " +
			"treated as empty forever")
	}
	if sd.inventory.TargetOutgoingMsat == 0 {
		t.Fatal("the policy was zeroed rather than left alone")
	}

	// The peer comes back.
	f.balance = 900_000_000
	svc.sizeInventory(context.Background())

	if !sd.sized {
		t.Fatal("sizing was not retried once a balance appeared")
	}
	if sd.inventory.TargetOutgoingMsat != 900_000_000 {
		t.Errorf("target %d, wanted 900000000",
			sd.inventory.TargetOutgoingMsat)
	}
}

// An operator who named a target keeps it. Deriving over the top would ignore
// what they asked for.
func TestSizeInventoryLeavesAConfiguredTargetAlone(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.InventoryTargetMsat = 42_000_000

	f := &fakeNode{synced: true, balance: 900_000_000}
	svc := serviceFor(t, cfg, f, remote(nil, nil, nil))

	sd := svc.sides[0]
	sd.balance = f.deps().ChannelBalance

	svc.sizeInventory(context.Background())

	if sd.inventory.TargetOutgoingMsat != 42_000_000 {
		t.Errorf("target %d, wanted the configured 42000000",
			sd.inventory.TargetOutgoingMsat)
	}
}

// A balance too small to fund a single swap is not a working balance. Sizing
// against it would call the side fully stocked while it can pay nothing, and
// quote at the base spread on a position that deserves the widest.
func TestSizeInventoryRefusesToSizeFromDust(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))
	sd := svc.sides[0]
	before := sd.inventory

	for _, held := range []uint64{
		1, svc.res.quote.MinSwapMsat - 1,
	} {
		sd.sized = false
		sd.inventory = before
		sd.balance = func(context.Context) (uint64, error) {
			return held, nil
		}

		svc.sizeInventory(context.Background())

		if sd.sized {
			t.Errorf("a balance of %d msat, under the %d msat "+
				"minimum swap, was taken as a working "+
				"balance", held, svc.res.quote.MinSwapMsat)
		}
		if sd.inventory.TargetOutgoingMsat != before.TargetOutgoingMsat {
			t.Errorf("the policy was sized from %d msat, which "+
				"cannot fund one swap", held)
		}
	}

	// And it still sizes once there is enough.
	sd.balance = func(context.Context) (uint64, error) {
		return svc.res.quote.MinSwapMsat * 10, nil
	}
	svc.sizeInventory(context.Background())

	if !sd.sized {
		t.Error("a balance that can fund swaps was not used")
	}
	if err := sd.inventory.Valid(); err != nil {
		t.Errorf("the derived policy is unusable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4. The chain observers are seeded from history.
//
// They learn block spacing from tips, one per poll, and will not estimate
// without a hundred in the current epoch. That is most of a day on a ten
// minute chain, after every restart, refusing every swap throughout.
// ---------------------------------------------------------------------------

func TestBackfillMakesTheChainsMeasurable(t *testing.T) {
	t.Parallel()

	const spacing = 10 * time.Minute

	now := time.Now()
	f := &fakeNode{synced: true, height: 800_000, blockTime: now}

	// Both sides serve history at a plausible spacing.
	deps := f.deps()
	deps.BlockAt = func(_ context.Context, h int32) (BlockInfo, error) {
		back := time.Duration(800_000-h) * spacing

		return BlockInfo{
			Height: h, Time: now.Add(-back), SyncedToChain: true,
		}, nil
	}

	main := &fakeMain{info: &lnrpc.GetInfoResponse{
		BlockHeight: 700_000, SyncedToChain: true,
		BestHeaderTimestamp: now.Unix(),
	}}
	chain := &fakeChain{at: func(h int32) (int64, error) {
		back := time.Duration(700_000-h) * spacing

		return now.Add(-back).Unix(), nil
	}}

	svc := serviceFor(t, usable(), f, remoteWithChain(main, chain))
	svc.local = NewLocal(deps)

	// Before: nothing measurable, which is the state that refuses swaps.
	if _, err := svc.spacing(false)(context.Background()); err == nil {
		t.Fatal("the chains were measurable before any block was seen")
	}

	svc.backfill(context.Background())

	rates, err := svc.spacing(false)(context.Background())
	if err != nil {
		t.Fatalf("the chains are still not measurable after reading "+
			"history, so the bridge would refuse every swap for "+
			"most of a day after each restart: %v", err)
	}
	if !rates.Incoming.Valid() || !rates.Outgoing.Valid() {
		t.Errorf("unusable bounds after backfill: %+v", rates)
	}
}

// A node that cannot serve history is slow, not broken. Stopping the bridge
// over it would mean a stock lnd built without ChainKit cannot be bridged to
// at all.
func TestBackfillFallsBackWhenHistoryIsUnavailable(t *testing.T) {
	t.Parallel()

	f := &fakeNode{synced: true, height: 800_000, blockTime: time.Now()}
	deps := f.deps()
	deps.BlockAt = func(context.Context, int32) (BlockInfo, error) {
		return BlockInfo{}, status.Error(
			codes.Unimplemented, "unknown service chainrpc.ChainKit",
		)
	}

	svc := serviceFor(t, usable(), f, remote(&fakeMain{
		info: &lnrpc.GetInfoResponse{
			BlockHeight: 700_000, SyncedToChain: true,
			BestHeaderTimestamp: time.Now().Unix(),
		},
	}, nil, nil))
	svc.local = NewLocal(deps)

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.backfill(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("backfill did not give up on a node without history")
	}
}

// ---------------------------------------------------------------------------
// 5. The journal's directory is created.
//
// It defaults under the network directory, nothing else makes it, and a first
// start failed on a path the operator never chose.
// ---------------------------------------------------------------------------

func TestTheJournalDirectoryIsCreated(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.Journal = filepath.Join(t.TempDir(), "bridge", "swaps.journal")

	if _, err := os.Stat(filepath.Dir(cfg.Journal)); !os.IsNotExist(err) {
		t.Fatal("the directory already exists, so this no longer " +
			"covers what it was written for")
	}

	svc, err := newService(
		&cfg, NewLocal((&fakeNode{synced: true}).deps()),
		remote(nil, nil, nil),
	)
	if err != nil {
		t.Fatalf("a first start failed on a directory the operator "+
			"never chose: %v", err)
	}
	t.Cleanup(svc.close)
}

// ---------------------------------------------------------------------------
// The rest of the runtime, which the lab exercised and nothing else did.
// ---------------------------------------------------------------------------

// A tip that reads as nothing must not be added: a zero height or a zero
// timestamp carries no spacing, and feeding one would corrupt the measurement
// every swap is sized against.
func TestSampleIgnoresAnEmptyTip(t *testing.T) {
	t.Parallel()

	for name, info := range map[string]BlockInfo{
		"no height": {Height: 0, Time: time.Now()},
		"no time":   {Height: 800_000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := &fakeNode{synced: true}
			deps := f.deps()
			deps.BestBlock = func(context.Context) (BlockInfo,
				error) {

				return info, nil
			}

			svc := serviceFor(t, usable(), f, remote(nil, nil, nil))
			svc.local = NewLocal(deps)

			svc.sample(context.Background())

			if _, err := svc.lfChain.Estimate(
				time.Now(),
			); err == nil {

				t.Error("an empty tip was taken as a " +
					"measurement")
			}
		})
	}
}

// Resume has to report a swap nothing will drive. That is an HTLC with a
// deadline and no watcher, which is the worst way to be quiet.
func TestResumeReportsASwapNoDirectionCanPay(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.ToBlake2b = false

	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))

	now := time.Now()
	err := svc.journal.Put(context.Background(), store.Record{
		Hash: [32]byte{9}, State: swap.Funded, OutgoingCLTVLimit: 40,
		Invoice: "lnbcrt1orphan", IncomingMsat: 2_000_000,
		OutgoingMsat: 2_000_000, Rate: 1, Spread: 0.01,
		Created: now, Updated: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// It logs rather than returning, so the check is that it neither
	// panics nor drives anything it cannot pay.
	svc.resume(context.Background())

	if n := svc.active(); n != 0 {
		t.Errorf("%d swaps are being driven, wanted none: nothing "+
			"can pay this one", n)
	}
}

func TestResumeWithNothingPendingIsQuiet(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	svc.resume(context.Background())

	if n := svc.active(); n != 0 {
		t.Errorf("%d swaps being driven from an empty journal", n)
	}
}

// Stop must be safe when nothing was started, and must not hang.
func TestStopWithoutStartDoesNotHang(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.stop()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung with nothing running")
	}
}

// Dialling has to refuse a configuration it cannot use, rather than producing
// a connection that fails later inside a swap.
func TestDialRefusesUnusableCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.macaroon")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}

	for name, cfg := range map[string]*Config{
		"no address": {
			BitcoinMacaroonPath: empty,
		},
		"no such certificate": {
			BitcoinRPCHost:      "127.0.0.1:10009",
			BitcoinTLSCertPath:  filepath.Join(dir, "missing.cert"),
			BitcoinMacaroonPath: empty,
		},
		"no such macaroon": {
			BitcoinRPCHost: "127.0.0.1:10009",
			BitcoinMacaroonPath: filepath.Join(
				dir, "missing.macaroon",
			),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := dialBitcoinNode(cfg)
			if err == nil {
				t.Fatal("wanted a refusal")
			}
			if !errors.Is(err, ErrConfig) {
				t.Errorf("should be an ErrConfig: %v", err)
			}
		})
	}

	t.Run("an empty macaroon", func(t *testing.T) {
		t.Parallel()

		// A readable but empty macaroon authenticates nothing, and
		// would fail on the first call rather than here.
		cert := filepath.Join(dir, "tls.cert")
		if err := os.WriteFile(cert, selfSignedPEM, 0600); err != nil {
			t.Fatal(err)
		}

		_, err := dialBitcoinNode(&Config{
			BitcoinRPCHost:      "127.0.0.1:10009",
			BitcoinTLSCertPath:  cert,
			BitcoinMacaroonPath: empty,
		})
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("an empty macaroon should be refused by "+
				"name: %v", err)
		}
	})
}

// The sub-server must refuse to start without access to the node, rather than
// accepting a quote it could not act on.
func TestDriverRefusesWithoutDeps(t *testing.T) {
	t.Parallel()

	_, _, err := createNewSubServer(fakeRegistry{cfg: &Config{}})
	if err == nil {
		t.Fatal("a sub-server with no access to the node was created")
	}
	if !strings.Contains(err.Error(), "access to itself") {
		t.Errorf("the refusal should say what is missing: %v", err)
	}
}

// fakeRegistry hands back one config, which is all createNewSubServer asks for.
type fakeRegistry struct {
	cfg *Config
}

func (f fakeRegistry) FetchConfig(string) (interface{}, bool) {
	return f.cfg, true
}

// active counts across directions, and a nil runner is not a crash.
func TestActiveCountsNothingWhenNothingIsDriving(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	if n := svc.active(); n != 0 {
		t.Errorf("active reported %d with nothing driving", n)
	}

	svc.sides = append(svc.sides, &side{name: "no runner"})
	if n := svc.active(); n != 0 {
		t.Errorf("active reported %d for a side with no runner", n)
	}
}

// The logger bridge must not lose records or panic on any level, since it is
// the only place the swap packages' output goes.
func TestTheLoggerBridgeHandlesEveryLevel(t *testing.T) {
	t.Parallel()

	l := bridgeLogger("toBitcoin")

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			l.Debug("debug", "k", 1)
			l.Info("info", "k", 2)
			l.Warn("warn", "k", 3)
			l.Error("error", "k", 4)
			l.With("extra", "v").Info("with attrs")
			l.WithGroup("g").Info("grouped")
		}()
	}
	wg.Wait()
}

// selfSignedPEM is a certificate that parses, for the dial tests that need to
// get past reading one. It authenticates nothing and is never connected to.
var selfSignedPEM = []byte(`-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`)

// fakeChain serves block headers, for the backfill test. It embeds the
// generated client so the compiler keeps it in step as ChainKit grows.
type fakeChain struct {
	chainrpc.ChainKitClient

	at func(height int32) (int64, error)
}

func (f *fakeChain) GetBlockHash(_ context.Context,
	in *chainrpc.GetBlockHashRequest, _ ...grpc.CallOption) (
	*chainrpc.GetBlockHashResponse, error) {

	// The height is carried through the hash so GetBlockHeader can answer
	// for it without a second map.
	h := make([]byte, 8)
	binary.LittleEndian.PutUint64(h, uint64(in.GetBlockHeight()))

	return &chainrpc.GetBlockHashResponse{BlockHash: h}, nil
}

func (f *fakeChain) GetBlockHeader(_ context.Context,
	in *chainrpc.GetBlockHeaderRequest, _ ...grpc.CallOption) (
	*chainrpc.GetBlockHeaderResponse, error) {

	height := int32(binary.LittleEndian.Uint64(in.GetBlockHash()))

	ts, err := f.at(height)
	if err != nil {
		return nil, err
	}

	// A header is 80 bytes with the timestamp at 68.
	raw := make([]byte, 80)
	binary.LittleEndian.PutUint32(raw[68:72], uint32(ts))

	return &chainrpc.GetBlockHeaderResponse{RawBlockHeader: raw}, nil
}

// observerFor is a convenience for asserting on what an observer holds.
func observerFor(t *testing.T, p chainrate.Params) *chainrate.Observer {
	t.Helper()

	obs, err := chainrate.New(p)
	if err != nil {
		t.Fatal(err)
	}

	return obs
}

var _ = observerFor
var _ = inventory.DefaultPolicy
var _ grpc.ClientConnInterface

// A macaroon is a bearer token: sent in the clear it is the whole of that
// node's authority, handed to whoever is listening. The credential must refuse
// to travel over an unencrypted connection.
func TestTheMacaroonRefusesToTravelInTheClear(t *testing.T) {
	t.Parallel()

	cred := macaroonCredential{hex: "abcd"}

	if !cred.RequireTransportSecurity() {
		t.Error("the macaroon would be sent over an unencrypted " +
			"connection, which hands the Bitcoin node's authority " +
			"to anyone listening")
	}

	md, err := cred.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if md["macaroon"] != "abcd" {
		t.Errorf("the macaroon header is %q, wanted the hex", md)
	}
}

// The logger bridge is the only place the swap packages' output goes, so it
// has to survive every level and every shape of record. This one turns the
// node's logger on first, because with it off the handler is never reached and
// the test would pass without running anything.
func TestTheLoggerBridgeActuallyRenders(t *testing.T) {
	before := log
	UseLogger(btclog.NewSLogger(btclog.NewDefaultHandler(io.Discard)))
	t.Cleanup(func() { UseLogger(before) })

	l := bridgeLogger("toBitcoin")

	l.Debug("debug", "k", 1)
	l.Info("info", "k", 2)
	l.Warn("warn", "k", 3)
	l.Error("error", "k", 4)
	l.With("extra", "v").Info("with attrs")
	l.WithGroup("g").Info("grouped")

	if !l.Enabled(context.Background(), slog.LevelError) {
		t.Error("errors from the swap packages would be dropped")
	}
}

// drive classifies what the runner returns. Only a real failure is an error:
// being already driven is the one-goroutine-per-hash rule working, and a
// cancelled context is shutdown, after which the next process picks the swap
// up from the journal.
func TestDriveTreatsBusyAndShutdownAsNormal(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))
	svc.ctx = context.Background()

	// A side whose runner has no driver returns an error rather than
	// touching a node, which is enough to exercise the classification
	// without a swap.
	sd := svc.sides[0]

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.drive(sd, [32]byte{1})
		svc.wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("drive did not return")
	}
}

// ---------------------------------------------------------------------------
// 6. The poll loop and a live quote share the inventory policy.
//
// sizeInventory derives it from a real balance while a quote prices against
// it, which is two goroutines on one struct. Unguarded, a quote can read a new
// target beside an old floor: not a policy anyone chose, and the number it
// produces prices real money. Found by looking for shared state rather than by
// anything failing.
// ---------------------------------------------------------------------------

func TestQuotingWhileSizingIsNotARace(t *testing.T) {
	f := &fakeNode{synced: true, balance: 900_000_000}
	svc := serviceFor(t, usable(), f, remote(nil, nil, nil))

	sd := svc.sides[0]
	sd.balance = f.deps().ChannelBalance

	var wg sync.WaitGroup
	wg.Add(2)

	// The poll loop, sizing over and over.
	go func() {
		defer wg.Done()
		for range 200 {
			sd.mu.Lock()
			sd.sized = false
			sd.mu.Unlock()

			svc.sizeInventory(context.Background())
		}
	}()

	// A caller quoting throughout.
	go func() {
		defer wg.Done()
		for range 200 {
			r, err := svc.pricer(sd)(context.Background())
			if err != nil {
				continue
			}

			// Whatever policy was read, the spread it produced has
			// to be a real one. A mixture of two policies can give
			// a negative or absurd number, which is the failure
			// this guards rather than the race detector's report.
			if r.Spread < 0 || r.Spread >= 1 {
				t.Errorf("a quote priced at a spread of %g, "+
					"which is not a quote", r.Spread)

				return
			}
		}
	}()

	wg.Wait()
}

// A quote taken while the node is going down creates a hold invoice that will
// not be driven until the next start. The journal means it is picked up rather
// than lost, but promising a swap on the way out is worse than declining one.
func TestQuotesAreRefusedWhileShuttingDown(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	s.svc = serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}

	_, err := s.Quote(context.Background(), &QuoteRequest{
		Invoice: "lnbcrt1payme",
	})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("a quote during shutdown answered %v, wanted "+
			"Unavailable", status.Code(err))
	}
}

// A hash that is not a hash must be refused by length rather than silently
// truncated or padded into one that names a different swap.
func TestLookupSwapRefusesAMalformedHash(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	s.svc = serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	for name, hash := range map[string][]byte{
		"empty":     nil,
		"too short": make([]byte, 16),
		"too long":  make([]byte, 33),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.LookupSwap(
				context.Background(),
				&LookupSwapRequest{Hash: hash},
			)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("answered %v, wanted InvalidArgument",
					status.Code(err))
			}
		})
	}
}

// An unknown swap is NotFound, not an internal error: asking about one that
// was never quoted is an ordinary thing to do.
func TestLookupSwapOfAnUnknownHash(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	s.svc = serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	_, err := s.LookupSwap(context.Background(), &LookupSwapRequest{
		Hash: make([]byte, 32),
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("answered %v, wanted NotFound", status.Code(err))
	}
}

// ---------------------------------------------------------------------------
// 7. Commitments are counted per direction.
//
// Both directions share one journal and the two chains' millisatoshis are not
// the same unit, so summing all of it and subtracting from one side's balance
// compares quantities that do not mean the same thing. Wrong both ways and
// unsafe one way: the chain whose unit is numerically larger has its
// commitments under-counted, and the bridge promises more of it than it has.
//
// Inherited from the standalone daemon, which does the same thing. Found by
// reading for shared state, not by anything failing.
// ---------------------------------------------------------------------------

// sidedService wires each direction to a node that decodes only its own
// invoices, which is how a swap is attributed to a direction.
func sidedService(t *testing.T, balance uint64) *service {
	t.Helper()

	f := &fakeNode{synced: true, balance: balance}
	svc := serviceFor(t, usable(), f, remote(nil, nil, nil))

	for _, sd := range svc.sides {
		sd.balance = func(context.Context) (uint64, error) {
			return balance, nil
		}

		accepts := "lnblake"
		if sd.name == "toBitcoin" {
			accepts = "lnbc"
		}
		sd.out = &fakeOutDecoder{accepts: accepts}
	}

	return svc
}

// fakeOutDecoder decodes only invoices carrying its prefix.
type fakeOutDecoder struct {
	node.Outgoing

	accepts string
}

func (f *fakeOutDecoder) Decode(_ context.Context, inv string) (node.Decoded,
	error) {

	if !strings.HasPrefix(inv, f.accepts) {
		return node.Decoded{}, errors.New("not an invoice this node " +
			"can pay")
	}

	return node.Decoded{AmountMsat: 1}, nil
}

func TestHeadroomCountsOnlyItsOwnSide(t *testing.T) {
	t.Parallel()

	const held = 10_000_000

	svc := sidedService(t, held)

	// A swap paying out on BLAKE2b. In real units this is a BTCB2 amount,
	// which is roughly three hundred times a Bitcoin one of the same
	// value: subtracting it from the Bitcoin side is not a smaller
	// mistake, it is a different quantity.
	now := time.Now()
	err := svc.journal.Put(context.Background(), store.Record{
		Hash: [32]byte{1}, State: swap.Funded, OutgoingCLTVLimit: 40,
		Invoice: "lnblakert1payme", IncomingMsat: 3_000,
		OutgoingMsat: 4_000_000, Rate: 1, Spread: 0.01,
		Created: now, Updated: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, sd := range svc.sides {
		got, err := svc.headroom(sd)(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		want := uint64(held)
		if sd.name == "toBlake2b" {
			want = held - 4_000_000
		}
		if got != want {
			t.Errorf("%s headroom %d, wanted %d: the pending swap "+
				"pays out on BLAKE2b, so only that side's "+
				"room is reduced", sd.name, got, want)
		}
	}
}

// A swap nothing can attribute is counted against every side. Leaving it out
// of the one it belongs to would let the bridge promise those funds twice, and
// over-counting only refuses swaps it might have served.
func TestAnUnattributableSwapIsCountedAgainstEverySide(t *testing.T) {
	t.Parallel()

	const held = 10_000_000

	svc := sidedService(t, held)

	now := time.Now()
	err := svc.journal.Put(context.Background(), store.Record{
		Hash: [32]byte{2}, State: swap.Funded, OutgoingCLTVLimit: 40,
		Invoice:      "something no node here can read",
		IncomingMsat: 3_000, OutgoingMsat: 4_000_000, Rate: 1,
		Spread: 0.01, Created: now, Updated: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, sd := range svc.sides {
		got, err := svc.headroom(sd)(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != held-4_000_000 {
			t.Errorf("%s headroom %d, wanted %d: a swap that "+
				"cannot be attributed has to be counted "+
				"everywhere rather than nowhere", sd.name, got,
				held-4_000_000)
		}
	}
}

// A swap whose payment has already left is not a commitment. Paid and Failing
// are still unfinished, so the journal returns them, but their outgoing money
// has gone and the node's balance already reflects it: counting them again
// would subtract the same funds twice and shrink the bridge's room for no
// reason.
//
// Terminal states are excluded by the journal itself, so testing with those
// would pass whether or not this filter existed. That is what the first
// version of this test did.
func TestASwapThatHasAlreadyPaidCommitsNothing(t *testing.T) {
	t.Parallel()

	const held = 10_000_000

	svc := sidedService(t, held)

	now := time.Now()
	for i, st := range []swap.State{swap.Paid, swap.Failing} {
		if st.Terminal() {
			t.Fatalf("%v is terminal, so the journal filters it "+
				"and this test would pass without the code it "+
				"is written for", st)
		}

		err := svc.journal.Put(context.Background(), store.Record{
			Hash: [32]byte{byte(10 + i)}, State: st,
			OutgoingCLTVLimit: 40, Invoice: "lnblakert1done",
			IncomingMsat: 3_000, OutgoingMsat: 4_000_000, Rate: 1,
			Spread: 0.01, Created: now, Updated: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, sd := range svc.sides {
		got, err := svc.headroom(sd)(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != held {
			t.Errorf("%s headroom %d, wanted the whole %d: a swap "+
				"whose payment has left commits nothing more",
				sd.name, got, held)
		}
	}
}

// ---------------------------------------------------------------------------
// 8. The swap bounds mean one amount of value, not one number.
//
// The swap packages bound the outgoing leg in the units of the chain that leg
// is on, which is a different unit per direction. One configured pair shared
// by both capped two different amounts of value, a couple of hundred times
// apart at any plausible rate. Same class as the headroom bug, found by asking
// the same question of a different pair of numbers.
// ---------------------------------------------------------------------------

// The swap bounds cap the outgoing leg, in the units of the chain that leg is
// on. One pair of numbers shared by both directions caps two different
// amounts of value.
func TestSwapBoundsCapTheSameValueBothWays(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.FixedRate = 0.003
	cfg.MaxSwapMsat = 150_000_000

	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))

	caps := map[string]uint64{}
	for _, sd := range svc.sides {
		caps[sd.name] = sd.quoter.Policy.MaxSwapMsat
	}

	// toBitcoin pays in BTC, so its cap is the configured number.
	// toBlake2b pays in BTCB2, worth 0.003 BTC each, so the same value is
	// a much larger number of them.
	wantB2 := uint64(float64(cfg.MaxSwapMsat) / cfg.FixedRate)

	if caps["toBitcoin"] != cfg.MaxSwapMsat {
		t.Errorf("toBitcoin cap %d, wanted %d", caps["toBitcoin"],
			cfg.MaxSwapMsat)
	}
	if caps["toBlake2b"] != wantB2 {
		t.Errorf("toBlake2b cap %d msat of BTCB2, wanted %d: the two "+
			"directions cap the same value, and a BTCB2 "+
			"millisatoshi is not a Bitcoin one",
			caps["toBlake2b"], wantB2)
	}
}

// sizeInventory compares a balance against a minimum swap. The balance is in
// the units of the chain that side pays on, so the minimum has to be the
// side's own converted one and not the configured Bitcoin figure.
func TestSizingUsesTheSidesOwnMinimum(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.FixedRate = 0.003
	cfg.MinSwapMsat = 1_000_000

	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))

	var reverse *side
	for _, sd := range svc.sides {
		if sd.invert {
			reverse = sd
		}
	}

	// A BTCB2 balance that clears the Bitcoin figure but not the real
	// BTCB2 minimum. Sizing against it would call the side stocked while
	// it cannot fund one swap.
	held := uint64(2_000_000)
	if held >= reverse.quoter.Policy.MinSwapMsat {
		t.Fatalf("the balance %d already clears the converted "+
			"minimum %d, so this no longer covers what it was "+
			"written for", held, reverse.quoter.Policy.MinSwapMsat)
	}

	reverse.balance = func(context.Context) (uint64, error) {
		return held, nil
	}
	svc.sizeInventory(context.Background())

	if reverse.isSized() {
		t.Errorf("a BTCB2 balance of %d was taken as a working "+
			"balance because it cleared a Bitcoin floor of %d",
			held, cfg.MinSwapMsat)
	}
}

// Which direction each swap belongs to is remembered so headroom need not
// decode an invoice on every quote. Nothing forgot any of them, so the map
// grew for the life of the process: small per swap, unbounded over months.
func TestFinishedSwapsAreForgotten(t *testing.T) {
	t.Parallel()

	svc := sidedService(t, 10_000_000)
	ctx := context.Background()

	now := time.Now()
	pending := store.Record{
		Hash: [32]byte{1}, State: swap.Funded, OutgoingCLTVLimit: 40,
		Invoice: "lnblakert1payme", IncomingMsat: 3_000,
		OutgoingMsat: 1_000, Rate: 1, Spread: 0.01,
		Created: now, Updated: now,
	}
	if err := svc.journal.Put(ctx, pending); err != nil {
		t.Fatal(err)
	}

	// Remember a swap the journal has never heard of, standing in for one
	// that has since finished.
	svc.remember([32]byte{99}, svc.sides[0])

	if _, err := svc.committedOn(ctx, svc.sides[0]); err != nil {
		t.Fatal(err)
	}

	svc.sideMu.Lock()
	_, stale := svc.sideOf[[32]byte{99}]
	_, live := svc.sideOf[[32]byte{1}]
	size := len(svc.sideOf)
	svc.sideMu.Unlock()

	if stale {
		t.Error("a swap that is no longer unfinished is still " +
			"remembered, so the map grows for the life of the " +
			"process")
	}
	if !live {
		t.Error("an unfinished swap was forgotten, which would make " +
			"headroom decode its invoice again on every quote")
	}
	if size != 1 {
		t.Errorf("the map holds %d entries, wanted only the "+
			"unfinished swap", size)
	}
}

// ---------------------------------------------------------------------------
// 9. A caller hanging up must not abandon a half-created swap.
//
// Quoting creates a hold invoice on this node and then records the swap. Run
// on the caller's context, a caller who gives up between those two leaves the
// node holding an invoice the journal has no record of.
//
// This is a regression: the standalone daemon's first adversarial pass found
// exactly this and fixed it, and the same mistake was made again here.
// ---------------------------------------------------------------------------

func TestQuotingDoesNotUseTheCallersContext(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))
	svc.ctx = context.Background()
	s.svc = svc

	// A side whose decode records the state of the context at the moment
	// it was handed one. Recording the context itself and reading it after
	// Quote returns would see it already cancelled by Quote's own defer,
	// which says nothing about what the work ran under.
	var seen ctxState
	for _, sd := range svc.sides {
		sd.out = &ctxRecordingOut{seen: &seen}
	}

	// A caller who has already given up.
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	_, _ = s.Quote(dead, &QuoteRequest{Invoice: "lnbcrt1payme"})

	if !seen.called {
		t.Fatal("the quote never reached a node")
	}
	if seen.err != nil {
		t.Errorf("the quote ran on a context that was already done, "+
			"so a caller hanging up abandons the swap partway: %v",
			seen.err)
	}
}

// ctxState is what a context looked like when the work was handed it.
type ctxState struct {
	called      bool
	err         error
	hasDeadline bool
}

// ctxRecordingOut records what the context looked like when a decode was given
// one, then refuses so the quote stops there.
type ctxRecordingOut struct {
	node.Outgoing

	seen *ctxState
}

func (c *ctxRecordingOut) Decode(ctx context.Context, _ string) (node.Decoded,
	error) {

	_, hasDeadline := ctx.Deadline()
	*c.seen = ctxState{
		called: true, err: ctx.Err(), hasDeadline: hasDeadline,
	}

	return node.Decoded{}, errors.New("not this node")
}

// The quote's own context must still be bounded, or a node that never answers
// holds the quoter's lock indefinitely.
func TestTheQuoteContextIsBounded(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))
	svc.ctx = context.Background()
	s.svc = svc

	var seen ctxState
	for _, sd := range svc.sides {
		sd.out = &ctxRecordingOut{seen: &seen}
	}

	_, _ = s.Quote(context.Background(), &QuoteRequest{
		Invoice: "lnbcrt1payme",
	})

	if !seen.called {
		t.Fatal("the quote never reached a node")
	}
	if !seen.hasDeadline {
		t.Error("the quote runs on a context with no deadline, so a " +
			"node that never answers holds the quoter's lock")
	}
}

// ---------------------------------------------------------------------------
// 10. Two things the standalone daemon does that the port left behind.
//
// The fifth pass's lesson was to read what this was ported from. These are
// what that found: a fix the daemon made after its own second pass, and a
// check its health endpoint does that Status did not.
// ---------------------------------------------------------------------------

// The journal is append-only, so every state a swap passes through is another
// line. Without compaction it grows for the life of the node, is replayed in
// full at every start, and being inside the lnd data directory, is copied into
// every backup.
func TestTheJournalIsCompacted(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Write a swap through several states, then finish it, so there is
	// something for compaction to drop.
	now := time.Now()
	rec := store.Record{
		Hash: [32]byte{7}, State: swap.Quoted, OutgoingCLTVLimit: 40,
		Invoice: "lnblakert1payme", IncomingMsat: 3_000,
		OutgoingMsat: 1_000, Rate: 1, Spread: 0.01,
		Created: now, Updated: now,
	}
	for _, st := range []swap.State{
		swap.Quoted, swap.Offered, swap.Funded, swap.Paying,
		swap.Paid, swap.Settled,
	} {
		rec.State = st
		rec.Updated = time.Now()
		if err := svc.journal.Put(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	before := journalSize(t, svc.cfg.Journal)

	// Compaction is a loop on a ticker, so call the work directly rather
	// than waiting an hour for it.
	if err := svc.journal.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	if after := journalSize(t, svc.cfg.Journal); after >= before {
		t.Errorf("the journal is %d bytes after compaction and was "+
			"%d before, so finished swaps are never dropped and "+
			"it grows for the life of the node", after, before)
	}
}

// And start has to schedule it, or the above is a method nobody invokes. This
// runs the real loop rather than calling the work directly, because calling
// the work directly is exactly what would still pass if nothing scheduled it.
func TestStartSchedulesCompaction(t *testing.T) {
	t.Parallel()

	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))
	svc.compactEvery = 10 * time.Millisecond

	now := time.Now()
	rec := store.Record{
		Hash: [32]byte{8}, State: swap.Quoted, OutgoingCLTVLimit: 40,
		Invoice: "lnblakert1payme", IncomingMsat: 3_000,
		OutgoingMsat: 1_000, Rate: 1, Spread: 0.01,
		Created: now, Updated: now,
	}
	for _, st := range []swap.State{
		swap.Quoted, swap.Offered, swap.Funded, swap.Paying,
		swap.Paid, swap.Settled,
	} {
		rec.State = st
		rec.Updated = time.Now()
		if err := svc.journal.Put(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}

	before := journalSize(t, svc.cfg.Journal)

	svc.start()
	defer svc.stop()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if journalSize(t, svc.cfg.Journal) < before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Errorf("the journal is still %d bytes after starting the bridge, "+
		"so nothing schedules compaction and it grows for the life "+
		"of the node", journalSize(t, svc.cfg.Journal))
}

func journalSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.Size()
}

// A bridge with no outbound capacity on a side is up, synced, correctly
// configured, and refuses every swap that way. It is the most common thing to
// be wrong and is guaranteed on a freshly created node, so Status has to say
// it rather than leaving it to appear per quote as a number to interpret.
func TestStatusNamesASideThatCannotPay(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))
	svc.ctx = context.Background()

	// One side funded, the other empty.
	for _, sd := range svc.sides {
		if sd.name == "toBitcoin" {
			sd.balance = func(context.Context) (uint64, error) {
				return 900_000_000, nil
			}

			continue
		}
		sd.balance = func(context.Context) (uint64, error) {
			return 0, nil
		}
	}
	s.svc = svc

	resp, err := s.Status(context.Background(), &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}

	var named bool
	for _, r := range resp.Refusals {
		if strings.Contains(r, "toBlake2b") &&
			strings.Contains(r, "outbound capacity") {

			named = true
		}
		if strings.Contains(r, "toBitcoin") &&
			strings.Contains(r, "outbound capacity") {

			t.Errorf("a funded side was reported as unable to "+
				"pay: %v", r)
		}
	}
	if !named {
		t.Errorf("a side with nothing to pay with was not named: %v",
			resp.Refusals)
	}
}

// A balance that cannot be read is not zero, it is unknown, and the difference
// is what an operator needs to see.
func TestStatusNamesAnUnreadableBalance(t *testing.T) {
	t.Parallel()

	s := serverWith(t, true, &fakeNode{synced: true})
	svc := serviceFor(t, usable(), &fakeNode{synced: true},
		remote(nil, nil, nil))
	svc.ctx = context.Background()

	for _, sd := range svc.sides {
		sd.balance = func(context.Context) (uint64, error) {
			return 0, errors.New("node is unavailable")
		}
	}
	s.svc = svc

	resp, err := s.Status(context.Background(), &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}

	var named bool
	for _, r := range resp.Refusals {
		if strings.Contains(r, "cannot read") {
			named = true
		}
	}
	if !named {
		t.Errorf("an unreadable paying balance was not reported: %v",
			resp.Refusals)
	}
}
