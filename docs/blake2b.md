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

Lightning Fork therefore keeps two identities apart:

| What | Value | Used for |
| --- | --- | --- |
| Genesis hash | unchanged | Identifying the connected node's network (the wallet backend compares block 0) |
| BOLT `chain_hash`, mainnet | `0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb`, the first BLAKE2b block | `init` networks, `open_channel`, gossip, channel backups |
| BOLT `chain_hash`, testnet4 / regtest / others | a BIP-340 tagged hash of the network's genesis (tag `Lightning Fork chain_hash`) | same; stable across testnet restarts |
| Invoice prefix, mainnet | `lnblake` | BOLT 11 invoices |
| Invoice prefix, testnet4 | `lntblake` | |
| Invoice prefix, regtest | `lnblakert` | |

Consequences:

- A Bitcoin invoice (`lnbc…`) is refused with a message naming the SHA256
  network. A Lightning Fork invoice is refused by every Bitcoin
  implementation, which is the intended failure.
- `open_channel`, `channel_announcement` and `channel_update` from a Bitcoin
  node carry the genesis hash and are ignored.
- Channel backups (`channel.backup`) written by this daemon carry the
  BLAKE2b chain hash. A backup from a Bitcoin `lnd` is refused: its channels
  were funded on the other chain with peers on the other chain, and nothing
  here could close them safely.

### The `init` networks list

BOLT 1 lets a node list the chains it serves in its `init` message. Core
Lightning sends it and drops a peer with no chain in common; `lnd` never
implemented it. Lightning Fork always sends its chain hash, and by default
disconnects a peer that does not list it, **including a peer that sends no
list at all**, because a silent peer is almost certainly a Bitcoin `lnd`
node sharing our genesis block.

`--allow-peers-without-networks` relaxes this to "disconnect only a peer that
lists other chains". Use it only on a test network, to interoperate with an
implementation that does not send the field.

## The activation-header check

Before the chain backend is used, and every five minutes afterwards, the
daemon reads the block header at the activation height from its Bitcoin node
and requires:

1. that the header is 164 bytes and carries the header-v2 bit;
2. that the block id this daemon computes for it is the id the node reports;
3. on mainnet, that the id is the pinned activation hash above.

Any failure, including an RPC error or a node that cannot serve the block,
is a refusal: the daemon does not start, or stops if the node changed chains
under it. "Cannot tell" is never "probably fine". Only ordinary chain data is
used, so any node on either chain can answer.

The outcome is written to `chain-identity.json` next to `channel.backup` in
the network's data directory (for example
`~/.lnd/data/chain/bitcoin/mainnet/chain-identity.json`), so a wrapper can
show it before the RPC server is reachable. `--bitcoin.chain-identity-file`
moves it to any absolute path, for a wrapper that should read the outcome
without being given the directory that holds the wallet and macaroons:

```json
{
  "state": "confirmed",
  "network": "mainnet",
  "chain_hash": "0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb",
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

## Replay across the two chains

Coins that existed before height 961640 exist on both chains, and a
signature made without the opt-in `SIGHASH_UNIFIED` flag is valid on both.
A channel whose funding transaction spends such coins therefore exists on
both chains too. Lightning Fork's answer is to sign every transaction it makes
alone with the opt-in flag, so its funding transactions, sweeps and wallet
sends cannot be replayed on the SHA256 chain; that work is tracked
separately from the chain-identity work in this document and is not yet
complete. Until then, prefer funding channels from coins that only exist on
this chain (coins received after the fork), and treat any channel funded from
pre-fork coins as replayable.

## Verifying the constants yourself

```sh
bitcoin-cli getblockhash 961640
# 0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb
bitcoin-cli getblockheader $(bitcoin-cli getblockhash 961640) false | wc -c
# 329: 164 bytes of header as hex, plus a newline
curl -s https://mempool.guide/api/block-height/961640
```
