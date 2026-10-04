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
- A Lightning node on the SHA256 chain, with outbound capacity on its
  channels. Either:
  - **one Lightning Fork runs for you** (`bridgerpc.sha256.supervised`): a
    stock lnd the platform starts, whose seed is derived from this node's,
    so there is nothing new to write down. It reads the full node on the
    SHA256 chain you already run and starts empty. See
    [bridge-sha256-node.md](bridge-sha256-node.md); or
  - **one you already run**: a stock lnd (v0.21.3-beta or later) with the
    `chainrpc` sub-server (release builds have it), its admin macaroon and
    TLS certificate.

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

With an LND you already run:

```
bridgerpc.enabled=true
bridgerpc.tosha256=true
bridgerpc.sha256.rpchost=10.0.0.5:10009
bridgerpc.sha256.tlscertpath=/path/to/sha256-node/tls.cert
bridgerpc.sha256.macaroonpath=/path/to/sha256-node/admin.macaroon
```

Either way the bridge trades at the market's rate, read live (below), and
charges 1.5% on top of it each way.

`tosha256` serves payers on this chain paying SHA256 invoices; `toblake2b` the
reverse, which starts once there is a rate. The node refuses to start the bridge with a combination of
settings that would refuse every swap, and says which numbers to change.

`lncli bridge status` says whether the bridge can serve swaps right now and,
if not, why, and how far the SHA256 node has got (`sha256_node`). If the SHA256
node cannot be reached, or turns out not to be a stock lnd on the SHA256 chain
on this node's network, the bridge stays down and tries again every minute
(every ten seconds while a SHA256 node Lightning Fork runs is coming up)
while this node runs as usual. `needs_operator`
lists swaps that need you: one that ended lost (paid out, and the payment
coming in could not be claimed), or one the bridge stopped driving because it
must not decide it alone. Watch that list. It is filled while the bridge is
on, and while it is off but still finishing swaps. `unfinished` counts the
swaps the journal has not finished (a lost one is final and not counted),
whatever the bridge's state: wait for it to be 0 before turning the bridge
off or moving it to another SHA256 node.

## The rate

The rate is SHA256 coin per BLAKE2b coin. By default
(`bridgerpc.ratesource=neoxa`) the bridge reads it from Neoxa every 30
seconds, through Tor when this node uses Tor:

- **The price** is Neoxa's BTCB2_BTC market, the middle of its last trade, best
  bid and best ask, so that one trade off the book does not move it. It is
  what payers' wallets check a quote against, so the premium they see is your
  fee. The bridge trades at the median of the last few readings.
- **The cross-check** is Neoxa's BTCB2_USDC market over a BTC/USD price
  (CoinGecko's, or Neoxa's own BTC_USDC market). If the two are more than 5%
  apart, or the cross-check cannot be read, nothing is quoted.
- **No market, no quotes.** With no reading in the last 90 seconds, or a
  ticker that has stopped updating, nothing is quoted. A move of 20% within
  ten minutes stops quoting for half an hour.
- **Your fee is widened by the market's own movement** over the last ten
  minutes, as payers' wallets allow for it.
- **Just before paying,** the bridge checks the swap again at the rate then. If
  the market has moved against it by more than the fee since the quote, what
  the payer sent is given back instead of paying out at a loss.

`lncli bridge status` shows the rate, the cross-check (`rate_cross_check`),
the market's movement (`rate_volatility`) and, when nothing is quoted, why.

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

Payers' wallets check every quote against Neoxa and, by default, refuse one
more than 5% above it (widened by the last hour's range), so a rate set far
from the market is refused rather than paid.

## The fee

`bridgerpc.spread` is your fee on top of the rate, as a fraction, for both
directions: 0.015 (1.5%) by default. `bridgerpc.fee.tosha256` and
`bridgerpc.fee.toblake2b` set one direction each, over it. Routing fees come out
of it (the bridge budgets 0.3% of each payout), so a fee must be above that.

The fee is a floor. The bridge charges more on the side it is running low on,
up to three times the fee near empty, and less, never below the routing budget
plus 0.1%, on the side that refills it.

Why 1.5%: payers allow 5% over the market by default, and three times 1.5% is
4.5%, so a bridge running low is still paid. Much below 1%, what is left after
routing is smaller than an ordinary two-minute move of this market, and quotes
funded a minute or two later are given back more often. A higher fee in one
direction makes sense when you want the coin the other one brings in.

## Participants

Each person you serve gets a **bridge code**: a credential that can only ask
this node for prices and quotes, under a root key of its own.

```
lncli bridge code --url https://<this node's onion>:8080 --label "Alice"
```

prints the code and its root key id. They add it in their dashboard under
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
them nothing, which is why the first one matters. Your own macaroons are not
limited.

The calls a participant's wallet makes, the checks it must run before paying,
and the refusal codes it can receive are described in `docs/payer-api.md` in
the `lightning-fork-bridge` repository.

## Back these up

The bridge keeps two files beside each other, by default under
`bridge/` in the network directory:

- `swaps.journal` records every swap in flight, including what was paid and
  the preimages that claim it. Losing it while a swap is running loses the
  record of an HTLC that is still out there.
- `rate.json` is the rate in force and when it was set.

Back up the SHA256 node's channels as you would any other node's.
