# BOLT 12 offers for `lnd`

Seven commits against `v0.21.3-beta` that let a node mint BOLT 12 offers,
answer invoice requests for them over onion messages, and fetch and pay other
nodes' offers. No side-car, no external daemon: the offer manager, the onion
messenger and the RPC live in `lnd`.

Apply with `git am 000*.patch` onto `v0.21.3-beta`.

## Why

A pool that pays its miners over Lightning needs somewhere reusable to pay.
A BOLT 11 invoice is single-use, so a recurring payout needs the payee to
produce a fresh one every cycle, which is exactly the thing an unattended
miner cannot do. A BOLT 12 offer is the reusable form, and OCEAN already pays
to offers today.

`lnd` has the wire types for blinded paths on master but no offers, no
`invoice_request` handling and no way to serve one. Today a node that wants to
be paid by such a pool has to run Core Lightning alongside it. These commits
close that gap.

## The commits

1. **`bolt12`: port the BOLT 12 codecs from LND master.** Offer, invoice
   request and invoice encoding, the `lno1`/`lnr1`/`lni1` bech32 forms, merkle
   signing and the TLV additions they need. Additive, all in `lnwire` and a new
   `bolt12` package.

2. **`offers`: mint, list and decode BOLT 12 offers.** An offer store, the
   issuer key derivation, and `lncli offer mint | list | decode`. The RPC is
   behind an `offersrpc` build tag in the usual way, with the default stub
   alongside it.

3. **`onionmsg`: carry BOLT 12 messages over onion messages.** The messenger,
   reply paths, the invoice-error path and pathfinding for blinded routes.

4. **`offers`: serve invoice requests and pay offers over onion messages.**
   The two halves that make an offer useful: answering a request with an
   invoice, and fetching an invoice from someone else's offer and paying it.

5. **Pay through blinded paths that start at this node, in the router.** A
   blinded path whose introduction node is the payer needs the router to treat
   the first hop as already reached.

6. **Issue an offer invoice with a path from this node when no peer can start
   one.** Where no peer can act as an introduction node — every peer's only
   channel is the one to this node, or none signals route blinding — the
   invoice would otherwise be unissuable. It falls back to a path starting
   here, which always exists, and reports that it did.

7. **Take onion messages from peers without channels, and reply where the
   request went.** A payer fetching an offer often has no channel with the
   issuer. Gating onion messages on having one makes offers unusable for the
   case they exist to serve. The gate is configurable
   (`protocol.onion-msg-channel-gate`) and global and per-peer rate limiters
   bound what any peer can send.

## Provenance and testing

These were developed in a fork of `lnd` that follows a different chain, and are
in production use there. **Nothing chain-specific is included here**: the BOLT
12 code touches no chain hashing and no chain identity, and the series was
rebased onto `v0.21.3-beta` and de-forked for review. The one adaptation needed
was mechanical — the fork distinguishes a chain hash from a genesis hash
because its chain shares Bitcoin's genesis block, and on Bitcoin the two are
the same value, so `cfg.ActiveNetParams.GenesisHash` is used directly.

Every commit in the series builds on its own, so the history bisects.
`go build ./...`, `go vet` and `gofmt` are clean, and the tests pass for
`bolt12`, `offers`, `offerserve`, `offerpay`, `onionmsg`, `onionmessage`,
`lnwire`, `routing`, `routing/route`, `lncfg`, `peer`, `lnrpc/routerrpc` and
`cmd/commands`.

Interoperability has been exercised against Core Lightning in a regtest
harness: the two peer both ways, open channels from each side, and pay each
other's BOLT 12 offers.

## Known gaps

- Offers are served and paid; **recurrence** (BOLT 12's `offer_recurrence`) is
  not implemented.
- The RPC is behind a build tag, matching how other optional subservers ship.
  Whether it should be on by default is a maintainer call.
- `docs/bolt12-offers.md` documents the operator-facing surface.
