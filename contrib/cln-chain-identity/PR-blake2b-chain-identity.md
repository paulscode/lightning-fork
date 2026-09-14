# PR proposal for privkeyio/lightning: a chain identity for the BLAKE2b chain

Target: `privkeyio/lightning`, on top of `v26.06.7-blake2b.3` (`a030d213c`).
Patch series: `0001`–`0005` in this directory (`git am *.patch`).
Branch in the local clone: `blake2b-chain-identity`.

## Title

Lightning identity for the Bitcoin BLAKE2b chain: chain_hash, invoice prefix, BOLT 12 chains, init networks

## Summary

`v26.06.7-blake2b.3` lets Core Lightning follow the chain across the
activation block, and its release notes say what it leaves open: a hard
fork does not change genesis, and BOLT identifies a network by its genesis
hash, so a node on the fork and a node on the old rules advertise the same
`chain_hash`, connect to each other, and merge their gossip. This series
closes that. The network called `bitcoin` (and `regtest`, `signet`,
`testnet4`) gets a `chain_hash` of its own, an invoice prefix of its own,
always names its chain in BOLT 12, and drops a peer that names no chain at
all. The values are the ones Lightning Fork (github.com/paulscode/lightning-fork,
the LND port) already runs with on mainnet, so the two implementations
agree byte for byte; they are written down in `doc/blake2b-chain-identity.md`,
which the series adds, and are open to change until a second implementation
has mainnet channels.

## Why the node needs this

With the values as released, tested in a regtest lab against Lightning Fork
(`v0.21.3-beta-blake2b.8`) on the same BLAKE2b chain:

| What | Released `v26.06.7-blake2b.3` | With this series |
| --- | --- | --- |
| `init` from Lightning Fork | dropped: "No common chain with this peer" (Lightning Fork drops it too: "peer advertises no common chain") | accepted, both directions |
| `init` from a stock Bitcoin LND (names no networks) | accepted | dropped: "Peer names no networks and this chain shares its genesis" |
| Channel from Core Lightning to Lightning Fork | never reached | opened, announced, active |
| Channel from Lightning Fork to Core Lightning | never reached | opened, announced, active |
| Gossip | never reached | each node in the other's graph; a third node learned through the first |
| BOLT 11 | `lnbcrt…` invoices, which a Bitcoin wallet would pay; Lightning Fork refuses them | `lnblakert…` both ways, paid both ways, and routed through a Lightning Fork node |
| BOLT 12 | offers without `offer_chains` read as Bitcoin offers (mainnet) | offers name the chain; paid both ways |

Nothing in the protocol changes: only the constants, and one rule for a
peer that sends no `networks` list.

## What each commit does

1. **bitcoin: give the BLAKE2b chain its own chain_hash and invoice prefix.**
   `genesis_blockhash`, which is the `chain_hash` everywhere it is used, becomes
   the id of block 961,640 on mainnet (a fact anyone can check with
   `getblockhash 961640`) and `TaggedHash("Lightning Fork chain_hash",
   genesis)` on regtest, signet and testnet4, which have no fixed activation
   block. The true genesis stays available as `block0_hash`
   (`chainparams_block0()`), since the wallet stamp and BOLT 12 need it.
   `lightning_hrp` becomes `blake`, `blakert`, `tbsblake`, `tblake`; the BOLT 11
   decoder therefore refuses `lnbc…`, and a Bitcoin wallet refuses ours.
   `when_lightning_became_cool` on mainnet is the activation height. A unit
   test pins every value in the byte order `getblockhash` prints.
2. **offers: name the chain in offers and requests; the implicit chain stays
   Bitcoin.** BOLT 12's "no `offer_chains` means Bitcoin" must keep meaning
   Bitcoin's genesis, not "whatever the network called bitcoin carries", or
   an offer minted here without chains would read as a Bitcoin offer to
   everyone else. `chainparams_bolt12_implicit()` decides: offers, invoice
   requests and invoices made here always name this chain; an offer or
   request that names none is Bitcoin's and is not answered.
3. **connectd: drop peers that name no networks on a chain that shares its
   genesis.** A peer that names a network we do not share is dropped
   already. A peer that names none is kept, which is right on Bitcoin and
   wrong here: LND never sent the field, so a silent peer is almost
   certainly a Bitcoin LND node that shares our genesis, and nothing before
   `open_channel` tells it apart. `--allow-peers-without-networks` keeps
   them, for testing. On Bitcoin (`block0_hash` equal to `chain_hash`)
   nothing changes.
4. **wallet: restamp a wallet created before the chain_hash changed.** A
   wallet stamped with this chain's block 0 (by an earlier build of this
   fork, or by official Core Lightning that followed the chain across
   activation) is the same wallet on the same chain: the stamp is rewritten
   to the `chain_hash` with a log line, instead of refusing to start.
5. **doc: the BLAKE2b chain identity.** The definition, identical to
   Lightning Fork's `docs/blake2b-chain-identity.md`, plus the CHANGELOG.

## What it deliberately does not change

- Address formats, derivation paths, `bip70_name` (`main`, so `bitcoin-cli`
  and `getblockchaininfo` work as before), ports, dust and funding limits.
- The `testnet` (testnet3) entry, which Knots no longer serves.
- Replay protection. The chain's `SIGHASH_UNIFIED` (hash type bit `0x20`)
  binds a signature to this chain; Lightning Fork opts in for every
  transaction it signs alone (wallet sends, sweeps, anchors) and not where
  the peer verifies under the protocol's fixed hash types. This series does
  not touch signing: a follow-up in `hsmd`/`libwally` is needed for the same
  policy, and until then this node's on-chain transactions remain
  replayable on Bitcoin when their inputs exist there (pre-fork coins). The
  document's section 4 states the policy.

## Migration for existing users of the fork

- The wallet restamps itself; no flag needed.
- Channels opened before this build announced themselves with Bitcoin's
  `chain_hash`. That gossip is ignored by nodes on the new identity, so such
  channels stay usable between their two peers but are not announced
  again; close and reopen them to have them announced. `gossip_store` can be
  deleted to drop the stale messages; it is rebuilt from peers.
- Peers on the old identity, or on Bitcoin, are dropped at `init` from now
  on. That is the point.
- Invoices issued before the upgrade carry `lnbcrt`/`lnbc`; they are not
  payable afterwards. Reissue.

## Testing

- `make bitcoin/test/run-chainparams-blake2b && ./bitcoin/test/run-chainparams-blake2b`
  passes; `make` passes with `--disable-rust` (Rust plugins were not built in
  the lab image; nothing in the series touches them).
- Lab: two Lightning Fork nodes and this build on a BLAKE2b regtest (Knots
  with activation at height 20). The scenario `scripts/scenario-cln-interop.sh`
  in the lightning-fork-lab repository does: peering both ways, funding,
  a channel opened from each side, gossip in both graphs (including a
  third node learned through the first), a BOLT 11 payment each way, a
  payment routed through Lightning Fork to a third node, a BOLT 12 offer
  paid each way, a cooperative close from Core Lightning and a force close
  from Lightning Fork, with both sides settling on chain.
  Result on 2026-09-14 (Lightning Fork `v0.21.3-beta-blake2b.8` plus the two
  changes below, this series on `v26.06.7-blake2b.3`):

  ```
  PASS: lf1 038905f7e53e1c6ea48c5efa429aa99a2ffeb996b955f8b8b0ac6d12f9d6c9093f, cln 025fa09dcaacf0b6aad4ede5495bb4c2025f7d42acdd2bf1257591439f1394ca04 (v26.06.7-blake2b.3-modded)
  PASS: connected from each side
  PASS: both wallets funded
  PASS: channel from cln active
  PASS: channel from lf1 active
  PASS: cln alias on lf1: cln-patched; lf1 alias on cln: lf1
  PASS: paid 100 sat to cln
  PASS: cln paid 200 sat to lf1
  PASS: routed payment complete, 51000 msat sent
  PASS: lf1 paid cln's offer, fee 0 msat
  PASS: cln paid lf1's offer
  PASS: both channels closed: cln states ["ONCHAIN","ONCHAIN"], cln funds 100295680000 msat
  CLN INTEROP PASSED
  ```
- The released build in the same lab: dropped at `init` by Lightning Fork
  and dropping it, both directions (above).
- Two things the lab found on the Lightning Fork side, fixed there and not
  part of this series: lnd drops onion messages from peers with no open
  channel (its channel-presence gate), and Core Lightning hands an onion
  message straight to any node it is connected to, channel or not, so a
  reply relayed through a Lightning Fork peer of the requester was lost
  there. Lightning Fork now has that gate off by default and builds its
  reply paths to start at itself when a request goes straight to its
  destination. Nothing in Core Lightning needed to change for it.

## Open questions for the maintainer

- Whether to keep the network name `bitcoin` (this series does, so
  `--network=bitcoin` and every script keep working) or add a `blake2b`
  network name as an alias.
- Whether dropping silent peers should be the default (this series: yes,
  matching Lightning Fork) or opt-in.
- The regtest/signet/testnet4 derivation: a tagged hash of genesis was
  chosen so the value is stable across regtest restarts; any other rule is
  fine as long as both implementations use it.
