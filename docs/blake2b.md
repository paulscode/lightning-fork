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
that. Two chains share one genesis hash, one network name and one address
format, so nothing a Lightning node normally checks can tell them apart.

## Chain identity

`lnd` identifies a network by its genesis hash: in the `init` handshake, in
`open_channel`, in gossip, in channel backups. On this chain that would make
a node indistinguishable from a Bitcoin node, and the failure is not an error
message but funds sent to the wrong chain.

Lightning Fork keeps `chain_hash` as the genesis hash both chains share, and
separates them in the four places it actually matters:

| What | Value | Used for |
| --- | --- | --- |
| Genesis hash and BOLT `chain_hash` | unchanged, and the same value the other chain uses | `init` networks, `open_channel`, gossip, channel backups. **Does not distinguish the two chains anywhere.** |
| `option_blake2b`, bit 68, even | set in `init` and `node_announcement` | Peering. A node that does not know the bit must hang up, per BOLT 1 |
| Gossip height floor | 961,640 | `channel_announcement` below the activation is ignored |
| `option_unified_sigs`, bit 70 | inside `channel_type` | Channels: both sides sign with `SIGHASH_UNIFIED` set |
| `option_blake2b` in the `9` field | **not implemented yet** | BOLT 11 invoices and BOLT 12 offers. Until it is, nothing in a payment artifact says which rules it belongs to |

Two designs have been withdrawn: a `chain_hash` of this chain's own, until
2026-09-17, and a BOLT 11 invoice prefix of its own, until 2026-09-19.
`docs/blake2b-chain-identity.md` section 8 explains both and what to do if you
ran either.

Consequences:

- **An invoice is not refused on its prefix any more.** This daemon used to
  mint `lnblake...` and refuse `lnbc...`; that was withdrawn, because the
  prefix is BOLT 11's currency field and a change of proof of work is not a
  change of currency. Both chains mint `lnbc...` now, so an invoice from
  either decodes on either. What is meant to separate them is
  `option_blake2b` as an even bit in the `9` field, which neither
  implementation sets yet. Until it does, the only thing preventing a
  cross-chain payment is that the two graphs do not meet, which is weaker
  than a refusal: the payment fails for want of a route rather than being
  declined. Invoices this daemon minted under the old prefix still decode, so
  that `listinvoices` keeps working across the upgrade.
- A node on the SHA256d chain never gets as far as sending `open_channel` or
  gossip: it disconnects at `init` on bit 68. Its announcements would not be
  ignored on `chain_hash` grounds if they did arrive, because it sends the
  same `chain_hash` this node does; what covers them is the height floor for
  pre-activation channels and the funding output lookup for the rest.
- BOLT 12 offers have the same gap, for the same reason: an offer names chains
  by `chain_hash`, so one minted on either chain reads as valid and for this
  chain. `option_blake2b` in `offer_features` is the fix and is not
  implemented either. See "Offers" in `docs/blake2b-chain-identity.md`.
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

Since `chain_hash` is shared, that check no longer separates the two chains;
it separates both of them from some third chain entirely. Bit 68 separates
the two, and does it from the other side, so it works whatever this node is
configured to do.

A peer that sends **no list at all** is kept. It used to be disconnected, on
the reasoning that `lnd` never sent the field so a silent peer was probably a
stock `lnd` node on the other chain. That was a heuristic standing in for a
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

Releases `0.21.3-beta-blake2b.6` through `.9` advertised a `chain_hash` of this
chain's own. Builds after 2026-09-17 advertise the genesis hash both chains
share. Two things follow, and an operator with channels needs both of them
before upgrading.

**Your channels are migrated, but back up first.** lnd keys channels by chain
hash, so a node that opened channels under the old value would otherwise fail
to start with `no chain bucket exists`. Channeldb migration 36 moves them, and
moves the resolver reports of closed channels with them; it does nothing at all
on a database that never held such a channel. It is one way: once migrated, the
older release will not find its channels either. lnd takes no backup of its own
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

Neither applies to a node with no channels, which can be upgraded whenever.

## Backends

Only `bitcoin.node=bitcoind`, pointed at a Bitcoin Knots v29.4.1 or later
node on the BLAKE2b chain, is supported. `btcd` cannot follow this chain and
`neutrino` would have to validate BLAKE2b proof of work itself against a
network with no filter servers; both are refused at configuration time.

## Configuration

| Option | Meaning |
| --- | --- |
| `bitcoin.blake2b-activation-height=N` | Required on regtest and simnet; must match the node's `-testactivationheight=blake2b@N`. Overrides the node-reported height on testnet4. Refused on mainnet. |
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
it is negotiated by default with any peer that supports it. What is left is a
channel funded from pre-fork coins on a channel type *without* the opt-in,
which is now only reachable with a peer that cannot do it. Prefer funding from
coins received after the activation.

The justice transactions handed to a watchtower are signed the legacy way,
because the tower reconstructs their witnesses without a hash type byte. That
is safe for a reason worth stating: such a transaction spends an output created
by a commitment transaction that was itself signed with the opt-in, so on the
SHA256d chain that output does not exist and there is nothing to replay
against.

A channel funded before the fork is the one thing this cannot protect: its
funding output exists on both chains, and its commitment transactions carry
the protocol's hash types. The daemon lists every such channel at startup,
at WARN, with the advice to close it and reopen with coins received after
block 961640. Restoring a stock LND seed here recovers the on-chain wallet
only; its pre-fork coins are the same keys on both chains, and spending them
here with the opt-in leaves their SHA256d twins untouched.

The opt-in is on by default and cannot be turned off on mainnet
(`--bitcoin.no-unified-sighash` is accepted on regtest, simnet and testnet4
for interoperability testing; a development build, which does not opt in,
refuses to start on mainnet). The hash type comes from the chain, not from
the request: a PSBT input that declares nothing or one of the usual
defaults is signed with the opt-in, whether the wallet funds it, signs it,
or finalizes it, with a local or a remote signer. A PSBT or funding
transaction that arrives with signatures made elsewhere without the bit,
partial or already finalized, is refused, since the whole transaction
would be replayable; `--bitcoin.allow-legacy-sighash` overrides that for an
operator who knows the coins exist on one chain only.

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

`funding/manager.go` waits the relay depth, not the consensus depth, before
marking a channel funded by a coinbase transaction as usable. Waiting the
shorter of the two would let you open a channel whose commitment and close
transactions cannot relay, and a channel you cannot close on time is worse than
one you cannot open yet. The depth comes from
`chaincfg.Params.RelayCoinbaseMaturity()` in btcd-blake2b, which returns the
ordinary 100 blocks on any network without the deployment.

### Known gap: the node's own wallet

**This node's wallet still offers freshly mined coins after 100
confirmations.** Coin selection happens inside upstream
`github.com/btcsuite/btcwallet`, which filters coinbase outputs against
`chainParams.CoinbaseMaturity`, and that field has to stay at 100 because btcd
also reads it for consensus validation. Our btcwallet fork is scoped to the
`wallet/txauthor` submodule, so the module carrying the filter is not replaced
and cannot be patched from here. Closing this means moving the fork onto the
v0.16.19 base and replacing the whole module.

Affected paths are every one that selects coins: `SendOutputs`, `CreateSimpleTx`,
`FundPsbt`, `ListUnspentWitness`, and so channel opens funded from the node
wallet.

The failure mode is a rejected broadcast, not lost funds: the transaction is
built, refused by the first node it reaches, and the operation fails. The coin
is still there and becomes spendable on schedule.

**Until this is closed, keep freshly mined coins out of this node's wallet.**
Send mining payouts to a wallet that follows the rule, and move them in once
they have 6480 confirmations.

## Verifying the constants yourself

```sh
bitcoin-cli getblockhash 961640
# 0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb
bitcoin-cli getblockheader $(bitcoin-cli getblockhash 961640) false | wc -c
# 329: 164 bytes of header as hex, plus a newline
curl -s https://mempool.guide/api/block-height/961640
```
