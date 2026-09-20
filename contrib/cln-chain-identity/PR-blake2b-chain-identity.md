# PR for privkeyio/lightning: a Lightning identity for the BLAKE2b chain

Target: `privkeyio/lightning`, branch `blake2b-unified` (head `24d027310`).
Patch series: `0001`-`0004` in this directory (`git am *.patch`).

**This replaces a seven-commit series, which itself replaced a fifteen-commit
one.** The fifteen gave the chain a `chain_hash` of its own; that was withdrawn
because `chain_hash` is the genesis hash both chains share, and applying it now
produces a build that peers with neither chain. The seven gave the chain its
own BOLT 11 invoice prefix; that is withdrawn here, because the prefix is BOLT
11's currency field and giving the chain one of its own states that it is a
different currency, which a change of proof of work is not. Separating invoices
is `option_blake2b`'s job, as an even bit in the `9` field.

## Title

The gossip floor at the activation height

## Summary

`v26.06.7-blake2b.4` follows the chain across the activation and signs with the
unified signature hash. One thing is still missing that this build can do on
its own, and it is the gossip floor.

**The gossip floor.** A funding output confirmed below block 961,640 exists for
nodes that did not upgrade too, and its spend may happen where this node cannot
see it, so a channel announced against one would sit in the graph forever with
nothing able to remove it. The `chain_hash` check cannot do this, because both
chains carry the same value: it tells either of them from some third chain, not
one from the other. This is the rule in the BOLT 7 half of
`lightning-blake2b/bolts#1`, placed where that text places it.

## What this series does not do

- **It does not touch `chain_hash`.** That was the first series' mistake.
- **It does not add or move feature bits.** This build already sets
  `option_blake2b` and `option_unified_sigs`. The numbers are agreed to move to
  512/513 and 514/515, which is a separate change and yours to make.
- **It does not change the BOLT 11 prefix.** The second series did. An invoice
  on this chain keeps `lnbc`, and `bitcoin/test/run-chainparams-blake2b.c`
  asserts the upstream prefixes so that a future change has to be deliberate.
- **It does not separate invoices or offers at all.** See below.

## The gap this leaves, stated plainly

With the prefix unchanged and `option_blake2b` not set in any payment artifact,
**an invoice or an offer minted on this chain is indistinguishable from one
minted on the earlier rules.** The only thing preventing a cross-chain payment
is that the two graphs do not meet, which is a weaker guarantee than an
explicit refusal.

Closing it is `lightning-blake2b/bolts#3`, which puts `option_blake2b` in the
BOLT 11 `9` field and in `offer_features`, `invreq_features` and
`invoice_features`. Neither implementation does that yet. It is deliberately
not in this series, because turning on a writer before the other end knows the
bit breaks payments between the two. Measured against an lnd port that does not
know bit 512: an offer carrying it is refused outright with `unknown even
feature bit set: bit 512`, and a BOLT 11 invoice carrying it decodes without
complaint and then fails when a route is sought, with `feature vector contains
unknown required features: [512]`. The second is worth knowing because the
invoice looks fine right up until it cannot be paid.

The safe way to do it is in two steps, and both are available today:

1. **Learn the bit without emitting the even form.** Here that is
   `FEATURE_REPRESENT_AS_OPTIONAL` for `BOLT11_FEATURE`, which works because
   `feature_offered` accepts either parity. On the lnd side the two switches
   are already independent.
2. **Switch to the even form.** That is the flag day, and it needs both ends to
   have done step 1 first.

Measured, by calling `features_unsupported(..., BOLT11_FEATURE)` on this build:

```
unaware node, invoice has even 512   -> REFUSED (bit 512)
unaware node, invoice has odd  513   -> ACCEPTED
phase-1 node, invoice has even 512   -> ACCEPTED
phase-1 node, invoice has odd  513   -> ACCEPTED
```

So a phase-1 node understands an incoming even bit while emitting only the odd
one, which a node that has done nothing still accepts. That is what makes the
first step safe to take alone.

## Also in here

`listinvoices` discarded the reason a string failed to decode and answered
`Invalid invstring`. `pay.c` has carried the reason all along. It matters more
once a feature bit is what separates invoices, because the reason then reads
`9: unknown feature bit 512`, which is not something a user could infer from
looking at the string.

## Commits

1. `gossipd: ignore channel announcements from before the proof of work changed`
2. `tests: pin the activation height, and that the invoice prefix is unchanged`
3. `doc: the chain identity, with the invoice prefix withdrawn`
4. `lightningd: say why an invstring was rejected in listinvoices`

## Unit tests

`make check-units` fails on exactly one target, `fuzz-open_channel`, and it
fails the same way on `24d027310` with nothing applied, so it is not this
series. Everything else passes, including `common/test/run-bolt11`, which the
previous series had to rewrite and this one leaves alone.

`bitcoin/test/run-chainparams-blake2b` is new and passes. It walks the table of
networks, pins each one's activation height and BOLT 11 prefix, and checks that
the table covers every network the build defines, so that one added later
cannot default to a height nobody looked at.

## What is not covered

The gossip floor is off unless `blake2b_activation_height` is non-zero, and it
is non-zero on mainnet only, because the test networks have no fixed activation
to pin. So the rule itself is not exercised by any test here or by the lab:
what is tested is the constant it reads and the condition that disables it. The
comparison is three lines and sits directly under an existing height check on
the same value, which is the argument for leaving it there rather than building
a harness around it, but it is a gap and not an oversight.

## Measured

In a regtest lab, against Lightning Fork (`github.com/paulscode/lightning-fork`,
an LND port) which implements the same values: the two peer, agree
`channel_type [12,22,70]`, exchange gossip, and close both cooperatively and by
force with `0x21` in both witnesses. An HTLC held across a force close produces
an HTLC-timeout transaction carrying `0xa3` from this node and `0x21` from the
other.

The previous series reported that the two could not pay each other, each
refusing the other's invoice on the prefix. That was a consequence of that
series changing the prefix on one side only. It is not a property of this one.
