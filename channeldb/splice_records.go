package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// A pending splice's state lives in the channel's own bucket: a record per
// splice transaction, and the revocation log of the peer's commitments on its
// funding output. Both are written in the same transaction as the commitment
// chain step they belong to, through the SpliceCommitWrite arguments of
// AppendRemoteCommitChain, UpdateCommitment and AdvanceCommitChainTail.

var (
	// spliceRecordsBucket holds a channel's pending splices.
	//
	// splice txid -> SpliceRecord
	spliceRecordsBucket = []byte("splice-records")

	// spliceRevocationLogBucket holds the revocation log of the peer's
	// commitments on each pending splice's funding output, with the same
	// entries as the channel's own revocation log.
	//
	// splice txid -> commitment height -> RevocationLog
	spliceRevocationLogBucket = []byte("splice-revocation-log")

	// spliceDroppedBucket keeps, for the life of the channel, the funding
	// outputs and keys of the splice transactions it dropped after
	// signing them, keyed by txid: one could still confirm after a deep
	// reorganisation, and the peer's revoked commitments on it must be
	// found and punished.
	spliceDroppedBucket = []byte("splice-dropped")

	// ErrSpliceRecordNotFound is returned for an unknown splice.
	ErrSpliceRecordNotFound = errors.New("splice record not found")
)

// SpliceRecord is a pending splice of a channel: its funding output and the
// commitments on it.
type SpliceRecord struct {
	// OutPoint is the splice's funding output.
	OutPoint wire.OutPoint

	// Capacity is its value.
	Capacity btcutil.Amount

	// LocalKey is our key in its 2-of-2 script.
	LocalKey keychain.KeyDescriptor

	// RemoteKey is the peer's key in it.
	RemoteKey *btcec.PublicKey

	// LocalDelta and RemoteDelta are the changes to each side's balance.
	LocalDelta, RemoteDelta btcutil.Amount

	// LocalCommit is our latest commitment on this funding with the
	// peer's signatures: what is published if the splice confirms and
	// the channel is closed from it.
	LocalCommit *ChannelCommitment

	// RemoteTail is the peer's current commitment on this funding, as we
	// signed it.
	RemoteTail *ChannelCommitment

	// RemoteTip is the peer's next commitment on this funding, signed
	// but not yet acknowledged, and RemoteTipSig our signatures for it,
	// sent again if the batch is retransmitted.
	RemoteTip    *ChannelCommitment
	RemoteTipSig *lnwire.CommitSig

	// SpliceSig is our commitment_signed for the splice itself, sent
	// again if the peer asks for it on reconnecting.
	SpliceSig *lnwire.CommitSig

	// Retired marks the funding a splice that locked moved the channel
	// away from, kept only until the peer's commitments on it that were
	// unrevoked at the lock are revoked and logged: a reorg can bring
	// that funding back, and a revoked commitment on it must still be
	// punished. It has no commitments of ours and is never signed for.
	Retired bool

	// Negotiation is what the splice machine keeps of the splice's
	// negotiation, nil for a record written without one.
	Negotiation *SpliceNegotiation
}

// SpliceNegotiation is what the splice machine keeps of a splice it
// negotiated, to carry on after a restart or reconnection.
type SpliceNegotiation struct {
	// WeInitiated is true for a splice this node started.
	WeInitiated bool

	// Tx is the agreed transaction, without witnesses.
	Tx *wire.MsgTx

	// FeeRate is its fee rate in sat/kw.
	FeeRate uint64

	// OurInputs are the wallet coins this node put in, leased until the
	// splice locks or dies.
	OurInputs []wire.OutPoint

	// OurTxSignatures and TheirTxSignatures are the tx_signatures each
	// side sent, once sent.
	OurTxSignatures   *lnwire.TxSignatures
	TheirTxSignatures *lnwire.TxSignatures

	// BroadcastHeight is the height at which this node first published
	// the signed transaction.
	BroadcastHeight uint32

	// LockedSent and LockedReceived record splice_locked each way.
	LockedSent, LockedReceived bool

	// LockedScid is the splice's short channel id, recorded with
	// LockedSent: a restart completes the lock without waiting for the
	// chain to tell its depth again. Zero if not known.
	LockedScid lnwire.ShortChannelID

	// AgreedAt is when the transaction was agreed, in Unix seconds: the
	// signing deadline runs from it across restarts. Zero if not known.
	AgreedAt int64

	// SignFailedAt is when we first failed to sign the transaction, the
	// peer's tx_signatures in, in Unix seconds: retried at each
	// reconnection, it is given up on a while after that.
	SignFailedAt int64

	// SignFailures counts our failures to sign it since SignFailedAt.
	SignFailures uint32

	// Inputs describe the transaction's inputs in its order, which
	// signing and checking the peer's signatures need after a restart.
	Inputs []SpliceInput

	// OursReplaced is set on the peer's replacement of a splice this node
	// started (WeInitiated is then false: the peer built it).
	OursReplaced bool

	// Dead is set once one of the transaction's inputs was spent
	// elsewhere, deep enough: it can never confirm. Its funding stays in
	// the channel's set, signed for, as the peer's does, until another
	// transaction of the splice locks or the channel closes.
	Dead bool

	// Disowned is set when the peer aborted the transaction after our
	// tx_signatures, before sending its own, and dropped it on its side.
	// It holds ours and could still publish it, so it stays in the
	// channel's set, the channel's state there kept current; but it is
	// no longer being signed, and a cooperative close, which spends the
	// funding output it spends, may end it.
	Disowned bool

	// OurOutputs are the outputs this node added: change, or the
	// destination of funds moved out.
	OurOutputs []*wire.TxOut
}

// SpliceInput is an input of a splice transaction as negotiated.
type SpliceInput struct {
	// SerialID says which side added it.
	SerialID uint64

	// Value and PkScript describe the output it spends.
	Value    btcutil.Amount
	PkScript []byte

	// Shared marks the input spending the channel's funding output.
	Shared bool
}

// SpliceNegotiation TLV types.
const (
	negWeInitiatedType     tlv.Type = 0
	negTxType              tlv.Type = 1
	negFeeRateType         tlv.Type = 2
	negOurInputsType       tlv.Type = 3
	negOurTxSigsType       tlv.Type = 4
	negTheirTxSigsType     tlv.Type = 5
	negBroadcastHeightType tlv.Type = 6
	negLockedType          tlv.Type = 7
	negInputsType          tlv.Type = 8
	negOurOutputsType      tlv.Type = 9
	negLockedScidType      tlv.Type = 11
	negAgreedAtType        tlv.Type = 13
	negSignFailedAtType    tlv.Type = 15
	negSignFailuresType    tlv.Type = 17
)

// encode serializes a negotiation as a TLV stream.
func (n *SpliceNegotiation) encode(w io.Writer) error {
	var (
		records []tlv.Record
		flags   uint8
	)
	if n.WeInitiated {
		flags |= 1
	}
	if n.OursReplaced {
		flags |= 2
	}
	if n.Dead {
		flags |= 4
	}
	if n.Disowned {
		flags |= 8
	}
	records = append(records, tlv.MakePrimitiveRecord(
		negWeInitiatedType, &flags,
	))

	if n.Tx != nil {
		var b bytes.Buffer
		if err := n.Tx.SerializeNoWitness(&b); err != nil {
			return err
		}
		tx := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(
			negTxType, &tx,
		))
	}
	feeRate := n.FeeRate
	records = append(records, tlv.MakePrimitiveRecord(
		negFeeRateType, &feeRate,
	))

	if len(n.OurInputs) > 0 {
		var b bytes.Buffer
		for i := range n.OurInputs {
			if err := graphdb.WriteOutpoint(
				&b, &n.OurInputs[i],
			); err != nil {
				return err
			}
		}
		inputs := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(
			negOurInputsType, &inputs,
		))
	}

	sigs := func(t tlv.Type, m *lnwire.TxSignatures) error {
		if m == nil {
			return nil
		}
		var b bytes.Buffer
		if err := m.Encode(&b, 0); err != nil {
			return err
		}
		blob := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(t, &blob))

		return nil
	}
	if err := sigs(negOurTxSigsType, n.OurTxSignatures); err != nil {
		return err
	}
	if err := sigs(negTheirTxSigsType, n.TheirTxSignatures); err != nil {
		return err
	}

	height := n.BroadcastHeight
	records = append(records, tlv.MakePrimitiveRecord(
		negBroadcastHeightType, &height,
	))
	var locked uint8
	if n.LockedSent {
		locked |= 1
	}
	if n.LockedReceived {
		locked |= 2
	}
	records = append(records, tlv.MakePrimitiveRecord(
		negLockedType, &locked,
	))

	if len(n.Inputs) > 0 {
		var b bytes.Buffer
		for _, in := range n.Inputs {
			shared := uint8(0)
			if in.Shared {
				shared = 1
			}
			err := WriteElements(&b, in.SerialID,
				uint64(in.Value), shared,
				uint16(len(in.PkScript)),
			)
			if err != nil {
				return err
			}
			if _, err := b.Write(in.PkScript); err != nil {
				return err
			}
		}
		inputs := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(
			negInputsType, &inputs,
		))
	}

	if len(n.OurOutputs) > 0 {
		var b bytes.Buffer
		for _, o := range n.OurOutputs {
			err := WriteElements(&b, uint64(o.Value),
				uint16(len(o.PkScript)))
			if err != nil {
				return err
			}
			if _, err := b.Write(o.PkScript); err != nil {
				return err
			}
		}
		outputs := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(
			negOurOutputsType, &outputs,
		))
	}
	if scid := n.LockedScid.ToUint64(); scid != 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			negLockedScidType, &scid,
		))
	}
	if n.AgreedAt > 0 {
		agreed := uint64(n.AgreedAt)
		records = append(records, tlv.MakePrimitiveRecord(
			negAgreedAtType, &agreed,
		))
	}
	if n.SignFailedAt > 0 {
		failed := uint64(n.SignFailedAt)
		records = append(records, tlv.MakePrimitiveRecord(
			negSignFailedAtType, &failed,
		))
	}
	if n.SignFailures > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			negSignFailuresType, &n.SignFailures,
		))
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// decodeSpliceNegotiation reads a negotiation.
func decodeSpliceNegotiation(r io.Reader) (*SpliceNegotiation, error) {
	var (
		n                  SpliceNegotiation
		flags, locked      uint8
		tx, inputs         []byte
		ourSigs, theirSigs []byte
		spliceInputs       []byte
		ourOutputs         []byte
		scid, agreed       uint64
		signFailed         uint64
	)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(negWeInitiatedType, &flags),
		tlv.MakePrimitiveRecord(negTxType, &tx),
		tlv.MakePrimitiveRecord(negFeeRateType, &n.FeeRate),
		tlv.MakePrimitiveRecord(negOurInputsType, &inputs),
		tlv.MakePrimitiveRecord(negOurTxSigsType, &ourSigs),
		tlv.MakePrimitiveRecord(negTheirTxSigsType, &theirSigs),
		tlv.MakePrimitiveRecord(
			negBroadcastHeightType, &n.BroadcastHeight,
		),
		tlv.MakePrimitiveRecord(negLockedType, &locked),
		tlv.MakePrimitiveRecord(negInputsType, &spliceInputs),
		tlv.MakePrimitiveRecord(negOurOutputsType, &ourOutputs),
		tlv.MakePrimitiveRecord(negLockedScidType, &scid),
		tlv.MakePrimitiveRecord(negAgreedAtType, &agreed),
		tlv.MakePrimitiveRecord(negSignFailedAtType, &signFailed),
		tlv.MakePrimitiveRecord(negSignFailuresType, &n.SignFailures),
	)
	if err != nil {
		return nil, err
	}
	if err := stream.Decode(r); err != nil {
		return nil, err
	}

	n.WeInitiated = flags&1 != 0
	n.OursReplaced = flags&2 != 0
	n.Dead = flags&4 != 0
	n.Disowned = flags&8 != 0
	n.LockedSent = locked&1 != 0
	n.LockedReceived = locked&2 != 0
	n.LockedScid = lnwire.NewShortChanIDFromInt(scid)
	if agreed <= math.MaxInt64 {
		n.AgreedAt = int64(agreed)
	}
	if signFailed <= math.MaxInt64 {
		n.SignFailedAt = int64(signFailed)
	}

	if tx != nil {
		n.Tx = &wire.MsgTx{}
		err := n.Tx.DeserializeNoWitness(bytes.NewReader(tx))
		if err != nil {
			return nil, err
		}
	}
	if len(inputs)%36 != 0 {
		return nil, fmt.Errorf("splice inputs of %d bytes",
			len(inputs))
	}
	for rd := bytes.NewReader(inputs); rd.Len() > 0; {
		var op wire.OutPoint
		if err := graphdb.ReadOutpoint(rd, &op); err != nil {
			return nil, err
		}
		n.OurInputs = append(n.OurInputs, op)
	}

	for rd := bytes.NewReader(spliceInputs); rd.Len() > 0; {
		var (
			in        SpliceInput
			value     uint64
			shared    uint8
			scriptLen uint16
		)
		err := ReadElements(rd, &in.SerialID, &value, &shared,
			&scriptLen)
		if err != nil {
			return nil, err
		}
		in.PkScript = make([]byte, scriptLen)
		if _, err := io.ReadFull(rd, in.PkScript); err != nil {
			return nil, err
		}
		in.Value = btcutil.Amount(value)
		in.Shared = shared == 1
		n.Inputs = append(n.Inputs, in)
	}

	sigs := func(b []byte) (*lnwire.TxSignatures, error) {
		if b == nil {
			return nil, nil
		}
		var m lnwire.TxSignatures
		if err := m.Decode(bytes.NewReader(b), 0); err != nil {
			return nil, err
		}

		return &m, nil
	}
	if n.OurTxSignatures, err = sigs(ourSigs); err != nil {
		return nil, err
	}
	if n.TheirTxSignatures, err = sigs(theirSigs); err != nil {
		return nil, err
	}

	for rd := bytes.NewReader(ourOutputs); rd.Len() > 0; {
		var (
			value     uint64
			scriptLen uint16
		)
		if err := ReadElements(rd, &value, &scriptLen); err != nil {
			return nil, err
		}
		script := make([]byte, scriptLen)
		if _, err := io.ReadFull(rd, script); err != nil {
			return nil, err
		}
		n.OurOutputs = append(n.OurOutputs, &wire.TxOut{
			Value: int64(value), PkScript: script,
		})
	}

	return &n, nil
}

// Txid returns the splice transaction's id.
func (r *SpliceRecord) Txid() chainhash.Hash {
	return r.OutPoint.Hash
}

// PeerAtRisk is what of the splice the peer could undo by double-spending
// its inputs in a reorganisation: the value of the inputs it added, or what
// it put in if more. A splice that loses its inputs never existed, so a
// peer adding a large input and taking most of it back as an output risks
// the channel as much as one putting it all in.
func (r *SpliceRecord) PeerAtRisk() btcutil.Amount {
	risk := max(r.RemoteDelta, 0)
	n := r.Negotiation
	if n == nil {
		return risk
	}
	var inputs btcutil.Amount
	for _, in := range n.Inputs {
		// The initiator's serial ids are even.
		if in.Shared || (in.SerialID%2 == 0) == n.WeInitiated {
			continue
		}
		inputs += in.Value
	}

	return max(risk, inputs)
}

// SpliceCommitWrite is what a commitment chain step writes for one pending
// splice, in the same transaction as the step.
type SpliceCommitWrite struct {
	// Txid names the splice.
	Txid chainhash.Hash

	// LocalCommit, with UpdateCommitment, is our new commitment on the
	// splice's funding with the peer's signatures.
	LocalCommit *ChannelCommitment

	// RemoteTip and RemoteTipSig, with AppendRemoteCommitChain, are the
	// peer's new commitment on the splice's funding and our signatures.
	RemoteTip    *ChannelCommitment
	RemoteTipSig *lnwire.CommitSig

	// OurOutputIndex and TheirOutputIndex, with AdvanceCommitChainTail,
	// are the output indexes of the peer's commitment on this funding
	// that is being revoked, for its revocation log entry.
	OurOutputIndex, TheirOutputIndex uint32
}

// SpliceRecord TLV types.
const (
	spliceOutPointType     tlv.Type = 0
	spliceCapacityType     tlv.Type = 1
	spliceLocalKeyType     tlv.Type = 2
	spliceRemoteKeyType    tlv.Type = 3
	spliceLocalDeltaType   tlv.Type = 4
	spliceRemoteDeltaType  tlv.Type = 5
	spliceLocalCommitType  tlv.Type = 6
	spliceRemoteTailType   tlv.Type = 7
	spliceRemoteTipType    tlv.Type = 8
	spliceRemoteTipSigType tlv.Type = 9
	spliceSpliceSigType    tlv.Type = 10
	spliceRetiredType      tlv.Type = 11
	spliceNegotiationType  tlv.Type = 12
)

// encodeSpliceRecord serializes a record as a TLV stream.
func encodeSpliceRecord(w io.Writer, r *SpliceRecord) error {
	if r.RemoteKey == nil || r.LocalKey.PubKey == nil {
		return errors.New("splice record without both keys")
	}

	var op bytes.Buffer
	if err := graphdb.WriteOutpoint(&op, &r.OutPoint); err != nil {
		return err
	}
	var localKey bytes.Buffer
	if err := WriteElements(&localKey,
		uint32(r.LocalKey.Family), r.LocalKey.Index,
		r.LocalKey.PubKey,
	); err != nil {
		return err
	}

	opBytes := op.Bytes()
	capacity := uint64(r.Capacity)
	localKeyBytes := localKey.Bytes()
	remoteKey := r.RemoteKey.SerializeCompressed()
	localDelta := uint64(r.LocalDelta)
	remoteDelta := uint64(r.RemoteDelta)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(spliceOutPointType, &opBytes),
		tlv.MakePrimitiveRecord(spliceCapacityType, &capacity),
		tlv.MakePrimitiveRecord(spliceLocalKeyType, &localKeyBytes),
		tlv.MakePrimitiveRecord(spliceRemoteKeyType, &remoteKey),
		tlv.MakePrimitiveRecord(spliceLocalDeltaType, &localDelta),
		tlv.MakePrimitiveRecord(spliceRemoteDeltaType, &remoteDelta),
	}

	commit := func(t tlv.Type, c *ChannelCommitment) error {
		if c == nil {
			return nil
		}
		var b bytes.Buffer
		if err := serializeChanCommit(&b, c); err != nil {
			return err
		}
		auxData := c.extractTlvData()
		if err := auxData.encode(&b); err != nil {
			return err
		}
		blob := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(t, &blob))

		return nil
	}
	sig := func(t tlv.Type, s *lnwire.CommitSig) error {
		if s == nil {
			return nil
		}
		var b bytes.Buffer
		if err := s.Encode(&b, 0); err != nil {
			return err
		}
		blob := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(t, &blob))

		return nil
	}
	if err := commit(spliceLocalCommitType, r.LocalCommit); err != nil {
		return err
	}
	if err := commit(spliceRemoteTailType, r.RemoteTail); err != nil {
		return err
	}
	if err := commit(spliceRemoteTipType, r.RemoteTip); err != nil {
		return err
	}
	if err := sig(spliceRemoteTipSigType, r.RemoteTipSig); err != nil {
		return err
	}
	if err := sig(spliceSpliceSigType, r.SpliceSig); err != nil {
		return err
	}
	if r.Retired {
		retired := uint8(1)
		records = append(records, tlv.MakePrimitiveRecord(
			spliceRetiredType, &retired,
		))
	}
	if r.Negotiation != nil {
		var b bytes.Buffer
		if err := r.Negotiation.encode(&b); err != nil {
			return err
		}
		blob := b.Bytes()
		records = append(records, tlv.MakePrimitiveRecord(
			spliceNegotiationType, &blob,
		))
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// decodeSpliceRecord reads a record.
func decodeSpliceRecord(rd io.Reader) (*SpliceRecord, error) {
	var (
		opBytes, localKeyBytes, remoteKey  []byte
		capacity, localDelta, remoteDelta  uint64
		localCommit, remoteTail, remoteTip []byte
		remoteTipSig, spliceSig            []byte
		retired                            uint8
		negotiation                        []byte
		r                                  SpliceRecord
	)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(spliceOutPointType, &opBytes),
		tlv.MakePrimitiveRecord(spliceCapacityType, &capacity),
		tlv.MakePrimitiveRecord(spliceLocalKeyType, &localKeyBytes),
		tlv.MakePrimitiveRecord(spliceRemoteKeyType, &remoteKey),
		tlv.MakePrimitiveRecord(spliceLocalDeltaType, &localDelta),
		tlv.MakePrimitiveRecord(spliceRemoteDeltaType, &remoteDelta),
		tlv.MakePrimitiveRecord(spliceLocalCommitType, &localCommit),
		tlv.MakePrimitiveRecord(spliceRemoteTailType, &remoteTail),
		tlv.MakePrimitiveRecord(spliceRemoteTipType, &remoteTip),
		tlv.MakePrimitiveRecord(spliceRemoteTipSigType, &remoteTipSig),
		tlv.MakePrimitiveRecord(spliceSpliceSigType, &spliceSig),
		tlv.MakePrimitiveRecord(spliceRetiredType, &retired),
		tlv.MakePrimitiveRecord(spliceNegotiationType, &negotiation),
	)
	if err != nil {
		return nil, err
	}
	parsed, err := stream.DecodeWithParsedTypes(rd)
	if err != nil {
		return nil, err
	}
	for _, t := range []tlv.Type{
		spliceOutPointType, spliceCapacityType, spliceLocalKeyType,
		spliceRemoteKeyType, spliceLocalDeltaType,
		spliceRemoteDeltaType,
	} {
		if _, ok := parsed[t]; !ok {
			return nil, fmt.Errorf("splice record without type %d",
				t)
		}
	}

	err = graphdb.ReadOutpoint(bytes.NewReader(opBytes), &r.OutPoint)
	if err != nil {
		return nil, err
	}
	r.Capacity = btcutil.Amount(capacity)
	var family uint32
	err = ReadElements(bytes.NewReader(localKeyBytes),
		&family, &r.LocalKey.Index, &r.LocalKey.PubKey,
	)
	if err != nil {
		return nil, err
	}
	r.LocalKey.Family = keychain.KeyFamily(family)
	r.RemoteKey, err = btcec.ParsePubKey(remoteKey)
	if err != nil {
		return nil, err
	}
	r.LocalDelta = btcutil.Amount(int64(localDelta))
	r.RemoteDelta = btcutil.Amount(int64(remoteDelta))
	r.Retired = retired == 1
	if negotiation != nil {
		r.Negotiation, err = decodeSpliceNegotiation(
			bytes.NewReader(negotiation),
		)
		if err != nil {
			return nil, err
		}
	}

	commit := func(b []byte) (*ChannelCommitment, error) {
		if b == nil {
			return nil, nil
		}
		rd := bytes.NewReader(b)
		c, err := deserializeChanCommit(rd)
		if err != nil {
			return nil, err
		}
		var auxData commitTlvData
		if err := auxData.decode(rd); err != nil {
			return nil, err
		}
		c.amendTlvData(auxData)

		return &c, nil
	}
	sig := func(b []byte) (*lnwire.CommitSig, error) {
		if b == nil {
			return nil, nil
		}
		var s lnwire.CommitSig
		if err := s.Decode(bytes.NewReader(b), 0); err != nil {
			return nil, err
		}

		return &s, nil
	}
	if r.LocalCommit, err = commit(localCommit); err != nil {
		return nil, err
	}
	if r.RemoteTail, err = commit(remoteTail); err != nil {
		return nil, err
	}
	if r.RemoteTip, err = commit(remoteTip); err != nil {
		return nil, err
	}
	if r.RemoteTipSig, err = sig(remoteTipSig); err != nil {
		return nil, err
	}
	if r.SpliceSig, err = sig(spliceSig); err != nil {
		return nil, err
	}

	return &r, nil
}

// putSpliceRecord writes a record into the channel's bucket.
func putSpliceRecord(chanBucket kvdb.RwBucket, r *SpliceRecord) error {
	bucket, err := chanBucket.CreateBucketIfNotExists(spliceRecordsBucket)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := encodeSpliceRecord(&b, r); err != nil {
		return err
	}
	txid := r.Txid()

	return bucket.Put(txid[:], b.Bytes())
}

// fetchSpliceRecord reads a record from the channel's bucket.
func fetchSpliceRecord(chanBucket kvdb.RBucket,
	txid chainhash.Hash) (*SpliceRecord, error) {

	bucket := chanBucket.NestedReadBucket(spliceRecordsBucket)
	if bucket == nil {
		return nil, ErrSpliceRecordNotFound
	}
	v := bucket.Get(txid[:])
	if v == nil {
		return nil, ErrSpliceRecordNotFound
	}

	return decodeSpliceRecord(bytes.NewReader(v))
}

// PutSpliceRecord stores a pending splice of the channel, replacing any
// record of the same transaction.
func (c *OpenChannel) PutSpliceRecord(r *SpliceRecord) error {
	c.Lock()
	defer c.Unlock()

	return kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}

		return putSpliceRecord(chanBucket, r)
	}, func() {})
}

// SpliceRecords returns the channel's pending splices.
func (c *OpenChannel) SpliceRecords() ([]*SpliceRecord, error) {
	c.RLock()
	defer c.RUnlock()

	// A channel held only in memory has none.
	if c.Db == nil {
		return nil, nil
	}

	var records []*SpliceRecord
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		// A channel not yet saved (during funding) has none.
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil

		case err != nil:
			return err
		}
		bucket := chanBucket.NestedReadBucket(spliceRecordsBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(_, v []byte) error {
			r, err := decodeSpliceRecord(bytes.NewReader(v))
			if err != nil {
				return err
			}
			records = append(records, r)

			return nil
		})
	}, func() {
		records = nil
	})

	return records, err
}

// DeleteSpliceRecord removes a pending splice that will never lock. Its
// revocation log stays: a revoked commitment on its funding can't confirm
// once the splice is dead, but the entries cost little and the death may be
// reorged out.
func (c *OpenChannel) DeleteSpliceRecord(txid chainhash.Hash) error {
	c.Lock()
	defer c.Unlock()

	return kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		return deleteSpliceRecord(chanBucket, txid)
	}, func() {})
}

// RetireSpliceRecord drops a pending splice that won't lock: one aborted,
// declared dead, or a replacement that lost to another transaction of the
// splice. One that could still confirm, our tx_signatures out and its
// commitments exchanged, is kept as retired, without commitments of ours,
// until the peer's commitments on it are revoked and logged, and its funding
// output is remembered: a reorganisation could still confirm it. Any other
// is deleted. It reports whether the splice was retired.
func (c *OpenChannel) RetireSpliceRecord(txid chainhash.Hash) (bool, error) {
	c.Lock()
	defer c.Unlock()

	var retired bool
	err := kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}

		retired, err = retireSpliceRecord(chanBucket, txid)

		return err
	}, func() {
		retired = false
	})

	return retired, err
}

// retireSpliceRecord is RetireSpliceRecord in a transaction.
func retireSpliceRecord(chanBucket kvdb.RwBucket,
	txid chainhash.Hash) (bool, error) {

	r, err := fetchSpliceRecord(chanBucket, txid)
	if err != nil {
		return false, err
	}
	if r.RemoteTail == nil {
		return false, deleteSpliceRecord(chanBucket, txid)
	}

	// Its commitments were exchanged: the peer may still name it in a
	// batch, signed before it learnt, retransmitted after a restart, so
	// it is remembered either way.
	dropped, err := chanBucket.CreateBucketIfNotExists(spliceDroppedBucket)
	if err != nil {
		return false, err
	}
	var b bytes.Buffer
	err = encodeSpliceRecord(&b, &SpliceRecord{
		OutPoint: r.OutPoint, Capacity: r.Capacity,
		LocalKey: r.LocalKey, RemoteKey: r.RemoteKey, Retired: true,
	})
	if err != nil {
		return false, err
	}
	if err := dropped.Put(txid[:], b.Bytes()); err != nil {
		return false, err
	}

	// Without our tx_signatures it can't confirm: nothing to log.
	unsigned := r.Negotiation != nil &&
		r.Negotiation.OurTxSignatures == nil
	if unsigned {
		return false, deleteSpliceRecord(chanBucket, txid)
	}

	r.Retired = true
	r.LocalCommit, r.SpliceSig, r.RemoteTipSig = nil, nil, nil
	r.LocalDelta, r.RemoteDelta = 0, 0
	r.Negotiation = nil

	return true, putSpliceRecord(chanBucket, r)
}

// DroppedSplices returns the txids of the splice transactions the channel
// dropped once their commitments were exchanged.
func (c *OpenChannel) DroppedSplices() ([]chainhash.Hash, error) {
	c.RLock()
	defer c.RUnlock()

	var txids []chainhash.Hash
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		dropped := chanBucket.NestedReadBucket(spliceDroppedBucket)
		if dropped == nil {
			return nil
		}

		return dropped.ForEach(func(k, _ []byte) error {
			var txid chainhash.Hash
			copy(txid[:], k)
			txids = append(txids, txid)

			return nil
		})
	}, func() {
		txids = nil
	})
	if errors.Is(err, ErrNoActiveChannels) ||
		errors.Is(err, ErrChannelNotFound) {

		return nil, nil
	}

	return txids, err
}

// DroppedSplice returns what the channel keeps of a splice transaction it
// dropped after signing it: its retired record while the peer's commitments
// on it are still to be logged, then its funding output and keys alone.
func (c *OpenChannel) DroppedSplice(txid chainhash.Hash) (*SpliceRecord,
	error) {

	c.RLock()
	defer c.RUnlock()

	var r *SpliceRecord
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		dropped := chanBucket.NestedReadBucket(spliceDroppedBucket)
		if dropped == nil {
			return ErrSpliceRecordNotFound
		}
		v := dropped.Get(txid[:])
		if v == nil {
			return ErrSpliceRecordNotFound
		}
		known, err := decodeSpliceRecord(bytes.NewReader(v))
		if err != nil {
			return err
		}
		r = known

		retired, err := fetchSpliceRecord(chanBucket, txid)
		if err == nil && retired.Retired {
			r = retired
		}

		return nil
	}, func() {
		r = nil
	})
	if err != nil {
		return nil, err
	}

	return r, nil
}

// FetchSpliceRevocationLog returns the revocation log entry of the peer's
// commitment at height on a splice's funding output.
func (c *OpenChannel) FetchSpliceRevocationLog(txid chainhash.Hash,
	height uint64) (*RevocationLog, error) {

	c.RLock()
	defer c.RUnlock()

	var rl RevocationLog
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		logs := chanBucket.NestedReadBucket(spliceRevocationLogBucket)
		if logs == nil {
			return ErrLogEntryNotFound
		}
		log := logs.NestedReadBucket(txid[:])
		if log == nil {
			return ErrLogEntryNotFound
		}
		rl, err = fetchRevocationLog(log, height)

		return err
	}, func() {
		rl = RevocationLog{}
	})
	if err != nil {
		return nil, err
	}

	return &rl, nil
}

// SpliceRevocationsAt returns the splices whose logs hold the peer's revoked
// commitment at height: the commitments on fundings other than the one in the
// channel's own log.
func (c *OpenChannel) SpliceRevocationsAt(height uint64) ([]chainhash.Hash,
	error) {

	c.RLock()
	defer c.RUnlock()

	var txids []chainhash.Hash
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		logs := chanBucket.NestedReadBucket(spliceRevocationLogBucket)
		if logs == nil {
			return nil
		}

		key := makeLogKey(height)

		return logs.ForEach(func(k, _ []byte) error {
			log := logs.NestedReadBucket(k)
			if log == nil || log.Get(key[:]) == nil {
				return nil
			}
			var txid chainhash.Hash
			copy(txid[:], k)
			txids = append(txids, txid)

			return nil
		})
	}, func() {
		txids = nil
	})

	return txids, err
}

// applySpliceWrites applies a commitment chain step's writes for the
// pending splices: our new commitments, the peer's new ones with our
// signatures, or, with revoke, the peer's revoked ones moved into their
// revocation logs.
//
// On a channel with option_independent_secrets, commitSecret is the peer's
// per-commitment secret for the commitment number secretHeight. One secret
// revokes the commitment with that number on every funding output, so it is
// stored with each splice's log entry at that number too: a breach on a
// splice's funding is looked up in the splice's log.
func applySpliceWrites(chanBucket kvdb.RwBucket, writes []SpliceCommitWrite,
	revoke bool, noAmtData bool, commitSecret fn.Option[[32]byte],
	secretHeight uint64) error {

	for _, w := range writes {
		r, err := fetchSpliceRecord(chanBucket, w.Txid)
		if err != nil {
			return fmt.Errorf("splice %v: %w", w.Txid, err)
		}

		if w.LocalCommit != nil {
			r.LocalCommit = w.LocalCommit
		}
		if w.RemoteTip != nil {
			r.RemoteTip = w.RemoteTip
			r.RemoteTipSig = w.RemoteTipSig
		}

		if revoke {
			// A retired funding revokes its last commitment
			// without a next one.
			if r.RemoteTail == nil ||
				(r.RemoteTip == nil && !r.Retired) {

				return fmt.Errorf("splice %v: nothing to "+
					"revoke", w.Txid)
			}
			logs, err := chanBucket.CreateBucketIfNotExists(
				spliceRevocationLogBucket,
			)
			if err != nil {
				return err
			}
			log, err := logs.CreateBucketIfNotExists(w.Txid[:])
			if err != nil {
				return err
			}
			secret := fn.None[[32]byte]()
			if r.RemoteTail.CommitHeight == secretHeight {
				secret = commitSecret
			}
			err = putRevocationLogWithSecret(
				log, r.RemoteTail, w.OurOutputIndex,
				w.TheirOutputIndex, noAmtData, secret,
			)
			if err != nil {
				return err
			}
			r.RemoteTail, r.RemoteTip = r.RemoteTip, nil
			r.RemoteTipSig = nil

			// Everything on a retired funding is logged.
			if r.Retired && r.RemoteTail == nil {
				err := deleteSpliceRecord(chanBucket, w.Txid)
				if err != nil {
					return err
				}

				continue
			}
		}

		if err := putSpliceRecord(chanBucket, r); err != nil {
			return err
		}
	}

	return nil
}

// deleteSpliceRecord removes a record from the channel's bucket.
func deleteSpliceRecord(chanBucket kvdb.RwBucket, txid chainhash.Hash) error {
	bucket := chanBucket.NestedReadWriteBucket(spliceRecordsBucket)
	if bucket == nil || bucket.Get(txid[:]) == nil {
		return ErrSpliceRecordNotFound
	}

	return bucket.Delete(txid[:])
}

// FindPreviousStateFor is FindPreviousState for a revoked commitment that
// spends funding: the entry of the peer's commitment on that funding at the
// height. While a splice was pending, and for the funding a lock replaced,
// those are in the splice's own log; otherwise in the channel's.
func (c *OpenChannel) FindPreviousStateFor(funding wire.OutPoint,
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	rl, err := c.FetchSpliceRevocationLog(funding.Hash, updateNum)
	switch {
	case err == nil:
		return rl, nil, nil

	case !errors.Is(err, ErrLogEntryNotFound) &&
		!errors.Is(err, ErrNoActiveChannels) &&
		!errors.Is(err, ErrChannelNotFound):

		return nil, nil, err
	}

	return c.FindPreviousState(updateNum)
}

// UpdateSpliceRecord changes a pending splice's record in one transaction.
func (c *OpenChannel) UpdateSpliceRecord(txid chainhash.Hash,
	update func(r *SpliceRecord) error) error {

	c.Lock()
	defer c.Unlock()

	return kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		r, err := fetchSpliceRecord(chanBucket, txid)
		if err != nil {
			return err
		}
		if err := update(r); err != nil {
			return err
		}
		if r.Txid() != txid {
			return errors.New("splice record's outpoint changed")
		}

		return putSpliceRecord(chanBucket, r)
	}, func() {})
}

// EnsureSpliceRecord stores a pending splice's funding unless a record of it
// exists, which must then name the same funding.
func (c *OpenChannel) EnsureSpliceRecord(r *SpliceRecord) error {
	c.Lock()
	defer c.Unlock()

	return kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}

		have, err := fetchSpliceRecord(chanBucket, r.Txid())
		switch {
		case errors.Is(err, ErrSpliceRecordNotFound):
			return putSpliceRecord(chanBucket, r)

		case err != nil:
			return err
		}

		same := have.OutPoint == r.OutPoint &&
			have.Capacity == r.Capacity &&
			have.LocalDelta == r.LocalDelta &&
			have.RemoteDelta == r.RemoteDelta &&
			have.LocalKey.KeyLocator == r.LocalKey.KeyLocator &&
			have.LocalKey.PubKey.IsEqual(r.LocalKey.PubKey) &&
			have.RemoteKey.IsEqual(r.RemoteKey)
		if !same {
			return fmt.Errorf("splice %v recorded with another "+
				"funding", r.Txid())
		}

		return nil
	}, func() {})
}

// lastSpliceKey holds the channel's most recent splice that ended.
var lastSpliceKey = []byte("last-splice")

// LastSplice is a channel's most recent splice that ended: locked, aborted
// or dead.
type LastSplice struct {
	// WeInitiated is true for a splice this node started.
	WeInitiated bool

	// LocalDelta and RemoteDelta are the changes to each side's balance.
	LocalDelta, RemoteDelta btcutil.Amount

	// Txid names its transaction, if one was agreed.
	Txid chainhash.Hash

	// Time is when it ended, in Unix seconds.
	Time int64

	// Outcome is "locked", "aborted" or "dead".
	Outcome string

	// Reason is why it didn't lock: a refusal code, or the peer's
	// printable text when ReasonFromPeer.
	Reason         string
	ReasonFromPeer bool
}

// PutLastSplice records the channel's most recent splice that ended.
func (c *OpenChannel) PutLastSplice(l *LastSplice) error {
	c.Lock()
	defer c.Unlock()

	var b bytes.Buffer
	err := WriteElements(&b, l.WeInitiated, uint64(l.LocalDelta),
		uint64(l.RemoteDelta), l.Txid, uint64(l.Time),
		[]byte(l.Outcome), []byte(l.Reason), l.ReasonFromPeer,
	)
	if err != nil {
		return err
	}

	return kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}

		return chanBucket.Put(lastSpliceKey, b.Bytes())
	}, func() {})
}

// LastSplice returns the channel's most recent splice that ended, if any.
func (c *OpenChannel) LastSplice() (*LastSplice, error) {
	c.RLock()
	defer c.RUnlock()

	if c.Db == nil {
		return nil, nil
	}

	var last *LastSplice
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil

		case err != nil:
			return err
		}

		v := chanBucket.Get(lastSpliceKey)
		if v == nil {
			return nil
		}

		var (
			l                 LastSplice
			local, remote, ts uint64
			outcome, reason   []byte
		)
		err = ReadElements(bytes.NewReader(v), &l.WeInitiated, &local,
			&remote, &l.Txid, &ts, &outcome, &reason,
			&l.ReasonFromPeer,
		)
		if err != nil {
			return err
		}
		l.LocalDelta = btcutil.Amount(int64(local))
		l.RemoteDelta = btcutil.Amount(int64(remote))
		l.Time = int64(ts)
		l.Outcome, l.Reason = string(outcome), string(reason)
		last = &l

		return nil
	}, func() {
		last = nil
	})

	return last, err
}

// spliceHoldsKey holds the peer's coins in locked splices that aren't deep
// enough yet to rule out a reorganisation.
var spliceHoldsKey = []byte("splice-holds")

// SpliceHold is the peer's coins in a locked splice, held until the splice
// is deep enough.
type SpliceHold struct {
	// Txid names the splice.
	Txid chainhash.Hash

	// Amount is what the peer put in.
	Amount btcutil.Amount

	// PkScript and HeightHint find the splice's confirmations again.
	PkScript   []byte
	HeightHint uint32
}

// SpliceHolds returns the channel's holds.
func (c *OpenChannel) SpliceHolds() ([]SpliceHold, error) {
	c.RLock()
	defer c.RUnlock()

	if c.Db == nil {
		return nil, nil
	}

	var holds []SpliceHold
	err := kvdb.View(c.Db.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		switch {
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return nil

		case err != nil:
			return err
		}

		holds, err = decodeSpliceHolds(chanBucket.Get(spliceHoldsKey))

		return err
	}, func() {
		holds = nil
	})

	return holds, err
}

// PutSpliceHold records a hold, replacing one for the same splice.
func (c *OpenChannel) PutSpliceHold(h SpliceHold) error {
	return c.updateSpliceHolds(func(holds []SpliceHold) []SpliceHold {
		kept := holds[:0]
		for _, old := range holds {
			if old.Txid != h.Txid {
				kept = append(kept, old)
			}
		}

		return append(kept, h)
	})
}

// DeleteSpliceHold drops a splice's hold.
func (c *OpenChannel) DeleteSpliceHold(txid chainhash.Hash) error {
	return c.updateSpliceHolds(func(holds []SpliceHold) []SpliceHold {
		kept := holds[:0]
		for _, h := range holds {
			if h.Txid != txid {
				kept = append(kept, h)
			}
		}

		return kept
	})
}

// updateSpliceHolds rewrites the channel's holds.
func (c *OpenChannel) updateSpliceHolds(
	change func([]SpliceHold) []SpliceHold) error {

	c.Lock()
	defer c.Unlock()

	return kvdb.Update(c.Db.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, c.IdentityPub, &c.FundingOutpoint, c.ChainHash,
		)
		if err != nil {
			return err
		}
		holds, err := decodeSpliceHolds(chanBucket.Get(spliceHoldsKey))
		if err != nil {
			return err
		}
		holds = change(holds)
		if len(holds) == 0 {
			return chanBucket.Delete(spliceHoldsKey)
		}

		var b bytes.Buffer
		if err := WriteElements(&b, uint16(len(holds))); err != nil {
			return err
		}
		for _, h := range holds {
			err := WriteElements(&b, h.Txid, uint64(h.Amount),
				h.PkScript, h.HeightHint)
			if err != nil {
				return err
			}
		}

		return chanBucket.Put(spliceHoldsKey, b.Bytes())
	}, func() {})
}

// decodeSpliceHolds decodes the channel's holds; none for nil.
func decodeSpliceHolds(v []byte) ([]SpliceHold, error) {
	if v == nil {
		return nil, nil
	}

	r := bytes.NewReader(v)
	var n uint16
	if err := ReadElements(r, &n); err != nil {
		return nil, err
	}
	holds := make([]SpliceHold, 0, n)
	for i := uint16(0); i < n; i++ {
		var (
			h      SpliceHold
			amount uint64
		)
		err := ReadElements(r, &h.Txid, &amount, &h.PkScript,
			&h.HeightHint)
		if err != nil {
			return nil, err
		}
		h.Amount = btcutil.Amount(amount)
		holds = append(holds, h)
	}

	return holds, nil
}
