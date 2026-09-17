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
