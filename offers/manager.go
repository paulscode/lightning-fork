package offers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OfferHRP is the bech32 human-readable part of an offer.
	OfferHRP = "lno"

	// InvoiceRequestHRP is the bech32 human-readable part of an invoice
	// request.
	InvoiceRequestHRP = "lnr"

	// InvoiceHRP is the bech32 human-readable part of an invoice.
	InvoiceHRP = "lni"

	// MaxDescriptionBytes bounds offer_description. The spec sets no
	// limit; an offer is meant to be shown and signed by humans, so a
	// kilobyte is generous.
	MaxDescriptionBytes = 1024

	// MaxIssuerBytes bounds offer_issuer.
	MaxIssuerBytes = 256

	// MaxLabelBytes bounds the operator's label.
	MaxLabelBytes = 256

	// metadataBytes is the size of offer_metadata.
	metadataBytes = 16

	// secretTag domain-separates the offer secret from the other uses of
	// the key it is derived from.
	secretTag = "lightning-fork/offers/secret/v1"

	// metadataTag and pathTag domain-separate the two things derived from
	// the offer secret.
	metadataTag = "offer-metadata"
	pathTag     = "offer-path-secret"
)

// DefaultMaxOffers bounds how many offers a node keeps unless configured
// otherwise. Offers are minted by the operator, not by peers, so this only
// stops a runaway script.
const DefaultMaxOffers = 1000

var (
	// ErrDescriptionRequired is returned when an offer with an amount has
	// no description, which the spec forbids.
	ErrDescriptionRequired = errors.New("an offer with an amount needs " +
		"a description")

	// ErrInvalidUTF8 is returned when a text field is not UTF-8.
	ErrInvalidUTF8 = errors.New("text fields must be UTF-8")

	// ErrControlCharacters is returned when a text field carries control
	// characters, which would corrupt a terminal or a signed message.
	ErrControlCharacters = errors.New("text fields must not carry " +
		"control characters")

	// ErrTextTooLong is returned when a text field is over its limit.
	ErrTextTooLong = errors.New("text field too long")

	// ErrExpiryInPast is returned when an offer's expiry has already
	// passed.
	ErrExpiryInPast = errors.New("the expiry is in the past")

	// ErrPathsConflict is returned when paths are both forced and
	// forbidden.
	ErrPathsConflict = errors.New("with-paths and no-paths exclude each " +
		"other")

	// ErrQuantityConflict is returned when a quantity maximum and "any
	// quantity" are both asked for.
	ErrQuantityConflict = errors.New("quantity-max and quantity-any " +
		"exclude each other")

	// ErrTooManyOffers is returned when the node already keeps MaxOffers.
	ErrTooManyOffers = errors.New("too many offers")

	// ErrWrongChain is returned when a decoded offer, request or invoice
	// is not for this chain.
	ErrWrongChain = errors.New("not for this chain")

	// ErrNotAnOffer is returned when a string decodes to something other
	// than an offer.
	ErrNotAnOffer = errors.New("not an offer")

	// ErrOfferDisabled is returned when a request names a disabled offer.
	ErrOfferDisabled = errors.New("offer is disabled")

	// ErrOfferExpired is returned when a request names an expired offer.
	ErrOfferExpired = errors.New("offer has expired")
)

// PathBuilder builds blinded paths to this node for use as offer_paths, with
// a path_id derived from the given secret so that a message arriving over
// one of them names the offer. Implemented by the onion-message layer.
type PathBuilder interface {
	// BuildOfferPaths returns zero or more blinded paths to this node. An
	// empty result is not an error: it means no suitable introduction
	// peer is available, and the offer must then rely on offer_issuer_id.
	BuildOfferPaths(ctx context.Context,
		pathSecret [32]byte) ([]lnwire.BlindedPath, error)
}

// Config holds what the manager needs.
type Config struct {
	// ChainHash is the chain every offer names and every request must
	// name.
	ChainHash [32]byte

	// IssuerKey is the key that signs invoices for the offers, published
	// as offer_issuer_id. It is the node's identity key.
	IssuerKey keychain.KeyDescriptor

	// Secret is a node secret that offer_metadata and each offer's path
	// secret are derived from, so that the same offer is minted again
	// after a restore from seed, and so that an offer can be recognised
	// as this node's from its fields alone. Use DeriveSecret.
	Secret [32]byte

	// Store persists the offers.
	Store Store

	// Clock is the source of time.
	Clock clock.Clock

	// PathBuilder builds offer_paths. Optional: without one, or when it
	// returns no paths, offers carry only offer_issuer_id, which suits a
	// node that peers can reach directly.
	PathBuilder PathBuilder

	// NodeReachable reports whether this node can be reached by node id
	// alone, that is, whether it announces an address. When it does not,
	// offers get blinded paths if a builder is available.
	NodeReachable func() bool

	// MaxOffers bounds how many offers the node keeps; zero means
	// DefaultMaxOffers.
	MaxOffers int
}

// CreateParams describes an offer to mint.
type CreateParams struct {
	// Description is offer_description. Required when Amount is set. For
	// an OCEAN or CONVOY payout it is the pool's mandated text.
	Description string

	// AmountMsat is offer_amount, or zero for an amount the payer
	// chooses.
	AmountMsat uint64

	// AbsoluteExpiry is when the offer stops being valid, or zero for
	// never.
	AbsoluteExpiry time.Time

	// Issuer is offer_issuer, free text naming who is paid.
	Issuer string

	// QuantityMax is the largest quantity a request may ask for; zero
	// leaves quantities out of the offer unless QuantityAny is set.
	QuantityMax uint64

	// QuantityAny lets a request ask for any quantity: offer_quantity_max
	// is set to zero, which is how the spec spells "no limit".
	QuantityAny bool

	// Label is the operator's own note, kept with the record only.
	Label string

	// WithPaths forces blinded paths onto the offer even when the node is
	// reachable by id. NoPaths forbids them even when it is not. Neither
	// set: paths only when the node is unreachable by id.
	WithPaths bool
	NoPaths   bool
}

// Manager mints, keeps and serves this node's offers.
type Manager struct {
	cfg Config
}

// NewManager returns a manager.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("offers: store required")
	}
	if cfg.IssuerKey.PubKey == nil {
		return nil, errors.New("offers: issuer key required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.ChainHash == [32]byte{} {
		return nil, errors.New("offers: chain hash required")
	}
	if cfg.Secret == [32]byte{} {
		return nil, errors.New("offers: secret required")
	}
	if cfg.MaxOffers <= 0 {
		cfg.MaxOffers = DefaultMaxOffers
	}

	return &Manager{cfg: cfg}, nil
}

// DeriveSecret derives the offer secret from the wallet's base encryption
// key, the way the channel backup key is derived, so it exists in every
// wallet configuration including a remote signer and comes back with the
// seed. The tag keeps it apart from the backup key.
func DeriveSecret(keyRing keychain.KeyRing) ([32]byte, error) {
	desc, err := keyRing.DeriveKey(keychain.KeyLocator{
		Family: keychain.KeyFamilyBaseEncryption,
		Index:  0,
	})
	if err != nil {
		return [32]byte{}, fmt.Errorf("offer secret: %w", err)
	}
	if desc.PubKey == nil {
		return [32]byte{}, errors.New("offer secret: no key")
	}
	h := sha256.New()
	h.Write([]byte(secretTag))
	h.Write(desc.PubKey.SerializeCompressed())

	var secret [32]byte
	copy(secret[:], h.Sum(nil))

	return secret, nil
}

// ChainHash returns the chain every offer is minted for.
func (m *Manager) ChainHash() [32]byte {
	return m.cfg.ChainHash
}

// IssuerKey returns the key published as offer_issuer_id.
func (m *Manager) IssuerKey() keychain.KeyDescriptor {
	return m.cfg.IssuerKey
}

// checkText rejects text that is over its limit, not UTF-8 or carrying
// control characters.
func checkText(field, value string, max int) error {
	if len(value) > max {
		return fmt.Errorf("%s: %w (limit %d bytes)", field,
			ErrTextTooLong, max)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s: %w", field, ErrInvalidUTF8)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s: %w", field, ErrControlCharacters)
		}
	}

	return nil
}

// metadataFor derives offer_metadata for an offer from its other fields: a
// keyed hash the node can recompute, so the same fields always mint the
// same offer and a foreign offer with our issuer id is told apart from ours.
func (m *Manager) metadataFor(o *bolt12.Offer) ([]byte, error) {
	bare := *o
	bare.OfferMetadata = tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]{}
	root, err := bolt12.OfferID(&bare)
	if err != nil {
		return nil, fmt.Errorf("offer fields: %w", err)
	}
	mac := hmac.New(sha256.New, m.cfg.Secret[:])
	mac.Write([]byte(metadataTag))
	mac.Write(root[:])

	return mac.Sum(nil)[:metadataBytes], nil
}

// baseRoot is the Merkle root of an offer's fields without offer_metadata
// and offer_paths: what the offer is, apart from how it is reached and how
// it is recognised. The path secret derives from it, since the paths carry
// the secret and so cannot be covered by it.
func baseRoot(o *bolt12.Offer) ([32]byte, error) {
	bare := *o
	bare.OfferMetadata = tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]{}
	bare.OfferPaths = tlv.OptionalRecordT[
		tlv.TlvType16, lnwire.BlindedPaths,
	]{}

	return bolt12.OfferID(&bare)
}

// IsOurs reports whether an offer was minted by this node: its metadata is
// the keyed hash of its other fields. It needs no stored state, so it holds
// across a restore from seed.
func (m *Manager) IsOurs(o *bolt12.Offer) bool {
	var got []byte
	o.OfferMetadata.WhenSome(func(r tlv.RecordT[tlv.TlvType4, tlv.Blob]) {
		got = []byte(r.Val)
	})
	if len(got) != metadataBytes {
		return false
	}
	want, err := m.metadataFor(o)
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(got, want) == 1
}

// PathSecretFor derives the secret an offer's blinded paths carry as their
// path_id, from the offer's fields other than its metadata and paths. It
// can be recomputed from a request that mirrors the offer, so a message
// arriving over one of the paths can be checked without stored state.
func (m *Manager) PathSecretFor(o *bolt12.Offer) ([32]byte, error) {
	root, err := baseRoot(o)
	if err != nil {
		return [32]byte{}, fmt.Errorf("offer fields: %w", err)
	}
	mac := hmac.New(sha256.New, m.cfg.Secret[:])
	mac.Write([]byte(pathTag))
	mac.Write(root[:])

	var secret [32]byte
	copy(secret[:], mac.Sum(nil))

	return secret, nil
}

// CreateOffer mints an offer for this chain, stores it and returns its
// record. Minting the same offer twice returns the stored record and
// reports created as false, since the offer's identity is a function of its
// fields.
func (m *Manager) CreateOffer(ctx context.Context,
	params CreateParams) (*Record, bool, error) {

	if err := checkText(
		"description", params.Description, MaxDescriptionBytes,
	); err != nil {
		return nil, false, err
	}
	if err := checkText("issuer", params.Issuer, MaxIssuerBytes); err != nil {
		return nil, false, err
	}
	if err := checkText("label", params.Label, MaxLabelBytes); err != nil {
		return nil, false, err
	}
	if params.AmountMsat != 0 && strings.TrimSpace(params.Description) == "" {
		return nil, false, ErrDescriptionRequired
	}
	now := m.cfg.Clock.Now().Truncate(time.Second).UTC()
	expiry := params.AbsoluteExpiry
	if !expiry.IsZero() {
		expiry = expiry.Truncate(time.Second).UTC()
		if !expiry.After(now) {
			return nil, false, ErrExpiryInPast
		}
	}
	if params.WithPaths && params.NoPaths {
		return nil, false, ErrPathsConflict
	}
	if params.QuantityMax != 0 && params.QuantityAny {
		return nil, false, ErrQuantityConflict
	}

	offer := &bolt12.Offer{
		OfferChains: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](bolt12.ChainsRecord{
				Chains: [][32]byte{m.cfg.ChainHash},
			}),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](
				m.cfg.IssuerKey.PubKey,
			),
		),

		// Say which proof of work rules this offer is written under. A
		// reader which has not upgraded cannot read the even bit and
		// refuses the offer rather than paying an invoice it could not
		// settle; chain_hash cannot tell it, because both sides carry
		// the same genesis hash.
		OfferFeatures: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](
				*bolt12.Blake2bVector(),
			),
		),
	}
	if params.AmountMsat != 0 {
		offer.OfferAmount = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType8](
				bolt12.TUint64(params.AmountMsat),
			),
		)
	}
	if params.Description != "" {
		offer.OfferDescription = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](
				tlv.Blob(params.Description),
			),
		)
	}
	if !expiry.IsZero() {
		offer.OfferAbsoluteExpiry = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType14](
				bolt12.TUint64(expiry.Unix()),
			),
		)
	}
	if params.Issuer != "" {
		offer.OfferIssuer = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType18](
				tlv.Blob(params.Issuer),
			),
		)
	}
	if params.QuantityMax != 0 || params.QuantityAny {
		offer.OfferQuantityMax = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType20](
				bolt12.TUint64(params.QuantityMax),
			),
		)
	}

	// Blinded paths when the node cannot be reached by its id, or when
	// asked for; never when forbidden. The paths carry a path_id derived
	// from the offer's other fields, so it is known before they exist.
	wantPaths := params.WithPaths
	if !params.WithPaths && !params.NoPaths && m.cfg.NodeReachable != nil {
		wantPaths = !m.cfg.NodeReachable()
	}
	pathSecret, err := m.PathSecretFor(offer)
	if err != nil {
		return nil, false, err
	}
	if wantPaths && m.cfg.PathBuilder != nil {
		paths, err := m.cfg.PathBuilder.BuildOfferPaths(ctx, pathSecret)
		if err != nil {
			return nil, false, fmt.Errorf("offer paths: %w", err)
		}
		if len(paths) > 0 {
			offer.OfferPaths = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType16](
					lnwire.BlindedPaths{Paths: paths},
				),
			)
		} else {
			log.Warnf("No introduction peer for offer paths; the " +
				"offer names the node id only")
		}
	} else if wantPaths {
		log.Warnf("Offer paths wanted but no path builder is " +
			"configured; the offer names the node id only")
	}

	// The metadata covers every other field, paths included, so an
	// offer altered in any way stops verifying as ours.
	metadata, err := m.metadataFor(offer)
	if err != nil {
		return nil, false, err
	}
	offer.OfferMetadata = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType4](tlv.Blob(metadata)),
	)

	// The writer checks, with the chain pinned: an offer that named no
	// chain would read as a Bitcoin mainnet offer.
	if err := bolt12.ValidateOfferWriteOnChain(
		offer, m.cfg.ChainHash,
	); err != nil {
		return nil, false, fmt.Errorf("offer does not validate: %w", err)
	}
	encoded, err := offer.Encode()
	if err != nil {
		return nil, false, fmt.Errorf("encode offer: %w", err)
	}
	// Read it back the way a payer will, against our chain, before
	// anything is stored or shown.
	decoded, err := bolt12.DecodeOffer(encoded)
	if err != nil {
		return nil, false, fmt.Errorf("re-decode offer: %w", err)
	}
	if err := bolt12.ValidateOfferRead(
		decoded, now, m.cfg.ChainHash, bolt12.Blake2bFeatures,
	); err != nil {
		return nil, false, fmt.Errorf("offer does not validate: %w", err)
	}
	if !m.IsOurs(decoded) {
		return nil, false, errors.New("offer does not verify as ours")
	}
	id, err := bolt12.OfferID(decoded)
	if err != nil {
		return nil, false, fmt.Errorf("offer id: %w", err)
	}
	str, err := bolt12.Encode(OfferHRP, encoded)
	if err != nil {
		return nil, false, fmt.Errorf("bech32 encode: %w", err)
	}

	// The same fields mint the same offer; hand back the one we keep.
	if existing, err := m.cfg.Store.Get(OfferID(id)); err == nil {
		log.Infof("Offer %v already exists (%s)", existing.ID,
			describe(existing))

		return existing, false, nil
	} else if !errors.Is(err, ErrOfferNotFound) {
		return nil, false, err
	}

	all, err := m.cfg.Store.List()
	if err != nil {
		return nil, false, err
	}
	if len(all) >= m.cfg.MaxOffers {
		return nil, false, ErrTooManyOffers
	}

	record := &Record{
		ID:              OfferID(id),
		Offer:           encoded,
		Bolt12:          str,
		Description:     params.Description,
		AmountMsat:      params.AmountMsat,
		AbsoluteExpiry:  expiry,
		CreatedAt:       now,
		Active:          true,
		IssuerKeyFamily: uint32(m.cfg.IssuerKey.Family),
		IssuerKeyIndex:  m.cfg.IssuerKey.Index,
		PathSecret:      pathSecret,
		Label:           params.Label,
	}
	if err := m.cfg.Store.Put(record); err != nil {
		return nil, false, err
	}

	log.Infof("Created offer %v (%s)", record.ID, describe(record))

	return record, true, nil
}

// describe returns a short human description of a record for logs.
func describe(r *Record) string {
	amount := "any amount"
	if r.AmountMsat != 0 {
		amount = fmt.Sprintf("%d msat", r.AmountMsat)
	}
	desc := []rune(r.Description)
	if len(desc) > 40 {
		desc = append(desc[:40], '…')
	}

	return fmt.Sprintf("%s, %q", amount, string(desc))
}

// ListOffers returns the stored offers, newest first.
func (m *Manager) ListOffers(activeOnly bool) ([]*Record, error) {
	records, err := m.cfg.Store.List()
	if err != nil {
		return nil, err
	}
	out := records[:0]
	for _, r := range records {
		if activeOnly && !r.Active {
			continue
		}
		out = append(out, r)
	}
	sortNewestFirst(out)

	return out, nil
}

// sortNewestFirst orders records by creation time, newest first, with the
// id as a tiebreak so the order is stable.
func sortNewestFirst(records []*Record) {
	for i := 1; i < len(records); i++ {
		for j := i; j > 0 && newer(records[j], records[j-1]); j-- {
			records[j], records[j-1] = records[j-1], records[j]
		}
	}
}

func newer(a, b *Record) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}

	return a.ID.String() < b.ID.String()
}

// LookupOffer returns one stored offer.
func (m *Manager) LookupOffer(id OfferID) (*Record, error) {
	return m.cfg.Store.Get(id)
}

// DisableOffer marks an offer as disabled: it stays listed, and requests for
// it are refused.
func (m *Manager) DisableOffer(id OfferID) error {
	return m.cfg.Store.Update(id, func(r *Record) error {
		r.Active = false

		return nil
	})
}

// EnableOffer reverses DisableOffer.
func (m *Manager) EnableOffer(id OfferID) error {
	return m.cfg.Store.Update(id, func(r *Record) error {
		r.Active = true

		return nil
	})
}

// ServeableOffer returns the stored offer a request may be served for, or
// why it may not: unknown, disabled or expired. Expiry follows the spec's
// reader: an offer is expired once the current time is past
// offer_absolute_expiry.
func (m *Manager) ServeableOffer(id OfferID) (*Record, error) {
	record, err := m.cfg.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if !record.Active {
		return nil, ErrOfferDisabled
	}
	if !record.AbsoluteExpiry.IsZero() &&
		m.cfg.Clock.Now().After(record.AbsoluteExpiry) {

		return nil, ErrOfferExpired
	}

	return record, nil
}

// CountInvoice records that an invoice was issued for the offer.
func (m *Manager) CountInvoice(id OfferID) error {
	return m.cfg.Store.Update(id, func(r *Record) error {
		r.InvoicesIssued++

		return nil
	})
}

// Decoded is the result of decoding any BOLT 12 string.
type Decoded struct {
	// HRP is the string's human-readable part: lno, lnr or lni.
	HRP string

	// Offer, InvoiceRequest and Invoice hold the decoded message; exactly
	// one is set.
	Offer          *bolt12.Offer
	InvoiceRequest *bolt12.InvoiceRequest
	Invoice        *bolt12.Invoice

	// OfferID is the id of the offer the message is or mirrors, when it
	// has one.
	OfferID *OfferID

	// ForThisChain is whether the message names this node's chain. It is
	// read off the message's chain fields alone: a message that names no
	// chain is for Bitcoin mainnet by the spec's default, so it is never
	// for this chain. A message for another chain still decodes, so the
	// caller can say which chain it names.
	ForThisChain bool

	// Chains lists the chains an offer names, or the chain a request or
	// invoice names. Empty means the message names none.
	Chains [][32]byte

	// ValidationError is why an offer does not pass the reader checks
	// for this chain and time, or nil when it does. Requests and invoices
	// are not validated here; that needs the offer they answer.
	ValidationError error

	// Ours is whether an offer was minted by this node.
	Ours bool
}

// DecodeBolt12 decodes an offer, invoice request or invoice string and
// reports which chain it is for. Anything that parses is returned; a
// string that does not parse is an error.
func (m *Manager) DecodeBolt12(s string) (*Decoded, error) {
	hrp, data, err := bolt12.Decode(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	out := &Decoded{HRP: hrp}
	switch hrp {
	case OfferHRP:
		offer, err := bolt12.DecodeOffer(data)
		if err != nil {
			return nil, fmt.Errorf("decode offer: %w", err)
		}
		out.Offer = offer
		out.Chains = offerChains(offer)
		out.ForThisChain = namesChain(out.Chains, m.cfg.ChainHash)
		id, err := bolt12.OfferID(offer)
		if err == nil {
			oid := OfferID(id)
			out.OfferID = &oid
		}
		out.ValidationError = bolt12.ValidateOfferRead(
			offer, m.cfg.Clock.Now(), m.cfg.ChainHash,
			bolt12.Blake2bFeatures,
		)
		out.Ours = out.ForThisChain && m.IsOurs(offer)

	case InvoiceRequestHRP:
		ir, err := bolt12.DecodeInvoiceRequest(data)
		if err != nil {
			return nil, fmt.Errorf("decode invoice request: %w", err)
		}
		out.InvoiceRequest = ir
		if chain, ok := requestChain(ir); ok {
			out.Chains = [][32]byte{chain}
		}
		out.ForThisChain = namesChain(out.Chains, m.cfg.ChainHash)
		if id, err := bolt12.RequestOfferID(ir); err == nil {
			oid := OfferID(id)
			out.OfferID = &oid
		}

	case InvoiceHRP:
		inv, err := bolt12.DecodeInvoice(data)
		if err != nil {
			return nil, fmt.Errorf("decode invoice: %w", err)
		}
		out.Invoice = inv
		if chain, ok := invoiceChain(inv); ok {
			out.Chains = [][32]byte{chain}
		}
		out.ForThisChain = namesChain(out.Chains, m.cfg.ChainHash)
		if id, err := bolt12.InvoiceOfferID(inv); err == nil {
			oid := OfferID(id)
			out.OfferID = &oid
		}

	default:
		return nil, fmt.Errorf("unknown BOLT 12 prefix %q", hrp)
	}

	return out, nil
}

// tlv14 is the record type of offer_absolute_expiry.
type tlv14 = tlv.RecordT[tlv.TlvType14, bolt12.TUint64]

// unixTime turns spec seconds into a time.
func unixTime(secs uint64) time.Time {
	return time.Unix(int64(secs), 0).UTC()
}

// optBlob returns a blob record's bytes, or nil.
func optBlob[T tlv.TlvType](r tlv.OptionalRecordT[T, tlv.Blob]) []byte {
	var out []byte
	r.WhenSome(func(rec tlv.RecordT[T, tlv.Blob]) {
		out = []byte(rec.Val)
	})

	return out
}

// namesChain reports whether a chain list names the given chain. An empty
// list names Bitcoin mainnet by the spec's default, which is never ours.
func namesChain(chains [][32]byte, chain [32]byte) bool {
	for _, c := range chains {
		if c == chain {
			return true
		}
	}

	return false
}

// offerChains lists the chains an offer names, with the spec's default
// applied: an absent offer_chains means Bitcoin mainnet.
//
// That default used not to matter here. This chain advertised a chain_hash of
// its own, so an offer naming no chain was for some other chain and reading
// the absence as "not ours" was right by accident. Since 2026-09-17 this
// chain's chain_hash is the mainnet genesis, so an offer that omits the field
// names this chain, and most offers omit it. Keeping the old reading would
// have refused the common case while ValidateOfferRead, which applies the
// default, accepted it.
//
// It defers to bolt12.OfferChains rather than repeating the rule, because two
// implementations of one default are what allowed them to disagree.
func offerChains(o *bolt12.Offer) [][32]byte {
	return bolt12.OfferChains(o)
}

// requestChain returns the chain a request names, if it names one.
func requestChain(ir *bolt12.InvoiceRequest) ([32]byte, bool) {
	var (
		chain [32]byte
		ok    bool
	)
	ir.InvreqChain.WhenSome(func(r tlv.RecordT[tlv.TlvType80, [32]byte]) {
		chain, ok = r.Val, true
	})

	return chain, ok
}

// invoiceChain returns the chain an invoice names, if it names one.
func invoiceChain(inv *bolt12.Invoice) ([32]byte, bool) {
	var (
		chain [32]byte
		ok    bool
	)
	inv.InvreqChain.WhenSome(func(r tlv.RecordT[tlv.TlvType80, [32]byte]) {
		chain, ok = r.Val, true
	})

	return chain, ok
}

// IssuerID returns the issuer id of an offer, if it names one.
func IssuerID(o *bolt12.Offer) *btcec.PublicKey {
	var key *btcec.PublicKey
	o.OfferIssuerID.WhenSome(func(r tlv.RecordT[tlv.TlvType22, *btcec.PublicKey]) {
		key = r.Val
	})

	return key
}
