# PR proposal for privkeyio/lightning: a chain identity for the BLAKE2b chain

Target: `privkeyio/lightning`, branch `blake2b-unified` (head `24d027310`),
which is where `v26.06.7-blake2b.4` and the unified-sigs work live. Retargeted
from `v26.06.7-blake2b`, which is still at `893f767e8` and is not where the
next release will come from. The series rebases onto `blake2b-unified` with no
conflicts.
Patch series: `0001`-`0005` in this directory (`git am *.patch`); the
lightning-fork-lab repository builds and tests it (`make cln`,
`make cln-interop`).

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
on the same BLAKE2b chain (`v0.21.3-beta-blake2b.8` when the released build
was tried, `.9` for the series; the `init` check did not change between
them):

| What | Released `v26.06.7-blake2b.3` | With this series |
| --- | --- | --- |
| `init` from Lightning Fork | dropped: "No common chain with this peer" (Lightning Fork drops it too: "peer advertises no common chain") | accepted, both directions |
| `init` from a stock Bitcoin LND (names no networks) | accepted | dropped: "Peer names no networks and this chain shares its genesis" |
| Channel from Core Lightning to Lightning Fork | never reached | opened, announced, active |
| Channel from Lightning Fork to Core Lightning | never reached | opened, announced, active |
| Gossip | never reached | each node in the other's graph; a third node learned through the first |
| BOLT 11 | `lnbcrt...` invoices, which a Bitcoin wallet accepts as its own; Lightning Fork refuses them | `lnblakert...` both ways, paid both ways, and routed through a Lightning Fork node |
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
   decoder therefore refuses `lnbc...` ("Prefix bc is not for bitcoin
   (expected blake)"), and a Bitcoin wallet refuses ours. `decode` surfaces the
   decoder's reason, wrapped in the standard `command_fail_badparam`
   envelope (`string: <reason>: invalid token '<invoice>'`), so the user is
   told which chain the prefix belongs to rather than getting a generic
   parse failure. The prefix a network had before is kept as
   `legacy_lightning_hrp`, so a bookkeeper database from before the
   currency-less accounting migration keeps its history. `when_lightning_became_cool`
   on mainnet is the activation height. A unit test pins every value in the
   byte order `getblockhash` prints; the BOLT 11 unit test decodes the spec's
   `lnbc` vectors against a copy of the entry that keeps `bc`, and round-trips
   an `lnblake` invoice; the pytest fixture's regtest `chain_hash` is updated.
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
   wrong here: lnd has never sent the field (through 0.21), so a silent peer
   is almost certainly a Bitcoin lnd node that shares our genesis, and
   nothing before `open_channel` tells it apart. `--allow-peers-without-networks` keeps
   them, for testing. On Bitcoin (`block0_hash` equal to `chain_hash`)
   nothing changes.
4. **wallet: restamp a wallet created before the chain_hash changed.** A
   wallet stamped with this chain's block 0 (by an earlier build of this
   fork, or by official Core Lightning that followed the chain across
   activation) is the same wallet on the same chain. Since the same stamp
   is what a Bitcoin wallet carries, the restamp is a one-way door behind
   `--database-upgrade=true`, the flag this fork's non-final versions
   already require for a database upgrade: without it the node logs what
   it found and refuses to start. With it, the stamp is rewritten and the
   announcement signatures peers gave for existing channels are cleared,
   since they were over the old `chain_hash`, and the gossip store is
   removed, since it holds the old announcement and gossipd keeps the first
   announcement it has for a channel; on the next reestablish each channel
   exchanges fresh signatures and announces itself again under the new
   identity, without being closed, and the store is rebuilt from peers.
5. **doc: the BLAKE2b chain identity.** The definition, identical to
   Lightning Fork's `docs/blake2b-chain-identity.md`, plus the CHANGELOG.

## Added in response to review

6. **wallet: gate the restamp on the wallet's own history, not on a flag.**
   `--database-upgrade=true` cannot carry this decision, because at least one
   distribution passes it unconditionally. On mainnet the `chain_hash` came
   from the activation block, so a wallet that followed the fork has that
   block at that height and one from the SHA256d chain has a different one.
   Matched, restamp; different, refuse and no flag overrides; absent, fall
   back to a flag.

7. **connectd: keep peers that name no networks by default.** Dropping them
   dropped every `cln-application` dashboard. The `init` drop was defence in
   depth rather than the isolation, since `open_channel` and
   `channel_announcement` both carry `chain_hash`, so it is now opt-in and
   renamed `--drop-peers-without-networks` for the action it takes.

8. **doc: reserve an odd, high feature bit.** 32769 odd in `init` and
   `node_announcement`, 32768 even in invoices and offers, adopting Chris
   Guida's asymmetry and range and conceding the 2100/2101 this document
   reserved before.

9. **wallet: a dedicated flag for the case the wallet cannot answer.**
   `--restamp-wallet-for-this-chain`, because a wallet created after the fork
   has no record of the activation block and the generic upgrade flag is set
   unconditionally by some builds.

10. **tests: skip the five that carry foreign-chain BOLT 11 fixtures.** Their
    fixtures are signed invoices for other chains, either the spec's mainnet
    vectors or regtest invoices hand-made for cases a node cannot generate on
    request. Changing a prefix invalidates the signature and the signing keys
    are not available. The reason is attached to each skip, and the spec
    vectors keep their coverage in `common/test/run-bolt11`, which checks them
    against a chainparams entry that keeps `bc`.

11. **tests: name the chain in canned dbs, fix invoice-prefix assertions.** The
    fixture's `bip173_prefix` is the address prefix, not the invoice prefix,
    and on this chain they differ: addresses stay `bcrt1...` and invoices are
    `lnblakert`. About a dozen tests were using one for the other. The fixture
    gains `lightning_hrp`, and the tests that genuinely check an address keep
    `bip173_prefix`.

12. **wallet: a successful restamp is unusual, not broken.** `log_broken`
    prints `**BROKEN**`, which alarms an operator who did as instructed and
    which this project's own test framework treats as a failed run.

13. **tests: canned dbs set at runtime need the flag too.** The two remaining
    call sites that build their options later rather than from the fixture.

14. **bolt11: say which chain a foreign prefix belongs to, and keep the
    reason.** `Unknown chain bc` is true and unhelpful; the old prefix is
    already recorded as `legacy_lightning_hrp`, so it is named. And
    `listinvoices` was discarding the decoder's reason while `pay.c` had
    always kept it.

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

- Start once with `--database-upgrade=true`: the wallet is restamped with the
  chain's `chain_hash`, one way. Without the flag the node logs what it found
  and stops, so nobody upgrades a wallet by accident, least of all a Bitcoin
  one that was pointed at this build by mistake.
- Channels opened before this build announced themselves with Bitcoin's
  `chain_hash`. The restamp clears their remote announcement signatures and
  removes the gossip store, so after both peers upgrade and reconnect each
  channel exchanges fresh signatures and is announced again under the new
  identity by itself; nothing needs closing, and the store is rebuilt from
  peers. This was tested (below). Channels funded before the activation
  block stay unannounced, since gossipd ignores channels older than
  `when_lightning_became_cool` and such a channel exists on Bitcoin too.
- Peers on the old identity, or on Bitcoin, are dropped at `init` from now
  on, which is what the change is for.
- Invoices issued before the upgrade carry `lnbcrt`/`lnbc`; they are not
  payable afterwards. Reissue.

## Testing

- `make` passes with `--disable-rust`; the Rust, protobuf and Python gRPC
  stubs for the new options are regenerated by msggen in the series, though
  not compiled here (no Rust toolchain in the lab build). Unit
  tests run: `bitcoin/test/run-chainparams-blake2b` (new),
  `common/test/run-bolt11`, `plugins/test/run-decode_guess_type`,
  `common/test/run-bolt12-encode-test`, `wallet/test/run-wallet`.
- **The python suite is now run**, which it was not when this was first
  opened, and running it is what produced commits 10 to 14. On
  `blake2b-unified`, `tests/test_db.py` and `tests/test_invoices.py` give
  **21 failures with this series applied and the same 21 without it**,
  identical by name. I built the branch unpatched to be able to say that
  rather than assert it; the list is in the lab repository. Those 21 are
  pre-existing on that branch. Five tests are skipped with the reason
  attached, since their fixtures are signed BOLT 11 invoices for other chains
  and cannot be re-signed.
- Lab: two Lightning Fork nodes and this build on a BLAKE2b regtest (Knots
  with activation at height 20). The scenario `scripts/scenario-cln-interop.sh`
  in the lightning-fork-lab repository does: peering both ways, funding,
  a channel opened from each side, gossip in both graphs (including a
  third node learned through the first), a BOLT 11 payment each way, a
  payment routed through Lightning Fork to a third node, a BOLT 12 offer
  paid each way, a cooperative close from Core Lightning and a force close
  from Lightning Fork, with both sides settling on chain. (lf1 keeps the
  closes of earlier runs in its list, hence more than two close types.)
  Result on 2026-09-14 (Lightning Fork `v0.21.3-beta-blake2b.9`, this series on
  `v26.06.7-blake2b` at `893f767e8`, built by the lab's Dockerfile from these
  patches):

  ```
  PASS: lf1 03f8f2c7d8309dbc241eb3707061c9da769e2f4a896350394eaeff934a168b4237, cln 02bb6c805628485e517e4d94d7cf2406cfef2005b0ad1657200a0de4325409b3f8 (v26.06.7-blake2b.3-5-g639e8bb, chain identity applied)
  PASS: connected from each side (cln's connection is outbound)
  PASS: lnd-sha dropped at init
  PASS: both wallets funded (lf1 from 750e46a6f63bdc6ac43d6b7be524cab0c3f743d3dd5aa6853bb996a74bf5955f:0)
  PASS: channel from cln active
  PASS: channel from lf1 active
  PASS: cln alias on lf1: cln; lf1 alias on cln: lf1
  PASS: paid 100 sat to cln
  PASS: cln paid 200 sat to lf1
  PASS: routed payment complete, 51000 msat sent
  PASS: lf1 paid cln's offer, fee 0 msat
  PASS: cln paid lf1's offer
  PASS: both channels closed: cln states ["ONCHAIN","ONCHAIN"], lf1 close types ["COOPERATIVE_CLOSE","COOPERATIVE_CLOSE","LOCAL_FORCE_CLOSE","COOPERATIVE_CLOSE","LOCAL_FORCE_CLOSE"], cln funds 100594896000 msat
  CLN INTEROP PASSED
  ```
- The released build in the same lab: dropped at `init` by Lightning Fork
  and dropping it, both directions (above).
- The upgrade path, in the same lab: two nodes on the released build opened
  an announced channel under Bitcoin's identity; the patched build on the
  same data refused to start without `--database-upgrade=true` and said
  why; with the flag both restamped, removed their gossip stores, exchanged
  announcement signatures again on reestablish and announced the channel
  anew; a fresh node on the patched build learned it from them, which it
  could only do under the new `chain_hash`, and so did Lightning Fork.
  Without the store removal the fresh node learned nothing: the old
  announcement stayed in the store and was the one peers were offered,
  which is why the restamp removes it. Result:

  ```
  PASS: released build up: v26.06.7-blake2b.3, invoices lnbcrt
  PASS: channel 780x1x0 announced under the old identity
  PASS: refused with the message, and stopped
  PASS: both restamped, gossip stores removed, invoices lnblakert
  PASS: fresh patched node sees 780x1x0 under the new identity
  PASS: lf1 sees channel 780x1x0 between mig1 and mig2 after the migration
  MIGRATION PASSED
  ```
- Cooperative closes need the two nodes' fee estimates to overlap. lnd
  sends `closing_signed` without a fee range, and Core Lightning then only
  accepts an offer inside its own range; on the regtest lab lnd's fallback
  of 25 sat/vB against Core Lightning's 1 sat/vB ended every cooperative
  close unilateral after the `close` timeout, until lnd was given a fee
  estimate (`fee.url`); with one, the transcript above records the close as
  mutual on Core Lightning's side and cooperative on Lightning Fork's. On
  mainnet both nodes estimate from a live mempool, so the ranges should
  overlap; that is not something the lab can show.
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
- Testnet3: Lightning Fork gives it a tagged-hash `chain_hash` and the
  `tblake` prefix too; this series leaves Core Lightning's `testnet` entry
  as Bitcoin's, since Knots no longer serves that network. Either both
  should carry it or neither.
- The bookkeeper's CSV exports name the asset `btc` for the `bc` prefix
  (`plugins/bkpr/incomestmt.c`); what to call this chain's coin there
  (`btcb2`?) is the maintainer's choice and is not changed here.
- Replay protection (`SIGHASH_UNIFIED` for transactions this node signs
  alone) is the larger follow-up; see "What it deliberately does not
  change".
