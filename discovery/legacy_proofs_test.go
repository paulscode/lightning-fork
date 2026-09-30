package discovery

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/legacychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

// mainnetLegacyHash is the chain_hash mainnet advertised until 2026-09-17,
// which the test harness's announcements are re-signed under.
func mainnetLegacyHash(t *testing.T) chainhash.Hash {
	t.Helper()

	legacy, ok := legacychain.For(*chaincfg.MainNetParams.GenesisHash)
	require.True(t, ok)

	return legacy
}

// signAnnUnder signs a copy of ann with its chain_hash set to under, then
// restores the chain_hash it carries: an announcement as releases up to .9
// made it, and as this fork relays it. keys are the two node keys and the two
// bitcoin keys, in that order.
func signAnnUnder(t *testing.T, ann *lnwire.ChannelAnnouncement1,
	under chainhash.Hash,
	keys [4]*btcec.PrivateKey) *lnwire.ChannelAnnouncement1 {

	t.Helper()

	a := *ann
	carried := a.ChainHash
	a.ChainHash = under

	sigs := make([]lnwire.Sig, 4)
	for i, key := range keys {
		sig, err := netann.SignAnnouncement(
			&mock.SingleSigner{Privkey: key}, testKeyLoc, &a,
		)
		require.NoError(t, err)

		sigs[i], err = lnwire.NewSigFromSignature(sig)
		require.NoError(t, err)
	}
	a.NodeSig1, a.NodeSig2 = sigs[0], sigs[1]
	a.BitcoinSig1, a.BitcoinSig2 = sigs[2], sigs[3]
	a.ChainHash = carried

	return &a
}

// assertNoBroadcast fails if anything is broadcast for a while.
func assertNoBroadcast(t *testing.T, tCtx *testCtx) {
	t.Helper()

	select {
	case msg := <-tCtx.broadcastedMessage:
		t.Fatalf("unexpected broadcast of %T", msg.msg)
	case <-time.After(2 * trickleDelay):
	}
}

// awaitBroadcastAnn waits for a channel announcement to be broadcast,
// skipping anything else, and returns it.
func awaitBroadcastAnn(t *testing.T,
	tCtx *testCtx) *lnwire.ChannelAnnouncement1 {

	t.Helper()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-tCtx.broadcastedMessage:
			ann, ok := msg.msg.(*lnwire.ChannelAnnouncement1)
			if ok {
				return ann
			}

		case <-deadline:
			t.Fatal("no channel announcement was broadcast")
		}
	}
}

func storedProof(t *testing.T, tCtx *testCtx,
	scid lnwire.ShortChannelID) *models.ChannelAuthProof {

	t.Helper()

	info, _, _, err := tCtx.router.GetChannelByID(scid)
	require.NoError(t, err)

	return info.AuthProof
}

// TestLegacyProofAcceptedNotRelayed checks that a remote announcement signed
// under the withdrawn chain_hash still enters this node's graph, and is not
// broadcast: no other implementation can check it.
func TestLegacyProofAcceptedNotRelayed(t *testing.T) {
	t.Parallel()

	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tCtx, err := createTestCtx(t, 0, false)
			require.NoError(t, err)

			ann, err := tCtx.createRemoteChannelAnnouncement(0)
			require.NoError(t, err)
			if legacy {
				ann = signAnnUnder(
					t, ann, mainnetLegacyHash(t),
					[4]*btcec.PrivateKey{
						remoteKeyPriv1, remoteKeyPriv2,
						bitcoinKeyPriv1, bitcoinKeyPriv2,
					},
				)
			}

			peer := &mockPeer{
				remoteKeyPriv1.PubKey(), nil, nil,
				atomic.Bool{},
			}
			sendRemoteMsg(t, tCtx, ann, peer)

			require.NotNil(t, storedProof(t, tCtx, ann.ShortChannelID),
				"accepted into the graph either way")
			require.Equal(t, legacy,
				tCtx.gossiper.legacyProofs.has(ann.ShortChannelID))

			if legacy {
				assertNoBroadcast(t, tCtx)
			} else {
				got := awaitBroadcastAnn(t, tCtx)
				require.Equal(t, ann.ShortChannelID,
					got.ShortChannelID)
			}
		})
	}
}

// TestLegacyProofUpgradedByFreshAnnouncement checks that a known channel's
// legacy proof is replaced when an announcement signed over the message as
// sent arrives, and that nothing else replaces it.
func TestLegacyProofUpgradedByFreshAnnouncement(t *testing.T) {
	t.Parallel()

	tCtx, err := createTestCtx(t, 0, false)
	require.NoError(t, err)

	fresh, err := tCtx.createRemoteChannelAnnouncement(0)
	require.NoError(t, err)

	keys := [4]*btcec.PrivateKey{
		remoteKeyPriv1, remoteKeyPriv2, bitcoinKeyPriv1, bitcoinKeyPriv2,
	}
	old := signAnnUnder(t, fresh, mainnetLegacyHash(t), keys)
	scid := old.ShortChannelID

	peer := &mockPeer{remoteKeyPriv1.PubKey(), nil, nil, atomic.Bool{}}
	sendRemoteMsg(t, tCtx, old, peer)
	require.True(t, tCtx.gossiper.legacyProofs.has(scid))
	assertNoBroadcast(t, tCtx)

	// Another copy of the legacy announcement changes nothing.
	sendRemoteMsg(t, tCtx, old, peer)
	require.True(t, tCtx.gossiper.legacyProofs.has(scid))
	require.Equal(t, old.NodeSig1.ToSignatureBytes(),
		storedProof(t, tCtx, scid).NodeSig1())
	assertNoBroadcast(t, tCtx)

	// Nor does one validly signed, but for other bitcoin keys than the
	// stored channel's: its signatures do not cover what this node knows
	// of the channel.
	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	mismatched := *fresh
	copy(mismatched.BitcoinKey2[:], otherKey.PubKey().SerializeCompressed())
	signed := signAnnUnder(
		t, &mismatched, mismatched.ChainHash,
		[4]*btcec.PrivateKey{
			remoteKeyPriv1, remoteKeyPriv2, bitcoinKeyPriv1,
			otherKey,
		},
	)
	require.NoError(t, netann.ValidateChannelAnnStrict(signed))
	sendRemoteMsg(t, tCtx, signed, peer)
	require.True(t, tCtx.gossiper.legacyProofs.has(scid))
	assertNoBroadcast(t, tCtx)

	// The real one replaces the proof and is announced.
	sendRemoteMsg(t, tCtx, fresh, peer)
	require.False(t, tCtx.gossiper.legacyProofs.has(scid))

	proof := storedProof(t, tCtx, scid)
	require.Equal(t, fresh.NodeSig1.ToSignatureBytes(), proof.NodeSig1())
	require.Equal(t, fresh.BitcoinSig2.ToSignatureBytes(),
		proof.BitcoinSig2())

	got := awaitBroadcastAnn(t, tCtx)
	require.Equal(t, scid, got.ShortChannelID)
	require.NoError(t, netann.ValidateChannelAnnStrict(got),
		"what goes out now verifies for everyone")
}

// resignHarness is a test context holding one of this node's channels whose
// proof was signed under the withdrawn chain_hash, as a node upgraded from
// .9 has.
type resignHarness struct {
	tCtx       *testCtx
	batch      *annBatch
	remotePeer *mockPeer
	sentMsgs   chan lnwire.Message
	resigned   chan lnwire.ShortChannelID
	scid       lnwire.ShortChannelID
}

func newResignHarness(t *testing.T) *resignHarness {
	t.Helper()

	tCtx, err := createTestCtx(t, proofMatureDelta, false)
	require.NoError(t, err)

	sentMsgs := make(chan lnwire.Message, 10)
	tCtx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(
		target [33]byte, peerChan chan<- lnpeer.Peer) {

		pk, _ := btcec.ParsePubKey(target[:])
		select {
		case peerChan <- &mockPeer{
			pk, sentMsgs, tCtx.gossiper.quit, atomic.Bool{},
		}:
		case <-tCtx.gossiper.quit:
		}
	}

	// Signed under the current chain_hash: the halves both sides sign
	// again.
	batch, err := tCtx.createLocalAnnouncements(0)
	require.NoError(t, err)

	remotePeer := &mockPeer{
		remoteKeyPriv1.PubKey(), sentMsgs, tCtx.gossiper.quit,
		atomic.Bool{},
	}

	// The channel as a node upgraded from .9 holds it: in the graph, with
	// the proof both sides assembled back then, under the old chain_hash.
	// This node's own announcement arrives locally, as the funding manager
	// hands it over; it carries no proof.
	sendLocalMsg(t, tCtx, batch.chanAnn)
	assertNoBroadcast(t, tCtx)

	old := signAnnUnder(
		t, batch.chanAnn, mainnetLegacyHash(t),
		[4]*btcec.PrivateKey{
			selfKeyPriv, remoteKeyPriv1, bitcoinKeyPriv1,
			bitcoinKeyPriv2,
		},
	)
	require.NoError(t, tCtx.router.AddProof(
		old.ShortChannelID, models.NewV1ChannelAuthProof(
			old.NodeSig1.ToSignatureBytes(),
			old.NodeSig2.ToSignatureBytes(),
			old.BitcoinSig1.ToSignatureBytes(),
			old.BitcoinSig2.ToSignatureBytes(),
		),
	))

	// As the startup scan finds it.
	tCtx.gossiper.legacyProofs.add(old.ShortChannelID)
	require.True(t, tCtx.gossiper.legacyProofs.isLegacyAnn(old))

	h := &resignHarness{
		tCtx:       tCtx,
		batch:      batch,
		remotePeer: remotePeer,
		sentMsgs:   sentMsgs,
		resigned:   make(chan lnwire.ShortChannelID, 1),
		scid:       old.ShortChannelID,
	}

	// What the funding manager does: sign this node's half and hand it
	// back to the gossiper, without waiting.
	tCtx.gossiper.cfg.ResignChannelProof = func(
		scid lnwire.ShortChannelID) error {

		h.resigned <- scid
		go func() {
			_ = tCtx.gossiper.ProcessLocalAnnouncement(
				batch.localProofAnn,
			)
		}()

		return nil
	}

	return h
}

// assertReplaced checks the proof was replaced by the fresh one and the
// channel announced again, in a form every implementation can check.
func (h *resignHarness) assertReplaced(t *testing.T) {
	t.Helper()

	got := awaitBroadcastAnn(t, h.tCtx)
	require.Equal(t, h.scid, got.ShortChannelID)
	require.NoError(t, netann.ValidateChannelAnnStrict(got))

	require.False(t, h.tCtx.gossiper.legacyProofs.has(h.scid))
	proof := storedProof(t, h.tCtx, h.scid)
	require.Equal(t, h.batch.chanAnn.NodeSig1.ToSignatureBytes(),
		proof.NodeSig1())
	require.Equal(t, h.batch.chanAnn.NodeSig2.ToSignatureBytes(),
		proof.NodeSig2())
}

// assertHalfSentToPeer checks this node's fresh half went to the peer.
func (h *resignHarness) assertHalfSentToPeer(t *testing.T) {
	t.Helper()

	select {
	case msg := <-h.sentMsgs:
		sig, ok := msg.(*lnwire.AnnounceSignatures1)
		require.True(t, ok, "sent %T", msg)
		require.Equal(t, h.batch.localProofAnn.NodeSignature,
			sig.NodeSignature)

	case <-time.After(3 * time.Second):
		t.Fatal("this node's half was not sent to the peer")
	}
}

// TestLegacyProofResignedPeerFirst: the peer, upgraded, sends its fresh half
// first. This node answers with its own, and the new proof replaces the old.
func TestLegacyProofResignedPeerFirst(t *testing.T) {
	t.Parallel()

	h := newResignHarness(t)

	sendRemoteMsg(t, h.tCtx, h.batch.remoteProofAnn, h.remotePeer)

	select {
	case scid := <-h.resigned:
		require.Equal(t, h.scid, scid)
	case <-time.After(3 * time.Second):
		t.Fatal("this node did not sign its half again")
	}

	h.assertHalfSentToPeer(t)
	h.assertReplaced(t)
}

// TestLegacyProofResignedLocalFirst: this node signs first, as its startup
// scan has it do; the half is sent to the peer, kept, and completed when the
// peer's arrives. Until then the stored proof stays, and so does the half.
func TestLegacyProofResignedLocalFirst(t *testing.T) {
	t.Parallel()

	h := newResignHarness(t)

	sendLocalMsg(t, h.tCtx, h.batch.localProofAnn)
	h.assertHalfSentToPeer(t)
	assertNoBroadcast(t, h.tCtx)

	require.True(t, h.tCtx.gossiper.legacyProofs.has(h.scid))
	require.False(t, h.tCtx.gossiper.isMsgStale(
		t.Context(), h.batch.localProofAnn,
	), "the half replacing a legacy proof is not stale")

	sendRemoteMsg(t, h.tCtx, h.batch.remoteProofAnn, h.remotePeer)
	h.assertReplaced(t)

	require.True(t, h.tCtx.gossiper.isMsgStale(
		t.Context(), h.batch.localProofAnn,
	), "once replaced, the half has done its job")

	select {
	case <-h.resigned:
		t.Fatal("signed again though this node's half was in")
	default:
	}
}

// TestLegacyProofResignRefusesLegacyHalf: a peer that has not upgraded
// answers with its old half, signed under the withdrawn chain_hash. That is
// no replacement, and the stored proof stays.
func TestLegacyProofResignRefusesLegacyHalf(t *testing.T) {
	t.Parallel()

	h := newResignHarness(t)

	old := signAnnUnder(
		t, h.batch.chanAnn, mainnetLegacyHash(t),
		[4]*btcec.PrivateKey{
			selfKeyPriv, remoteKeyPriv1, bitcoinKeyPriv1,
			bitcoinKeyPriv2,
		},
	)
	oldHalf := *h.batch.remoteProofAnn
	oldHalf.NodeSignature = old.NodeSig2
	oldHalf.BitcoinSignature = old.BitcoinSig2

	sendLocalMsg(t, h.tCtx, h.batch.localProofAnn)
	h.assertHalfSentToPeer(t)

	err := mustProcess(t, h.tCtx.gossiper.ProcessRemoteAnnouncement(
		t.Context(), &oldHalf, h.remotePeer,
	))
	require.Error(t, err)

	require.True(t, h.tCtx.gossiper.legacyProofs.has(h.scid))
	require.Equal(t, old.NodeSig1.ToSignatureBytes(),
		storedProof(t, h.tCtx, h.scid).NodeSig1())
	assertNoBroadcast(t, h.tCtx)
}

// fakeSeries answers the queries the scan and the served series make.
type fakeSeries struct {
	ChannelGraphTimeSeries

	anns     []lnwire.Message
	horizon  []lnwire.Message
	fetched  [][]lnwire.ShortChannelID
	fetchErr error
	known    map[lnwire.ShortChannelID]bool
}

func (f *fakeSeries) FilterKnownChanIDs(_ chainhash.Hash,
	superSet []graphdb.ChannelUpdateInfo,
	_ func(graphdb.ChannelUpdateInfo) bool) ([]lnwire.ShortChannelID,
	error) {

	var unknown []lnwire.ShortChannelID
	for _, c := range superSet {
		if !f.known[c.ShortChannelID] {
			unknown = append(unknown, c.ShortChannelID)
		}
	}

	return unknown, nil
}

func (f *fakeSeries) FilterChannelRange(_ chainhash.Hash, start, end uint32,
	_ bool) ([]graphdb.BlockChannelRange, error) {

	var ranges []graphdb.BlockChannelRange
	for _, m := range f.anns {
		ann, ok := m.(*lnwire.ChannelAnnouncement1)
		if !ok {
			continue
		}
		h := ann.ShortChannelID.BlockHeight
		if h < start || h > end {
			continue
		}
		ranges = append(ranges, graphdb.BlockChannelRange{
			Height: h,
			Channels: []graphdb.ChannelUpdateInfo{
				{ShortChannelID: ann.ShortChannelID},
			},
		})
	}

	return ranges, nil
}

func (f *fakeSeries) FetchChanAnns(_ chainhash.Hash,
	scids []lnwire.ShortChannelID) ([]lnwire.Message, error) {

	f.fetched = append(f.fetched, scids)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}

	want := make(map[lnwire.ShortChannelID]bool)
	for _, s := range scids {
		want[s] = true
	}

	var out []lnwire.Message
	for _, m := range f.anns {
		switch msg := m.(type) {
		case *lnwire.ChannelAnnouncement1:
			if want[msg.ShortChannelID] {
				out = append(out, msg)
			}
		case *lnwire.ChannelUpdate1:
			if want[msg.ShortChannelID] {
				out = append(out, msg)
			}
		default:
			out = append(out, msg)
		}
	}

	return out, nil
}

func (f *fakeSeries) UpdatesInHorizon(_ context.Context, _,
	_ time.Time) iter.Seq2[lnwire.Message, error] {

	return func(yield func(lnwire.Message, error) bool) {
		for _, m := range f.horizon {
			if !yield(m, nil) {
				return
			}
		}
	}
}

// testAnn returns an announcement at height, signed under signedUnder and
// carrying the mainnet chain_hash, between node1 and node2.
func testAnn(t *testing.T, height uint32, signedUnder chainhash.Hash,
	node1, node2 *btcec.PrivateKey) *lnwire.ChannelAnnouncement1 {

	t.Helper()

	a := &lnwire.ChannelAnnouncement1{
		ChainHash: *chaincfg.MainNetParams.GenesisHash,
		ShortChannelID: lnwire.ShortChannelID{
			BlockHeight: height, TxIndex: 1,
		},
		Features: lnwire.NewRawFeatureVector(),
	}
	copy(a.NodeID1[:], node1.PubKey().SerializeCompressed())
	copy(a.NodeID2[:], node2.PubKey().SerializeCompressed())
	copy(a.BitcoinKey1[:], bitcoinKeyPub1.SerializeCompressed())
	copy(a.BitcoinKey2[:], bitcoinKeyPub2.SerializeCompressed())

	return signAnnUnder(t, a, signedUnder, [4]*btcec.PrivateKey{
		node1, node2, bitcoinKeyPriv1, bitcoinKeyPriv2,
	})
}

// TestLegacyProofScan checks the startup scan: every legacy proof in the
// graph is recorded, this node's own are handed on to be signed again, and
// current ones are left alone.
func TestLegacyProofScan(t *testing.T) {
	t.Parallel()

	current := *chaincfg.MainNetParams.GenesisHash
	legacy := mainnetLegacyHash(t)

	own := testAnn(t, 1000, legacy, selfKeyPriv, remoteKeyPriv1)
	theirs := testAnn(t, 1001, legacy, remoteKeyPriv1, remoteKeyPriv2)
	fine := testAnn(t, 1002, current, selfKeyPriv, remoteKeyPriv2)
	upd, err := createUpdateAnnouncement(1002, 0, selfKeyPriv, 1)
	require.NoError(t, err)

	series := &fakeSeries{
		anns: []lnwire.Message{own, theirs, fine, upd},
	}

	var self [33]byte
	copy(self[:], selfKeyPriv.PubKey().SerializeCompressed())

	var resigned []lnwire.ShortChannelID
	l := newLegacyProofs()
	require.False(t, l.scanned.Load())

	confirm := func(l *legacyProofs) func(*lnwire.ChannelAnnouncement1) bool {
		return func(a *lnwire.ChannelAnnouncement1) bool {
			l.add(a.ShortChannelID)
			return true
		}
	}
	err = l.scan(
		t.Context(), series, current, 0, 2000, self, confirm(l),
		func(scid lnwire.ShortChannelID) {
			resigned = append(resigned, scid)
		},
	)
	require.NoError(t, err)

	require.True(t, l.scanned.Load())
	require.True(t, l.has(own.ShortChannelID))
	require.True(t, l.has(theirs.ShortChannelID))
	require.False(t, l.has(fine.ShortChannelID))
	require.Equal(t, 2, l.len())
	require.Equal(t, []lnwire.ShortChannelID{own.ShortChannelID}, resigned)

	// After the scan, a lookup decides, without the signatures.
	require.True(t, l.isLegacyAnn(own))
	require.False(t, l.isLegacyAnn(fine))

	// Nothing below the floor is scanned.
	l2 := newLegacyProofs()
	require.NoError(t, l2.scan(
		t.Context(), series, current, 1001, 2000, self, confirm(l2),
		func(lnwire.ShortChannelID) {},
	))
	require.False(t, l2.has(own.ShortChannelID))

	// A channel whose proof was replaced since its batch was read is not
	// marked: confirm, which checks the stored proof again, says no.
	l3 := newLegacyProofs()
	require.NoError(t, l3.scan(
		t.Context(), series, current, 0, 2000, self,
		func(a *lnwire.ChannelAnnouncement1) bool {
			if a.ShortChannelID == theirs.ShortChannelID {
				return false
			}
			l3.add(a.ShortChannelID)
			return true
		},
		func(lnwire.ShortChannelID) {},
	))
	require.True(t, l3.has(own.ShortChannelID))
	require.False(t, l3.has(theirs.ShortChannelID))

	// A scan that fails partway is not a finished one: announcements
	// keep being checked by their signatures.
	l4 := newLegacyProofs()
	failing := &fakeSeries{anns: series.anns, fetchErr: errors.New("db")}
	require.Error(t, l4.scan(
		t.Context(), failing, current, 0, 2000, self, confirm(l4),
		func(lnwire.ShortChannelID) {},
	))
	require.False(t, l4.scanned.Load())
	require.True(t, l4.isLegacyAnn(theirs),
		"checked by its signatures, since the scan did not finish")
	require.True(t, l2.has(theirs.ShortChannelID))
}

// TestLegacyProofBeforeScan: until the scan has run, an announcement about to
// go out is checked by its signatures, and a legacy one is remembered.
func TestLegacyProofBeforeScan(t *testing.T) {
	t.Parallel()

	l := newLegacyProofs()
	old := testAnn(
		t, 1000, mainnetLegacyHash(t), remoteKeyPriv1, remoteKeyPriv2,
	)
	fine := testAnn(
		t, 1001, *chaincfg.MainNetParams.GenesisHash, remoteKeyPriv1,
		remoteKeyPriv2,
	)

	require.True(t, l.isLegacyAnn(old))
	require.True(t, l.has(old.ShortChannelID), "remembered")
	require.False(t, l.isLegacyAnn(fine))
	require.False(t, l.has(fine.ShortChannelID))
}

// TestServedSeries checks what the syncers may send peers: nothing about a
// channel funded below the gossip floor, and no legacy proof, while node
// announcements and everything else pass.
func TestServedSeries(t *testing.T) {
	t.Parallel()

	const floor = 1000

	current := *chaincfg.MainNetParams.GenesisHash
	legacy := mainnetLegacyHash(t)

	fine := testAnn(t, floor, current, remoteKeyPriv1, remoteKeyPriv2)
	old := testAnn(t, floor+1, legacy, remoteKeyPriv1, remoteKeyPriv2)
	preFloor := testAnn(t, floor-1, current, remoteKeyPriv1, remoteKeyPriv2)

	fineUpd, err := createUpdateAnnouncement(floor, 0, remoteKeyPriv1, 1)
	require.NoError(t, err)
	fineUpd.ShortChannelID = fine.ShortChannelID
	oldUpd, err := createUpdateAnnouncement(floor+1, 0, remoteKeyPriv1, 1)
	require.NoError(t, err)
	oldUpd.ShortChannelID = old.ShortChannelID
	preFloorUpd, err := createUpdateAnnouncement(
		floor-1, 0, remoteKeyPriv1, 1,
	)
	require.NoError(t, err)
	preFloorUpd.ShortChannelID = preFloor.ShortChannelID
	nodeAnn, err := createNodeAnnouncement(remoteKeyPriv1, 1)
	require.NoError(t, err)

	all := []lnwire.Message{
		fine, fineUpd, old, oldUpd, preFloor, preFloorUpd, nodeAnn,
	}
	// oldUpd is dropped too, but only once the legacy proof is known; the
	// first pass learns it from the announcement in the same reply.
	want := []lnwire.Message{fine, fineUpd, nodeAnn}

	inner := &fakeSeries{anns: all, horizon: all}
	s := &servedSeries{
		ChannelGraphTimeSeries: inner,
		legacy:                 newLegacyProofs(),
		floor:                  floor,
	}

	got, err := s.FetchChanAnns(current, []lnwire.ShortChannelID{
		fine.ShortChannelID, old.ShortChannelID,
		preFloor.ShortChannelID,
	})
	require.NoError(t, err)
	require.Equal(t, want, got)

	var horizon []lnwire.Message
	for msg, err := range s.UpdatesInHorizon(
		t.Context(), time.Time{}, time.Now(),
	) {
		require.NoError(t, err)
		horizon = append(horizon, msg)
	}
	require.Equal(t, want, horizon)

	// A range reply lists only what a query for it would return. The
	// legacy proof is known by now, from the fetches above.
	require.True(t, s.legacy.has(old.ShortChannelID))
	ranges, err := s.FilterChannelRange(current, 0, 2*floor, false)
	require.NoError(t, err)
	var listed []lnwire.ShortChannelID
	for _, r := range ranges {
		for _, c := range r.Channels {
			listed = append(listed, c.ShortChannelID)
		}
	}
	require.Equal(t, []lnwire.ShortChannelID{fine.ShortChannelID}, listed)

	// With no floor, only the legacy proof is held back.
	s.floor = 0
	got, err = s.FetchChanAnns(current, []lnwire.ShortChannelID{
		fine.ShortChannelID, old.ShortChannelID,
		preFloor.ShortChannelID,
	})
	require.NoError(t, err)
	require.Equal(t, []lnwire.Message{
		fine, fineUpd, preFloor, preFloorUpd, nodeAnn,
	}, got)

	// A channel held under a legacy proof is asked for again when a peer
	// lists it, so that the proof its nodes signed afresh can replace it;
	// other known channels are not.
	inner.known = map[lnwire.ShortChannelID]bool{
		fine.ShortChannelID: true, old.ShortChannelID: true,
	}
	unknown, err := s.FilterKnownChanIDs(current, []graphdb.ChannelUpdateInfo{
		{ShortChannelID: fine.ShortChannelID},
		{ShortChannelID: old.ShortChannelID},
		{ShortChannelID: preFloor.ShortChannelID},
	}, func(graphdb.ChannelUpdateInfo) bool { return false })
	require.NoError(t, err)
	require.ElementsMatch(t, []lnwire.ShortChannelID{
		old.ShortChannelID, preFloor.ShortChannelID,
	}, unknown)
}

// preFloorHarness is a test context whose graph holds a channel funded below
// the gossip floor, between node1 and node2, as channels stored before the
// floor existed are.
func preFloorHarness(t *testing.T, node1, node2 *btcec.PrivateKey,
	height uint32) (*testCtx, chan lnwire.Message) {

	t.Helper()

	const floor = 1000

	tCtx, err := createTestCtx(t, floor+10, false)
	require.NoError(t, err)
	tCtx.gossiper.cfg.MinAnnouncementHeight = floor

	sent := make(chan lnwire.Message, 10)
	tCtx.gossiper.reliableSender.cfg.NotifyWhenOnline = func(
		target [33]byte, peerChan chan<- lnpeer.Peer) {

		pk, _ := btcec.ParsePubKey(target[:])
		select {
		case peerChan <- &mockPeer{
			pk, sent, tCtx.gossiper.quit, atomic.Bool{},
		}:
		case <-tCtx.gossiper.quit:
		}
	}

	ann := testAnn(
		t, height, *chaincfg.MainNetParams.GenesisHash, node1, node2,
	)
	edge, err := models.NewV1Channel(
		ann.ShortChannelID.ToUint64(), ann.ChainHash, ann.NodeID1,
		ann.NodeID2, &models.ChannelV1Fields{
			BitcoinKey1Bytes: ann.BitcoinKey1,
			BitcoinKey2Bytes: ann.BitcoinKey2,
		},
		models.WithChanProof(models.NewV1ChannelAuthProof(
			ann.NodeSig1.ToSignatureBytes(),
			ann.NodeSig2.ToSignatureBytes(),
			ann.BitcoinSig1.ToSignatureBytes(),
			ann.BitcoinSig2.ToSignatureBytes(),
		)),
	)
	require.NoError(t, err)
	edge.Capacity = 1_000_000
	require.NoError(t, tCtx.router.AddEdge(t.Context(), edge))

	return tCtx, sent
}

// channelUpdate returns an update for the channel at height TxIndex 1, as
// testAnn places it, signed by key for direction dir.
func channelUpdate(t *testing.T, height uint32, dir lnwire.ChanUpdateChanFlags,
	key *btcec.PrivateKey) *lnwire.ChannelUpdate1 {

	t.Helper()

	upd, err := createUpdateAnnouncement(height, dir, key, testTimestamp)
	require.NoError(t, err)
	upd.ShortChannelID = lnwire.ShortChannelID{
		BlockHeight: height, TxIndex: 1,
	}
	require.NoError(t, signUpdate(key, upd))

	return upd
}

func policyStored(tCtx *testCtx, scid lnwire.ShortChannelID,
	dir int) bool {

	tCtx.router.mu.Lock()
	defer tCtx.router.mu.Unlock()

	policies := tCtx.router.edges[scid.ToUint64()]

	return len(policies) == 2 && policies[dir].LastUpdate.Unix() != 0
}

// TestPreFloorChannelUpdates checks BOLT-blake2b #7 for a channel funded below
// the gossip floor that is already in the graph: it is treated as
// unannounced. Only its own peer's update, for that peer's side of a channel
// with this node, is used, and nothing about it is broadcast; this node's own
// update goes to the peer directly.
func TestPreFloorChannelUpdates(t *testing.T) {
	t.Parallel()

	const height = 990

	peerOf := func(key *btcec.PrivateKey) *mockPeer {
		return &mockPeer{key.PubKey(), nil, nil, atomic.Bool{}}
	}

	t.Run("own peer's update is used, not relayed", func(t *testing.T) {
		t.Parallel()

		tCtx, _ := preFloorHarness(
			t, remoteKeyPriv1, selfKeyPriv, height,
		)
		upd := channelUpdate(t, height, 0, remoteKeyPriv1)

		sendRemoteMsg(t, tCtx, upd, peerOf(remoteKeyPriv1))
		require.True(t, policyStored(tCtx, upd.ShortChannelID, 0))
		assertNoBroadcast(t, tCtx)
	})

	t.Run("relayed by another peer: ignored", func(t *testing.T) {
		t.Parallel()

		tCtx, _ := preFloorHarness(
			t, remoteKeyPriv1, selfKeyPriv, height,
		)
		upd := channelUpdate(t, height, 0, remoteKeyPriv1)

		sendRemoteMsg(t, tCtx, upd, peerOf(remoteKeyPriv2))
		require.False(t, policyStored(tCtx, upd.ShortChannelID, 0))
		assertNoBroadcast(t, tCtx)
	})

	t.Run("a channel not this node's: ignored", func(t *testing.T) {
		t.Parallel()

		tCtx, _ := preFloorHarness(
			t, remoteKeyPriv1, remoteKeyPriv2, height,
		)
		upd := channelUpdate(t, height, 0, remoteKeyPriv1)

		sendRemoteMsg(t, tCtx, upd, peerOf(remoteKeyPriv1))
		require.False(t, policyStored(tCtx, upd.ShortChannelID, 0))
		assertNoBroadcast(t, tCtx)
	})

	t.Run("own update goes to the peer only", func(t *testing.T) {
		t.Parallel()

		tCtx, sent := preFloorHarness(
			t, remoteKeyPriv1, selfKeyPriv, height,
		)
		upd := channelUpdate(t, height, 1, selfKeyPriv)

		sendLocalMsg(t, tCtx, upd)
		require.True(t, policyStored(tCtx, upd.ShortChannelID, 1))

		select {
		case msg := <-sent:
			assertMessage(t, upd, msg)
		case <-time.After(3 * time.Second):
			t.Fatal("own update was not sent to the peer")
		}
		assertNoBroadcast(t, tCtx)
	})

	t.Run("above the floor: relayed as ever", func(t *testing.T) {
		t.Parallel()

		tCtx, _ := preFloorHarness(
			t, remoteKeyPriv1, remoteKeyPriv2, 1000,
		)
		upd := channelUpdate(t, 1000, 0, remoteKeyPriv1)

		sendRemoteMsg(t, tCtx, upd, peerOf(remoteKeyPriv2))
		require.True(t, policyStored(tCtx, upd.ShortChannelID, 0))

		select {
		case msg := <-tCtx.broadcastedMessage:
			assertMessage(t, upd, msg.msg)
		case <-time.After(3 * time.Second):
			t.Fatal("update above the floor was not broadcast")
		}
	})
}

// forceRetransmit fires the retransmit ticker past the rebroadcast interval.
func forceRetransmit(t *testing.T, tCtx *testCtx) {
	t.Helper()

	retransmit, ok := tCtx.gossiper.cfg.RetransmitTicker.(*ticker.Force)
	require.True(t, ok)

	future := time.Unix(int64(testTimestamp), 0).Add(
		rebroadcastInterval + 10*time.Second,
	)
	select {
	case retransmit.Force <- future:
	case <-time.After(2 * time.Second):
		t.Fatal("unable to force tick")
	}
}

// broadcastsFor collects what is broadcast for a while.
func broadcastsFor(tCtx *testCtx, d time.Duration) []lnwire.Message {
	var msgs []lnwire.Message
	deadline := time.After(d)
	for {
		select {
		case m := <-tCtx.broadcastedMessage:
			msgs = append(msgs, m.msg)
		case <-deadline:
			return msgs
		}
	}
}

// TestRetransmitLegacyProof: the daily retransmit of this node's channels
// leaves out an announcement whose proof only this fork can check, and keeps
// its update, which the counterparty on an earlier release still routes by.
func TestRetransmitLegacyProof(t *testing.T) {
	t.Parallel()

	h := newResignHarness(t)
	h.tCtx.gossiper.cfg.ResignChannelProof = nil

	sendLocalMsg(t, h.tCtx, h.batch.chanUpdAnn1)
	for _, m := range broadcastsFor(h.tCtx, 3*trickleDelay) {
		require.IsType(t, &lnwire.ChannelUpdate1{}, m,
			"this node's update for it is still broadcast")
	}

	forceRetransmit(t, h.tCtx)
	var upd int
	for _, m := range broadcastsFor(h.tCtx, 3*trickleDelay) {
		switch m.(type) {
		case *lnwire.ChannelAnnouncement1:
			t.Fatal("the legacy announcement was retransmitted")
		case *lnwire.ChannelUpdate1:
			upd++
		}
	}
	require.Equal(t, 1, upd, "the refreshed update goes out")
}

// TestRetransmitPreFloor: a channel of this node's funded below the floor is
// unannounced; its refreshed update goes to the peer alone.
func TestRetransmitPreFloor(t *testing.T) {
	t.Parallel()

	// This node first: the test graph takes direction 0 as the outgoing
	// policy.

	const height = 990
	tCtx, sent := preFloorHarness(t, selfKeyPriv, remoteKeyPriv1, height)
	upd := channelUpdate(t, height, 0, selfKeyPriv)
	sendLocalMsg(t, tCtx, upd)
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("own update was not sent to the peer")
	}
	assertNoBroadcast(t, tCtx)

	forceRetransmit(t, tCtx)
	select {
	case m := <-sent:
		require.IsType(t, &lnwire.ChannelUpdate1{}, m)
	case <-time.After(3 * time.Second):
		t.Fatal("the refreshed update did not go to the peer")
	}
	for _, m := range broadcastsFor(tCtx, 3*trickleDelay) {
		switch m.(type) {
		case *lnwire.ChannelAnnouncement1, *lnwire.ChannelUpdate1:
			t.Fatalf("broadcast %T for a channel below the floor", m)
		}
	}
}

// TestPolicyUpdatePreFloor: a fee change on such a channel goes to the peer
// alone as well.
func TestPolicyUpdatePreFloor(t *testing.T) {
	t.Parallel()

	const height = 990
	tCtx, sent := preFloorHarness(t, selfKeyPriv, remoteKeyPriv1, height)
	upd := channelUpdate(t, height, 0, selfKeyPriv)
	sendLocalMsg(t, tCtx, upd)
	<-sent

	info, e1, _, err := tCtx.router.GetChannelByID(upd.ShortChannelID)
	require.NoError(t, err)
	require.NotNil(t, e1)
	e1.FeeBaseMSat = 4242

	require.NoError(t, tCtx.gossiper.PropagateChanPolicyUpdate(
		[]EdgeWithInfo{{Info: info, Edge: e1}},
	))
	select {
	case m := <-sent:
		got, ok := m.(*lnwire.ChannelUpdate1)
		require.True(t, ok)
		require.EqualValues(t, 4242, got.BaseFee)
	case <-time.After(3 * time.Second):
		t.Fatal("the fee change did not go to the peer")
	}
	assertNoBroadcast(t, tCtx)
}

// TestLegacyProofRelayedUpdates: a peer's update for a channel held under a
// legacy proof is kept but not relayed, since no other implementation can
// place it; the same update for an ordinary channel is.
func TestLegacyProofRelayedUpdates(t *testing.T) {
	t.Parallel()

	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			t.Parallel()

			tCtx, err := createTestCtx(t, 0, false)
			require.NoError(t, err)
			ann, err := tCtx.createRemoteChannelAnnouncement(0)
			require.NoError(t, err)
			if legacy {
				ann = signAnnUnder(
					t, ann, mainnetLegacyHash(t),
					[4]*btcec.PrivateKey{
						remoteKeyPriv1, remoteKeyPriv2,
						bitcoinKeyPriv1, bitcoinKeyPriv2,
					},
				)
			}
			peer := &mockPeer{
				remoteKeyPriv1.PubKey(), nil, nil,
				atomic.Bool{},
			}
			sendRemoteMsg(t, tCtx, ann, peer)
			_ = broadcastsFor(tCtx, 3*trickleDelay)

			upd, err := createUpdateAnnouncement(
				0, 0, remoteKeyPriv1, testTimestamp,
			)
			require.NoError(t, err)
			upd.ShortChannelID = ann.ShortChannelID
			require.NoError(t, signUpdate(remoteKeyPriv1, upd))
			sendRemoteMsg(t, tCtx, upd, peer)
			require.True(t, policyStored(tCtx, upd.ShortChannelID, 0))

			got := broadcastsFor(tCtx, 3*trickleDelay)
			if legacy {
				require.Empty(t, got)
			} else {
				require.Len(t, got, 1)
			}
		})
	}
}

// TestLegacyProofResignOnce: a peer sending halves that do not pair cannot
// have this node sign again on demand. It signs once; its half then waits in
// the store, and the peer's next valid half completes the proof.
func TestLegacyProofResignOnce(t *testing.T) {
	t.Parallel()

	h := newResignHarness(t)

	old := signAnnUnder(
		t, h.batch.chanAnn, mainnetLegacyHash(t),
		[4]*btcec.PrivateKey{
			selfKeyPriv, remoteKeyPriv1, bitcoinKeyPriv1,
			bitcoinKeyPriv2,
		},
	)
	bad := *h.batch.remoteProofAnn
	bad.NodeSignature = old.NodeSig2
	bad.BitcoinSignature = old.BitcoinSig2

	// The first bad half prompts one re-sign; this node's half fails to
	// pair with it and is kept instead.
	sendRemoteMsg(t, h.tCtx, &bad, h.remotePeer)
	select {
	case <-h.resigned:
	case <-time.After(3 * time.Second):
		t.Fatal("this node did not sign its half")
	}
	h.assertHalfSentToPeer(t)

	// Wait for this node's half to be handled and kept.
	require.Eventually(t, func() bool {
		_, err := h.tCtx.gossiper.cfg.WaitingProofStore.Get(
			channeldb.NewWaitingProof(false, h.batch.localProofAnn).
				Key(),
		)
		return err == nil
	}, 3*time.Second, 50*time.Millisecond)

	// More bad halves: no further signing.
	for i := 0; i < 3; i++ {
		_ = mustProcess(t, h.tCtx.gossiper.ProcessRemoteAnnouncement(
			t.Context(), &bad, h.remotePeer,
		))
	}
	select {
	case <-h.resigned:
		t.Fatal("signed again on demand")
	case <-time.After(3 * trickleDelay):
	}

	// And a good half completes the proof with the kept one.
	sendRemoteMsg(t, h.tCtx, h.batch.remoteProofAnn, h.remotePeer)
	h.assertReplaced(t)
}

// TestMarkResigned: a channel is signed again at most once per run.
func TestMarkResigned(t *testing.T) {
	t.Parallel()

	l := newLegacyProofs()
	a := lnwire.ShortChannelID{BlockHeight: 1000, TxIndex: 1}
	b := lnwire.ShortChannelID{BlockHeight: 1001, TxIndex: 1}

	require.True(t, l.markResigned(a))
	require.False(t, l.markResigned(a))
	require.True(t, l.markResigned(b))
}
