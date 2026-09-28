# Superseded

This series was the gossip half of the proposal to Core Lightning. It is kept
for its reasoning, not as something to apply.

It landed upstream as `privkeyio/lightning#1`, merged into `blake2b-unified`
on 2026-09-25 and released in `v26.06.8-blake2b.5`, extended in review: the
floor now also covers `channel_update` for a pre-activation channel, stops the
seeker probing below the activation, and has a testnet4 height (150,308). The
rules are in `lightning-blake2b/bolts#1`.

The invoice prefix the series once gave the chain was withdrawn on both sides
and is not in what shipped.
