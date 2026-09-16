# Lightning on the Bitcoin BLAKE2b chain: identity constants

This is the set of values a Lightning implementation needs to run on the
Bitcoin BLAKE2b chain without ever being mistaken for, or mistaking a peer
for, a node on the Bitcoin (SHA256d) chain. Lightning Fork implements it;
it is written down here so that a second implementation can agree with it
byte for byte, and so that the values are open to change before more than
one implementation has channels on mainnet.

The chain is Bitcoin Knots' hard fork that replaced SHA256d proof of work
with BLAKE2b at block 961,640 on 2026-08-30. Everything below the fork
height, including the genesis block, is shared with Bitcoin, which is the
whole problem: BOLT 1, 2, 7 and 11 identify a chain by its genesis hash or
a prefix derived from it, and on this chain those are Bitcoin's.

## 1. `chain_hash`

BOLT 1's `networks` record, BOLT 2's `open_channel`, BOLT 7's channel
announcements and updates, and channel backups all carry a 32-byte
`chain_hash`. On this chain it is **not** the genesis hash.

| Network | `chain_hash` (hex, in the order `getblockhash` prints) | Derivation |
| --- | --- | --- |
| mainnet | `0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb` | the block id of block 961,640, the first BLAKE2b block |
| testnet4 | `572c94664c77fb4ce6a9c4ee50ed8f0eb1bd363061342194ac66fca694aa63a6` | `TaggedHash("Lightning Fork chain_hash", genesis)` |
| signet | `c283e28a744edd1bf7a47946620de35ae2e8ac84dc0a2b21e29f1ebf36ec6589` | same |
| regtest | `2594d57b43169a2856ded0623840f0863b9e967b936f6f3d7945da28d909ab1a` | same |

`TaggedHash` is BIP 340's: `SHA256(SHA256(tag) || SHA256(tag) || msg)`,
with the tag as ASCII. Byte order matters: `genesis` is the 32-byte genesis
block id in its internal (wire) order, which is the reverse of what
`getblockhash` prints, and the 32-byte digest is taken as a block id in
that same internal order, so the table above shows it reversed. On the
wire (`init`, `open_channel`, gossip, BOLT 12) every `chain_hash` is sent
in internal order, exactly like Bitcoin's genesis hash is. For regtest:
`TaggedHash("Lightning Fork chain_hash", reverse(0f9188f1...2206))` gives the
digest `1aab09d9...9425`, which is the wire value, printed as `2594d57b...ab1a`.
Mainnet uses the activation block's id
rather than a tagged hash so that the value is a fact about the chain
itself, checkable against any node with `getblockhash 961640`; the test
networks have no fixed activation block (regtest chooses its height per
run), so they derive from their genesis instead.

Two nodes with different `chain_hash` values do not share a chain and must
not open channels or relay gossip to each other. A node whose `init` lists
Bitcoin's genesis hash, or lists nothing, is a Bitcoin node.

## 2. `init` networks

Every `init` message carries the `networks` TLV (type 1) with the single
`chain_hash` above. On receiving `init`, a node disconnects a peer whose
list does not contain its `chain_hash`. A peer that sends no list at all is
also disconnected by default: LND never sent the record, so a silent peer is
almost certainly a Bitcoin LND node that shares the genesis block. An
implementation may offer a switch to tolerate silent peers for testing; it
should not be the default on mainnet.

## 3. Invoice prefix

BOLT 11's human-readable part is `ln` followed by a network prefix. On this
chain:

| Network | Prefix | Example |
| --- | --- | --- |
| mainnet | `blake` | `lnblake10n1...` |
| testnet4 | `tblake` | `lntblake...` |
| signet | `tbsblake` | `lntbsblake...` |
| regtest | `blakert` | `lnblakert...` |

An invoice with Bitcoin's prefix (`lnbc`, `lntb`, `lntbs`, `lnbcrt`) is
refused, and a Bitcoin wallet refuses these, which is intended: an invoice
is the last thing a user sees before paying, and it must not be payable on
the other chain. Fallback on-chain addresses in invoices keep Bitcoin's
address formats, since the address formats are shared.

## 4. Replay protection

The chain's `SIGHASH_UNIFIED` (hash type bit `0x20`) binds a signature to
this chain: it commits to `TaggedHash("UnifiedSighash", message)` over a
BIP 341-shaped message that covers every spent output and a script type
byte. A Lightning node opts in for every transaction it signs alone:

| Transaction | Hash type |
| --- | --- |
| on-chain wallet sends, funding inputs it contributes | `ALL \| UNIFIED` (`0x21`) |
| sweeps of its own outputs after a close, second-level HTLC transactions it broadcasts, justice transactions it broadcasts itself, anchor spends | `ALL \| UNIFIED` (`0x21`) |
| taproot key-path spends of the above | `ALL \| UNIFIED` (`0x21`), 65-byte signature; `SIGHASH_DEFAULT` cannot carry the bit |

It does **not** opt in where the peer verifies the signature under the
protocol's fixed hash types: commitment transactions (`SIGHASH_ALL`), the
HTLC signatures exchanged in `commitment_signed` (`SIGHASH_ALL`, or
`SINGLE|ANYONECANPAY` for anchor channels), cooperative closes, and justice
transactions pre-signed for a watchtower. A channel funded from coins that
existed before the fork therefore remains replayable through its commitment
transactions until a channel type requiring the opt-in on both sides
exists; implementations should warn about such channels and prefer funding
from coins received after the fork.

## 5. Feature bits

Two bits are in use on this chain, and Lightning Fork implements both at the
numbers they were deployed at rather than at the numbers this document once
proposed:

| Bit | Name | Meaning |
| --- | --- | --- |
| 68 / 69 | `option_blake2b` | This node follows the Bitcoin BLAKE2b chain. |
| 70 / 71 | `option_unified_sigs` | This node can negotiate a channel whose commitment, second-level HTLC and cooperative close signatures opt into `SIGHASH_UNIFIED`. |

Core Lightning's port of this chain assigned both and ships them. A number
already on the wire is the number, whatever it should have been, so
interoperating with it beats being right about it alone.

Where each form is set differs, and the difference is the point:

- **Odd (69, 71) in `init` and `node_announcement`.** A peer that does not
  understand an odd bit ignores it and stays connected, which is what lets a
  pre-fork channel with an unpatched counterparty still be closed
  cooperatively. An even bit there refuses more than it means to, including
  client applications that speak the wire protocol only to reach a node's RPC
  and have no reason to know what chain they are on.
- **Even (70) inside `channel_type`.** A channel type is a set of even bits by
  construction, and there the strictness is right: both sides must agree on
  the digest they sign, and a peer that does not understand the bit must not
  end up in such a channel.
- **Neither, in invoices and offers.** A payer on the SHA256d chain must be
  refused, and the invoice prefix and `chain_hash` already do that without a
  feature bit.

This is Chris Guida's asymmetry, from the migration plan, and it is a better
answer than one choice for everywhere. Peering wants to be permissive, because
during a migration you have to be able to talk to nodes that have not moved
yet. Payment and channel opening want to be strict, because the failure there
costs money rather than a reconnect.

### The numbers are the wrong ones, and this document still says so

The highest pair BOLT 9 has assigned is 66/67
(`option_onion_messages_only_channels`), so 68 and 70 are the next numbers the
spec will hand out, not spare ones. A pair in the range implementations already
treat as custom, such as **32769 / 32768**, sits clear of that and is the
conservative choice. An earlier revision of this document reserved 32769/32768
on exactly that reasoning, and before that 2100/2101, which was no better.

They have not moved because one implementation shipped first and a second one
that used different numbers would simply not interoperate. If the pair does
move, this document and both implementations move with it, and the cost of
moving rises with every channel opened under the current numbers.

An even bit in `init` also runs BOLT 9 backwards. The spec introduces a feature
as an optional odd bit and upgrades it to a compulsory even one later, "which
will be refused by outdated nodes". Starting at the compulsory end refuses
peers before there is anything to be compatible with.

And no feature bit is what keeps the two chains apart. `chain_hash` does that,
in the `init` networks list, in `open_channel` and in `channel_announcement`. A
node on the SHA256d chain cannot open a channel here or have its gossip
accepted here whatever bits it sets. What `option_blake2b` says is narrower:
that this node knows what the peer is declaring.

## 6. Block header

Not a Lightning constant, but every node on this chain has to parse it:
from block 961,640, block headers are 164 bytes (the 80-byte layout plus a
second section), the block id is a BLAKE2b digest of the header rather
than SHA256d, and the header's time field is offset. A node that reads
block headers itself (rather than through a Bitcoin node's RPC) needs the
Knots definition; Lightning Fork's is in the btcd fork's `wire` package.

## 7. What is deliberately unchanged

- Address formats (`bc1...`, `1...`, `3...`) and the derivation paths, so a
  seed restores the same wallet.
- The genesis hash as the wallet backend's notion of "which network is this
  node on": the chain-identity check (reading the header at the activation
  height) does the work the genesis hash cannot.
- The Lightning protocol messages, feature bits and channel types, apart
  from the values above.

## Status

Implemented in Lightning Fork (`github.com/paulscode/lightning-fork`) and
running on mainnet. A patch series implementing the same values in
Core Lightning, on top of `privkeyio/lightning` `blake2b-unified`, is
kept under `contrib/cln-chain-identity/` in this repository with
its proposal; with it applied, the two implementations peer, open channels
from either side, pay each other's invoices and offers, and close, in the
regtest lab. Open to change until a second implementation has mainnet
channels; changes after that would strand channels. Discussion: open an
issue on the repository above.
