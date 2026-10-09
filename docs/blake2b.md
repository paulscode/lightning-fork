# Lightning Fork on the Bitcoin BLAKE2b chain

Lightning Fork is a fork of `lnd` that follows the Bitcoin BLAKE2b chain: the
Bitcoin Knots v29.4.1 hard fork that replaced SHA256d proof of work with
BLAKE2b at mainnet height 961640 on 2026-08-30. This document is what a node
operator needs to know about how it differs from `lnd`, and why.

## What the chain changed, and what it did not

Four consensus changes shipped together:

- BLAKE2b proof of work, permanent.
- A 164-byte "header v2" block header (the classic 80 bytes followed by 84
  bytes of new fields), signalled by the top bit of the version word.
- An opt-in signature hash flag, `SIGHASH_UNIFIED`, permanent.
- Temporary data limits ("RDTS"): an 800,000 weight-unit block cap and
  script restrictions, expiring by median time past on 2027-09-01.

Three things deliberately did **not** change: the genesis block, the address
format (`bc1...`), and key derivation. Every hard problem below follows from
that. A node which has upgraded and one which has not share one genesis hash,
one network name and one address format, so nothing a Lightning node normally
checks can tell them apart.

## Chain identity

`lnd` identifies a network by its genesis hash: in the `init` handshake, in
`open_channel`, in gossip, in channel backups. This is Bitcoin, continuous
since 2009, with its proof of work changed and nothing else — so that hash is
the same one a node which has not upgraded carries, and on its own it would
make the two indistinguishable. The failure would not be an error message but
funds sent somewhere they cannot be settled.

So `chain_hash` stays the genesis hash both sides share, and the answer is
given in the four places it actually matters:

| What | Value | Used for |
| --- | --- | --- |
| Genesis hash and BOLT `chain_hash` | unchanged, and the same value a node which has not upgraded uses | `init` networks, `open_channel`, gossip, channel backups. **Says nothing about which rules a node follows, anywhere.** |
| `option_blake2b`, bit 512, even | set in `init` and `node_announcement` | Peering. A node that does not know the bit must hang up, per BOLT 1 |
| Gossip height floor | 961,640 (testnet4: 150,308) | `channel_announcement` below the activation is ignored; a channel below it already in the graph is treated as unannounced |
| `option_unified_sigs`, bit 514 | inside `channel_type` | Channels: both sides sign with `SIGHASH_UNIFIED` set |
| `option_blake2b` in the `9` field and the BOLT 12 vectors | bit 512, even | BOLT 11 invoices, and BOLT 12 offers, invoice requests and invoices. A payer which has not upgraded refuses them on the unknown even bit rather than attempting a payment it could not settle |
| Watchtower `Init` bit 512, even | set by every tower and client | Watchtower sessions. A tower which has not upgraded refuses on the unknown even bit, and this node refuses a tower or client that does not set 512 or 513 |

The bit numbers, the height floor and the unified signature hash are specified
in [lightning-blake2b/bolts](https://github.com/lightning-blake2b/bolts),
shared with privkeyio's Core Lightning:
[bolts#3](https://github.com/lightning-blake2b/bolts/pull/3) (512/513) and
[bolts#1](https://github.com/lightning-blake2b/bolts/pull/1) (514/515 and the
gossip rules), both merged on 2026-09-29. Earlier
builds emitted 68 and 70, which were never allocated to anything; moving to the
allocated pair is a flag day, because a build on either pair refuses one on the
other in both directions.

Two designs have been withdrawn: a `chain_hash` of its own, until
2026-09-17, and a BOLT 11 invoice prefix of its own, until 2026-09-19.
`docs/blake2b-chain-identity.md` section 8 explains both and what to do if you
ran either.

Consequences:

- **An invoice is not refused on its prefix any more.** This daemon used to
  mint `lnblake...` and refuse `lnbc...`; that was withdrawn, because the
  prefix is BOLT 11's currency field and a change of proof of work is not a
  change of currency. Both sides mint `lnbc...`, so an invoice from either
  decodes on either. What answers the question is `option_blake2b` as an even
  bit in the `9` field, which this daemon now sets and privkeyio's Core
  Lightning sets too. A payer which has not upgraded cannot read the bit and
  refuses the invoice outright, rather than failing later for want of a route.
  Invoices this daemon minted under the old prefix still decode, so that
  `listinvoices` keeps working across the upgrade.
- A node which has not upgraded never gets as far as sending `open_channel` or
  gossip: it disconnects at `init` on bit 512. A peer that stays connected
  without setting 512 or 513, such as a client that speaks the wire protocol
  only to reach a node's RPC, is kept, as BOLT 9 requires, but no channel is
  opened with it in either direction: `open_channel` from it is answered with
  `peer does not set option_blake2b`, and `openchannel` to it fails the same
  way. Its announcements would not be
  ignored on `chain_hash` grounds if they did arrive, because it sends the
  same `chain_hash` this node does; what covers them is the height floor for
  pre-activation channels and the funding output lookup for the rest.
- BOLT 12 artifacts are answered the same way. An offer names chains by
  `chain_hash`, so one written by a node which has not upgraded reads as valid
  and for this chain. `option_blake2b` is set in `offer_features`,
  `invreq_features` and `invoice_features`, and this node refuses an offer,
  request or invoice that does not carry it. See "Offers" in
  `docs/blake2b-chain-identity.md`.
- Channel backups (`channel.backup`) written by this daemon carry the shared
  chain hash, and backups written before the change carry the old value; both
  are accepted, per network. A backup from a Bitcoin `lnd` carries the same
  value too, so what keeps its channels out is `option_unified_sigs` in the
  channel type rather than the hash.

### The `init` networks list

BOLT 1 lets a node list the chains it serves in its `init` message. Core
Lightning sends it and drops a peer with no chain in common; `lnd` never
implemented it. Lightning Fork always sends its chain hash, and disconnects a
peer that lists chains without ours among them.

Since `chain_hash` is shared, that check says nothing about whether a peer has
upgraded; it only keeps out a node on some third chain entirely. Bit 512
answers it, and answers it from the other side, so it works whatever this node
is configured to do.

A peer that sends **no list at all** is kept. It used to be disconnected, on
the reasoning that `lnd` never sent the field so a silent peer was probably a
stock `lnd` node which had not upgraded. That was a heuristic standing in for a
mechanism: sending the list is optional in BOLT 1, so it also dropped client
applications that speak the wire protocol only to reach a node's RPC.
`--require-peer-networks` restores the old behaviour for an operator who wants
it. `--allow-peers-without-networks` is accepted and ignored, so a
configuration written before the change still starts.

## The activation-header check

Before the chain backend is used, and every five minutes afterwards, the
daemon reads the block header at the activation height from its Bitcoin node
and requires:

1. that the header is 164 bytes and carries the header-v2 bit;
2. that the block id this daemon computes for it is the id the node reports;
3. on mainnet, that the id is the pinned activation hash above.

Any failure, including an RPC error or a node that cannot serve the block,
is a refusal: the daemon does not start, or stops if the node changed chains
under it. A node that reports another network in `getblockchaininfo` (a
signet or regtest node behind a mainnet configuration) is refused at once
rather than waited for, since it would never reach the activation height. "Cannot tell" is never "probably fine". Only ordinary chain data is
used, so any node on either chain can answer.

The outcome is written to `chain-identity.json` next to `channel.backup` in
the network's data directory (for example
`~/.lnd/data/chain/bitcoin/mainnet/chain-identity.json`), so a wrapper can
show it before the RPC server is reachable. `--bitcoin.chain-identity-file`
moves it to any absolute path, for a wrapper that should read the outcome
without being given the directory that holds the wallet and macaroons. Once
confirmed, the file also carries `reduced_data`, the state of the chain's
temporary block-size reduction as the node reports it (`active`, `height`,
`expiry_time`), refreshed with every re-check and logged when it changes:

```json
{
  "state": "confirmed",
  "network": "mainnet",
  "chain_hash": "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
  "activation_height": 961640,
  "activation_hash": "0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb",
  "updated_at": "2026-09-12T23:00:00Z"
}
```

`state` is one of `waiting` (the node has not reached the activation height
yet; `node_headers` says where it is), `confirmed`, `refused` (`reason` says
why), or `skipped` (integration builds only).

Note that `lnd` unlocks the wallet before it builds the chain backend, so the
check runs after wallet creation or unlock.

## Upgrading past the chain_hash change

Releases `0.21.3-beta-blake2b.6` through `.9` advertised a `chain_hash` of
their own. Builds after 2026-09-17 advertise the genesis hash both sides
share. Two things follow, and an operator with channels needs both of them
before upgrading.

**Your channels are migrated, but back up first.** lnd keys channels by chain
hash, so a node that opened channels under the old value would otherwise fail
to start with `no chain bucket exists`. Channeldb migration 36 moves them, and
moves the resolver reports of closed channels with them. Migration 37 moves the
rest of what was stored under the old value: the contract resolutions and
timelocked outputs of a channel that is closing, without which a close in
progress at the upgrade would never be swept, and the chain hash each channel
in the graph records. Neither does anything on a database that never held such
state. It is one way: once migrated, the older release refuses to open the
database at all. lnd takes no backup of its own
before a migration, so copy `channel.db` out of
`data/graph/<network>/` while the node is stopped, before starting the new
build. If the migration finds anything it does not recognise it stops and
changes nothing, because the whole of it runs in one transaction.

**Upgrading is a flag day for whoever is on the other end of a channel.** While
one end has upgraded and the other has not, the two cannot peer at all: they
disagree about `chain_hash`, and the upgraded one sends an even feature bit the
other does not know. The channel stays intact and stops being usable until both
ends move, then resumes. Nothing in the migration can change this; it is what
changing a chain identifier costs. If you have channels, agree a time with your
peers rather than upgrading and hoping.

**Gossip signed before the change is kept, and signed again.** A public
channel's `channel_announcement`, and every `channel_update` made before the
upgrade, were signed over the old value. A node checks such a message under the
current chain hash and, failing that, under this network's old one and nothing
else, and takes a message that names the old value as naming this chain, so
those channels stay in its own graph and can be routed through.

It does not pass them on. Only this fork can check such a signature: every
other implementation checks it over the message as sent, and Core Lightning
answers each one with a `Bad node_signature` warning. From
`0.21.3-beta-blake2b.14` an announcement whose proof holds only under the old
value is kept out of what the node broadcasts and serves to peers, and so are
updates for such a channel: served ones, and other nodes' relayed ones. The
node's own updates for one of its channels are still broadcast, since the
peer on the other end routes by them.

Instead the channel is announced again. At startup the node finds every
channel of its own whose proof was made under the old value and signs its
half of a new one, over the message as it is sent now; when its peer, also on
`.14` or later, does the same, the new proof replaces the old and the channel
is announced to everyone. A peer on an earlier release, which still holds the
old proof as complete, answers with the whole old announcement; that fails the
check a replacement must pass, and the old proof stays until the peer
upgrades. A
node that learns a third party's channel this way replaces the old proof it
held with the new one. Channel updates need none of this: a node signs a fresh
one at least every two weeks, so any made before the change have long been
replaced.

Neither applies to a node with no channels, which can be upgraded whenever.

## Backends

Only `bitcoin.node=bitcoind`, pointed at a Bitcoin Knots v29.4.1 or later
node which has upgraded, is supported. `btcd` cannot follow the BLAKE2b proof
of work and
`neutrino` would have to validate BLAKE2b proof of work itself against a
network with no filter servers; both are refused at configuration time.

## Configuration

| Option | Meaning |
| --- | --- |
| `bitcoin.blake2b-activation-height=N` | Required on regtest and simnet; must match the node's `-testactivationheight=blake2b@N`. On testnet4 it overrides the node-reported height, and the gossip floor, which is otherwise 150,308 as the spec fixes it. Refused on mainnet. |
| `bitcoin.chain-hash-override=<hex>` | Regtest only: advertise this chain hash instead of the built-in one, for interoperability testing. |
| `require-peer-networks` | Off by default; see above. Disconnects peers that send no networks list. `allow-peers-without-networks` is accepted and ignored, so configurations written before this still start. |

Everything else is `lnd` as documented upstream. Data directories, macaroon
paths, `lncli` and the RPC surface are unchanged; `lncli getinfo` reports
`chain: bitcoin`, `network: mainnet` and a version string ending in
`-blake2b.<n>`.

## Replay protection

Coins that existed before height 961640 exist on both chains, and a
signature made the ordinary way is valid on both: a transaction spending
such coins on one chain can be copied to the other, where it spends their
twins. The BLAKE2b chain's answer is an opt-in signature hash,
`SIGHASH_UNIFIED` (hash type bit `0x20`): a signature that carries it
commits to a message no other chain computes (a BIP341-shaped message under
the tag `UnifiedSighash`, covering every spent output and a script type
byte), so it is invalid anywhere the fork is not active, and the bit itself
is committed to, so it cannot be stripped.

Lightning Fork opts in for everything it signs alone: on-chain sends from
its wallet, channel funding inputs it contributes, sweeps of its own outputs
after a close, second-level HTLC transactions it broadcasts, justice
transactions it broadcasts itself, and anchors. Those transactions cannot be
replayed on the SHA256d chain, whichever coins they spend. Taproot key-path
spends, which normally carry no hash type byte, carry `ALL|UNIFIED` (`0x21`)
and are one byte longer.

Signatures a peer verifies are a different matter, because both sides have to
compute the same digest: commitment signatures, the HTLC signatures exchanged
with the peer, and cooperative closes. Those follow the channel type. A channel
that negotiated `option_unified_sigs` signs them `0x21`, or `0xa3` for the
peer's half of a second-level HTLC on an anchor channel; a channel that did not
signs them the way BOLT 3 already says, so a peer that has never heard of the
opt-in stays able to verify.

That channel type is what closes the replay hole for bilateral signatures, and
BOLT 2 now requires it on every new channel: a node that signs under the
opt-in, which is every node on mainnet, opens no channel without it and fails
an `open_channel` whose `channel_type` lacks it or that has none. A peer that
cannot sign under it can still connect, route and keep the channels it has, but
gets no new one. It is not the only
thing standing in the way, though, for a channel this node funds. Its funding
inputs are signed with the opt-in like every other on-chain spend, so the
funding transaction cannot be replayed on the SHA256d chain and the funding
output never exists there, whichever coins paid for it. Nothing is left on that
side for a cooperative close or a revoked commitment to spend, even on a
channel type without the opt-in. The exceptions are funding transactions this
node does not sign: one signed elsewhere without the bit and let through with
`--bitcoin.allow-legacy-sighash`, see below, and a channel opened with a
`chan_point_shim`, whose funding transaction is built and broadcast entirely
outside this node and is never checked here.

Simple taproot channels, which are off by default (`--protocol.simple-taproot-chans`),
never carry `option_unified_sigs`. Their commitment and closing signatures are
MuSig2 partial signatures under `SIGHASH_DEFAULT`, which has no hash type byte
to carry the bit, and opting them in would be a change to the wire format that
nobody has specified. Under the requirement above a node that signs under the
opt-in therefore neither opens nor accepts a new one, until that change is
specified; one opened earlier keeps working, bound to this chain by its
funding transaction alone.

The justice transactions handed to a watchtower are signed the legacy way,
because the tower reconstructs their witnesses without a hash type byte. That
is safe for a reason worth stating: such a transaction spends an output created
by a commitment transaction that was itself signed with the opt-in, so on the
SHA256d chain that output does not exist and there is nothing to replay
against.

What does matter is which chain the tower watches. A tower learns it only from
the genesis hash in the watchtower `Init` message, which does not tell the two
apart, so up to `.13` a client would open sessions with a stock SHA256d tower,
and that tower would watch a chain where the breaches it guards against never
appear. From `.14` every tower and client of this fork sets an even bit 512 in
its `Init`: a stock tower or client refuses it at the handshake, and this node
refuses one that sets neither 512 nor 513. A tower running an earlier Lightning
Fork release does not set it either, so update a tower together with its
clients.

A channel funded before the fork is the one thing this cannot protect: its
funding output exists on both chains, and its commitment transactions carry
the protocol's hash types. The daemon lists every such channel at startup,
at WARN, with the advice to close it and open a new one, which this node funds
with the opt-in and so cannot be replayed, whatever coins pay for it. Restoring a stock LND seed here recovers the on-chain wallet
only; its pre-fork coins are the same keys on both chains, and spending them
here with the opt-in leaves their SHA256d twins untouched.

The opt-in is on by default and cannot be turned off on mainnet
(`--bitcoin.no-unified-sighash` is accepted on regtest, simnet and testnet4
for interoperability testing; a development build, which does not opt in,
refuses to start on mainnet). The hash type comes from the chain, not from
the request: a PSBT input that declares nothing or one of the usual
defaults is signed with the opt-in, and one that declares any other hash type
(`SIGHASH_ALL|ANYONECANPAY`, `NONE`, `SINGLE`) has the bit added, which keeps
what it commits to. That holds whether the wallet funds it, signs it, or
finalizes it, and whether the input carries a witness UTXO or only the
transaction it spends; what the wallet has signed is checked again before it
is handed back. In a PSBT workflow the party declaring a hash type is often
the counterparty, and a declaration without the bit would otherwise get it a
signature replayable on the SHA256d chain. A PSBT or funding
transaction that arrives with signatures made elsewhere without the bit,
partial or already finalized, is refused, since the whole transaction
would be replayable; `--bitcoin.allow-legacy-sighash` overrides that for an
operator who knows the coins exist on one chain only.

A node used as the remote signer for a watch-only Lightning Fork must run with
`--bitcoin.allow-legacy-sighash`. The watch-only node decides every hash type,
and asks for the legacy one on purpose in two places: the justice signatures
it hands a watchtower, and the commitment signatures of a channel opened
without `option_unified_sigs`. A signer that raised those to the unified hash
would return signatures over a digest nobody checks against, so the watch-only
node compares the hash type of every signature it gets back with the one it
asked for, and refuses a mismatch with a message that names this option.

Two things stay as they were. Bare and P2SH inputs, which the wallet never
hands out addresses for, are signed with `SIGHASH_ALL`: the legacy signer
has no way to opt in, and a legacy digest with the bit appended is what
the chain rejects. Sweeps whose descriptors were stored before this
version keep the hash type they were stored with, which the chain still
accepts; they opt in once the channel they belong to is resolved and new
descriptors are written.

## Long coinbase maturity

Knots deploys a temporary rule
([bitcoinknots/bitcoin#419](https://github.com/bitcoinknots/bitcoin/pull/419))
that raises the wait on newly mined coins from 100 blocks to 6480, roughly 45
days at a ten minute block target. The release notes say a full year is being
considered for October 2026, and after that withholding rewards entirely from
blocks not produced through the miner's own DATUM Gateway.

**Open channels are not affected.** A Lightning transaction spends the P2WSH
funding output, not a coinbase, so the rule never touches a commitment, an HTLC
or a close. There is nothing to do about existing channels and no reason to
close one over this.

### The rule has two faces

Consensus refuses a spend only inside a deployment window, and only for coins
mined at or after the start height. Relay is blunter: an upgraded node requires
the full 6480 blocks of *every* coinbase spend it will accept into its mempool,
whatever height the coin was mined at, and it keeps requiring it after the
window closes. Knots' own wallet follows relay rather than consensus.

Two consequences that surprise people:

- A coinbase mined well before the fork, buried 200 blocks deep, is a
  consensus-valid spend that no upgraded node will relay.
- The relay rule is not height-gated. `CoinbaseMaturityLong` is a static
  chainparam, so a node starts enforcing it the moment it is upgraded, not at
  the flag day.

### What this node does

A channel funded by a coinbase transaction is refused by the side that did
not fund it, as BOLT 2 requires of a node following these rules: once the
funding confirms, `funding/manager.go` forgets the channel and tells the peer,
the way it does when a funding never confirms. For the whole relay wait no
commitment transaction of such a channel could be broadcast while its HTLCs
expired, and the node not funding it has nothing in it to lose, unless the
funder pushed it an amount at opening (`push_amt`), which it gives up with the
channel. Zero-conf channels are exempt, since they already rest on trust in
the funder.

As the funder, the coinbase is this node's own, and `funding/manager.go`
waits the relay depth, not the consensus depth, before marking the channel
usable. Waiting the shorter of the two would let you open a channel whose
commitment and close transactions cannot relay, and a channel you cannot
close on time is worse than one you cannot open yet. A peer following these
rules refuses such a channel anyway, so in practice the coins are better
spent on an ordinary channel once they mature. The depth comes from
`chaincfg.Params.RelayCoinbaseMaturity()` in btcd-blake2b, which returns the
ordinary 100 blocks on any network without the deployment.

The wait is for the chain to reach a height, not for a confirmation count.
That is not a style choice: `chainntnfs` refuses any confirmation request
above `MaxNumConfs`, which is 144, so asking for 6480 fails the registration
and leaves the channel pending for good with one line in the log. Builds up
to and including `0.21.3-beta-blake2b.10` had exactly that bug; it was found
by running the rule for real rather than a scaled stand-in, which is the whole
argument for doing so.

### The node's own wallet

The wallet spends a coinbase output only once a spend of it will relay: 6480
confirmations on mainnet while the rule is deployed, not 100. Coin selection
happens inside upstream `github.com/btcsuite/btcwallet`, which filters
coinbase outputs against `chainParams.CoinbaseMaturity`. That field has to stay
at 100 for anything validating blocks, so lnd hands btcwallet a copy of the
chain parameters with the relay depth in it, and changes nothing else.

Every path that selects coins follows: `SendOutputs`, `CreateSimpleTx`,
`FundPsbt`, `ListUnspentWitness`, and so channel opens funded from the node
wallet. Until then the coin is in none of the wallet's spendable balances,
the same as a coinbase younger than 100 blocks anywhere else;
`lncli walletbalance` reports it as `immature_coinbase_balance`, and it becomes
spendable at the relay depth, when it counts in the wallet's balances again.

Releases up to `0.21.3-beta-blake2b.12` offered such a coin after 100
confirmations. A send or channel open built from it was refused by the first
node it reached; nothing was lost, the operation failed.

## Verifying the constants yourself

```sh
bitcoin-cli getblockhash 961640
# 0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb
bitcoin-cli getblockheader $(bitcoin-cli getblockhash 961640) false | wc -c
# 329: 164 bytes of header as hex, plus a newline
curl -s https://mempool.guide/api/block-height/961640
```
