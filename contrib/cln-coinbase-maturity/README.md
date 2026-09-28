# Superseded

This series was the first proposal for the Core Lightning half of the coinbase
funding output problem. It is kept for its reasoning and measurements, not as
something to apply.

What landed instead is `privkeyio/lightning#11`, merged into `blake2b-unified`
on 2026-09-25 and released in `v26.06.8-blake2b.5`. Review there replaced the
series' gate -- hold a coinbase funded channel until its funding can be spent
-- with a refusal: a channel whose funding transaction is transaction 0 of its
block is forgotten before a confirmation is counted. The wait leaked, because
channeld still sent `channel_ready` at the negotiated depth, and it left the
fundee holding a channel it could not close for the length of the wait.

Kept from this series and merged: `relay_coinbase_maturity` in chainparams,
the wallet refusing to spend a coinbase before a spend of it will relay, and
the PSBT version conversion that used to end in `abort()`.

Dropped: the gate itself (patch 0004), the developer override that only
existed to test it (0005), and the startup re-arm of the depth watch (0003),
which trusted a stored `short_channel_id` across a possible reorg.
