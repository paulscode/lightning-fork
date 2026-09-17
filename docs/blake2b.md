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
format (`bc1…`), and key derivation. Every hard problem below follows from
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
| Invoice prefix, mainnet | `lnblake` | BOLT 11 invoices |
| Invoice prefix, testnet4 | `lntblake` | |
| Invoice prefix, regtest | `lnblakert` | |

Until 2026-09-17 this daemon used a `chain_hash` of its own and that was what
kept the chains apart. `docs/blake2b-chain-identity.md` section 8 explains the
change and what to do if you implemented the old values.

Consequences:

- A Bitcoin invoice (`lnbc…`) is refused with a message naming the SHA256
  network. A Lightning Fork invoice is refused by every Bitcoin
  implementation, which is the intended failure.
- A node on the SHA256d chain never gets as far as sending `open_channel` or
  gossip: it disconnects at `init` on bit 68. Its announcements would not be
  ignored on `chain_hash` grounds if they did arrive, because it sends the
  same `chain_hash` this node does; what covers them is the height floor for
  pre-activation channels and the funding output lookup for the rest.
- BOLT 12 offers are the gap: an offer names chains by `chain_hash`, so one
  minted on either chain reads as valid and for this chain. See section 6 of
  the chain-identity document.
- Channel backups (`channel.backup`) written by this daemon carry the shared
  chain hash, and backups written before the change carry the old value; both
  are accepted, per network. A backup from a Bitcoin `lnd` carries the same
  value too, so what keeps its channels out is `option_unified_sigs` in the
  channel type rather than the hash.

### The `init` networks list

BOLT 1 lets a node list the chains it serves in its `init` message. Core
Lightning sends it and drops a peer with no chain in common; `lnd` never
implemented it. Lightning Fork always sends its chain hash, and by default
disconnects a peer that does not list it, **including a peer that sends no
list at all**.

Since `chain_hash` is shared, this no longer separates the two chains: bit 68
does that, and it does it from the other side, so it works whatever this node
is configured to do. The `networks` check now distinguishes both chains from
some third chain entirely, and the silent-peer rule is defence in depth.

`--allow-peers-without-networks` relaxes this to "disconnect only a peer that
lists other chains". Sending the TLV is optional, so the silent-peer rule has
false positives, including client applications that speak the wire protocol
only to reach a node's RPC.

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
| `allow-peers-without-networks` | Off by default; see above. |

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

It does not opt in where a peer must be able to verify the signature under
the protocol's fixed hash types: commitment transaction signatures, the HTLC
signatures exchanged with the peer, and cooperative closes. Those are
bilateral, and a channel opened with a peer that does not implement the
opt-in has to stay valid to that peer. A commitment transaction that spends
a funding output funded from pre-fork coins therefore remains replayable
until the channel type that requires the opt-in on both sides exists; until
then, prefer funding channels from coins received after the fork. The
justice transactions handed to a watchtower are signed the legacy way too,
because the tower reconstructs their witnesses without a hash type byte.

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

## Verifying the constants yourself

```sh
bitcoin-cli getblockhash 961640
# 0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb
bitcoin-cli getblockheader $(bitcoin-cli getblockhash 961640) false | wc -c
# 329: 164 bytes of header as hex, plus a newline
curl -s https://mempool.guide/api/block-height/961640
```
