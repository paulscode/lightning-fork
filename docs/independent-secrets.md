# Independent per-commitment secrets

Lightning Fork supports `option_independent_secrets` (feature bits 266/267,
proposed in a draft bLIP).
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

- **Signalled by default.** The optional bit 267 is set in `init` and
  `node_announcement`. A new channel uses the add-on when both peers signal it
  and agree on it in `channel_type`. With a peer that doesn't signal it, the
  channel opens the normal way.
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

`lncli listchannels` and `lncli pendingchannels` report the add-on per channel
as `independent_secrets`.

## Turning it off

```
[protocol]
protocol.no-independent-secrets=true
```

New channels then keep requiring shachain secrets. Channels already opened with
the add-on keep it.

## Downgrading

Downgrading to a version of Lightning Fork without this change is not
supported once any channel has used the add-on, because such a version can't
read the channel's stored state. lnd keeps a copy of a closed channel's state,
so that stays true after the channel closes.

To make that fail clearly, the database moves to version 39 when it first
stores a channel with the add-on, whether new or restored from a backup. An
older version then refuses to start with a clear error ("Refusing to revert
from db_version=39"). Without that, it would fail to decode the channel while
the channel is open or still being resolved, and once it has closed it would
start and fail its `closedchannels` and `pendingchannels` calls. A node that
never had such a channel stays at the version before, and can still go back
to an older release.

## Testing against it

Integration builds (`-tags integration`) have a dev option,
`--dev.independent-secrets-sender`, that makes this node generate its own
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
