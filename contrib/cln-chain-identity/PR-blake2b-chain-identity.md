# PR for privkeyio/lightning: a Lightning identity for the BLAKE2b chain

Target: `privkeyio/lightning`, branch `blake2b-unified` (head `24d027310`).
Patch series: `0001`-`0007` in this directory (`git am *.patch`).

**This replaces an earlier fifteen-commit series.** That one gave the chain a
`chain_hash` of its own. It is withdrawn: `chain_hash` is the genesis hash both
chains share, and the chains are separated where it actually matters instead.
Applying the old series now produces a build that peers with neither chain,
which is measured rather than predicted. The rework is discussed on the PR and
the reasoning is in `doc/blake2b-chain-identity.md` section 8.

## Title

Lightning identity for the BLAKE2b chain: the invoice prefix and the gossip
floor

## Summary

`v26.06.7-blake2b.4` follows the chain across the activation and signs with the
unified signature hash. Two things are still missing before a node on this
chain can be told from a node on the chain that did not upgrade, and one of
them currently stops our two implementations paying each other at all.

**The invoice prefix.** `chain_hash` is shared, so a BOLT 11 invoice carries
nothing that says which chain it is for. The prefix is the only place that can.
This series gives the chain `lnblake`, `lntblake`, `lntbsblake` and
`lnblakert`, and keeps the old prefix per network so that `decode` still
recognises an `lnbc` string and refuses it with a reason rather than failing to
classify it.

The direction that matters most is outward. A node on the other chain predates
this chain and will never be updated, so with a shared prefix an invoice minted
here is decoded there, found well formed, and paid on the other chain. That is
an argument from compatibility with deployed software, not from which chain is
which.

**The gossip floor.** A funding output from below block 961,640 exists for
nodes that did not upgrade too, and its spend may happen where this node cannot
see it, so a channel announced against one would sit in the graph forever. The
`chain_hash` check cannot do this, because both chains carry the same value.
This is the rule in the BOLT 7 half of `lightning-blake2b/bolts#1`, placed
where that text places it.

## What this series does not do

- **It does not touch `chain_hash`.** That was the previous series' mistake.
- **It does not add feature bits.** This build already sets `option_blake2b`
  and `option_unified_sigs`.
- **It does not fix BOLT 12.** An offer names chains by `chain_hash`, so an
  offer minted on either chain reads as valid and for the reader's own chain,
  on both implementations, with no warning. That gap is real and is
  deliberately left open: minting offers that name a chain the other
  implementation does not recognise would break fetching between us, and that
  is not a thing to do unilaterally. `doc/blake2b-chain-identity.md`, which
  commit 5 adds, sets the gap out in full under "Offers: a known gap", and I
  have raised it in a comment here as the one thing I would most like a view
  on.
- **It does not restamp wallets.** The previous series did. That check is
  reached only when the wallet is stamped with block 0 and the chain's
  `chain_hash` is not, so with the two equal it cannot fire. Measured: a wallet
  synced on one chain and restarted against the other rescans it to the tip and
  says nothing.

## Measured

In a regtest lab, against Lightning Fork (`github.com/paulscode/lightning-fork`,
an LND port) which implements the same values:

- **Without this series**: the two peer, agree `channel_type [12,22,70]`,
  exchange gossip, and close both cooperatively and by force with `0x21` in
  both witnesses. They cannot pay each other. Each refuses the other's BOLT 11
  invoice on the prefix, before a route is considered:

      cln -> lnd:  Prefix blakert is not for regtest
      lnd -> cln:  invoice is for the SHA256 chain (prefix "lnbcrt500u"), not
                   the BLAKE2b chain this node follows (expected prefix
                   lnblakert)

- **With it**: the same run pays in both directions, and both closes still
  carry `0x21`.

That is the whole of what this series is for.

## Commits

1. `bitcoin: give the BLAKE2b chain its own invoice prefix`
2. `bolt11: say which chain a foreign prefix belongs to, and keep the reason`
3. `gossipd: ignore channel announcements from before the proof of work changed`
4. `tests: pin the invoice prefixes and the activation height`
5. `doc: the chain identity, rewritten for the design that replaced chain_hash`
6. `tests: skip the five that carry foreign-chain BOLT 11 fixtures`
7. `tests: the invoice prefix, in the fixtures and the assertions`

## Unit tests

`make check-units` fails on exactly one target, `fuzz-open_channel`, and it
fails the same way on `24d027310` with nothing applied, so it is not this
series. Every other target passes, including the three this series touches.

Running them is what found a bug worth recording. The commit that says which
chain a foreign prefix belongs to read `chainparams->legacy_lightning_hrp`
while decoding, and decoding does not require a configured network: the daemon
always has one, but `fuzz-bolt11` does not, so that was a null read and the
target segfaulted. Upstream never dereferences `chainparams` there. It is
guarded now, and the target passes.

That bug was in the previous series too, unnoticed, because the unit tests were
never run against it.

## The python suite

Run against the files the prefix reaches, on this series and on `24d027310`
with nothing applied, and the two failure sets now match. That suite is failing
a great deal on its own, before any of this: 85 of the tests in those files
fail on plain upstream. What matters is that the series adds nothing to that,
and it does not.

Getting there took two fixes, both found by the run and neither visible without
it. Both were in the previous series too.

**The bookkeeper could not read its own history.** It decodes the BOLT 11
strings it stored, with no chain check at all, and those were written with the
old prefix. `chainparams_by_lightning_hrp` only knew current prefixes, so the
decode failed and the migration aborted: `failed to parse bolt11 lnbcrt1...:
Prefix bcrt is the SHA256d chain's`. That is accounting data lost on upgrade
for every payment made before it. The lookup now falls back to the prefix a
network used to carry, in a second pass so that a prefix still in use always
wins: testnet3 uses `tb` today and testnet4 used to, and an invoice saying `tb`
is testnet3's.

This does not make an old invoice payable. A caller that cares which chain an
invoice is for passes `must_be_chain`, and that path compares against
`lightning_hrp` directly rather than coming through the lookup. Checked:
`lnbcrt` is still refused on the pay path, and now decodes on the read path.

**Two bookkeeper tests asserted the prefix as a literal.** `'currency': 'bcrt'`
in eighteen places across `test_migration` and `test_migration_no_bkpr`, which
the earlier series' test commit missed because it only changed the sites that
went through `chainparams`. They read the fixture now.

One test, `test_wallet.py::test_reserveinputs`, fails under `-n 4` and passes
alone on both this series and plain upstream. Flaky, not a regression.

## Still to verify

The rest of the suite, beyond the files the prefix reaches.
