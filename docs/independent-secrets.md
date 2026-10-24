# Independent per-commitment secrets

Lightning Fork supports `option_independent_secrets` (feature bits 266/267,
specified in the fork's BOLTs,
[lightning-blake2b/bolts#17](https://github.com/lightning-blake2b/bolts/pull/17)).
On a channel with it, this node accepts per-commitment secrets from its peer
that do not come from a shachain, and stores every one of them.

## Why

Each time a channel updates, each side reveals a secret that cancels its old
state. If it later broadcasts that old state anyway, the other side uses the
secret to take the whole channel.

BOLT 3 requires those secrets to come from one seed (a shachain), so the
receiver can store them in 49 entries. A node whose keys are held k-of-n then
has to give that seed to every signer, and any one signer can pass it to the
channel peer, which can then steal the channel the moment the node broadcasts
its current state. Secrets that need not come from one seed can be generated
jointly by the signers, so no single signer ever holds them.

This node's part is to accept such secrets. It does not change how this node
generates its own: they still come from its shachain.

## What it does

- **Off by default.** With `protocol.independent-secrets` set, the optional
  bit 267 is set in `init` and `node_announcement`. A new channel uses the
  add-on when both peers signal it and agree on it in `channel_type`. With a
  peer that doesn't signal it, the channel opens the normal way.
- **The even bit, 266,** is only valid in `channel_type`. A peer that sets it
  in `init` is refused, and a node that sets it in `node_announcement` is
  treated as requiring a feature lnd doesn't know: it isn't paid from its
  announcement or routed through.
- **Storage.** Each secret is stored with the revocation log entry of the
  commitment it revokes, written in the same database transaction. That's 38
  bytes per revoked state (a 32-byte secret in a TLV record), next to data the
  node keeps for every revoked state anyway.
- **Checks.** A secret must generate the point of the commitment it revokes and
  be a valid private key. It doesn't have to be related to earlier secrets.
- **Breaches.** When the chain watcher sees a revoked state on chain, it builds
  the breach retribution from that state's revocation log entry, secret
  included; the watchtower client does the same for its backups.
- **Splices.** While a splice is pending, the peer has a commitment on each
  funding output at the same number, and one secret revokes all of them. The
  secret is stored with each of their log entries, the splice's own included.
- **Static channel backups** are unchanged. They never carried revocation
  state, so a node restored from one can't punish a breach, with or without
  the add-on.
- **Existing channels** keep their type and behave exactly as before.

`lncli listchannels`, `lncli pendingchannels` and `lncli closedchannels`
report the add-on per channel as `independent_secrets`. The channel acceptor
RPC gets `wants_independent_secrets` for a proposed channel, so an acceptor
can see the add-on and refuse it per channel; its `commitment_type` leaves the
bit out, as it does for `option_unified_sigs`.

## Turning it on

```
[protocol]
protocol.independent-secrets=true
```

Turning it off again only affects new channels: those already opened with the
add-on keep it.

## Downgrading

Downgrading to a version of Lightning Fork without this change is not
supported once any channel has used the add-on, because such a version can't
read the channel's stored state. lnd keeps a copy of a closed channel's state,
so that stays true after the channel closes.

To make that fail clearly, the database moves to version 39 when it first
stores a channel with the add-on. That includes the pending channel saved
when a peer starts an open with us, before any funding transaction exists, so
with the option on, a peer that offers it can mark the database just by
starting an open, even one it never completes. An older version then refuses
to start with a clear error ("Refusing to revert from db_version=39").
Without that, it would fail to decode the channel while
the channel is open or still being resolved, and once it has closed it would
start and fail its `closedchannels` and `pendingchannels` calls. A node that
never had such a channel, even a pending one, stays at the version before, and
can still go back to an older release. That's why the add-on is off by
default for now: a node that leaves it off can't be marked.

## Testing against it

Integration builds (`-tags integration`) have a dev option,
`--dev.independent-secrets-sender` (which needs `protocol.independent-secrets`),
that makes this node generate its own
secrets on new channels with the add-on using a key derivation function
instead of a shachain:

```
secret(i) = HMAC-SHA256(root, "independent-per-commitment-secret" ||
                              i as 8 bytes big-endian || counter as 1 byte)
```

The counter starts at 0 and only goes up in the negligible case that the
result is not a valid private key. That exercises the peer's support for
independent secrets with a real node. The secrets are regenerated from the
root lnd already stores; the channel records that it uses them in a local
channel type bit, so it keeps using them for its whole life, even if the
option is later turned off. It is not available in release builds: a single
signer gains nothing from it.

A node restored from a static channel backup gets a shachain producer back,
since the backup doesn't record that the dev sender was used. Such a node
can't regenerate the secrets it revealed on the restored channel, so it can't
sweep its own `to_local` output with an external tool after a force close.
That only matters for testing: release builds don't have the dev sender.
