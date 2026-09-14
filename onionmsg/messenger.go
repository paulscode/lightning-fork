// Package onionmsg carries BOLT 12 messages over onion messages. It builds
// blinded paths to this node, sends a payload to a node or to a blinded path
// by finding a route through the graph, and turns delivered onion messages
// into typed invoice requests, invoices and invoice errors. It sits on the
// upstream onionmessage package, which relays and delivers the raw messages.
package onionmsg

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DefaultMaxHops bounds the graph search for a route to an
	// introduction node or a destination.
	DefaultMaxHops = 4

	// DefaultMaxPaths is how many blinded paths an offer gets, each
	// through a different introduction peer.
	DefaultMaxPaths = 2

	// maxConcurrentHandlers bounds how many delivered messages are being
	// handled at once. Past it, messages are dropped rather than queued,
	// so a flood cannot stall delivery.
	maxConcurrentHandlers = 16

	// maxConcurrentDials bounds how many outbound connections a send may
	// be making at once.
	maxConcurrentDials = 2

	// ConnectTimeout bounds a connection made to deliver a message.
	ConnectTimeout = 30 * time.Second
)

var (
	// ErrNoPeers is returned when no connected peer can be an
	// introduction node.
	ErrNoPeers = errors.New("no connected peer supports onion messages")

	// ErrDestinationIsSelf is returned when a message would be sent to
	// this node.
	ErrDestinationIsSelf = errors.New("destination is this node")

	// ErrUnreachable is returned when no route to the destination or its
	// introduction node exists and it cannot, or may not, be connected
	// to directly.
	ErrUnreachable = errors.New("destination is unreachable")

	// ErrBadDestination is returned for a destination that names neither
	// a node nor a path, or a path that is malformed.
	ErrBadDestination = errors.New("destination names no node or path")

	// ErrTooBusy is returned to no one; it is logged when a delivered
	// message is dropped because every handler slot is taken.
	ErrTooBusy = errors.New("all handler slots are busy")
)

// Peer describes a connected peer as far as onion messaging cares.
type Peer struct {
	// Pub is the peer's node id.
	Pub *btcec.PublicKey

	// OnionMessages is whether the peer advertises onion messages.
	OnionMessages bool

	// HasChannel is whether we have an open channel with the peer, which
	// is what lets it relay to and from us under the usual relay policy,
	// and what makes it likely to stay connected.
	HasChannel bool
}

// Config holds what the messenger needs from the node.
type Config struct {
	// NodeKey is this node's id.
	NodeKey *btcec.PublicKey

	// Graph is searched for routes to introduction nodes.
	Graph graphdb.NodeTraverser

	// FetchChannelEdge returns the channel a short channel id names, for
	// resolving an introduction node given as a channel and direction.
	FetchChannelEdge func(ctx context.Context,
		scid uint64) (*models.ChannelEdgeInfo, error)

	// FetchNodeAddrs returns a node's announced addresses, for connecting
	// to an introduction node no route leads to.
	FetchNodeAddrs func(ctx context.Context,
		node route.Vertex) ([]net.Addr, error)

	// Peers lists the connected peers whose features are known.
	Peers func() []Peer

	// ConnectPeer connects to a node at one of its addresses and returns
	// once the peer is active and its features are known, so that Peers
	// lists it, or when ctx ends. A node that is already a peer is not
	// an error.
	ConnectPeer func(ctx context.Context, pub *btcec.PublicKey,
		addrs []net.Addr) error

	// SendToPeer sends an onion message to a connected peer.
	SendToPeer func(ctx context.Context, peer [33]byte,
		pathKey *btcec.PublicKey, onion []byte) error

	// Subscribe returns a client for delivered onion messages.
	Subscribe func() (*subscribe.Client, error)

	// DecryptBlindedData decrypts the encrypted data a blinded path put
	// on our hop, with our node key and the message's path key.
	DecryptBlindedData func(pathKey *btcec.PublicKey,
		data []byte) ([]byte, error)

	// NextPathKey derives the path key for the hop after ours from the
	// one at ours, for sending over a path whose introduction node we
	// are.
	NextPathKey func(pathKey *btcec.PublicKey) (*btcec.PublicKey, error)

	// MaxHops bounds the route search; zero means DefaultMaxHops.
	MaxHops int

	// MaxPaths is how many paths an offer gets; zero means
	// DefaultMaxPaths.
	MaxPaths int
}

// Destination is where a message goes: a node by id, or a blinded path.
type Destination struct {
	// NodeID reaches a node by its id. The node has to be a peer or in
	// the graph.
	NodeID *btcec.PublicKey

	// Path reaches whoever built the path, through its introduction node.
	Path *lnwire.BlindedPath
}

// SendOption adjusts a send.
type SendOption func(*sendOptions)

type sendOptions struct {
	allowConnect bool
}

// AllowConnect lets the send open a connection to the first hop or the
// destination when no route through connected peers exists. It is for
// sends this node initiates; a reply to an inbound message must not make
// the node dial out, since the reply path is the sender's to choose.
func AllowConnect() SendOption {
	return func(o *sendOptions) { o.allowConnect = true }
}

// Inbound is a delivered BOLT 12 message.
type Inbound struct {
	// Peer is the peer the onion message came in from.
	Peer [33]byte

	// PathKey is the message's path key at our hop.
	PathKey *btcec.PublicKey

	// PathID is the path_id of the blinded path the message arrived
	// over, if it arrived over one of ours and carried one.
	PathID []byte

	// ReplyPath is where a reply goes, if the sender gave one.
	ReplyPath *lnwire.BlindedPath

	// Exactly one of these is set, by the message's type.
	InvoiceRequest *bolt12.InvoiceRequest
	Invoice        *bolt12.Invoice
	InvoiceError   *InvoiceError

	// Records is every payload record with a type of 64 or above, raw.
	Records record.CustomSet
}

// Handler receives delivered messages of one kind.
type Handler func(ctx context.Context, msg *Inbound)

// Messenger sends and receives BOLT 12 messages over onion messages.
type Messenger struct {
	cfg Config

	handlersMu      sync.RWMutex
	requestHandlers []Handler
	invoiceHandlers []Handler
	errorHandlers   []Handler

	mu      sync.Mutex
	started bool
	client  *subscribe.Client
	quit    chan struct{}
	wg      sync.WaitGroup

	sem     chan struct{}
	dialSem chan struct{}
}

// New returns a messenger.
func New(cfg Config) (*Messenger, error) {
	switch {
	case cfg.NodeKey == nil:
		return nil, errors.New("onionmsg: node key required")
	case cfg.Peers == nil, cfg.SendToPeer == nil, cfg.Subscribe == nil,
		cfg.DecryptBlindedData == nil, cfg.NextPathKey == nil:

		return nil, errors.New("onionmsg: peers, send, subscribe, " +
			"decrypt and next-path-key hooks required")
	}
	if cfg.MaxHops <= 0 {
		cfg.MaxHops = DefaultMaxHops
	}
	if cfg.MaxPaths <= 0 {
		cfg.MaxPaths = DefaultMaxPaths
	}

	return &Messenger{
		cfg:     cfg,
		sem:     make(chan struct{}, maxConcurrentHandlers),
		dialSem: make(chan struct{}, maxConcurrentDials),
	}, nil
}

// Start subscribes to delivered onion messages.
func (m *Messenger) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return nil
	}
	client, err := m.cfg.Subscribe()
	if err != nil {
		return fmt.Errorf("subscribe to onion messages: %w", err)
	}
	m.client = client
	m.quit = make(chan struct{})
	m.started = true
	m.wg.Add(1)
	go m.receive(client, m.quit)

	return nil
}

// Stop ends the subscription and waits for handlers. The messenger can be
// started again afterwards.
func (m *Messenger) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = false
	close(m.quit)
	m.client.Cancel()
	m.mu.Unlock()

	m.wg.Wait()

	return nil
}

// OnInvoiceRequest registers a handler for delivered invoice requests.
func (m *Messenger) OnInvoiceRequest(h Handler) {
	m.handlersMu.Lock()
	defer m.handlersMu.Unlock()

	m.requestHandlers = append(m.requestHandlers, h)
}

// OnInvoice registers a handler for delivered invoices.
func (m *Messenger) OnInvoice(h Handler) {
	m.handlersMu.Lock()
	defer m.handlersMu.Unlock()

	m.invoiceHandlers = append(m.invoiceHandlers, h)
}

// OnInvoiceError registers a handler for delivered invoice errors.
func (m *Messenger) OnInvoiceError(h Handler) {
	m.handlersMu.Lock()
	defer m.handlersMu.Unlock()

	m.errorHandlers = append(m.errorHandlers, h)
}

// receive turns delivered onion messages into Inbound messages and hands
// them to the handlers. The handlers' context ends when the messenger
// stops.
func (m *Messenger) receive(client *subscribe.Client, quit chan struct{}) {
	defer m.wg.Done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		select {
		case update, ok := <-client.Updates():
			if !ok {
				return
			}
			oMsg, ok := update.(*onionmessage.OnionMessageUpdate)
			if !ok {
				log.Warnf("Onion message update of type %T",
					update)
				continue
			}
			inbound, handlers, err := m.parse(oMsg)
			if err != nil {
				log.Debugf("Dropping onion message from %x: %v",
					oMsg.Peer, err)
				continue
			}
			if inbound == nil {
				// Not a BOLT 12 message; not ours to handle.
				continue
			}
			m.dispatch(ctx, inbound, handlers, quit)

		case <-client.Quit():
			return

		case <-quit:
			return
		}
	}
}

// parse decodes the BOLT 12 content of a delivered onion message. It returns
// nil without error for a message that carries none.
func (m *Messenger) parse(oMsg *onionmessage.OnionMessageUpdate) (*Inbound,
	[]Handler, error) {

	pathKey, err := btcec.ParsePubKey(oMsg.PathKey[:])
	if err != nil {
		return nil, nil, fmt.Errorf("path key: %w", err)
	}
	inbound := &Inbound{
		Peer:      oMsg.Peer,
		PathKey:   pathKey,
		ReplyPath: oMsg.ReplyPath,
		Records:   oMsg.CustomRecords,
	}

	var (
		handlers []Handler
		kinds    int
	)
	if raw, ok := oMsg.CustomRecords[uint64(TypeInvoiceRequest)]; ok {
		ir, err := bolt12.DecodeInvoiceRequest(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("invoice_request: %w", err)
		}
		inbound.InvoiceRequest = ir
		handlers = m.handlers(&m.requestHandlers)
		kinds++
	}
	if raw, ok := oMsg.CustomRecords[uint64(TypeInvoice)]; ok {
		inv, err := bolt12.DecodeInvoice(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("invoice: %w", err)
		}
		inbound.Invoice = inv
		handlers = m.handlers(&m.invoiceHandlers)
		kinds++
	}
	if raw, ok := oMsg.CustomRecords[uint64(TypeInvoiceError)]; ok {
		ie, err := DecodeInvoiceError(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("invoice_error: %w", err)
		}
		inbound.InvoiceError = ie
		handlers = m.handlers(&m.errorHandlers)
		kinds++
	}
	switch kinds {
	case 0:
		return nil, nil, nil
	case 1:
	default:
		return nil, nil, errors.New("more than one BOLT 12 message " +
			"in one onion message")
	}

	// The path_id, if the message came over one of our blinded paths.
	// The decryption writes over its input, and the update is shared
	// with every other subscriber, so it works on a copy.
	if len(oMsg.EncryptedRecipientData) > 0 {
		plain, err := m.cfg.DecryptBlindedData(
			pathKey, bytes.Clone(oMsg.EncryptedRecipientData),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("recipient data: %w", err)
		}
		data, err := record.DecodeBlindedRouteData(bytes.NewReader(plain))
		if err != nil {
			return nil, nil, fmt.Errorf("recipient data: %w", err)
		}
		data.PathID.WhenSome(
			func(r tlv.RecordT[tlv.TlvType6, []byte]) {
				inbound.PathID = r.Val
			},
		)
	}

	return inbound, handlers, nil
}

func (m *Messenger) handlers(list *[]Handler) []Handler {
	m.handlersMu.RLock()
	defer m.handlersMu.RUnlock()

	out := make([]Handler, len(*list))
	copy(out, *list)

	return out
}

// dispatch runs the handlers for a message, each in its own goroutine.
// When every slot is taken the message is dropped, so a flood of messages
// cannot back up delivery.
func (m *Messenger) dispatch(ctx context.Context, msg *Inbound,
	handlers []Handler, quit chan struct{}) {

	for _, h := range handlers {
		select {
		case m.sem <- struct{}{}:
		case <-quit:
			return
		default:
			log.Warnf("Dropping message from peer %x: %v", msg.Peer,
				ErrTooBusy)

			return
		}
		m.wg.Add(1)
		go func(h Handler) {
			defer m.wg.Done()
			defer func() { <-m.sem }()

			h(ctx, msg)
		}(h)
	}
}

// BuildOfferPaths builds blinded paths to this node for an offer, each
// through a different connected peer that has a channel with us and speaks
// onion messages, carrying the secret as path_id. Without such a peer it
// returns none, and the offer names the node id instead. Offer paths are
// long-lived, which is why a peer with no channel, whose connection is
// likely to go, is not used for them.
//
// This implements offers.PathBuilder.
func (m *Messenger) BuildOfferPaths(_ context.Context,
	pathSecret [32]byte) ([]lnwire.BlindedPath, error) {

	intros, _ := m.introPeers(m.cfg.Peers())
	if len(intros) == 0 {
		return nil, nil
	}
	if len(intros) > m.cfg.MaxPaths {
		intros = intros[:m.cfg.MaxPaths]
	}
	var paths []lnwire.BlindedPath
	for _, intro := range intros {
		// The path's session key derives from the secret and the
		// introduction node, so minting the same offer again through
		// the same peer builds the same path, and the same offer.
		sessionKey := offerPathSessionKey(pathSecret, intro)
		path, err := m.buildPathToSelf(intro, pathSecret[:], sessionKey)
		if err != nil {
			return nil, err
		}
		paths = append(paths, *path)
	}

	return paths, nil
}

// offerPathSessionKey derives the blinding session key for an offer path
// from the offer's path secret and the introduction node.
func offerPathSessionKey(pathSecret [32]byte,
	intro *btcec.PublicKey) *btcec.PrivateKey {

	mac := hmac.New(sha256.New, pathSecret[:])
	mac.Write([]byte("offer-path-session-key"))
	mac.Write(intro.SerializeCompressed())
	key, _ := btcec.PrivKeyFromBytes(mac.Sum(nil))

	return key
}

// BuildReplyPath builds one blinded path to this node for a reply_path,
// carrying pathID. It goes through an introduction peer when one is
// available, a channel peer first, and is otherwise a single-hop path
// naming this node.
func (m *Messenger) BuildReplyPath(_ context.Context,
	pathID []byte) (*lnwire.BlindedPath, error) {

	withChannel, without := m.introPeers(m.cfg.Peers())
	switch {
	case len(withChannel) > 0:
		return m.buildPathToSelf(withChannel[0], pathID, nil)
	case len(without) > 0:
		return m.buildPathToSelf(without[0], pathID, nil)
	default:
		return m.buildPathToSelf(nil, pathID, nil)
	}
}

// introPeers splits the peers that can be introduction nodes into those
// with a channel and those without.
func (m *Messenger) introPeers(peers []Peer) ([]*btcec.PublicKey,
	[]*btcec.PublicKey) {

	var withChannel, without []*btcec.PublicKey
	for _, p := range peers {
		if p.Pub == nil || !p.OnionMessages {
			continue
		}
		if p.HasChannel {
			withChannel = append(withChannel, p.Pub)
		} else {
			without = append(without, p.Pub)
		}
	}

	return withChannel, without
}

// buildPathToSelf builds a blinded path ending at this node, through intro
// when it is not nil, with pathID in the final hop's data, under the given
// session key or a fresh one.
func (m *Messenger) buildPathToSelf(intro *btcec.PublicKey, pathID []byte,
	sessionKey *btcec.PrivateKey) (*lnwire.BlindedPath, error) {

	var (
		nodes []*btcec.PublicKey
		datas []*record.BlindedRouteData
	)
	if intro != nil {
		nodes = append(nodes, intro)
		datas = append(datas, record.NewNonFinalBlindedRouteDataOnionMessage(
			fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
				m.cfg.NodeKey,
			), nil, nil,
		))
	}
	nodes = append(nodes, m.cfg.NodeKey)
	datas = append(datas, record.NewFinalHopBlindedRouteData(nil, pathID))

	info, err := buildBlindedPath(nodes, datas, sessionKey)
	if err != nil {
		return nil, err
	}

	return fromSphinxPath(info.Path)
}

// buildBlindedPath blinds a path through nodes with their hop data, padded
// so that every hop's encrypted data has the same length, under the given
// session key or a fresh one.
func buildBlindedPath(nodes []*btcec.PublicKey,
	datas []*record.BlindedRouteData,
	sessionKey *btcec.PrivateKey) (*sphinx.BlindedPathInfo, error) {

	plains, err := encodePadded(datas)
	if err != nil {
		return nil, err
	}
	hops := make([]*sphinx.HopInfo, len(nodes))
	for i := range nodes {
		hops[i] = &sphinx.HopInfo{NodePub: nodes[i], PlainText: plains[i]}
	}
	if sessionKey == nil {
		sessionKey, err = btcec.NewPrivateKey()
		if err != nil {
			return nil, err
		}
	}
	info, err := sphinx.BuildBlindedPath(sessionKey, hops)
	if err != nil {
		return nil, fmt.Errorf("build blinded path: %w", err)
	}

	return info, nil
}

// encodePadded encodes hop data, padding every hop to the same length so a
// hop cannot tell its position from the length. Each hop gets a padding
// record, sized so the encodings come out equal; the record's own length
// prefix can shift the total, so the sizing is repeated until they match.
func encodePadded(datas []*record.BlindedRouteData) ([][]byte, error) {
	encode := func() ([][]byte, int, bool, error) {
		plains := make([][]byte, len(datas))
		longest, equal := 0, true
		for i, d := range datas {
			plain, err := record.EncodeBlindedRouteData(d)
			if err != nil {
				return nil, 0, false, fmt.Errorf("hop data: %w",
					err)
			}
			plains[i] = plain
			if i > 0 && len(plain) != len(plains[0]) {
				equal = false
			}
			if len(plain) > longest {
				longest = len(plain)
			}
		}

		return plains, longest, equal, nil
	}
	pads := make([]int, len(datas))
	for i, d := range datas {
		d.Padding = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType1](make([]byte, 0)),
		)
		pads[i] = 0
	}
	for attempt := 0; attempt < 4; attempt++ {
		plains, longest, equal, err := encode()
		if err != nil {
			return nil, err
		}
		if equal {
			return plains, nil
		}
		for i, d := range datas {
			pads[i] += longest - len(plains[i])
			d.Padding = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType1](
					make([]byte, pads[i]),
				),
			)
		}
	}
	plains, _, _, err := encode()

	return plains, err
}

// fromSphinxPath converts a built blinded path to its wire form.
func fromSphinxPath(p *sphinx.BlindedPath) (*lnwire.BlindedPath, error) {
	intro, err := lnwire.NewPubkeyIntro(p.IntroductionPoint)
	if err != nil {
		return nil, err
	}
	out := &lnwire.BlindedPath{
		IntroductionNode: intro,
		BlindingPoint:    p.BlindingPoint,
	}
	for _, h := range p.BlindedHops {
		out.Hops = append(out.Hops, lnwire.BlindedHop{
			BlindedNodeID: h.BlindedNodePub,
			EncryptedData: h.CipherText,
		})
	}

	return out, nil
}

// checkDestination refuses a destination that would break the onion
// builder or the wire encoder further down.
func checkDestination(dest Destination) error {
	switch {
	case dest.Path != nil && dest.NodeID != nil:
		return ErrBadDestination
	case dest.NodeID != nil:
		return nil
	case dest.Path == nil:
		return ErrBadDestination
	}
	p := dest.Path
	if p.BlindingPoint == nil || p.IntroductionNode == nil ||
		len(p.Hops) == 0 {

		return fmt.Errorf("%w: path is incomplete", ErrBadDestination)
	}
	if pk, ok := p.IntroductionNode.(lnwire.PubkeyIntro); ok &&
		pk.Pubkey == nil {

		return fmt.Errorf("%w: path has no introduction node",
			ErrBadDestination)
	}
	for i, h := range p.Hops {
		if h.BlindedNodeID == nil {
			return fmt.Errorf("%w: hop %d has no node id",
				ErrBadDestination, i)
		}
	}

	return nil
}

// Send delivers a payload to a destination, with an optional reply path
// for the recipient to answer over. The payload records must have types of
// 64 or above; the BOLT 12 ones are TypeInvoiceRequest, TypeInvoice and
// TypeInvoiceError. Without AllowConnect, only connected peers and routes
// through them are used.
func (m *Messenger) Send(ctx context.Context, dest Destination,
	payload []*lnwire.FinalHopTLV, replyPath *lnwire.BlindedPath,
	opts ...SendOption) error {

	if err := checkDestination(dest); err != nil {
		return err
	}
	var options sendOptions
	for _, opt := range opts {
		opt(&options)
	}
	s := &sender{m: m, peers: m.cfg.Peers(), allowConnect: options.allowConnect}

	var (
		// full is the whole blinded path the onion is built over,
		// from our first peer to the recipient.
		full *sphinx.BlindedPath
		// first is the peer the onion message is handed to.
		first *btcec.PublicKey
		err   error
	)
	if dest.Path != nil {
		full, first, err = s.pathToBlindedPath(ctx, dest.Path)
	} else {
		full, first, err = s.pathToNode(ctx, dest.NodeID)
	}
	if err != nil {
		return err
	}

	sphinxPath, err := route.OnionMessageBlindedPathToSphinxPath(
		full, replyPath, payload,
	)
	if err != nil {
		return fmt.Errorf("onion path: %w", err)
	}
	onion, err := buildOnion(sphinxPath)
	if err != nil {
		return err
	}

	var peer [33]byte
	copy(peer[:], first.SerializeCompressed())

	return m.cfg.SendToPeer(ctx, peer, full.BlindingPoint, onion)
}

// buildOnion wraps a path into an onion packet, at the regular size when
// the payload fits and at the large onion-message size otherwise.
func buildOnion(path *sphinx.PaymentPath) ([]byte, error) {
	sessionKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	var pkt *sphinx.OnionPacket
	for _, size := range []int{
		sphinx.MaxRoutingPayloadSize, sphinx.MaxOnionMessagePayloadSize,
	} {
		pkt, err = sphinx.NewOnionPacket(
			path, sessionKey, nil, sphinx.DeterministicPacketFiller,
			sphinx.WithMaxPayloadSize(size),
		)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("onion packet: %w", err)
	}
	var b bytes.Buffer
	if err := pkt.Encode(&b); err != nil {
		return nil, fmt.Errorf("encode onion: %w", err)
	}

	return b.Bytes(), nil
}

// sender is the state of one send: the peers as of its start, and whether
// it may dial out.
type sender struct {
	m            *Messenger
	peers        []Peer
	allowConnect bool
}

// pathToNode builds the blinded path for a message to a node by id: a
// route to it through the graph, or the node itself when it is a peer.
func (s *sender) pathToNode(ctx context.Context,
	dest *btcec.PublicKey) (*sphinx.BlindedPath, *btcec.PublicKey, error) {

	if dest.IsEqual(s.m.cfg.NodeKey) {
		return nil, nil, ErrDestinationIsSelf
	}
	hops, err := s.routeTo(ctx, dest)
	if err != nil {
		return nil, nil, err
	}

	datas := make([]*record.BlindedRouteData, len(hops))
	for i := range hops {
		if i < len(hops)-1 {
			datas[i] = record.NewNonFinalBlindedRouteDataOnionMessage(
				fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
					hops[i+1],
				), nil, nil,
			)
		} else {
			datas[i] = record.NewFinalHopBlindedRouteData(nil, nil)
		}
	}
	info, err := buildBlindedPath(hops, datas, nil)
	if err != nil {
		return nil, nil, err
	}

	return info.Path, hops[0], nil
}

// pathToBlindedPath builds the full blinded path for a message to a
// recipient's blinded path: our own hops up to its introduction node, with
// the last of them switching to the recipient's blinding point, followed
// by the recipient's hops.
func (s *sender) pathToBlindedPath(ctx context.Context,
	path *lnwire.BlindedPath) (*sphinx.BlindedPath, *btcec.PublicKey,
	error) {

	intro, err := s.m.ResolveIntro(ctx, path.IntroductionNode)
	if err != nil {
		return nil, nil, err
	}
	recv := &sphinx.BlindedPath{
		IntroductionPoint: intro,
		BlindingPoint:     path.BlindingPoint,
	}
	for _, h := range path.Hops {
		recv.BlindedHops = append(recv.BlindedHops, &sphinx.BlindedHopInfo{
			BlindedNodePub: h.BlindedNodeID,
			CipherText:     h.EncryptedData,
		})
	}

	// When we are the path's introduction node, the recipient is a peer
	// of ours who built the path through us: process our own hop and
	// send from the next one.
	if intro.IsEqual(s.m.cfg.NodeKey) {
		recv, err = s.m.peelOwnHop(ctx, recv)
		if err != nil {
			return nil, nil, err
		}
		intro = recv.IntroductionPoint
		if intro.IsEqual(s.m.cfg.NodeKey) {
			return nil, nil, ErrDestinationIsSelf
		}
	}
	hops, err := s.routeTo(ctx, intro)
	if err != nil {
		return nil, nil, err
	}

	// The introduction node is a peer: the recipient's path is the whole
	// path.
	if len(hops) == 1 {
		return recv, intro, nil
	}

	// Our hops lead to the introduction node; the last of them is told
	// to forward to it under the recipient's first path key, which is
	// the path's blinding point, or the key after our own hop when we
	// peeled it.
	ours := hops[:len(hops)-1]
	datas := make([]*record.BlindedRouteData, len(ours))
	for i := range ours {
		var override *btcec.PublicKey
		if i == len(ours)-1 {
			override = recv.BlindingPoint
		}
		datas[i] = record.NewNonFinalBlindedRouteDataOnionMessage(
			fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
				hops[i+1],
			), override, nil,
		)
	}
	info, err := buildBlindedPath(ours, datas, nil)
	if err != nil {
		return nil, nil, err
	}
	full := &sphinx.BlindedPath{
		IntroductionPoint: info.Path.IntroductionPoint,
		BlindingPoint:     info.Path.BlindingPoint,
		BlindedHops: append(
			info.Path.BlindedHops, recv.BlindedHops...,
		),
	}

	return full, hops[0], nil
}

// peelOwnHop processes the first hop of a blinded path that starts at this
// node, and returns the path from the next hop on, under the path key that
// hop expects.
func (m *Messenger) peelOwnHop(ctx context.Context,
	path *sphinx.BlindedPath) (*sphinx.BlindedPath, error) {

	if len(path.BlindedHops) < 2 {
		return nil, ErrDestinationIsSelf
	}
	plain, err := m.cfg.DecryptBlindedData(
		path.BlindingPoint, bytes.Clone(path.BlindedHops[0].CipherText),
	)
	if err != nil {
		return nil, fmt.Errorf("own hop of path: %w", err)
	}
	data, err := record.DecodeBlindedRouteData(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("own hop of path: %w", err)
	}

	var next *btcec.PublicKey
	data.NextNodeID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType4, *btcec.PublicKey]) {
			next = r.Val
		},
	)
	if next == nil {
		var scid lnwire.ShortChannelID
		data.ShortChannelID.WhenSome(
			func(r tlv.RecordT[tlv.TlvType2, lnwire.ShortChannelID]) {
				scid = r.Val
			},
		)
		if scid.ToUint64() == 0 || m.cfg.FetchChannelEdge == nil {
			return nil, errors.New("own hop of path names no " +
				"next node")
		}
		edge, err := m.cfg.FetchChannelEdge(ctx, scid.ToUint64())
		if err != nil {
			return nil, fmt.Errorf("own hop's next channel: %w", err)
		}
		other, err := edge.OtherNodeKeyBytes(
			m.cfg.NodeKey.SerializeCompressed(),
		)
		if err != nil {
			return nil, fmt.Errorf("own hop's next channel: %w", err)
		}
		next, err = btcec.ParsePubKey(other[:])
		if err != nil {
			return nil, err
		}
	}

	nextKey, err := m.cfg.NextPathKey(path.BlindingPoint)
	if err != nil {
		return nil, fmt.Errorf("next path key: %w", err)
	}
	data.NextBlindingOverride.WhenSome(
		func(r tlv.RecordT[tlv.TlvType8, *btcec.PublicKey]) {
			nextKey = r.Val
		},
	)

	return &sphinx.BlindedPath{
		IntroductionPoint: next,
		BlindingPoint:     nextKey,
		BlindedHops:       path.BlindedHops[1:],
	}, nil
}

// ResolveIntro turns an introduction node into a node id, looking a channel
// and direction up in the graph.
func (m *Messenger) ResolveIntro(ctx context.Context,
	node lnwire.IntroductionNode) (*btcec.PublicKey, error) {

	switch n := node.(type) {
	case lnwire.PubkeyIntro:
		return n.Pubkey, nil

	case lnwire.SciddirIntro:
		if m.cfg.FetchChannelEdge == nil {
			return nil, errors.New("cannot resolve a channel " +
				"introduction node without a graph")
		}
		scid := lnwire.NewShortChanIDFromInt(
			binary.BigEndian.Uint64(n.SCID[:]),
		)
		edge, err := m.cfg.FetchChannelEdge(ctx, scid.ToUint64())
		if err != nil {
			return nil, fmt.Errorf("introduction channel %v: %w",
				scid, err)
		}
		key := edge.NodeKey1Bytes
		if n.Direction == 1 {
			key = edge.NodeKey2Bytes
		}
		pub, err := btcec.ParsePubKey(key[:])
		if err != nil {
			return nil, fmt.Errorf("introduction node: %w", err)
		}

		return pub, nil

	default:
		return nil, fmt.Errorf("introduction node of type %T", node)
	}
}

// routeTo finds the nodes a message passes through to reach dest, dest
// last. A peer is reached directly. Otherwise the graph is searched for
// the shortest route whose first hop is a connected peer. When there is
// none and dialling is allowed, the first hop of the shortest route, or
// the node itself, is connected to at its announced addresses.
func (s *sender) routeTo(ctx context.Context,
	dest *btcec.PublicKey) ([]*btcec.PublicKey, error) {

	if s.isPeer(dest) {
		return []*btcec.PublicKey{dest}, nil
	}
	if s.m.cfg.Graph == nil {
		return s.dial(ctx, dest, nil)
	}

	// The shortest route from us, whatever its first hop.
	direct, err := s.findPath(ctx, s.m.cfg.NodeKey, dest)
	if err != nil {
		return nil, err
	}
	if len(direct) > 0 && s.isPeer(direct[0]) {
		return direct, nil
	}

	// Its first hop is not a connected peer: the shortest route that
	// starts at one, within the same hop limit, and not back through
	// us.
	var best []*btcec.PublicKey
	for _, p := range s.peers {
		if p.Pub == nil || !p.OnionMessages || p.Pub.IsEqual(dest) {
			continue
		}
		rest, err := s.findPathFrom(ctx, p.Pub, dest, s.m.cfg.MaxHops-1)
		if err != nil {
			return nil, err
		}
		if len(rest) == 0 || s.passesThroughUs(rest) {
			continue
		}
		candidate := append([]*btcec.PublicKey{p.Pub}, rest...)
		if best == nil || len(candidate) < len(best) {
			best = candidate
		}
	}
	if best != nil {
		return best, nil
	}

	return s.dial(ctx, dest, direct)
}

// passesThroughUs reports whether a route has this node on it.
func (s *sender) passesThroughUs(hops []*btcec.PublicKey) bool {
	for _, h := range hops {
		if h.IsEqual(s.m.cfg.NodeKey) {
			return true
		}
	}

	return false
}

// findPath searches the graph for a route from source to dest within the
// hop limit, returning the nodes after source with dest last, or nothing
// when there is none.
func (s *sender) findPath(ctx context.Context, source,
	dest *btcec.PublicKey) ([]*btcec.PublicKey, error) {

	return s.findPathFrom(ctx, source, dest, s.m.cfg.MaxHops)
}

func (s *sender) findPathFrom(ctx context.Context, source,
	dest *btcec.PublicKey, maxHops int) ([]*btcec.PublicKey, error) {

	if maxHops < 1 {
		return nil, nil
	}
	path, err := onionmessage.FindPath(
		ctx, s.m.cfg.Graph, route.NewVertex(source),
		route.NewVertex(dest), maxHops,
	)
	switch {
	case err == nil:
	case errors.Is(err, onionmessage.ErrNoPathFound),
		errors.Is(err, onionmessage.ErrNodeNotFound),
		errors.Is(err, onionmessage.ErrDestinationNoOnionSupport):

		return nil, nil

	default:
		return nil, fmt.Errorf("route search: %w", err)
	}
	hops := make([]*btcec.PublicKey, 0, len(path))
	for _, v := range path {
		pub, err := btcec.ParsePubKey(v[:])
		if err != nil {
			return nil, err
		}
		hops = append(hops, pub)
	}

	return hops, nil
}

// dial connects to the first hop of a route, or to the destination, when
// the send may open connections.
func (s *sender) dial(ctx context.Context, dest *btcec.PublicKey,
	direct []*btcec.PublicKey) ([]*btcec.PublicKey, error) {

	if !s.allowConnect {
		return nil, fmt.Errorf("%w: no route through a connected peer",
			ErrUnreachable)
	}
	if len(direct) > 0 {
		if err := s.connect(ctx, direct[0]); err == nil {
			return direct, nil
		} else {
			log.Debugf("First hop %x of the route to %x cannot "+
				"be connected: %v", direct[0].SerializeCompressed(),
				dest.SerializeCompressed(), err)
		}
	}
	if err := s.connect(ctx, dest); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	return []*btcec.PublicKey{dest}, nil
}

// isPeer reports whether a node is a connected peer that speaks onion
// messages.
func (s *sender) isPeer(pub *btcec.PublicKey) bool {
	for _, p := range s.peers {
		if p.Pub != nil && p.Pub.IsEqual(pub) {
			return p.OnionMessages
		}
	}

	return false
}

// connect connects to a node at its announced addresses and waits for it
// to become a usable peer.
func (s *sender) connect(ctx context.Context, pub *btcec.PublicKey) error {
	if s.m.cfg.FetchNodeAddrs == nil || s.m.cfg.ConnectPeer == nil {
		return errors.New("no way to connect")
	}
	addrs, err := s.m.cfg.FetchNodeAddrs(ctx, route.NewVertex(pub))
	if err != nil {
		return fmt.Errorf("addresses of %x: %w",
			pub.SerializeCompressed(), err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%x announces no address",
			pub.SerializeCompressed())
	}

	select {
	case s.m.dialSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.m.dialSem }()

	ctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	if err := s.m.cfg.ConnectPeer(ctx, pub, addrs); err != nil {
		return err
	}

	// The peers as of now, with the new one in them.
	s.peers = s.m.cfg.Peers()
	if !s.isPeer(pub) {
		return fmt.Errorf("%x connected but does not support onion "+
			"messages", pub.SerializeCompressed())
	}

	return nil
}
