# BOLT 12 offers

Lightning Fork can mint BOLT 12 offers, answer invoice requests for them
over onion messages, and fetch and pay other nodes' offers. The first of
these is what a miner needs to be paid over Lightning by a pool that pays to
offers, the way OCEAN does on the SHA256d chain and CONVOY is expected to on
this one.

Everything here is native to the node. There is no side-car and nothing to
run besides `lnd`.

## Being paid by a pool

1. Mint an offer with the description the pool asks for. OCEAN wants
   `OCEAN Payouts for <address>`, where the address is the payout address
   registered with the pool; a pool on this chain will say what it wants.
   Leave the amount out so the pool chooses it.

   ```
   lncli offer create --description "OCEAN Payouts for bc1q..."
   ```

   The response carries the offer as an `lno1...` string. Give that to the
   pool. The offer names this chain, so a node on another chain cannot use
   it.

2. Keep the node reachable. The pool sends its invoice requests over onion
   messages, which travel through Lightning peers. The node needs a channel
   with a peer that speaks onion messages, and that peer needs to be
   connected when a payout comes. If the node announces no address, the
   offer carries a blinded path through such a peer, so the pool can reach
   the node without knowing it; the node picks that peer when the offer is
   minted, and re-minting the offer later picks again.

3. Have a channel with inbound capacity for the payouts. A payout is an
   ordinary Lightning payment to an invoice the node issues for the request,
   with blinded payment paths so the pool never learns the node's channels.

Run `lncli offer invoices` to see the invoices issued for the offer and
whether each was settled, and `lncli offer list` for the count.

## The same offer after a restore

An offer's identity is a function of its fields under a secret the node
derives from its seed. Minting an offer with the same description again
after restoring the node from seed produces the same `lno1...` string, and
the response says `"created": false`. An offer with blinded paths comes out
the same as long as the same peers are there to be its introduction nodes;
with other peers it is a new offer, and the old one still works. A request for an offer the node no
longer has a record of, but which verifies as its own, is also served and
the record restored. So a pool keeps paying to the string it already holds.

Channel state is not part of this: a restored node still needs a channel to
receive on, as with any Lightning node. Nor is whether an offer had been
disabled: an offer restored from a request comes back enabled, and the log
says so.

## Paying an offer

```
lncli offer fetchinvoice lno1... --amount_msat 100000
lncli offer pay lno1... --amount_msat 100000
```

`fetchinvoice` sends an invoice request to the offer's issuer and returns
the signed invoice once it has been checked against the request; nothing is
paid. `pay` does the same and then pays the invoice, or pays an invoice
fetched earlier with `--invoice lni1...`. An offer with an amount needs no
`--amount_msat`; giving one raises the amount, as a tip, but cannot lower
it. An offer that sells by quantity takes `--quantity`.

A payment that fails may still settle, as any Lightning payment may, so a
failed `pay` reports the payment hash to track and the invoice it was
paying. Retry with `--invoice` and that invoice, not with the offer: paying
the offer again fetches a new invoice, and a retry that settles alongside
the first would pay twice. Paying the same invoice again after it settled
returns the first payment's preimage. Automation should fetch once and pay
the invoice.

The node needs a route to the issuer's introduction node for the request,
which it finds through the graph or, for a fetch it makes itself, by
connecting to the node at its announced address.

When one of the invoice's payment paths starts at this node, because this
node is the issuer's channel peer, the node processes its own hop of the
path itself and pays on through that channel. Such a payment is sent as a
single part.

## Decoding

```
lncli offer decode lno1...
```

decodes an offer, an invoice request (`lnr1...`) or an invoice (`lni1...`)
and reports whether it names this chain, whether it passes the checks a
payer makes, and whether this node minted it. An offer that names no chain
is a Bitcoin mainnet offer and is reported as not for this chain.

## What the node does with a request

A request is served only when the offer it mirrors verifies as this node's,
it arrived the way the offer requires (over one of the offer's blinded
paths when it has them, and not over a blinded path when it has none), and
the offer is enabled and unexpired. A request that fails a check gets an
`invoice_error` with a short message; one for an offer that is not this
node's, or that came the wrong way, is ignored. Requests are answered at
most ten a second across all peers, and a repeated request gets its earlier
invoice back rather than a new one, while that invoice lasts.

Requests without a reply path are ignored, since nothing could be sent
back, and requests are answered at most five a second across all peers and
one a second from any one peer. Invoices issued for offers are payable for
an hour and carry the node's usual blinded payment paths, under the
`routing.blinding.*` settings. Each invoice is an invoice in the node's
registry like any other; the records the offer layer keeps for unpaid ones
are dropped a week after they expire, and
`gc-canceled-invoices-on-the-fly` keeps the registry from holding on to
cancelled ones.

## Settings

Onion messages are on by default. With `protocol.no-onion-messages` the
node still mints offers but cannot serve or pay them: it says so at start,
and fetching or paying is refused with "onion messages are disabled".

The sub-server is built with the `offersrpc` build tag, which the release
builds carry. Its RPCs are under `offersrpc.Offers`, and over REST under
`/v2/offers`. The macaroon entities are `invoices` for minting, listing and
decoding, `offchain` read for fetching and `offchain` write for paying.

## Compatibility

The offer, request and invoice formats follow BOLT 12 as implemented in
LND, Core Lightning and LDK, with this chain's hash in `offer_chains` and
`invreq_chain`. The onion messages follow BOLT 4. Which pools pay to offers
on this chain, and what description they ask for, is the pool's to say.
