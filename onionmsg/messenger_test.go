package onionmsg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// node is a party on the fake network, with the sphinx router a real node
// would process onion messages with.
type node struct {
	key    *btcec.PrivateKey
	router *sphinx.Router
}

func (n *node) pub() *btcec.PublicKey { return n.key.PubKey() }

func (n *node) id() [33]byte {
	var id [33]byte
	copy(id[:], n.key.PubKey().SerializeCompressed())

	return id
}

// sent is one onion message handed to a peer.
type sent struct {
	from    [33]byte
	to      [33]byte
	pathKey *btcec.PublicKey
	onion   []byte
}

// network is a fake network: nodes, who is connected to whom, a graph for
// route finding, announced addresses, and a log of every send.
type network struct {
	t     *testing.T
	nodes map[[33]byte]*node

	mu      sync.Mutex
	peers   map[[33]byte][]Peer
	edges   map[route.Vertex][]route.Vertex
	feats   map[route.Vertex]*lnwire.FeatureVector
	addrs   map[route.Vertex][]net.Addr
	chans   map[uint64]*models.ChannelEdgeInfo
	sent    []sent
	connect []route.Vertex

	// deafOnConnect makes the next connected node not speak onion
	// messages.
	deafOnConnect bool
}

func newNetwork(t *testing.T) *network {
	return &network{
		t:     t,
		nodes: make(map[[33]byte]*node),
		peers: make(map[[33]byte][]Peer),
		edges: make(map[route.Vertex][]route.Vertex),
		feats: make(map[route.Vertex]*lnwire.FeatureVector),
		addrs: make(map[route.Vertex][]net.Addr),
		chans: make(map[uint64]*models.ChannelEdgeInfo),
	}
}

func (n *network) newNode() *node {
	key, err := btcec.NewPrivateKey()
	require.NoError(n.t, err)
	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: key}, sphinx.NewNoOpReplayLog(),
	)
	require.NoError(n.t, router.Start())
	n.t.Cleanup(router.Stop)
	nd := &node{key: key, router: router}
	n.nodes[nd.id()] = nd

	return nd
}

// peer connects a and b, both speaking onion messages, with or without a
// channel between them. A channel also becomes a graph edge.
func (n *network) peer(a, b *node, channel bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.peers[a.id()] = append(n.peers[a.id()], Peer{
		Pub: b.pub(), OnionMessages: true, HasChannel: channel,
	})
	n.peers[b.id()] = append(n.peers[b.id()], Peer{
		Pub: a.pub(), OnionMessages: true, HasChannel: channel,
	})
	if channel {
		n.edgeLocked(a, b)
	}
}

// edge adds a graph edge between a and b, both advertising onion messages,
// without connecting them.
func (n *network) edge(a, b *node) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.edgeLocked(a, b)
}

func (n *network) edgeLocked(a, b *node) {
	va, vb := route.NewVertex(a.pub()), route.NewVertex(b.pub())
	n.edges[va] = append(n.edges[va], vb)
	n.edges[vb] = append(n.edges[vb], va)
	onion := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.OnionMessagesOptional),
		lnwire.Features,
	)
	n.feats[va] = onion
	n.feats[vb] = onion
}

// graph implements graphdb.NodeTraverser over the network's edges.
type graph struct{ n *network }

func (g graph) ForEachNodeDirectedChannel(_ context.Context,
	nodePub route.Vertex, cb func(channel *graphdb.DirectedChannel) error,
	_ func()) error {

	g.n.mu.Lock()
	neighbours := append([]route.Vertex{}, g.n.edges[nodePub]...)
	g.n.mu.Unlock()
	for _, other := range neighbours {
		err := cb(&graphdb.DirectedChannel{OtherNode: other})
		if err != nil {
			return err
		}
	}

	return nil
}

func (g graph) FetchNodeFeatures(_ context.Context,
	nodePub route.Vertex) (*lnwire.FeatureVector, error) {

	g.n.mu.Lock()
	defer g.n.mu.Unlock()
	if f, ok := g.n.feats[nodePub]; ok {
		return f, nil
	}

	return lnwire.EmptyFeatureVector(), nil
}

// messenger builds a Messenger for a node on the network, receiving through
// a subscribe server the test feeds.
func (n *network) messenger(nd *node, opts ...func(*Config)) (*Messenger,
	*subscribe.Server) {

	server := subscribe.NewServer()
	require.NoError(n.t, server.Start())
	n.t.Cleanup(func() { _ = server.Stop() })

	cfg := Config{
		NodeKey: nd.pub(),
		Graph:   graph{n},
		FetchChannelEdge: func(_ context.Context,
			scid uint64) (*models.ChannelEdgeInfo, error) {

			n.mu.Lock()
			defer n.mu.Unlock()
			if e, ok := n.chans[scid]; ok {
				return e, nil
			}

			return nil, errors.New("no such channel")
		},
		FetchNodeAddrs: func(_ context.Context,
			v route.Vertex) ([]net.Addr, error) {

			n.mu.Lock()
			defer n.mu.Unlock()

			return n.addrs[v], nil
		},
		Peers: func() []Peer {
			n.mu.Lock()
			defer n.mu.Unlock()

			return append([]Peer{}, n.peers[nd.id()]...)
		},
		ConnectPeer: func(_ context.Context, pub *btcec.PublicKey,
			addrs []net.Addr) error {

			n.mu.Lock()
			defer n.mu.Unlock()
			n.connect = append(n.connect, route.NewVertex(pub))
			var to [33]byte
			copy(to[:], pub.SerializeCompressed())
			other, ok := n.nodes[to]
			if !ok {
				return errors.New("no such node")
			}
			n.peers[nd.id()] = append(n.peers[nd.id()], Peer{
				Pub: other.pub(), OnionMessages: !n.deafOnConnect,
			})
			n.peers[to] = append(n.peers[to], Peer{
				Pub: nd.pub(), OnionMessages: true,
			})

			return nil
		},
		SendToPeer: func(_ context.Context, peer [33]byte,
			pathKey *btcec.PublicKey, onion []byte) error {

			n.mu.Lock()
			defer n.mu.Unlock()
			for _, p := range n.peers[nd.id()] {
				var id [33]byte
				copy(id[:], p.Pub.SerializeCompressed())
				if id == peer {
					n.sent = append(n.sent, sent{
						from: nd.id(), to: peer,
						pathKey: pathKey, onion: onion,
					})

					return nil
				}
			}

			return errors.New("not a peer")
		},
		Subscribe: server.Subscribe,
		DecryptBlindedData: func(pathKey *btcec.PublicKey,
			data []byte) ([]byte, error) {

			return nd.router.DecryptBlindedHopData(pathKey, data)
		},
		NextPathKey: nd.router.NextEphemeral,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	m, err := New(cfg)
	require.NoError(n.t, err)

	return m, server
}

// delivered is what the final node of an onion message sees.
type delivered struct {
	at      [33]byte
	from    [33]byte
	pathKey *btcec.PublicKey
	payload *lnwire.OnionMessagePayload
	hops    [][33]byte
}

// route processes the last sent onion message through the network the way
// real nodes would, hop by hop, and returns what the final node sees.
func (n *network) route() *delivered {
	n.mu.Lock()
	require.NotEmpty(n.t, n.sent, "nothing was sent")
	s := n.sent[len(n.sent)-1]
	n.sent = n.sent[:len(n.sent)-1]
	n.mu.Unlock()

	var pkt sphinx.OnionPacket
	require.NoError(n.t, pkt.Decode(bytes.NewReader(s.onion)))
	pathKey := s.pathKey
	from, at := s.from, s.to
	var hops [][33]byte
	for i := 0; i < 10; i++ {
		nd, ok := n.nodes[at]
		require.True(n.t, ok, "message sent to unknown node")
		hops = append(hops, at)

		processed, err := nd.router.ProcessOnionPacket(
			&pkt, nil, 10, sphinx.WithBlindingPoint(pathKey),
		)
		require.NoError(n.t, err, "hop %d cannot process the onion", i)
		payload := lnwire.NewOnionMessagePayload()
		_, err = payload.Decode(
			bytes.NewReader(processed.Payload.Payload),
		)
		require.NoError(n.t, err)
		encrypted := bytes.Clone(payload.EncryptedData)
		if processed.Action == sphinx.ExitNode {
			payload.EncryptedData = encrypted

			return &delivered{
				at: at, from: from, pathKey: pathKey,
				payload: payload, hops: hops,
			}
		}

		plain, err := nd.router.DecryptBlindedHopData(
			pathKey, payload.EncryptedData,
		)
		require.NoError(n.t, err)
		data, err := record.DecodeBlindedRouteData(bytes.NewReader(plain))
		require.NoError(n.t, err)
		require.True(n.t, data.NextNodeID.IsSome(), "next node id")
		var next *btcec.PublicKey
		data.NextNodeID.WhenSome(
			func(r tlv.RecordT[tlv.TlvType4, *btcec.PublicKey]) {
				next = r.Val
			},
		)
		nextKey, err := nd.router.NextEphemeral(pathKey)
		require.NoError(n.t, err)
		data.NextBlindingOverride.WhenSome(
			func(r tlv.RecordT[tlv.TlvType8, *btcec.PublicKey]) {
				nextKey = r.Val
			},
		)

		// A real node only forwards to a connected peer.
		n.mu.Lock()
		connected := false
		for _, p := range n.peers[at] {
			if p.Pub.IsEqual(next) {
				connected = true
			}
		}
		n.mu.Unlock()
		require.True(n.t, connected, "hop %x forwards to %x which "+
			"is not its peer", at, next.SerializeCompressed())

		pkt = *processed.NextPacket
		pathKey = nextKey
		from = at
		copy(at[:], next.SerializeCompressed())
	}
	n.t.Fatal("onion message did not terminate")

	return nil
}

// update turns a delivery into the update the recipient's messenger sees.
func (d *delivered) update() *onionmessage.OnionMessageUpdate {
	var pathKey [33]byte
	copy(pathKey[:], d.pathKey.SerializeCompressed())
	records := make(record.CustomSet)
	for _, r := range d.payload.FinalHopTLVs {
		records[uint64(r.TLVType)] = r.Value
	}

	return &onionmessage.OnionMessageUpdate{
		Peer:                   d.from,
		PathKey:                pathKey,
		CustomRecords:          records,
		ReplyPath:              d.payload.ReplyPath,
		EncryptedRecipientData: d.payload.EncryptedData,
	}
}

// pathID reads the path_id the recipient node sees in its hop data.
func (d *delivered) pathID(t *testing.T, nd *node) []byte {
	if len(d.payload.EncryptedData) == 0 {
		return nil
	}
	plain, err := nd.router.DecryptBlindedHopData(
		d.pathKey, d.payload.EncryptedData,
	)
	require.NoError(t, err)
	data, err := record.DecodeBlindedRouteData(bytes.NewReader(plain))
	require.NoError(t, err)
	var id []byte
	data.PathID.WhenSome(func(r tlv.RecordT[tlv.TlvType6, []byte]) {
		id = r.Val
	})

	return id
}

func payload(t tlv.Type, value []byte) []*lnwire.FinalHopTLV {
	return []*lnwire.FinalHopTLV{{TLVType: t, Value: value}}
}

// TestInvoiceErrorCodec round-trips invoice_error and refuses bad ones.
func TestInvoiceErrorCodec(t *testing.T) {
	t.Parallel()

	field := uint64(82)
	full := &InvoiceError{
		ErroneousField: &field,
		SuggestedValue: []byte{1, 2, 3},
		Message:        "amount too low ✓",
	}
	b, err := full.Encode()
	require.NoError(t, err)
	got, err := DecodeInvoiceError(b)
	require.NoError(t, err)
	require.Equal(t, full, got)

	plain := &InvoiceError{Message: "no"}
	b, err = plain.Encode()
	require.NoError(t, err)
	got, err = DecodeInvoiceError(b)
	require.NoError(t, err)
	require.Equal(t, plain, got)
	require.Nil(t, got.ErroneousField)
	require.Nil(t, got.SuggestedValue)

	_, err = (&InvoiceError{Message: "bad\xff"}).Encode()
	require.Error(t, err)
	_, err = DecodeInvoiceError([]byte{5, 2, 0xff, 0xfe})
	require.Error(t, err, "not UTF-8")
	_, err = DecodeInvoiceError([]byte{1, 1, 5})
	require.Error(t, err, "no text")
	_, err = DecodeInvoiceError([]byte{5})
	require.Error(t, err, "truncated")
	_, err = DecodeInvoiceError(nil)
	require.Error(t, err, "empty")
	_, err = (&InvoiceError{}).Encode()
	require.Error(t, err, "no message")
	_, err = (&InvoiceError{
		Message: "x", SuggestedValue: []byte{1},
	}).Encode()
	require.Error(t, err, "a value for no field")
	_, err = DecodeInvoiceError([]byte{3, 1, 1, 5, 1, 'x'})
	require.Error(t, err, "a value for no field")

	// Over-long text is cut on a rune boundary: three-byte runes do not
	// divide the limit, so the cut lands a byte short of it.
	long := bytes.Repeat([]byte("€"), MaxInvoiceErrorBytes)
	got, err = DecodeInvoiceError(append(
		[]byte{5, 0xfd, byte(len(long) >> 8), byte(len(long))}, long...,
	))
	require.NoError(t, err)
	require.True(t, utf8.ValidString(got.Message))
	require.Equal(t, MaxInvoiceErrorBytes-1, len(got.Message))
}

// TestBuildPathsToSelf builds offer and reply paths and sends over them.
func TestBuildPathsToSelf(t *testing.T) {
	t.Parallel()

	nw := newNetwork(t)
	recipient := nw.newNode()
	intro1, intro2, noChan := nw.newNode(), nw.newNode(), nw.newNode()
	deaf := nw.newNode()
	nw.peer(recipient, intro1, true)
	nw.peer(recipient, intro2, true)
	nw.peer(recipient, noChan, false)
	nw.peer(recipient, deaf, true)
	nw.mu.Lock()
	peers := nw.peers[recipient.id()]
	peers[len(peers)-1].OnionMessages = false
	nw.mu.Unlock()

	m, _ := nw.messenger(recipient)
	secret := [32]byte{7, 7, 7}
	paths, err := m.BuildOfferPaths(context.Background(), secret)
	require.NoError(t, err)
	require.Len(t, paths, 2, "one per channel peer, capped")
	for _, p := range paths {
		require.Equal(t, len(p.Hops[0].EncryptedData),
			len(p.Hops[1].EncryptedData), "hop data padded alike")
	}
	intros := map[[33]byte]bool{}
	for _, p := range paths {
		pk, ok := p.IntroductionNode.(lnwire.PubkeyIntro)
		require.True(t, ok)
		var id [33]byte
		copy(id[:], pk.Pubkey.SerializeCompressed())
		intros[id] = true
		require.Len(t, p.Hops, 2, "intro and recipient")
		require.NotNil(t, p.BlindingPoint)
	}
	require.True(t, intros[intro1.id()] && intros[intro2.id()],
		"the peers with channels are the introduction nodes")

	// The same secret through the same peers builds the same paths, so
	// an offer minted again is the same offer; another secret does not.
	again, err := m.BuildOfferPaths(context.Background(), secret)
	require.NoError(t, err)
	require.Equal(t, paths, again)
	other, err := m.BuildOfferPaths(context.Background(), [32]byte{8})
	require.NoError(t, err)
	require.NotEqual(t, paths, other)

	// A sender connected to the first intro sends over the path and the
	// message arrives with the secret as path_id.
	sender := nw.newNode()
	nw.peer(sender, intro1, true)
	nw.peer(sender, intro2, true)
	s, _ := nw.messenger(sender)
	for _, p := range paths {
		path := p
		err = s.Send(context.Background(), Destination{Path: &path},
			payload(TypeInvoiceRequest, []byte{1}), nil)
		require.NoError(t, err)
		d := nw.route()
		require.Equal(t, recipient.id(), d.at)
		require.Len(t, d.hops, 2)
		require.Equal(t, secret[:], d.pathID(t, recipient))
		require.Equal(t, []byte{1}, d.payload.FinalHopTLVs[0].Value)
	}

	// A reply path goes through a channel peer; without one, through a
	// peer with no channel, which an offer path never uses; with no
	// peer at all it names the node itself.
	reply, err := m.BuildReplyPath(context.Background(), []byte{9})
	require.NoError(t, err)
	require.Len(t, reply.Hops, 2)
	pk, ok := reply.IntroductionNode.(lnwire.PubkeyIntro)
	require.True(t, ok)
	require.True(t, pk.Pubkey.IsEqual(intro1.pub()) ||
		pk.Pubkey.IsEqual(intro2.pub()))
	chanless := nw.newNode()
	transient := nw.newNode()
	nw.peer(chanless, transient, false)
	c, _ := nw.messenger(chanless)
	paths, err = c.BuildOfferPaths(context.Background(), secret)
	require.NoError(t, err)
	require.Empty(t, paths, "no channel peer: no offer paths")
	reply, err = c.BuildReplyPath(context.Background(), []byte{9})
	require.NoError(t, err)
	require.Len(t, reply.Hops, 2, "a reply path may use it")
	lonely := nw.newNode()
	l, _ := nw.messenger(lonely)
	reply, err = l.BuildReplyPath(context.Background(), []byte{9})
	require.NoError(t, err)
	require.Len(t, reply.Hops, 1)
	pk, ok = reply.IntroductionNode.(lnwire.PubkeyIntro)
	require.True(t, ok)
	require.True(t, pk.Pubkey.IsEqual(lonely.pub()))
	paths, err = l.BuildOfferPaths(context.Background(), secret)
	require.NoError(t, err)
	require.Empty(t, paths, "no peer: the offer names the node id")

	// The one-hop reply path is usable by a peer of the lonely node.
	nw.peer(sender, lonely, false)
	err = s.Send(context.Background(), Destination{Path: reply},
		payload(TypeInvoice, []byte{2}), nil)
	require.NoError(t, err)
	d := nw.route()
	require.Equal(t, lonely.id(), d.at)
	require.Equal(t, []byte{9}, d.pathID(t, lonely))
}

// TestSendToNode reaches a node by id: as a peer, through the graph, or by
// connecting to it.
func TestSendToNode(t *testing.T) {
	t.Parallel()

	nw := newNetwork(t)
	sender, peer, far, x := nw.newNode(), nw.newNode(), nw.newNode(),
		nw.newNode()
	nw.peer(sender, peer, true)
	nw.peer(peer, x, true)
	nw.peer(x, far, true)
	s, _ := nw.messenger(sender)
	ctx := context.Background()

	// A peer.
	err := s.Send(ctx, Destination{NodeID: peer.pub()},
		payload(TypeInvoiceRequest, []byte{1}), nil)
	require.NoError(t, err)
	d := nw.route()
	require.Equal(t, peer.id(), d.at)
	require.Equal(t, [][33]byte{peer.id()}, d.hops)
	require.Nil(t, d.pathID(t, peer))

	// Three hops through the graph.
	reply, err := s.BuildReplyPath(ctx, []byte{5})
	require.NoError(t, err)
	err = s.Send(ctx, Destination{NodeID: far.pub()},
		payload(TypeInvoiceRequest, []byte{2}), reply)
	require.NoError(t, err)
	d = nw.route()
	require.Equal(t, far.id(), d.at)
	require.Equal(t, [][33]byte{peer.id(), x.id(), far.id()}, d.hops)
	require.NotNil(t, d.payload.ReplyPath, "the reply path rides along")
	require.Equal(t, reply.BlindingPoint, d.payload.ReplyPath.BlindingPoint)

	// The reply path leads back to the sender.
	f, _ := nw.messenger(far)
	err = f.Send(ctx, Destination{Path: d.payload.ReplyPath},
		payload(TypeInvoice, []byte{3}), nil)
	require.NoError(t, err)
	d = nw.route()
	require.Equal(t, sender.id(), d.at)
	require.Equal(t, []byte{5}, d.pathID(t, sender))

	// Too far for the hop limit: unreachable, no addresses.
	limited, _ := nw.messenger(sender, func(c *Config) { c.MaxHops = 2 })
	err = limited.Send(ctx, Destination{NodeID: far.pub()},
		payload(TypeInvoiceRequest, []byte{4}), nil)
	require.ErrorIs(t, err, ErrUnreachable)

	// Not in the graph, but with an address: connect and send directly,
	// but only when the send may dial out.
	island := nw.newNode()
	nw.mu.Lock()
	nw.addrs[route.NewVertex(island.pub())] = []net.Addr{
		&net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 9735},
	}
	nw.mu.Unlock()
	err = s.Send(ctx, Destination{NodeID: island.pub()},
		payload(TypeInvoiceRequest, []byte{5}), nil)
	require.ErrorIs(t, err, ErrUnreachable, "no dialling by default")
	require.Empty(t, nw.connect)
	err = s.Send(ctx, Destination{NodeID: island.pub()},
		payload(TypeInvoiceRequest, []byte{5}), nil, AllowConnect())
	require.NoError(t, err)
	require.Equal(t, []route.Vertex{route.NewVertex(island.pub())},
		nw.connect)
	d = nw.route()
	require.Equal(t, island.id(), d.at)

	// Nothing known about the node at all.
	ghost := nw.newNode()
	err = s.Send(ctx, Destination{NodeID: ghost.pub()},
		payload(TypeInvoiceRequest, []byte{6}), nil, AllowConnect())
	require.ErrorIs(t, err, ErrUnreachable)

	// A node that connects but does not speak onion messages.
	deaf := nw.newNode()
	nw.mu.Lock()
	nw.addrs[route.NewVertex(deaf.pub())] = []net.Addr{
		&net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 9735},
	}
	nw.deafOnConnect = true
	nw.mu.Unlock()
	err = s.Send(ctx, Destination{NodeID: deaf.pub()},
		payload(TypeInvoiceRequest, []byte{7}), nil, AllowConnect())
	require.ErrorIs(t, err, ErrUnreachable)
	require.ErrorContains(t, err, "onion messages")
	nw.mu.Lock()
	nw.deafOnConnect = false
	nw.mu.Unlock()

	// Ourselves, and malformed destinations.
	err = s.Send(ctx, Destination{NodeID: sender.pub()}, nil, nil)
	require.ErrorIs(t, err, ErrDestinationIsSelf)
	err = s.Send(ctx, Destination{}, nil, nil)
	require.ErrorIs(t, err, ErrBadDestination)
	err = s.Send(ctx, Destination{NodeID: peer.pub(), Path: reply}, nil,
		nil)
	require.ErrorIs(t, err, ErrBadDestination)
	err = s.Send(ctx, Destination{Path: &lnwire.BlindedPath{}}, nil, nil)
	require.ErrorIs(t, err, ErrBadDestination)
	broken := *reply
	broken.BlindingPoint = nil
	err = s.Send(ctx, Destination{Path: &broken}, nil, nil)
	require.ErrorIs(t, err, ErrBadDestination)
	broken = *reply
	broken.Hops = []lnwire.BlindedHop{{EncryptedData: []byte{1}}}
	err = s.Send(ctx, Destination{Path: &broken}, nil, nil)
	require.ErrorIs(t, err, ErrBadDestination)
	broken = *reply
	broken.IntroductionNode = lnwire.PubkeyIntro{}
	err = s.Send(ctx, Destination{Path: &broken}, nil, nil)
	require.ErrorIs(t, err, ErrBadDestination)

	// A payload too large for even the large onion fails cleanly.
	err = s.Send(ctx, Destination{NodeID: peer.pub()},
		payload(TypeInvoiceRequest, make([]byte, 40_000)), nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnreachable)
}

// TestRouteThroughConnectedPeer takes a longer route through a connected
// peer when the shortest route's first hop is not connected.
func TestRouteThroughConnectedPeer(t *testing.T) {
	t.Parallel()

	nw := newNetwork(t)
	sender, offline, x, dest := nw.newNode(), nw.newNode(), nw.newNode(),
		nw.newNode()
	// Graph: sender-offline-dest (2 hops) and sender-x-y-dest (3 hops);
	// only x is a connected peer of the sender.
	y := nw.newNode()
	nw.edge(sender, offline)
	nw.edge(offline, dest)
	nw.peer(sender, x, true)
	nw.peer(x, y, true)
	nw.peer(y, dest, true)
	s, _ := nw.messenger(sender)
	ctx := context.Background()

	err := s.Send(ctx, Destination{NodeID: dest.pub()},
		payload(TypeInvoiceRequest, []byte{1}), nil)
	require.NoError(t, err)
	d := nw.route()
	require.Equal(t, dest.id(), d.at)
	require.Equal(t, [][33]byte{x.id(), y.id(), dest.id()}, d.hops)
	require.Empty(t, nw.connect, "no dialling was needed")

	// With no such route and dialling allowed, the shortest route's
	// first hop is connected to.
	nw.mu.Lock()
	delete(nw.edges, route.NewVertex(y.pub()))
	nw.edges[route.NewVertex(x.pub())] = []route.Vertex{
		route.NewVertex(sender.pub()),
	}
	nw.addrs[route.NewVertex(offline.pub())] = []net.Addr{
		&net.TCPAddr{IP: net.IPv4(10, 0, 0, 3), Port: 9735},
	}
	nw.mu.Unlock()
	err = s.Send(ctx, Destination{NodeID: dest.pub()},
		payload(TypeInvoiceRequest, []byte{2}), nil)
	require.ErrorIs(t, err, ErrUnreachable)
	err = s.Send(ctx, Destination{NodeID: dest.pub()},
		payload(TypeInvoiceRequest, []byte{2}), nil, AllowConnect())
	require.NoError(t, err)
	require.Equal(t, []route.Vertex{route.NewVertex(offline.pub())},
		nw.connect)
	nw.peer(offline, dest, true)
	d = nw.route()
	require.Equal(t, dest.id(), d.at)
	require.Equal(t, [][33]byte{offline.id(), dest.id()}, d.hops)
}

// TestSendToBlindedPath reaches a recipient's path with the introduction
// node as a peer, some hops away, or named by channel and direction.
func TestSendToBlindedPath(t *testing.T) {
	t.Parallel()

	nw := newNetwork(t)
	sender, x, intro, recipient := nw.newNode(), nw.newNode(),
		nw.newNode(), nw.newNode()
	nw.peer(sender, x, true)
	nw.peer(x, intro, true)
	nw.peer(intro, recipient, true)
	r, _ := nw.messenger(recipient)
	s, _ := nw.messenger(sender)
	ctx := context.Background()

	path, err := r.BuildReplyPath(ctx, []byte{0xaa})
	require.NoError(t, err)

	// Two of our hops, then the recipient's two.
	big := bytes.Repeat([]byte{0xbb}, 2000)
	err = s.Send(ctx, Destination{Path: path},
		payload(TypeInvoiceRequest, big), nil)
	require.NoError(t, err)
	d := nw.route()
	require.Equal(t, recipient.id(), d.at)
	require.Equal(t, [][33]byte{x.id(), intro.id(), recipient.id()},
		d.hops)
	require.Equal(t, []byte{0xaa}, d.pathID(t, recipient))
	require.Equal(t, big, d.payload.FinalHopTLVs[0].Value,
		"a large payload rides in a large onion")

	// The introduction node as a direct peer of the sender.
	nw.peer(sender, intro, false)
	err = s.Send(ctx, Destination{Path: path},
		payload(TypeInvoiceRequest, []byte{1}), nil)
	require.NoError(t, err)
	d = nw.route()
	require.Equal(t, recipient.id(), d.at)
	require.Equal(t, [][33]byte{intro.id(), recipient.id()}, d.hops)
	require.Equal(t, []byte{0xaa}, d.pathID(t, recipient))

	// The introduction node named by channel and direction.
	scid := uint64(0x0102030405060708)
	edge := &models.ChannelEdgeInfo{
		NodeKey1Bytes: route.NewVertex(intro.pub()),
		NodeKey2Bytes: route.NewVertex(recipient.pub()),
	}
	nw.mu.Lock()
	nw.chans[scid] = edge
	nw.mu.Unlock()
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], scid)
	sciddir, err := lnwire.NewSciddirIntro(0, raw)
	require.NoError(t, err)
	byChan := *path
	byChan.IntroductionNode = sciddir
	err = s.Send(ctx, Destination{Path: &byChan},
		payload(TypeInvoiceRequest, []byte{2}), nil)
	require.NoError(t, err)
	d = nw.route()
	require.Equal(t, recipient.id(), d.at)
	require.Equal(t, []byte{0xaa}, d.pathID(t, recipient))

	// Direction 1 names the other node, which is the recipient itself:
	// the message is then sent to it as the introduction node, and its
	// own path data does not decrypt at the wrong hop.
	sciddir, err = lnwire.NewSciddirIntro(1, raw)
	require.NoError(t, err)
	byChan.IntroductionNode = sciddir
	nw.peer(sender, recipient, false)
	err = s.Send(ctx, Destination{Path: &byChan},
		payload(TypeInvoiceRequest, []byte{3}), nil)
	require.NoError(t, err)
	nw.mu.Lock()
	last := nw.sent[len(nw.sent)-1]
	nw.sent = nw.sent[:len(nw.sent)-1]
	nw.mu.Unlock()
	require.Equal(t, recipient.id(), last.to)

	// An unknown channel.
	var unknown [8]byte
	sciddir, err = lnwire.NewSciddirIntro(0, unknown)
	require.NoError(t, err)
	byChan.IntroductionNode = sciddir
	err = s.Send(ctx, Destination{Path: &byChan}, nil, nil)
	require.Error(t, err)

	// A path that starts at us: the recipient built it through us as
	// its introduction node. We process our own hop and send on.
	viaSender := nw.newNode()
	nw.peer(viaSender, sender, true)
	v, _ := nw.messenger(viaSender)
	throughUs, err := v.BuildReplyPath(ctx, []byte{0xcc})
	require.NoError(t, err)
	pk, ok := throughUs.IntroductionNode.(lnwire.PubkeyIntro)
	require.True(t, ok)
	require.True(t, pk.Pubkey.IsEqual(sender.pub()))
	err = s.Send(ctx, Destination{Path: throughUs},
		payload(TypeInvoice, []byte{4}), nil)
	require.NoError(t, err)
	d = nw.route()
	require.Equal(t, viaSender.id(), d.at)
	require.Equal(t, [][33]byte{viaSender.id()}, d.hops)
	require.Equal(t, []byte{0xcc}, d.pathID(t, viaSender))
	require.Equal(t, []byte{4}, d.payload.FinalHopTLVs[0].Value)

	// The same, but the recipient is not our peer any more and is
	// reached through the graph: our last hop must switch to the key
	// the recipient's hop expects after ours, not the path's first key.
	far := nw.newNode()
	relay := nw.newNode()
	nw.peer(far, sender, true)
	f, _ := nw.messenger(far)
	viaUsFar, err := f.BuildReplyPath(ctx, []byte{0xdd})
	require.NoError(t, err)
	nw.mu.Lock()
	// Drop the direct connection, keep the graph edge, add a relay.
	nw.peers[sender.id()] = nw.peers[sender.id()][:len(nw.peers[sender.id()])-1]
	nw.peers[far.id()] = nil
	nw.mu.Unlock()
	nw.peer(sender, relay, true)
	nw.peer(relay, far, true)
	err = s.Send(ctx, Destination{Path: viaUsFar},
		payload(TypeInvoice, []byte{5}), nil)
	require.NoError(t, err)
	d = nw.route()
	require.Equal(t, far.id(), d.at)
	require.Equal(t, [][33]byte{relay.id(), far.id()}, d.hops)
	require.Equal(t, []byte{0xdd}, d.pathID(t, far))

	// A one-hop path to ourselves is not a destination.
	mine, err := s.BuildReplyPath(ctx, []byte{1})
	require.NoError(t, err)
	require.Len(t, mine.Hops, 2)
	lonely := nw.newNode()
	l, _ := nw.messenger(lonely)
	own, err := l.BuildReplyPath(ctx, []byte{1})
	require.NoError(t, err)
	err = l.Send(ctx, Destination{Path: own}, nil, nil)
	require.ErrorIs(t, err, ErrDestinationIsSelf)
}

// TestReceive feeds delivered onion messages through the subscription and
// checks what the handlers get.
func TestReceive(t *testing.T) {
	t.Parallel()

	nw := newNetwork(t)
	recipient, sender := nw.newNode(), nw.newNode()
	nw.peer(recipient, sender, true)
	r, server := nw.messenger(recipient)
	s, _ := nw.messenger(sender)
	ctx := context.Background()

	got := make(chan *Inbound, 8)
	r.OnInvoiceRequest(func(_ context.Context, msg *Inbound) { got <- msg })
	r.OnInvoice(func(_ context.Context, msg *Inbound) { got <- msg })
	r.OnInvoiceError(func(_ context.Context, msg *Inbound) { got <- msg })
	require.NoError(t, r.Start())
	require.NoError(t, r.Start(), "idempotent")
	t.Cleanup(func() { require.NoError(t, r.Stop()) })

	wait := func() *Inbound {
		select {
		case msg := <-got:
			return msg
		case <-time.After(5 * time.Second):
			t.Fatal("no message delivered")
		}

		return nil
	}
	none := func() {
		select {
		case msg := <-got:
			t.Fatalf("unexpected message %+v", msg)
		case <-time.After(200 * time.Millisecond):
		}
	}

	// A signed invoice request over one of our paths, with a reply path.
	path, err := r.BuildReplyPath(ctx, []byte("offer-path"))
	require.NoError(t, err)
	issuer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	chain := [32]byte{1}
	offer := &bolt12.Offer{
		OfferChains: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](bolt12.ChainsRecord{
				Chains: [][32]byte{chain},
			}),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("x")),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](issuer.PubKey()),
		),
	}
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, sender.pub(), []byte{1, 2}, chain,
	)
	require.NoError(t, err)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(1000)),
	)
	sig, err := bolt12.SignInvoiceRequest(ir, sender.key)
	require.NoError(t, err)
	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)
	irBytes, err := ir.Encode()
	require.NoError(t, err)
	reply, err := s.BuildReplyPath(ctx, []byte("reply"))
	require.NoError(t, err)
	err = s.Send(ctx, Destination{Path: path},
		payload(TypeInvoiceRequest, irBytes), reply)
	require.NoError(t, err)
	d := nw.route()
	require.Equal(t, recipient.id(), d.at)
	require.NoError(t, server.SendUpdate(d.update()))
	msg := wait()
	require.NotNil(t, msg.InvoiceRequest)
	require.Nil(t, msg.Invoice)
	require.Nil(t, msg.InvoiceError)
	require.Equal(t, []byte("offer-path"), msg.PathID)
	require.Equal(t, sender.id(), msg.Peer)
	require.NotNil(t, msg.ReplyPath)
	require.Equal(t, reply.BlindingPoint, msg.ReplyPath.BlindingPoint)
	require.NoError(t, bolt12.VerifyInvoiceRequest(msg.InvoiceRequest))
	require.Equal(t, irBytes, msg.Records[uint64(TypeInvoiceRequest)])

	// An invoice error to our node id: no path, no path_id.
	ieBytes, err := (&InvoiceError{Message: "no"}).Encode()
	require.NoError(t, err)
	err = s.Send(ctx, Destination{NodeID: recipient.pub()},
		payload(TypeInvoiceError, ieBytes), nil)
	require.NoError(t, err)
	d = nw.route()
	require.NoError(t, server.SendUpdate(d.update()))
	msg = wait()
	require.NotNil(t, msg.InvoiceError)
	require.Equal(t, "no", msg.InvoiceError.Message)
	require.Nil(t, msg.PathID)
	require.Nil(t, msg.ReplyPath)

	// Things that are dropped: no BOLT 12 record, two kinds at once, a
	// record that does not decode, recipient data that is not ours.
	u := d.update()
	u.CustomRecords = record.CustomSet{100: []byte{1}}
	require.NoError(t, server.SendUpdate(u))
	none()
	u = d.update()
	u.CustomRecords[uint64(TypeInvoiceRequest)] = irBytes
	require.NoError(t, server.SendUpdate(u))
	none()
	u = d.update()
	u.CustomRecords = record.CustomSet{
		uint64(TypeInvoice): []byte{0xff, 0xff},
	}
	require.NoError(t, server.SendUpdate(u))
	none()
	u = d.update()
	u.EncryptedRecipientData = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11,
		12, 13, 14, 15, 16, 17, 18}
	require.NoError(t, server.SendUpdate(u))
	none()
	require.NoError(t, server.SendUpdate("not an onion message"))
	none()

	// Stop waits for handlers and stops delivery; the messenger can be
	// started again and stopped again.
	require.NoError(t, r.Stop())
	require.NoError(t, r.Stop(), "idempotent")
	require.NoError(t, r.Start())
	err = s.Send(ctx, Destination{NodeID: recipient.pub()},
		payload(TypeInvoiceError, ieBytes), nil)
	require.NoError(t, err)
	d = nw.route()
	require.NoError(t, server.SendUpdate(d.update()))
	msg = wait()
	require.NotNil(t, msg.InvoiceError)
	require.NoError(t, r.Stop())
	require.NoError(t, r.Stop())
}

// TestDispatchDropsWhenBusy drops messages once every handler slot is
// taken, instead of queueing them.
func TestDispatchDropsWhenBusy(t *testing.T) {
	t.Parallel()

	nw := newNetwork(t)
	recipient, sender := nw.newNode(), nw.newNode()
	nw.peer(recipient, sender, true)
	r, server := nw.messenger(recipient)
	s, _ := nw.messenger(sender)
	ctx := context.Background()

	release := make(chan struct{})
	started := make(chan struct{}, maxConcurrentHandlers+1)
	r.OnInvoiceError(func(ctx context.Context, _ *Inbound) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	require.NoError(t, r.Start())

	ieBytes, err := (&InvoiceError{Message: "busy"}).Encode()
	require.NoError(t, err)
	err = s.Send(ctx, Destination{NodeID: recipient.pub()},
		payload(TypeInvoiceError, ieBytes), nil)
	require.NoError(t, err)
	u := nw.route().update()
	for i := 0; i < maxConcurrentHandlers; i++ {
		require.NoError(t, server.SendUpdate(u))
	}
	for i := 0; i < maxConcurrentHandlers; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not start")
		}
	}
	// One more is dropped, not queued.
	require.NoError(t, server.SendUpdate(u))
	select {
	case <-started:
		t.Fatal("message over the cap was handled")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	require.Eventually(t, func() bool {
		return len(r.sem) == 0
	}, 5*time.Second, 10*time.Millisecond)

	// Stop ends the handlers' context.
	blocked := make(chan struct{})
	r.OnInvoiceError(func(ctx context.Context, _ *Inbound) {
		<-ctx.Done()
		close(blocked)
	})
	require.NoError(t, server.SendUpdate(u))
	require.Eventually(t, func() bool { return len(r.sem) > 0 },
		5*time.Second, 10*time.Millisecond)
	require.NoError(t, r.Stop())
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("handler context was not cancelled by Stop")
	}
}

// TestNewRequirements checks the configuration checks and defaults.
func TestNewRequirements(t *testing.T) {
	t.Parallel()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	full := Config{
		NodeKey: key.PubKey(),
		Peers:   func() []Peer { return nil },
		SendToPeer: func(context.Context, [33]byte, *btcec.PublicKey,
			[]byte) error {

			return nil
		},
		Subscribe: func() (*subscribe.Client, error) { return nil, nil },
		DecryptBlindedData: func(*btcec.PublicKey, []byte) ([]byte,
			error) {

			return nil, nil
		},
		NextPathKey: func(*btcec.PublicKey) (*btcec.PublicKey, error) {
			return nil, nil
		},
	}
	m, err := New(full)
	require.NoError(t, err)
	require.Equal(t, DefaultMaxHops, m.cfg.MaxHops)
	require.Equal(t, DefaultMaxPaths, m.cfg.MaxPaths)

	for _, strip := range []func(c *Config){
		func(c *Config) { c.NodeKey = nil },
		func(c *Config) { c.Peers = nil },
		func(c *Config) { c.SendToPeer = nil },
		func(c *Config) { c.Subscribe = nil },
		func(c *Config) { c.DecryptBlindedData = nil },
		func(c *Config) { c.NextPathKey = nil },
	} {
		c := full
		strip(&c)
		_, err := New(c)
		require.Error(t, err)
	}
}
