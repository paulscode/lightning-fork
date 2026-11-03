# Running a bridge

A bridge lets people whose Lightning Fork nodes are on the BLAKE2b chain pay
Lightning invoices on the SHA256 chain, and the other way round, without giving
anyone custody of their money. The bridge issues a hold invoice on the payer's
chain with the same payment hash as the invoice they want paid. It can claim
that hold invoice only with the preimage, and it gets the preimage only by
paying the invoice. If it cannot pay, it cancels the hold invoice and the payer
gets their funds back.

This is for an operator who wants to offer that service to people they know.
It commits your own liquidity on both chains, so it is off unless you turn it
on.

## What you need

- This node, with channels on the BLAKE2b chain that payers can reach you
  through. Payers who open a channel to you have the shortest route.
- A full node backend for this node: bitcoind (Bitcoin Knots on the BLAKE2b
  chain) is recommended. On a neutrino light client the bridge runs only with
  `neutrino.connect` set to your own Knots node: a light client checks no
  transactions or scripts and believes the peers it talks to, and without
  `neutrino.connect` those include peers it finds itself or is given with
  `neutrino.addpeer`. The bridge refuses to start otherwise.
- A Lightning node on the SHA256 chain, with outbound capacity on its
  channels. Either:
  - **one Lightning Fork runs for you** (`bridgerpc.sha256.supervised`): a
    stock lnd the platform starts, whose seed is derived from this node's,
    so there is nothing new to write down. It reads the full node on the
    SHA256 chain you already run and starts empty. See
    [bridge-sha256-node.md](bridge-sha256-node.md); or
  - **one you already run**: a stock lnd (v0.21.3-beta or later) with the
    `chainrpc` sub-server (release builds have it), a macaroon baked for the
    bridge (below) and its TLS certificate.

## Turning it on

With a node Lightning Fork runs for you:

```
bridgerpc.enabled=true
bridgerpc.tosha256=true
bridgerpc.sha256.supervised=true
```

The packages do this from their Bridge setting, and run the node. By hand,
start a stock lnd once
`<networkdir>/bridge/sha256/wallet.password` exists, with
`--lnddir=<lnddir>/sha256-node --wallet-unlock-password-file=<that file>
--wallet-unlock-allow-create`, its gRPC on `127.0.0.1:10019` (or name it with
`bridgerpc.sha256.rpchost`), against your full node on the SHA256 chain.

With an LND you already run, bake the bridge a macaroon on that node rather
than giving it the admin one:

```
lncli bakemacaroon --save_to=/path/to/sha256-node/bridge.macaroon \
    info:read onchain:read invoices:read invoices:write offchain:read offchain:write
```

These cover every call the bridge makes there. `offchain:write` is what paying
needs, and it can still spend that node's channel funds, so keep the file as
carefully as an admin macaroon. Then:

```
bridgerpc.enabled=true
bridgerpc.tosha256=true
bridgerpc.sha256.rpchost=10.0.0.5:10009
bridgerpc.sha256.tlscertpath=/path/to/sha256-node/tls.cert
bridgerpc.sha256.macaroonpath=/path/to/sha256-node/bridge.macaroon
```

Either way the bridge trades at the market's rate, read live (below), and
charges 1.5% on top of it each way.

`tosha256` serves payers on this chain paying SHA256 invoices; `toblake2b` the
reverse, which starts once there is a rate. The node refuses to start the bridge with a combination of
settings that would refuse every swap, and says which numbers to change.

Leave `gc-canceled-invoices-on-the-fly` and `gc-canceled-invoices-on-startup`
off, on this node and on the SHA256 node. They are off by default, but some
packages offer them as settings (StartOS's LND package does). They delete
cancelled invoices, the bridge's cancelled hold invoices among them, and a
quote reads those as well as the bridge's journal, so that a payment hash a
payer already paid into is not quoted again if the journal has lost its
record. The bridge runs with them on, and this node logs a warning at startup
when either is on here. A swap whose hold invoice is deleted this way still
ends without paying out (an unpaid one as lapsed, one paid into as given
back). One the bridge had not cancelled itself is logged as a warning, since
a node other than the one that issued the invoice gives the same answer, and
that node keeps the payer's payment until it expires. A swap that had already
paid out does not end; it is listed in `needs_operator`, since the deleted
invoice was its only claim.

`lncli bridge status` says whether the bridge can serve swaps right now and,
if not, why, and how far the SHA256 node has got (`sha256_node`). If the SHA256
node cannot be reached, or turns out not to be a stock lnd on the SHA256 chain
on this node's network, the bridge stays down and tries again every minute
(every ten seconds while a SHA256 node Lightning Fork runs is coming up)
while this node runs as usual. `needs_operator`
lists swaps that need you: one that ended lost (paid out, and the payment
coming in could not be claimed), one the bridge stopped driving because it
must not decide it alone or could not go on with its record, or one it raised
while still driving it (a paying
node that contradicted itself ten times in a row), which stays listed until
its driving ends. Watch that list. It is filled while the bridge is
on, and while it is off but still finishing swaps. A swap stopped while paying
means the paying node has no record of a payment the bridge started, which
almost always means it never left. Once you have ruled out the two cases where
it did (that node's database restored from a backup, or payments deleted by
hand), `lncli bridge resolve <payment_hash>` asks the node again: no record or
failed refunds the payer, succeeded settles their payment, and still in
flight is refused. A swap stopped while paid means the payout was made and
the node that took the payer's payment has no record of the hold invoice that
claims it; once you have ruled out the bridge reading another node and a
restored database, the same command asks that node again: still no record
and the swap is recorded as lost, there after all and the bridge settles it.
It is only for a swap listed as stopped while paying or paid (one still being
driven is refused, and one stopped in another state on its record is driven
again when lnd restarts, once the cause given is fixed),
and the bridge must still point at the SHA256 node it used for that swap:
another one has no record of the payment, which reads as never sent. It needs
the admin macaroon. `unfinished` counts the
swaps the journal has not finished (a lost one is final and not counted),
whatever the bridge's state: wait for it to be 0 before turning the bridge
off or moving it to another SHA256 node.

## The rate

The rate is SHA256 coin per BLAKE2b coin. By default
(`bridgerpc.ratesource=neoxa`) the bridge reads the market every 30 seconds,
through Tor when this node uses Tor:

- **The price is read from order books at the largest swap**
  (`bridgerpc.maxswapmsat`): what selling that much BLAKE2b coin into the bids
  fetches, for swaps paying out SHA256 coin, and what buying it from the asks
  costs, for swaps paying out BLAKE2b coin. A small order placed at the top of
  a book and cancelled again moves this only by what it would fill. Each
  direction trades at the median of the last few readings, or at the newest
  one when that is worse for the bridge, so a quote made just after a sharp
  move is already at the new price. Books are compared
  with each other (below) at the smallest swap (`bridgerpc.minswapmsat`)
  instead: two honest books of different depth read apart at a large size.
- **The cross-check** is Neoxa's BTCB2_USDC book over a BTC/USD price. If the
  middles of the two Neoxa books are more than 5% apart, or the cross-check
  cannot be read, nothing is quoted.
- **A second venue** is NonKYC's BTCB2_USDT book over the same BTC/USD price.
  Each direction trades at whichever venue is worse for its payer, so pushing
  BLAKE2b coin up or down on one exchange gains nothing against the bridge. If
  the two are more than `bridgerpc.maxvenuegap` apart (4% by default) on the
  side a direction trades against (read at the smallest swap), that
  direction quotes nothing until they come back together; the other one goes
  on. If NonKYC cannot be read (it refuses many Tor exits), the bridge prices
  on Neoxa alone and says so in `rate_notes`.
- **BTC/USD** is CoinGecko's, asked at most every five minutes, or
  mempool.space's when CoinGecko does not answer. Only when neither has
  answered for fifteen minutes is Neoxa's own BTC_USDC market used, with the
  fee widened by 0.5%.
- **No market, no quotes.** With no reading in the last 90 seconds, or with
  Neoxa's ticker and book both unchanged for five minutes, nothing is quoted. A move of 10% within
  ten minutes (the most the fee widens for) stops quoting for half an hour.
- **What a reading cannot vouch for widens the fee rather than stopping the
  bridge.** A book that holds less than the largest swap is priced at what it
  holds, with the fee widened by up to 5% for the rest. A venue whose book
  carries no time of its own, or has not changed at all in ten minutes while
  the other venue moved, or Neoxa's ticker stopped while its book goes on
  changing, widens the fee by 0.5%.
- **The day's median.** The bridge keeps the median of the last 24 hours
  (started from Neoxa's hourly candles, so a restarted node has it at once).
  When the price is more than `bridgerpc.drift.band` (10%) from it, the
  direction that buys BLAKE2b coin at that price (toSHA256 above the median,
  toBLAKE2b below it) has its fee widened by `bridgerpc.drift.widen` (a
  quarter) of the distance past the band, and past `bridgerpc.drift.max`
  (35%) that direction quotes nothing.
- **Your fee is widened by the market's own movement** over the last ten
  minutes, as payers' wallets allow for it.
- **A quote's rate holds for one minute,** or until 10 seconds before the
  invoice being paid expires, if that is sooner. Payers' wallets fund a quote
  within seconds of asking for it.
- **An invoice needs 20 seconds left to be quoted**
  (`bridgerpc.minremaininglife`, which can be raised but not lowered: a quote
  needs 10 seconds to be paid in), so a point-of-sale invoice that lives a
  minute can be paid; one with less is refused as `expires_soon`, and the
  payer asks for a new one. The bridge pays an invoice while it has 5 seconds
  left, and gives up on a payout that has not arrived a few seconds after the
  invoice expires: the payment fails and the payer's funds come back. An
  invoice's expiry decides only whether the payout arrives in time, never who
  keeps the money, since the payer's payment is claimed only with the proof
  the payout returns.
- **Just before paying,** the bridge checks the swap again at the rate then,
  taken as for a quote. If
  what the payer sent no longer covers the payout and its routing budget, or
  the market moved against the bridge by more than half the quote's fee since
  it was quoted, the payment is given back instead of paid out.

`lncli bridge status` shows the rate (the middle of Neoxa's book), the
cross-check (`rate_cross_check`), the market's movement (`rate_volatility`),
what widened the fee or is missing from the reading (`rate_notes`) and, when a
direction or the whole bridge quotes nothing, why.

On test networks, which have no market, `bridgerpc.ratesource=fixed` trades at
a rate you set instead, with `bridgerpc.fixedrate` or

```
lncli bridge setrate 1.0
```

which takes effect at once, survives restarts, and stops being used after
`bridgerpc.ratemaxage` (one hour by default). It needs the admin macaroon. On
mainnet the node refuses `ratesource=fixed` at startup: a posted rate that has
drifted past the fee is drained by whoever notices, and the bridge cannot
tell, since the rate it checks against is that same number.

Payers' wallets check every quote against the market, the worse of Neoxa and
NonKYC for the payer, and by default refuse one more than 5% above it
(widened by the last hour's range, half the book's spread, and what the
quote's spread is above 1.5%, at most 5%), so a rate set far from the market
is refused rather than paid.

## The fee

`bridgerpc.spread` is your fee on top of the rate, as a fraction, for both
directions: 0.015 (1.5%) by default. `bridgerpc.fee.tosha256` and
`bridgerpc.fee.toblake2b` set one direction each, over it. Routing fees come out
of it (the bridge budgets 0.3% of each payout), so a fee must be above that.

The fee is a floor. The bridge charges more on the side it is running low on,
up to three times the fee near empty, and less, never below the routing budget
plus 0.1%, on the side that refills it.

Why 1.5%: payers allow 5% over the market by default, plus what a quote's
spread is above 1.5% (at most 5%), so a bridge running low (three times 1.5%
is 4.5%) or widened for the market is still paid. Payers measure from the
venues' tickers and the bridge prices from the side of the book a swap
trades against at its largest swap, so what payers see is your fee plus
about half the book's spread and what the book's depth costs, less what the
last hour's range and the spread add to their allowance; a base fee far above
1.5% counts against the premium and can be refused when the book is wide. Much below 1%, what is left after
routing is smaller than an ordinary one-minute move of this market, and quotes
funded later in their minute are given back more often. A higher fee in one
direction makes sense when you want the coin the other one brings in.

## Routes

Before quoting a payout, the bridge asks the node that will pay it (its SHA256
node, or this node for a payout on this chain) for a route to the invoice's
destination within two budgets, and asks again just before paying:

- **Fees:** 0.3% of the payout, and at least 10 sats; it comes out of the fee.
  A payout too small for its fee to cover that is refused.
- **Time-lock:** at most 386 blocks for a payout on the SHA256 chain (218 on
  this chain), or fewer when the chains' current pace allows less;
  `bridgerpc.outgoingcltvlimit` changes the ceiling for both. A value from
  an earlier version above about 218 with `toblake2b` on now stops the node
  starting, and says so: lower it, or leave it out.

With no such route the quote is refused (`no_route`) and nothing is held.
With one, the swap is sized to that route plus 40 blocks of slack, not to the
whole budget: the payer's payment is held for as long as the bridge's could
take to resolve, so a short route asks a short hold.

The search finds single paths, and a payout can be split over several. When
no single path carries a payout whose invoice allows splitting (two 100k
channels and a 150k payout), it is quoted if the paying node's spendable
balance on its active channels covers the payout and its fee budget, and the
smallest part lnd would split it into finds a path (lnd halves a part that
finds none, down to 10,000 sat, and does not split a payout under 20,000 sat
at all), as does a sixteenth of the payout when that is more (lnd splits into
at most sixteen parts). It is sized to the whole time-lock budget, since the parts may take
longer paths than the one found. A
funded swap with no route when it comes to pay (channels still coming up
after a restart, say) waits for one, and is given back if none appears in
time.

The budgets are the largest the bridge's timing safety allows. The payer's
hold must outlast the payout even if this chain's blocks come three times
faster than measured and the SHA256 chain's half again slower (the slowest
four days in its history, during the 2021 mining ban, ran 1.38 times what
was measured as they began), plus an hour to settle. Only the blocks before
the bridge's node would give up on the payer's held payment count: lnd
cancels an accepted hold invoice 18 blocks before its HTLC expires, so the
quote asks the payer for those on top. A node set to give up earlier must
say so: this node's `invoices.holdexpirydelta` is read, and for an SHA256
node of your own (which holds the payer's payment when the bridge pays
BLAKE2b invoices) set `bridgerpc.sha256.holdexpirydelta` to its value. The
supervised SHA256 node keeps lnd's default. A larger delta leaves less room
for the route, so each quote's route budget shrinks to fit. The payer's
hold is capped at 1,776 blocks, leaving room for their own route to the
bridge inside the 2,016 blocks every lnd allows. SHA256 routes that need
more than 386 blocks cannot be paid; a bridge node with a channel to a
well-connected peer reaches most destinations in far fewer.

## Participants

Each person you serve gets a **bridge code**: a credential that can only ask
this node for prices and quotes, under a root key of its own.

```
lncli bridge code --url https://<this node's onion>:8080
```

prints the code and its root key id. The code carries this node's alias as
the name the participant sees for the service (`--label` to give another);
keep your own note of whom each root key id is for. They add it in their dashboard under
"Paying SHA256 invoices". To revoke it:

```
lncli deletemacaroonid <root_key_id>
```

The `--url` is wherever their node reaches this node's REST port. An onion
needs nothing more, because Tor authenticates it; any other address also needs
`--cert`, the SHA-256 of the certificate it presents.

Each participant is limited on their own: how many quotes they may have unpaid
at once (`bridgerpc.participant.maxunpaid`, default 2), how many swaps
unfinished (`participant.maxinprogress`, 4), and how many quotes a minute
(`participant.perminute`, 6). An unpaid quote reserves your liquidity and costs
them nothing, which is why the first one matters. Your admin macaroon is not
limited. Any other macaroon of this node's own (one you baked for an app, say)
is counted as one participant, all of them together.

A participant sees the most each direction can pay rounded down to two
significant figures, and a quote of theirs above that is refused as if there
were no more, so asking for quotes tells them no more about your balance than
that. Your admin macaroon sees and quotes the exact amount.

A participant code is one payer's credential. Everyone holding it shares its
limits (2 unpaid quotes, 4 swaps in progress, 6 quotes a minute), so one
visitor can use them up for all. Don't publish a code; give each person or app
its own.

The calls a participant's wallet makes, the checks it must run before paying,
and the refusal codes it can receive are described in `docs/payer-api.md` in
the `lightning-fork-bridge` repository.

## Back these up

The bridge keeps two files beside each other, by default under
`bridge/` in the network directory:

- `swaps.journal` records every swap, including what was paid and the
  preimages that claim it. Losing it while a swap is running loses the
  record of an HTLC that is still out there. Every hour it is rewritten with
  one line per swap: each finished swap keeps its last state, which is what
  lets the bridge tell whether an invoice it has seen before may be quoted
  again.
- `rate.json` is the rate in force and when it was set.

Back up the SHA256 node's channels as you would any other node's.
