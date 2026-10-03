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
- A stock lnd (v0.21.3-beta) on the SHA256 chain, with outbound capacity on its
  channels and the `chainrpc` sub-server (release builds have it). Its admin
  macaroon and TLS certificate.

## Turning it on

```
bridgerpc.enabled=true
bridgerpc.tosha256=true
bridgerpc.sha256.rpchost=10.0.0.5:10009
bridgerpc.sha256.tlscertpath=/path/to/sha256-node/tls.cert
bridgerpc.sha256.macaroonpath=/path/to/sha256-node/admin.macaroon
bridgerpc.fixedrate=0.00483
```

`tosha256` serves payers on this chain paying SHA256 invoices; `toblake2b` the
reverse. `fixedrate` is SHA256 coin per BLAKE2b coin and has no default: a
wrong rate loses money on every swap and does it quietly. The node refuses to
start the bridge with a combination of settings that would refuse every swap,
and says which numbers to change.

`lncli bridge status` says whether the bridge can serve swaps right now and,
if not, why. If the SHA256 node cannot be reached, or turns out not to be a
stock lnd on the SHA256 chain on this node's network, the bridge stays down
and tries again every minute while this node runs as usual. `needs_operator`
lists swaps that need you: one that ended lost (paid out, and the payment
coming in could not be claimed), or one the bridge stopped driving because it
must not decide it alone. Watch that list.

## The rate

```
lncli bridge setrate 0.00490
```

takes effect at once, survives restarts and is stamped with when it was set.
Once it is older than `bridgerpc.ratemaxage` (24 hours by default) the bridge
stops quoting until you set it again, because a rate nobody has looked at in a
moving market is a loss waiting to be taken. Changing `bridgerpc.fixedrate` in
the configuration and restarting also sets it.

Payers' wallets check every quote against a public market price and refuse one
too far above it, so a rate set far from the market is refused rather than
paid.

## Participants

Each person you serve gets a **bridge code**: a credential that can only ask
this node for prices and quotes, under a root key of its own.

```
lncli bridge code --url https://<this node's onion>:8080 --label "Alice"
```

prints the code and its root key id. They add it in their dashboard under
"Paying Bitcoin invoices". To revoke it:

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
